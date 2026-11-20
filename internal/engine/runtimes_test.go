package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func (h *harness) hbRuntimes(id string, preflightID int64, avail []string, unavail ...api.UnavailableRuntime) {
	h.t.Helper()
	_, err := h.e.Heartbeat(h.ctx, id, api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), Runtimes: avail,
		UnavailableRuntimes: unavail, PreflightID: preflightID, MaxTasks: h.max[id], Running: []string{}})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) mac(id string) api.Mac {
	h.t.Helper()
	ms, err := h.e.Macs(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m
		}
	}
	h.t.Fatalf("mac %s not listed", id)
	return api.Mac{}
}

// A runtime the agent's preflight reports as unavailable is shown with its
// reason and never gets a task, even on the best-scoring Mac.
func TestPreflightUnavailableRuntimeIsNeverScheduled(t *testing.T) {
	h := newHarness(t)
	broken := h.enrol("broken", 1)
	ok := h.enrol("ok", 1)
	why := "program not found: /opt/homebrew/bin/sh"
	h.hbRuntimes(broken, 1, []string{}, api.UnavailableRuntime{Name: "sh", Program: "/opt/homebrew/bin/sh", Reason: why,
		Source: api.RuntimeSourcePreflight})
	// ok has a user at the console, so broken would score higher if eligible.
	if _, err := h.e.Heartbeat(h.ctx, ok, api.Heartbeat{State: api.AgentReady, Telemetry: activeUserTel(),
		Runtimes: []string{"sh"}, PreflightID: 1, MaxTasks: 1}); err != nil {
		t.Fatal(err)
	}
	m := h.mac(broken)
	if len(m.Runtimes) != 0 || len(m.UnavailableRuntimes) != 1 || m.UnavailableRuntimes[0].Reason != why {
		t.Fatalf("broken Mac view: runtimes %v unavailable %+v", m.Runtimes, m.UnavailableRuntimes)
	}
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	d := h.task(j.Tasks[0].ID)
	if d.AgentID != ok {
		t.Fatalf("task placed on %q, want ok", d.AgentID)
	}
	found := false
	for _, p := range d.Placement {
		if p.ID == broken && !p.OK && strings.Contains(p.Reason, "unavailable") && strings.Contains(p.Reason, why) {
			found = true
		}
	}
	if !found {
		t.Fatalf("placement must explain the unavailable runtime: %+v", d.Placement)
	}
}

