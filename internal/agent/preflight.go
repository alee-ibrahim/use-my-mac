package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Runtime preflight: the agent only advertises runtimes whose program it can
// actually execute, so the router never schedules a task onto a Mac where it
// would fail with "No such file or directory" (exit 127).

// ExecFaultRetry is how long a runtime marked unavailable after an exec
// failure stays unavailable when its program looks fine and has not changed
// (e.g. a script whose interpreter is missing). A change to the program file
// or a config reload retries it sooner.
const ExecFaultRetry = 15 * time.Minute

// CheckProgram reports why the agent's user cannot execute path ("" = it
// can): it must exist, be a regular file (after following symlinks) and be
// executable by this process.
func CheckProgram(path string) string {
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "program not found: " + path
	case errors.Is(err, fs.ErrPermission):
		return "program not accessible by the agent's user: " + path
	case err != nil:
		return "program unusable: " + err.Error()
	case !st.Mode().IsRegular():
		return "program is not a regular file: " + path
	}
	if err := canExec(path); err != nil {
		return "program not executable by the agent's user: " + path
	}
	return ""
}

// progStamp identifies a program file's version, to notice reinstalls.
func progStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d/%d/%v", st.Size(), st.ModTime().UnixNano(), st.Mode())
}

// execFault is a runtime the agent failed to execute.
type execFault struct {
	reason string
	at     time.Time
	stamp  string
}

// preflightLocked re-checks every allowlisted runtime. Exec faults survive
// a passing check until the program changes or ExecFaultRetry passes.
// Caller holds a.mu.
func (a *Agent) preflightLocked(now time.Time) {
	avail := []string{}
	unavail := []api.UnavailableRuntime{}
	names := make([]string, 0, len(a.runtimes))
	for n := range a.runtimes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		prog := a.runtimes[n][0]
		if why := CheckProgram(prog); why != "" {
			delete(a.faults, n) // the preflight reason supersedes it
			since := now
			if prev, ok := a.unavail[n]; ok && prev.Since != nil {
				since = *prev.Since
			}
			unavail = append(unavail, api.UnavailableRuntime{Name: n, Program: prog, Reason: why,
				Source: api.RuntimeSourcePreflight, Since: &since})
			continue
		}
		if f, ok := a.faults[n]; ok {
			if now.Sub(f.at) < ExecFaultRetry && progStamp(prog) == f.stamp {
				at := f.at
				unavail = append(unavail, api.UnavailableRuntime{Name: n, Program: prog, Reason: f.reason,
					Source: api.RuntimeSourceExec, Since: &at})
				continue
			}
			delete(a.faults, n)
		}
		avail = append(avail, n)
	}
	// Log changes only.
	prev := a.unavail
	a.unavail = map[string]api.UnavailableRuntime{}
	for _, u := range unavail {
		a.unavail[u.Name] = u
		if p, ok := prev[u.Name]; !ok || p.Reason != u.Reason {
			a.log.Warn("runtime unavailable", "runtime", u.Name, "reason", u.Reason)
		}
	}
	for n := range prev {
		if _, still := a.unavail[n]; !still {
			if _, allowed := a.runtimes[n]; allowed {
				a.log.Info("runtime available again", "runtime", n)
			}
		}
	}
	a.avail = avail
	a.lastPreflight = now
	id := now.UnixNano()
	if id <= a.preflightID {
		id = a.preflightID + 1
	}
	a.preflightID = id
}

// Preflight re-checks the runtimes now.
func (a *Agent) Preflight() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.preflightLocked(time.Now())
}

// ReloadRuntimes replaces the runtime allowlist (after a config reload) and
// re-runs the preflight. Exec faults are forgotten: the new config gets a
// fresh chance.
func (a *Agent) ReloadRuntimes(rts map[string][]string) {
	cp := make(map[string][]string, len(rts))
	for k, v := range rts {
		cp[k] = append([]string(nil), v...)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runtimes = cp
	a.faults = map[string]execFault{}
	a.preflightLocked(time.Now())
}

// RuntimeStatus returns the runtimes that passed the last preflight and the
// ones that did not.
func (a *Agent) RuntimeStatus() (avail []string, unavail []api.UnavailableRuntime) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runtimeStatusLocked()
}

func (a *Agent) runtimeStatusLocked() ([]string, []api.UnavailableRuntime) {
	avail := append([]string{}, a.avail...)
	var unavail []api.UnavailableRuntime
	for _, u := range a.unavail {
		unavail = append(unavail, u)
	}
	sort.Slice(unavail, func(i, j int) bool { return unavail[i].Name < unavail[j].Name })
	return avail, unavail
}

// markExecFault makes a runtime unavailable after the agent failed to
// execute it, until the next preflight in which it passes and its program
// has changed (or ExecFaultRetry passes).
func (a *Agent) markExecFault(name, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	argv, ok := a.runtimes[name]
	if !ok {
		return
	}
	now := time.Now()
	prog := argv[0]
	if why := CheckProgram(prog); why != "" {
		reason = why
	}
	a.faults[name] = execFault{reason: reason, at: now, stamp: progStamp(prog)}
	a.log.Warn("runtime marked unavailable after exec failure", "runtime", name, "reason", reason)
	a.preflightLocked(now)
}

// runtimeFor returns the argv prefix of an available runtime, or why it
// cannot run here.
func (a *Agent) runtimeFor(name string) ([]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	argv, ok := a.runtimes[name]
	if !ok {
		return nil, "runtime " + name + " is not allowlisted on this Mac"
	}
	if u, bad := a.unavail[name]; bad {
		return nil, "runtime " + name + " unavailable on this Mac: " + u.Reason
	}
	return argv, ""
}

// execFailureReason classifies how a task's process ended. It returns a
// reason when the Mac could not execute the runtime at all (the task never
// ran): exec ENOENT/EACCES, or the launcher's exit 127 ("command not
// found"), and exit 126 ("cannot execute") when the program also fails the
// preflight check. "" means a normal task failure.
func execFailureReason(prog string, startErr error, exitCode int) string {
	if startErr != nil {
		if errors.Is(startErr, fs.ErrNotExist) || errors.Is(startErr, fs.ErrPermission) {
			if why := CheckProgram(prog); why != "" {
				return why
			}
			return "cannot start: " + startErr.Error()
		}
		return ""
	}
	switch exitCode {
	case 127:
		if why := CheckProgram(prog); why != "" {
			return why
		}
		return "exit 127: command not found (program or its interpreter is missing)"
	case 126:
		if why := CheckProgram(prog); why != "" {
			return why
		}
	}
	return ""
}
