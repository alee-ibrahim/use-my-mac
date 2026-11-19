package engine

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Runtime availability.
//
// Agents advertise only the runtimes that pass their preflight (program
// exists, is a regular file, is executable by the agent's user) and report
// the rest as unavailable. When a task still cannot execute its runtime on a
// Mac (exit 127, exec ENOENT/EACCES) the router treats it as the Mac's
// fault: it marks the runtime unavailable on that Mac until the agent's next
// preflight shows it available again, and reschedules the task elsewhere
// without counting a failure (up to MaxMacFaults times per task).

// runtimeFault is a runtime the router marked unavailable on one Mac.
type runtimeFault struct {
	Reason string `json:"reason"`
	At     int64  `json:"at"`
	// PreflightID is the agent's preflight ID when the fault happened; a
	// heartbeat from a newer preflight listing the runtime clears it.
	PreflightID int64 `json:"preflight_id"`
}

// macFault returns why an agent's failure report is the Mac's fault rather
// than the task's ("" = it is the task's), and whether it is about the
// task's runtime. Older agents only send the exit code, so a bare 127
// ("command not found") counts too.
func macFault(u api.TaskUpdate) (why string, runtime bool) {
	if u.Outcome != api.OutcomeFailed && u.Outcome != api.OutcomeTimeout {
		return "", false
	}
	if u.RuntimeUnavailable != "" {
		return u.RuntimeUnavailable, true
	}
	if u.MacFault != "" {
		return u.MacFault, false
	}
	if u.Outcome != api.OutcomeFailed {
		return "", false
	}
	if u.ExitCode != nil && *u.ExitCode == 127 {
		return nz(u.Error, "exit status 127") + " (command not found)", true
	}
	return "", false
}

func (e *Engine) loadFaultsLocked(ctx context.Context, agentID string, l *live) {
	if l.faultsLoaded {
		return
	}
	l.faults = map[string]runtimeFault{}
	if r, err := e.st.GetAgentRuntimes(ctx, agentID); err == nil {
		_ = json.Unmarshal([]byte(r.Faults), &l.faults)
		if l.faults == nil {
			l.faults = map[string]runtimeFault{}
		}
		l.savedUnavail, l.savedFaults = r.Unavailable, r.Faults
	}
	l.faultsLoaded = true
}

// applyRuntimesLocked folds a heartbeat's runtime report and the router's
// faults into the effective view, persists it if it changed, and returns
// the runtimes the scheduler may use.
func (e *Engine) applyRuntimesLocked(ctx context.Context, agentID string, l *live, now time.Time) []string {
	e.loadFaultsLocked(ctx, agentID, l)
	hb := l.hb
	avail := map[string]bool{}
	for _, n := range hb.Runtimes {
		avail[n] = true
	}
	reported := map[string]bool{}
	for _, u := range hb.UnavailableRuntimes {
		reported[u.Name] = true
	}
	for _, n := range hb.InstallableRuntimes {
		reported[n] = true // a managed runtime it can install
	}
	for name, f := range l.faults {
		switch {
		case !avail[name] && !reported[name]:
			delete(l.faults, name) // no longer allowlisted
		case hb.PreflightID != 0 && hb.PreflightID != f.PreflightID && (avail[name] || slices.Contains(hb.InstallableRuntimes, name)):
			delete(l.faults, name)
			e.log.Info("runtime available again after preflight", "agent", agentID, "runtime", name)
		case hb.PreflightID == 0 && avail[name] && now.Sub(time.UnixMilli(f.At)) > e.cfg.LegacyFaultTTL:
			delete(l.faults, name)
			e.log.Info("retrying runtime on an agent without preflight", "agent", agentID, "runtime", name)
		}
	}
	e.recomputeRuntimesLocked(ctx, agentID, l, now)
	return l.runtimes
}