// Exit 127 is the Mac's fault: the task moves on without using a retry and
// the runtime is unavailable on that Mac until a newer preflight shows it.
func TestExit127IsMacFaultAndReschedules(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	b := h.enrol("b", 1)
	h.hbRuntimes(a, 10, []string{"sh"})
	if _, err := h.e.Heartbeat(h.ctx, b, api.Heartbeat{State: api.AgentReady, Telemetry: activeUserTel(),
		Runtimes: []string{"sh"}, PreflightID: 20, MaxTasks: 1}); err != nil {
		t.Fatal(err)
	}
	spec := simpleTask("t")
	spec.MaxRetries = ip(0) // any counted failure would be final
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	as := h.poll(a)
	if len(as) != 1 {
		t.Fatal("expected the task on a")
	}
	// An old agent: only the exit code.
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed,
		ExitCode: ip(127), Error: "exit status 127"}); err != nil {
		t.Fatal(err)
	}
	d := h.task(j.Tasks[0].ID)
	if d.State != api.StatePending || d.Failures != 0 || d.NotBefore != nil {
		t.Fatalf("want pending, no failure, no backoff; got %s failures=%d", d.State, d.Failures)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].Outcome != api.OutcomeUnavailable {
		t.Fatalf("attempt: %+v", d.Attempts)
	}
	m := h.mac(a)
	if len(m.Runtimes) != 0 || len(m.UnavailableRuntimes) != 1 || m.UnavailableRuntimes[0].Source != api.RuntimeSourceExec {
		t.Fatalf("a must lose sh: %+v / %+v", m.Runtimes, m.UnavailableRuntimes)
	}
	// Same preflight on the next heartbeat: still unavailable, task goes to b.
	h.hbRuntimes(a, 10, []string{"sh"})
	h.tick()
	if d = h.task(d.ID); d.AgentID != b {
		t.Fatalf("task must move to b, got %q", d.AgentID)
	}
	as = h.poll(b)
	if err := h.e.TaskUpdate(h.ctx, b, as[0].TaskID, api.TaskUpdate{Attempt: 2, Outcome: api.OutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	if d = h.task(d.ID); d.State != api.StateSucceeded || d.Failures != 0 {
		t.Fatalf("want succeeded, got %s/%d", d.State, d.Failures)
	}
	// The Mac's own health is untouched (no quarantine for a missing program).
	if m := h.mac(a); m.Quarantined != nil {
		t.Fatal("must not quarantine")
	}
	// A newer preflight that lists sh again clears the fault.
	h.hbRuntimes(a, 11, []string{"sh"})
	if m := h.mac(a); len(m.Runtimes) != 1 || len(m.UnavailableRuntimes) != 0 {
		t.Fatalf("a must get sh back: %+v / %+v", m.Runtimes, m.UnavailableRuntimes)
	}
	evs, err := h.e.Events(h.ctx, []string{api.EventRuntimeUnavailable}, "", time.Time{}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("want one runtime_unavailable event, got %d (%v)", len(evs), err)
	}
}

// The agent's explicit report works the same and the fault is kept across a
// router restart; agents without preflight get the runtime back after
// LegacyFaultTTL.
func TestRuntimeFaultPersistsAndLegacyTTL(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hbRuntimes(a, 0, []string{"sh"}) // legacy agent: no preflight ID
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	as := h.poll(a)
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed,
		Error: "start: no such file", RuntimeUnavailable: "program not found: /x/sh"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a router restart: forget the in-memory state.
	h.e.mu.Lock()
	h.e.live = map[string]*live{}
	h.e.mu.Unlock()
	h.hbRuntimes(a, 0, []string{"sh"})
	m := h.mac(a)
	if len(m.Runtimes) != 0 || len(m.UnavailableRuntimes) != 1 || m.UnavailableRuntimes[0].Reason != "program not found: /x/sh" {
		t.Fatalf("fault must survive a restart: %+v / %+v", m.Runtimes, m.UnavailableRuntimes)
	}
	h.tick()
	if d := h.task(j.Tasks[0].ID); d.State != api.StatePending || d.AgentID != "" {
		t.Fatalf("must not be placed on a again: %s on %q", d.State, d.AgentID)
	}
	h.advance(h.e.cfg.LegacyFaultTTL + time.Minute)
	h.hbRuntimes(a, 0, []string{"sh"})
	if m := h.mac(a); len(m.Runtimes) != 1 {
		t.Fatalf("legacy agent must get the runtime back after the TTL: %+v", m)
	}
}

// A task cannot bounce forever: after MaxMacFaults free reschedules, Mac
// faults count against max_retries.
func TestMacFaultCap(t *testing.T) {
	h := newHarness(t)
	h.e.cfg.MaxMacFaults = 2
	h.e.cfg.YieldExclude = 0
	a := h.enrol("a", 1)
	spec := simpleTask("t")
	spec.MaxRetries = ip(0)
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	id := j.Tasks[0].ID
	for attempt := 1; attempt <= 3; attempt++ {
		h.hbRuntimes(a, int64(attempt), []string{"sh"}) // new preflight clears the fault each time
		h.tick()
		as := h.poll(a)
		if len(as) != 1 {
			t.Fatalf("attempt %d: not placed (%+v)", attempt, h.task(id).Placement)
		}
		if err := h.e.TaskUpdate(h.ctx, a, id, api.TaskUpdate{Attempt: attempt, Outcome: api.OutcomeFailed,
			ExitCode: ip(127)}); err != nil {
			t.Fatal(err)
		}
		d := h.task(id)
		if attempt <= 2 && (d.State != api.StatePending || d.Failures != 0) {
			t.Fatalf("attempt %d: want free reschedule, got %s/%d", attempt, d.State, d.Failures)
		}
		if attempt == 3 && (d.State != api.StateFailed || d.Failures != 1 || !strings.Contains(d.Error, "limit")) {
			t.Fatalf("attempt 3: want counted failure, got %s/%d %q", d.State, d.Failures, d.Error)
		}
	}
}

// Ordinary failures (not 127, not runtime_unavailable) still count.
func TestMacFaultClassification(t *testing.T) {
	cases := []struct {
		u       api.TaskUpdate
		want    bool
		runtime bool
	}{
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(127)}, true, true},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(127), ProgramExit: true}, false, false},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(127), ProgramExit: true, RuntimeUnavailable: "gone"}, true, true},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, RuntimeUnavailable: "program not found: /x"}, true, true},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(1)}, false, false},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(126)}, false, false},
		{api.TaskUpdate{Outcome: api.OutcomeTimeout, ExitCode: ip(127)}, false, false},
		{api.TaskUpdate{Outcome: api.OutcomeFailed}, false, false},
		{api.TaskUpdate{Outcome: api.OutcomeFailed, ExitCode: ip(1), MacFault: "work volume full"}, true, false},
		{api.TaskUpdate{Outcome: api.OutcomeSucceeded, MacFault: "x"}, false, false},
	}
	for i, c := range cases {
		why, rt := macFault(c.u)
		if got := why != ""; got != c.want || (got && rt != c.runtime) {
			t.Errorf("case %d: macFault=%v runtime=%v want %v/%v", i, got, rt, c.want, c.runtime)
		}
	}
}

