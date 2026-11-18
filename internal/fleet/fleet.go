package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Action is one of the fleet subcommands.
type Action string

const (
	ActionBootstrap Action = "bootstrap"
	ActionInstall   Action = "install"
	ActionUpgrade   Action = "upgrade"
	ActionUninstall Action = "uninstall"
	ActionStatus    Action = "status"
)

// RouterAPI is the slice of the admin client the fleet needs: minting one-time
// enrolment tokens and reading the fleet to verify an install took. The real
// implementation is *client.Client; tests use a fake.
type RouterAPI interface {
	CreateEnrolmentToken(ctx context.Context, ttlHours int) (*api.EnrolmentToken, error)
	Macs(ctx context.Context) ([]api.Mac, error)
}

// DialFunc opens a connection to a host. Overridable for tests.
type DialFunc func(ctx context.Context, h ResolvedHost, opt SSHOptions) (Target, error)

// Options drive a fleet run.
type Options struct {
	Router        string
	Bundle        string // directory containing "umm-agent"
	SudoPassword  Secret // required for bootstrap
	EnrolTTLHours int
	Purge         bool // uninstall only
	// ReplaceConfig (install) overwrites agent.yaml on the Mac even if it was
	// edited locally or predates fleet's config tracking.
	ReplaceConfig bool
	Concurrency   int
	SSH           SSHOptions
	Dial          DialFunc
	Router_       RouterAPI // mint tokens + verify; may be nil (then no enrol/verify)
	Log           *slog.Logger
	Now           func() time.Time
	// VerifyWindow is how recently a Mac must have been seen to count as
	// "sending telemetry" after an install.
	VerifyWindow time.Duration
	// VerifyTimeout bounds how long install waits for the restarted agent's
	// first heartbeat (default 45s).
	VerifyTimeout time.Duration
	// Sleep is used between router checks while verifying. Overridable for tests.
	Sleep func(ctx context.Context, d time.Duration)
}

// HostResult is the per-host outcome.
type HostResult struct {
	Host     string
	Name     string
	Action   Action
	Status   string // "ok", "failed", "skipped"
	Message  string
	Err      error
	Duration time.Duration
}

// OK reports success.
func (r HostResult) OK() bool { return r.Status == "ok" }

// Runner executes actions across hosts.
type Runner struct {
	opt Options
}

// NewRunner builds a Runner, filling defaults.
func NewRunner(opt Options) *Runner {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 5
	}
	if opt.EnrolTTLHours <= 0 {
		opt.EnrolTTLHours = 24
	}
	if opt.VerifyWindow <= 0 {
		opt.VerifyWindow = 90 * time.Second
	}
	if opt.VerifyTimeout <= 0 {
		opt.VerifyTimeout = 45 * time.Second
	}
	if opt.Sleep == nil {
		opt.Sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if opt.Dial == nil {
		opt.Dial = func(ctx context.Context, h ResolvedHost, o SSHOptions) (Target, error) {
			return Dial(ctx, h, o)
		}
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Runner{opt: opt}
}

// Run performs action across hosts in parallel (bounded by Concurrency) and
// returns a result per host, sorted by host.
func (r *Runner) Run(ctx context.Context, hosts []ResolvedHost, action Action) []HostResult {
	results := make([]HostResult, len(hosts))
	sem := make(chan struct{}, r.opt.Concurrency)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h ResolvedHost) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := r.opt.Now()
			res := r.one(ctx, h, action)
			res.Host, res.Name, res.Action = h.Host, h.Name, action
			res.Duration = r.opt.Now().Sub(start)
			if res.Err != nil && res.Status == "" {
				res.Status = "failed"
			}
			results[i] = res
		}(i, h)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Host < results[j].Host })
	return results
}