// recomputeRuntimesLocked derives l.runtimes/l.unavail and saves changes.
func (e *Engine) recomputeRuntimesLocked(ctx context.Context, agentID string, l *live, now time.Time) {
	hb := l.hb
	l.runtimes = []string{}
	for _, n := range hb.Runtimes {
		if _, bad := l.faults[n]; !bad {
			l.runtimes = append(l.runtimes, n)
		}
	}
	l.installable = []string{}
	for _, n := range hb.InstallableRuntimes {
		if _, bad := l.faults[n]; !bad {
			l.installable = append(l.installable, n)
		}
	}
	l.unavail = nil
	seen := map[string]bool{}
	for _, u := range hb.UnavailableRuntimes {
		if u.Name == "" || seen[u.Name] {
			continue
		}
		seen[u.Name] = true
		if u.Source == "" {
			u.Source = api.RuntimeSourcePreflight
		}
		l.unavail = append(l.unavail, u)
	}
	var names []string
	for n := range l.faults {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		f := l.faults[n]
		l.unavail = append(l.unavail, api.UnavailableRuntime{Name: n, Reason: f.Reason,
			Source: api.RuntimeSourceExec, Since: tptr(f.At)})
	}
	sort.SliceStable(l.unavail, func(i, j int) bool { return l.unavail[i].Name < l.unavail[j].Name })

	un, fs := jsonStr(nonNilUnavail(l.unavail)), jsonStr(l.faults)
	if un == l.savedUnavail && fs == l.savedFaults {
		return
	}
	if err := e.st.PutAgentRuntimes(ctx, &store.AgentRuntimes{AgentID: agentID, Unavailable: un, Faults: fs,
		UpdatedAt: ms(now)}); err != nil {
		e.log.Warn("save runtime availability", "agent", agentID, "err", err)
		return
	}
	l.savedUnavail, l.savedFaults = un, fs
}

func nonNilUnavail(s []api.UnavailableRuntime) []api.UnavailableRuntime {
	if s == nil {
		return []api.UnavailableRuntime{}
	}
	return s
}

// runtimeFaultLocked handles an attempt that failed because of the Mac: it
// could not execute the task's runtime (runtime != "": mark the runtime
// unavailable there) or failed for its own reasons (runtime == "": disk
// full, unhealthy). The task is rescheduled elsewhere without counting a
// failure. After MaxMacFaults such attempts the task's attempts count as
// normal failures.
func (e *Engine) runtimeFaultLocked(ctx context.Context, t *store.Task, agentID, runtime, why string, exit *int, now time.Time) {
	name := agentID
	if a, err := e.st.GetAgent(ctx, agentID); err == nil {
		name = a.Name
	}
	l := e.liveFor(agentID)
	e.loadFaultsLocked(ctx, agentID, l)
	if runtime != "" {
		l.faults[runtime] = runtimeFault{Reason: why, At: ms(now), PreflightID: l.hb.PreflightID}
		e.recomputeRuntimesLocked(ctx, agentID, l, now)
		if err := e.st.SetAgentRuntimeList(ctx, agentID, jsonStr(nonNil(l.runtimes))); err != nil {
			e.log.Warn("save runtimes", "agent", agentID, "err", err)
		}
	}
	msg := "runtime " + runtime + " unavailable on " + name + ": " + why
	event := api.EventRuntimeUnavailable
	if runtime == "" {
		msg, event = "Mac fault on "+name+": "+why, api.EventMacFault
	}
	e.log.Warn("Mac fault; rescheduling task elsewhere", "agent", agentID, "mac", name,
		"runtime", runtime, "task", t.ID, "reason", why)

	prior := 0
	if att, err := e.st.Attempts(ctx, t.ID); err == nil {
		for _, a := range att {
			if a.Outcome == api.OutcomeUnavailable {
				prior++
			}
		}
	}
	if prior >= e.cfg.MaxMacFaults {
		e.Audit(ctx, event, "", agentID, msg+" (task "+t.Key+": Mac-fault limit "+
			strconv.Itoa(e.cfg.MaxMacFaults)+" reached, counted as a failure)")
		e.failLocked(ctx, t, api.OutcomeFailed, msg+" (Mac-fault limit reached)", exit, now)
		return
	}
	e.Audit(ctx, event, "", agentID, msg+" (task "+t.Key+" rescheduled)")
	exclude := e.cfg.YieldExclude
	if runtime != "" {
		// The runtime itself is off that Mac until a newer preflight lists
		// it again (e.g. once a managed install finished), so no exclusion.
		exclude = 0
	}
	e.requeueLocked(ctx, t, api.OutcomeUnavailable, msg, exclude, now)
}