// A Mac fault (disk full, unhealthy) reschedules without counting a failure
// and without touching the Mac's runtimes. An unhealthy Mac gets no work and
// its reason is shown.
func TestMacFaultReschedulesAndUnhealthyGetsNoWork(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	b := h.enrol("b", 1)
	h.hbRuntimes(a, 1, []string{"sh"})
	if _, err := h.e.Heartbeat(h.ctx, b, api.Heartbeat{State: api.AgentReady, Telemetry: activeUserTel(),
		Runtimes: []string{"sh"}, PreflightID: 1, MaxTasks: 1}); err != nil {
		t.Fatal(err)
	}
	spec := simpleTask("t")
	spec.MaxRetries = ip(0)
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	as := h.poll(a)
	if len(as) != 1 {
		t.Fatal("expected the task on a")
	}
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed,
		ExitCode: ip(1), Error: "exit status 1", MacFault: "work volume is full"}); err != nil {
		t.Fatal(err)
	}
	d := h.task(j.Tasks[0].ID)
	if d.State != api.StatePending || d.Failures != 0 || len(d.Attempts) != 1 || d.Attempts[0].Outcome != api.OutcomeUnavailable {
		t.Fatalf("want pending/unavailable without a failure; got %s failures=%d %+v", d.State, d.Failures, d.Attempts)
	}
	if m := h.mac(a); len(m.Runtimes) != 1 || len(m.UnavailableRuntimes) != 0 {
		t.Fatalf("a Mac fault must not touch runtimes: %+v / %+v", m.Runtimes, m.UnavailableRuntimes)
	}
	if evs, _ := h.e.Events(h.ctx, []string{api.EventMacFault}, "", time.Time{}, 10); len(evs) != 1 {
		t.Fatalf("want one mac.fault event, got %d", len(evs))
	}
	// a is now unhealthy: b gets the task even though a scores higher.
	why := "home directories readable by the agent's user: /Users/x (drwxr-xr-x)"
	if _, err := h.e.Heartbeat(h.ctx, a, api.Heartbeat{State: api.AgentUnhealthy, Reason: why, Telemetry: goodTel(),
		Runtimes: []string{"sh"}, PreflightID: 1, MaxTasks: 1, Running: []string{},
		Isolation: &api.Isolation{HomeDirs: "exposed", WorkVolume: "dedicated"}}); err != nil {
		t.Fatal(err)
	}
	m := h.mac(a)
	if m.AgentState != api.AgentUnhealthy || m.StateReason != why || m.Isolation == nil || m.Isolation.HomeDirs != "exposed" {
		t.Fatalf("unhealthy view: %+v", m)
	}
	h.tick()
	if d = h.task(d.ID); d.AgentID != b {
		t.Fatalf("task must go to b, got %q", d.AgentID)
	}
}