func (r *Runner) one(ctx context.Context, h ResolvedHost, action Action) HostResult {
	t, err := r.opt.Dial(ctx, h, r.opt.SSH)
	if err != nil {
		return HostResult{Status: "failed", Message: "connect", Err: err}
	}
	defer t.Close()
	switch action {
	case ActionBootstrap:
		return r.bootstrap(ctx, t, h)
	case ActionInstall:
		return r.install(ctx, t, h)
	case ActionUpgrade:
		return r.upgrade(ctx, t, h)
	case ActionUninstall:
		return r.uninstall(ctx, t, h)
	case ActionStatus:
		return r.status(ctx, t, h)
	}
	return HostResult{Status: "failed", Err: fmt.Errorf("unknown action %q", action)}
}

// --- actions ---------------------------------------------------------------

func (r *Runner) bootstrap(ctx context.Context, t Target, h ResolvedHost) HostResult {
	if r.opt.SudoPassword.Empty() {
		return HostResult{Status: "failed", Err: fmt.Errorf("bootstrap needs the admin sudo password (-sudo-password-file / -sudo-password-env / prompt)")}
	}
	// Preflight: the sudoers we will install must parse. Fail before touching
	// the Mac if the user name is unusual.
	if _, err := RenderSudoers(h.User); err != nil {
		return HostResult{Status: "failed", Err: err}
	}
	stage, cleanup, err := r.stage(ctx, t, map[string]staged{
		"bootstrap.sh": {bootstrapScript, 0o700},
		"install":      {installScript, 0o755},
		"upgrade":      {upgradeScript, 0o755},
		"uninstall":    {uninstallScript, 0o755},
	})
	if err != nil {
		return HostResult{Status: "failed", Message: "stage", Err: err}
	}
	defer cleanup()
	cmd := fmt.Sprintf("sudo -S -p '' /bin/sh %s %s %s", shellQuote(stage+"/bootstrap.sh"), shellQuote(stage), shellQuote(h.User))
	res, err := t.Run(ctx, cmd, r.opt.SudoPassword.Reader())
	if err != nil {
		return HostResult{Status: "failed", Message: "run bootstrap", Err: err}
	}
	if res.ExitCode != 0 {
		return HostResult{Status: "failed", Message: "bootstrap", Err: remoteErr("bootstrap", res)}
	}
	return HostResult{Status: "ok", Message: "sudoers installed; NOPASSWD for install/upgrade/uninstall"}
}

func (r *Runner) install(ctx context.Context, t Target, h ResolvedHost) HostResult {
	bin, err := r.agentBinary()
	if err != nil {
		return HostResult{Status: "failed", Err: err}
	}
	agentYAML, err := RenderAgentConfig(r.opt.Router, h)
	if err != nil {
		return HostResult{Status: "failed", Err: err}
	}
	plist, err := RenderPlist(h.ServiceLabel)
	if err != nil {
		return HostResult{Status: "failed", Err: err}
	}
	files := map[string]staged{
		"umm-agent":  {bin, 0o755},
		"plist":      {plist, 0o644},
		"agent.yaml": {agentYAML, 0o644},
		"label":      {[]byte(h.ServiceLabel + "\n"), 0o644},
	}
	// Mint a one-time enrolment token for this Mac. It is consumed by the
	// agent only if the Mac is not already enrolled; a re-run leaves an unused
	// token that expires. The token reaches the Mac as a 0600 staged file and
	// is passed to the agent over stdin, never argv/env.
	minted := false
	if r.opt.Router_ != nil {
		tok, terr := r.opt.Router_.CreateEnrolmentToken(ctx, r.opt.EnrolTTLHours)
		if terr != nil {
			return HostResult{Status: "failed", Message: "mint enrolment token", Err: terr}
		}
		files["enrol-token"] = staged{[]byte(tok.Token), 0o600}
		minted = true
	}
	if r.opt.ReplaceConfig {
		files["replace-config"] = staged{[]byte("1\n"), 0o644}
	}
	// Remember the router's last heartbeat for this Mac before the restart, so
	// verification below only accepts a heartbeat from the new agent process.
	var before *time.Time
	if minted {
		before = r.lastSeen(ctx, h)
	}
	stage, cleanup, err := r.stage(ctx, t, files)
	if err != nil {
		return HostResult{Status: "failed", Message: "stage", Err: err}
	}
	defer cleanup()
	cmd := fmt.Sprintf("sudo -n %s %s", shellQuote(scriptPath("install")), shellQuote(stage))
	res, err := t.Run(ctx, cmd, nil)
	if err != nil {
		return HostResult{Status: "failed", Message: "run install", Err: err}
	}
	if res.ExitCode == 2 && !minted {
		return HostResult{Status: "ok", Message: "installed (not enrolled: no router key given)"}
	}
	if res.ExitCode != 0 {
		return HostResult{Status: "failed", Message: "install", Err: remoteErr("install", res)}
	}
	msg := strings.TrimSpace(lastLine(res.Stdout))
	if w := warnings(res.Stderr); w != "" {
		msg += "; " + w
	}
	if minted {
		if v := r.verifyFresh(ctx, h, before); v != "" {
			msg += "; " + v
		}
	}
	return HostResult{Status: "ok", Message: msg}
}

