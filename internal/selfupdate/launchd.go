package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UpdaterInterval is the updater LaunchDaemon's StartInterval. The router
// warns when a Mac's updater has not checked in for 3 intervals.
const UpdaterInterval = 15 * time.Minute

// Roles of the launchd jobs that run the agent binary.
const (
	JobAgent    = "agent"    // <label>: the agent (as the service user)
	JobUpdater  = "updater"  // <label>.updater: this updater (root)
	JobRuntimes = "runtimes" // <label>.runtimes: the managed runtimes helper (root)
)

// Job is the launchd view of one job that runs the agent binary.
type Job struct {
	Role  string
	Label string
	Plist string
	// Loaded: launchd knows the job. Running: it has a process now.
	Loaded  bool
	Running bool
	State   string
	// Stale: launchd holds a launch constraint (LWCR) for the job that the
	// binary on disk no longer meets, so the next spawn fails (or the job
	// should be loaded and is not). Why says what was seen.
	Stale bool
	Why   string
}

// Service manages the launchd jobs that run the agent binary.
type Service interface {
	// Restart re-registers the agent's job (bootout + bootstrap) so it
	// starts the binary on disk with a fresh launch constraint.
	Restart(ctx context.Context) error
	// Jobs inspects the agent, updater and runtimes helper jobs.
	Jobs(ctx context.Context) ([]Job, error)
	// Reregister reloads one job. The updater's own job cannot be booted
	// out by the process it runs, so that reload is handed to a detached
	// process that waits for this one to exit.
	Reregister(ctx context.Context, j Job) error
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Launchctl manages the jobs with launchctl(1) in the system domain.
//
// Why re-register instead of kickstart: on Macs where Background Task
// Management tracks the LaunchDaemons, launchd attaches a lightweight code
// requirement (LWCR) to each job that pins the binary's cdhash. Replacing
// the binary leaves that constraint stale ("needs LWCR update"); the next
// spawn of the job fails with EX_CONFIG (78, "spawn failed"). kickstart
// keeps the old registration; bootout + bootstrap makes launchd and BTM
// evaluate the binary afresh.
type Launchctl struct {
	Path  string // default /bin/launchctl
	Label string // the agent's label; the updater is <Label>.updater, the helper <Label>.runtimes
	// Plist is the agent's plist; the other plists are looked for next to
	// it. Without a plist the jobs are only kickstarted.
	Plist string
	// Bin is the agent binary (for its cdhash); empty skips that check.
	Bin      string
	Codesign string // default /usr/bin/codesign
	Run      Runner // default: exec
	// Detach starts a shell script in its own session that outlives this
	// process (default: /bin/sh with setsid).
	Detach func(script string) error
	Sleep  func(ctx context.Context, d time.Duration)
}

func (l Launchctl) bin() string {
	if l.Path != "" {
		return l.Path
	}
	return "/bin/launchctl"
}

func (l Launchctl) run(ctx context.Context, args ...string) ([]byte, error) {
	r := l.Run
	if r == nil {
		r = execRunner
	}
	return r(ctx, l.bin(), args...)
}

func (l Launchctl) sleep(ctx context.Context, d time.Duration) {
	if l.Sleep != nil {
		l.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Restart re-registers the agent's job. Without a plist (or when it is
// missing) it falls back to kickstart -k.
func (l Launchctl) Restart(ctx context.Context) error {
	if l.Plist == "" || !fileExists(l.Plist) {
		out, err := l.run(ctx, "kickstart", "-k", "system/"+l.Label)
		if err != nil {
			return fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return l.reload(ctx, l.Label, l.Plist)
}

// reload boots a job out (stopping it), waits until launchd has forgotten
// it, then bootstraps it again from its plist.
func (l Launchctl) reload(ctx context.Context, label, plist string) error {
	svc := "system/" + label
	_, _ = l.run(ctx, "bootout", svc) // fails when not loaded: fine
	for i := 0; ; i++ {
		if _, err := l.run(ctx, "print", svc); err != nil {
			break
		}
		if i >= 100 || ctx.Err() != nil {
			return fmt.Errorf("%s did not unload", label)
		}
		l.sleep(ctx, 200*time.Millisecond)
	}
	return l.bootstrap(ctx, label, plist)
}

// bootstrap loads a plist, retrying: right after a bootout launchd can
// still refuse it for a moment ("Bootstrap failed: 5: Input/output error").
func (l Launchctl) bootstrap(ctx context.Context, label, plist string) error {
	var err error
	var out []byte
	for i := 0; i < 10; i++ {
		if out, err = l.run(ctx, "bootstrap", "system", plist); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		l.sleep(ctx, time.Second)
	}
	return fmt.Errorf("launchctl bootstrap %s: %v: %s", label, err, strings.TrimSpace(string(out)))
}

// Jobs inspects the jobs whose plists exist. Without an agent plist it
// returns nothing (nothing to re-register from).
func (l Launchctl) Jobs(ctx context.Context) ([]Job, error) {
	if l.Plist == "" {
		return nil, nil
	}
	dir := filepath.Dir(l.Plist)
	cdhash := ""
	if l.Bin != "" {
		cdhash = l.cdhash(ctx)
	}
	var jobs []Job
	for _, j := range []Job{
		{Role: JobAgent, Label: l.Label, Plist: l.Plist},
		{Role: JobUpdater, Label: l.Label + ".updater", Plist: filepath.Join(dir, l.Label+".updater.plist")},
		{Role: JobRuntimes, Label: l.Label + ".runtimes", Plist: filepath.Join(dir, l.Label+".runtimes.plist")},
	} {
		if !fileExists(j.Plist) {
			continue
		}
		out, err := l.run(ctx, "print", "system/"+j.Label)
		if err != nil {
			// Not loaded. The agent should always be; the helpers are
			// loaded by prepare / the installer.
			if j.Role == JobAgent {
				j.Stale, j.Why = true, "not loaded"
			}
			jobs = append(jobs, j)
			continue
		}
		p := ParseLaunchdPrint(string(out))
		j.Loaded, j.State = true, p.State
		j.Running = p.State == "running" || p.PID > 0
		j.Stale, j.Why = p.staleFor(cdhash)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// Reregister reloads a job; the updater's own job is reloaded by a detached
// shell once this process has exited.
func (l Launchctl) Reregister(ctx context.Context, j Job) error {
	if j.Role == JobUpdater {
		return l.reloadLater(j.Label, j.Plist)
	}
	if !j.Loaded {
		return l.bootstrap(ctx, j.Label, j.Plist)
	}
	return l.reload(ctx, j.Label, j.Plist)
}

// reloadLater boots the updater's job out and in again from a process in
// its own session (setsid), so neither the end of this process nor the
// bootout takes it down. It waits for this process to exit first (at most
// 30 min), so a cycle is never cut short. RunAtLoad then starts a fresh
// cycle with the new registration.
func (l Launchctl) reloadLater(label, plist string) error {
	detach := l.Detach
	if detach == nil {
		detach = detachShell
	}
	return detach(ReloadScript(l.bin(), label, plist, os.Getpid()))
}

// ReloadScript is the detached shell script that re-registers a job once
// process pid has exited.
func ReloadScript(launchctl, label, plist string, pid int) string {
	lc, svc, pl := shQuote(launchctl), shQuote("system/"+label), shQuote(plist)
	return `trap '' HUP INT TERM
i=0
while kill -0 ` + strconv.Itoa(pid) + ` 2>/dev/null && [ "$i" -lt 1800 ]; do i=$((i + 1)); sleep 1; done
sleep 2
` + lc + ` bootout ` + svc + ` 2>/dev/null
i=0
while ` + lc + ` print ` + svc + ` >/dev/null 2>&1 && [ "$i" -lt 100 ]; do i=$((i + 1)); sleep 0.2; done
n=0
until ` + lc + ` bootstrap system ` + pl + `; do
  n=$((n + 1)); [ "$n" -lt 10 ] || { echo "umm-updater: could not reload ` + label + `" >&2; exit 1; }
  sleep 1
done
echo "umm-updater: reloaded ` + label + ` (launch constraint refresh)" >&2
`
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var cdhashLine = regexp.MustCompile(`(?m)^CDHash=([0-9a-fA-F]{40,64})\s*$`)

// cdhash returns the binary's code directory hash ("" if unsigned or
// codesign is not available).
func (l Launchctl) cdhash(ctx context.Context) string {
	cs := l.Codesign
	if cs == "" {
		cs = "/usr/bin/codesign"
	}
	r := l.Run
	if r == nil {
		r = execRunner
	}
	out, err := r(ctx, cs, "-d", "-vvv", l.Bin)
	if err != nil {
		return ""
	}
	if m := cdhashLine.FindSubmatch(out); m != nil {
		return strings.ToLower(string(m[1]))
	}
	return ""
}

// LaunchdPrint is what the updater reads from `launchctl print system/<label>`.
// The format is not a stable interface; unknown output parses as empty.
type LaunchdPrint struct {
	State      string
	PID        int
	Properties []string
	LastExit   string
	// LWCRCDHashes are the cdhashes a launch constraint allows, lower case.
	LWCRCDHashes []string
}

var hexHash = regexp.MustCompile(`[0-9a-fA-F]{40,64}`)

// ParseLaunchdPrint parses `launchctl print` output.
func ParseLaunchdPrint(out string) LaunchdPrint {
	var p LaunchdPrint
	depth := 0
	jobState := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		top := depth <= 1
		switch {
		case strings.HasPrefix(t, "job state = "):
			jobState = strings.TrimPrefix(t, "job state = ")
		case top && strings.HasPrefix(t, "state = "):
			p.State = strings.TrimPrefix(t, "state = ")
		case top && strings.HasPrefix(t, "pid = "):
			p.PID, _ = strconv.Atoi(strings.TrimPrefix(t, "pid = "))
		case top && strings.HasPrefix(t, "last exit code = "):
			p.LastExit = strings.TrimPrefix(t, "last exit code = ")
		case top && strings.HasPrefix(t, "properties = "):
			for _, f := range strings.Split(strings.TrimPrefix(t, "properties = "), "|") {
				if f = strings.TrimSpace(f); f != "" {
					p.Properties = append(p.Properties, f)
				}
			}
		}
		depth += strings.Count(t, "{") - strings.Count(t, "}")
	}
	if jobState != "" {
		p.State = jobState
	}
	// The constraint looks like `reqs: {cdhash: {$in: [<hash>, ...]}}`
	// (or `cdhash: <hash>`): take the hashes up to the closing brace.
	lower := strings.ToLower(out)
	for i := 0; ; {
		k := strings.Index(lower[i:], "cdhash")
		if k < 0 {
			break
		}
		start := i + k + len("cdhash")
		end := strings.IndexByte(lower[start:], '}')
		if end < 0 {
			end = len(lower) - start
		}
		for _, h := range hexHash.FindAllString(lower[start:start+end], -1) {
			p.LWCRCDHashes = append(p.LWCRCDHashes, h)
		}
		i = start
	}
	return p
}

func (p LaunchdPrint) has(prop string) bool {
	for _, f := range p.Properties {
		if strings.EqualFold(f, prop) {
			return true
		}
	}
	return false
}

// staleFor says whether the job's launch constraint no longer matches a
// binary with this cdhash ("" = unknown).
func (p LaunchdPrint) staleFor(cdhash string) (bool, string) {
	if p.has("needs LWCR update") {
		return true, "launchd: needs LWCR update"
	}
	if cdhash != "" && len(p.LWCRCDHashes) > 0 && !matchesAny(cdhash, p.LWCRCDHashes) {
		return true, "launch constraint pins another cdhash than the binary's (" + shortHash(cdhash) + ")"
	}
	if strings.Contains(p.State, "spawn failed") && (p.has("has LWCR") || len(p.LWCRCDHashes) > 0) {
		return true, "spawn failed with a launch constraint (last exit " + p.LastExit + ")"
	}
	return false, ""
}

// matchesAny compares cdhashes, allowing one to be a truncation of the
// other (codesign prints 20 bytes; constraints may hold the full digest).
func matchesAny(h string, list []string) bool {
	for _, x := range list {
		if strings.HasPrefix(x, h) || strings.HasPrefix(h, x) {
			return true
		}
	}
	return false
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// errNoDetach is returned where processes cannot be detached.
var errNoDetach = errors.New("detached reload is only supported on Unix")