func (h *harness) hbSpace(id string, space, capacity uint64) {
	h.t.Helper()
	if _, err := h.e.Heartbeat(h.ctx, id, api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), Runtimes: []string{"sh"},
		PreflightID: 1, MaxTasks: h.max[id], Running: []string{},
		Isolation: &api.Isolation{WorkSpaceBytes: space, WorkCapacityBytes: capacity}}); err != nil {
		h.t.Fatal(err)
	}
}

// Tasks go only where their inputs plus outputs fit; a task no Mac could
// ever hold fails at once with a clear reason.
func TestWorkSpacePlacement(t *testing.T) {
	h := newHarness(t)
	small := h.enrol("small", 1)
	big := h.enrol("big", 1)
	h.hbSpace(small, 100<<30, 100<<30)
	h.hbSpace(big, 300<<30, 900<<30) // busy disk now, roomy when idle

	// 60 GiB blob input: needs 120 GiB, only "big" has room now.
	if err := h.e.Blobs().Put(h.ctx, "uploads/in.mov", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	spec := simpleTask("fits")
	spec.Inputs = []api.Input{{Name: "in", Blob: "uploads/in.mov"}}
	spec.Resources.DiskEstimateMB = 120 << 10
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	if d := h.task(j.Tasks[0].ID); d.AgentID != big {
		t.Fatalf("120 GiB task must go to big, got %q (%+v)", d.AgentID, d.Placement)
	}

	// 500 GiB task: no Mac has room now, but big could when idle: it waits.
	wait := simpleTask("waits")
	wait.Resources.DiskEstimateMB = 500 << 10
	// 1 TiB task: nobody could ever hold it: fails clearly.
	huge := simpleTask("huge")
	huge.Resources.DiskEstimateMB = 1 << 20
	j2 := h.submit(api.JobSpec{Tasks: []api.TaskSpec{wait, huge}})
	h.tick()
	for _, tk := range j2.Tasks {
		d := h.task(tk.ID)
		switch tk.Key {
		case "waits":
			if d.State != api.StatePending {
				t.Fatalf("waits: %s %s", d.State, d.Error)
			}
		case "huge":
			if d.State != api.StateFailed || !strings.Contains(d.Error, "no Mac has enough work space") ||
				!strings.Contains(d.Error, "900.0 GiB") {
				t.Fatalf("huge: %s %q", d.State, d.Error)
			}
		}
	}
}

// The default estimate is twice the inputs the router can size.
func TestDiskNeedFromBlobs(t *testing.T) {
	h := newHarness(t)
	if err := h.e.Blobs().Put(h.ctx, "uploads/a", strings.NewReader("12345"), 5); err != nil {
		t.Fatal(err)
	}
	spec := simpleTask("t")
	spec.Inputs = []api.Input{{Name: "a", Blob: "uploads/a"}, {Name: "u", URL: "https://example.com/x"}}
	spec.Resources.DiskMB = 1
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	ts, _ := h.e.st.TasksByJob(h.ctx, j.ID)
	h.e.mu.Lock()
	n := h.e.diskNeedLocked(h.ctx, ts[0], spec)
	h.e.mu.Unlock()
	if n != 10+1<<20 {
		t.Fatalf("need %d", n)
	}
}