func (r *Runner) upgrade(ctx context.Context, t Target, h ResolvedHost) HostResult {
	bin, err := r.agentBinary()
	if err != nil {
		return HostResult{Status: "failed", Err: err}
	}
	stage, cleanup, err := r.stage(ctx, t, map[string]staged{
		"umm-agent": {bin, 0o755},
		"label":     {[]byte(h.ServiceLabel + "\n"), 0o644},
	})
	if err != nil {
		return HostResult{Status: "failed", Message: "stage", Err: err}
	}
	defer cleanup()
	cmd := fmt.Sprintf("sudo -n %s %s", shellQuote(scriptPath("upgrade")), shellQuote(stage))
	res, err := t.Run(ctx, cmd, nil)
	if err != nil {
		return HostResult{Status: "failed", Message: "run upgrade", Err: err}
	}
	if res.ExitCode != 0 {
		return HostResult{Status: "failed", Message: "upgrade", Err: remoteErr("upgrade", res)}
	}
	return HostResult{Status: "ok", Message: strings.TrimSpace(lastLine(res.Stdout))}
}

func (r *Runner) uninstall(ctx context.Context, t Target, h ResolvedHost) HostResult {
	stage, cleanup, err := r.stage(ctx, t, map[string]staged{
		"label": {[]byte(h.ServiceLabel + "\n"), 0o644},
	})
	if err != nil {
		return HostResult{Status: "failed", Message: "stage", Err: err}
	}
	defer cleanup()
	arg := ""
	if r.opt.Purge {
		arg = " --purge"
	}
	cmd := fmt.Sprintf("sudo -n %s %s%s", shellQuote(scriptPath("uninstall")), shellQuote(stage), arg)
	res, err := t.Run(ctx, cmd, nil)
	if err != nil {
		return HostResult{Status: "failed", Message: "run uninstall", Err: err}
	}
	if res.ExitCode != 0 {
		return HostResult{Status: "failed", Message: "uninstall", Err: remoteErr("uninstall", res)}
	}
	return HostResult{Status: "ok", Message: strings.TrimSpace(lastLine(res.Stdout))}
}

func (r *Runner) status(ctx context.Context, t Target, h ResolvedHost) HostResult {
	// Read-only, no sudo: report the binary version and whether the daemon
	// plist is present. The router-side telemetry check adds the live view.
	cmd := "if [ -x /usr/local/bin/umm-agent ]; then /usr/local/bin/umm-agent version; else echo absent; fi; " +
		fmt.Sprintf("if [ -f /Library/LaunchDaemons/%s.plist ]; then echo installed; else echo not-installed; fi", h.ServiceLabel)
	res, err := t.Run(ctx, cmd, nil)
	if err != nil {
		return HostResult{Status: "failed", Message: "status", Err: err}
	}
	lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", " "))
	msg := strings.Join(lines, " ")
	if v := r.verify(ctx, h); v != "" {
		msg += "; " + v
	}
	return HostResult{Status: "ok", Message: msg}
}

// lastSeen returns the router's last heartbeat time for h, or nil.
func (r *Runner) lastSeen(ctx context.Context, h ResolvedHost) *time.Time {
	if r.opt.Router_ == nil {
		return nil
	}
	macs, err := r.opt.Router_.Macs(ctx)
	if err != nil {
		return nil
	}
	for _, m := range macs {
		if m.Name == h.Name && m.LastSeen != nil {
			t := *m.LastSeen
			return &t
		}
	}
	return nil
}

// verifyFresh waits (bounded by VerifyTimeout) for a heartbeat newer than
// before, i.e. one sent by the agent process the install just (re)started. A
// heartbeat recorded before the restart proves nothing about the new process,
// and the first one can take a few seconds to arrive. Comparing router
// timestamps with each other avoids clock skew with the admin workstation.
func (r *Runner) verifyFresh(ctx context.Context, h ResolvedHost, before *time.Time) string {
	if r.opt.Router_ == nil {
		return ""
	}
	deadline := r.opt.Now().Add(r.opt.VerifyTimeout)
	last := "not yet on the router"
	for {
		macs, err := r.opt.Router_.Macs(ctx)
		if err != nil {
			last = "router check failed: " + err.Error()
		} else {
			last = "not yet on the router"
			for _, m := range macs {
				if m.Name != h.Name {
					continue
				}
				if m.LastSeen != nil && (before == nil || m.LastSeen.After(*before)) {
					return "telemetry: online"
				}
				last = "registered but no heartbeat since restart"
			}
		}
		if ctx.Err() != nil || !r.opt.Now().Before(deadline) {
			return last + fmt.Sprintf(" (waited %s)", r.opt.VerifyTimeout)
		}
		r.opt.Sleep(ctx, 2*time.Second)
	}
}

// warnings returns the remote script's "warning:" lines from stderr, joined.
func warnings(stderr string) string {
	var out []string
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "warning: ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "; ")
}

// verify asks the router whether the Mac is present and sending telemetry.
func (r *Runner) verify(ctx context.Context, h ResolvedHost) string {
	if r.opt.Router_ == nil {
		return ""
	}
	macs, err := r.opt.Router_.Macs(ctx)
	if err != nil {
		return "router check failed: " + err.Error()
	}
	for _, m := range macs {
		if m.Name != h.Name {
			continue
		}
		if m.LastSeen != nil && r.opt.Now().Sub(*m.LastSeen) <= r.opt.VerifyWindow {
			return "telemetry: online"
		}
		if m.Online {
			return "telemetry: online"
		}
		return "registered but not seen recently"
	}
	return "not yet on the router"
}

// --- helpers ---------------------------------------------------------------

type staged struct {
	data []byte
	mode os.FileMode
}

// stage makes a temp dir and uploads files into it, returning a cleanup func.
func (r *Runner) stage(ctx context.Context, t Target, files map[string]staged) (string, func(), error) {
	dir, err := t.MkdirTemp(ctx)
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		// Best-effort removal of the staging dir (and any staged secrets in it).
		_, _ = t.Run(context.Background(), "rm -rf "+shellQuote(dir), nil)
	}
	// Deterministic order for predictable logs/tests.
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := files[n]
		if err := t.Upload(ctx, dir+"/"+n, f.data, f.mode); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("upload %s: %w", n, err)
		}
	}
	return dir, cleanup, nil
}

func (r *Runner) agentBinary() ([]byte, error) {
	if r.opt.Bundle == "" {
		return nil, fmt.Errorf("no bundle: set bundle in the inventory or pass -bundle (a directory containing umm-agent)")
	}
	p := filepath.Join(r.opt.Bundle, "umm-agent")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read agent binary %s: %w (build it with 'make agent-darwin')", p, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("agent binary %s is empty", p)
	}
	return b, nil
}

func remoteErr(what string, res Result) error {
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	detail = lastLine(detail)
	if detail == "" {
		detail = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return fmt.Errorf("%s failed (exit %d): %s", what, res.ExitCode, detail)
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// shellQuote wraps a string in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
