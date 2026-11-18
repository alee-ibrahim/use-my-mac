package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

type harness struct {
	max map[string]int
	t   *testing.T
	e   *Engine
	now time.Time
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	blobs, err := storage.NewLocal(t.TempDir(), "http://router.test", []byte("test-secret-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{max: map[string]int{}, t: t, now: time.Unix(1_800_000_000, 0), ctx: ctx}
	cfg := DefaultConfig()
	cfg.BackoffBase = 10 * time.Second
	cfg.QuarantineAfter = 0
	e, err := New(ctx, Options{Store: st, Blobs: blobs, Config: cfg, Now: func() time.Time { return h.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func goodTel() api.Telemetry {
	return api.Telemetry{CPUCount: 10, MemTotalBytes: 32 << 30, MemAvailableBytes: 24 << 30, MemoryPressure: "normal",
		Thermal: "nominal", OnAC: true, BatteryPercent: -1, GPUUtilPercent: 0, DiskFreeBytes: 500 << 30}
}

func activeUserTel() api.Telemetry {
	t := goodTel()
	t.ConsoleUser, t.UserIdleSeconds, t.UserCPUPercent = "staff", 1, 10
	return t
}

func (h *harness) enrol(name string, maxTasks int) string {
	h.t.Helper()
	tok, err := h.e.CreateEnrolToken(h.ctx, "test", time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	r, err := h.e.Enrol(h.ctx, api.EnrolRequest{Token: tok.Token, Name: name, Runtimes: []string{"sh"}, MaxTasks: maxTasks})
	if err != nil {
		h.t.Fatal(err)
	}
	// Token is single-use.
	if _, err := h.e.Enrol(h.ctx, api.EnrolRequest{Token: tok.Token, Name: name}); !errors.Is(err, ErrForbidden) {
		h.t.Fatalf("token reuse must fail, got %v", err)
	}
	if id, err := h.e.AuthAgent(h.ctx, r.Credential); err != nil || id != r.AgentID {
		h.t.Fatalf("auth agent: %v", err)
	}
	h.max[r.AgentID] = maxTasks
	return r.AgentID
}

func (h *harness) hb(id string, tel api.Telemetry, state string, running ...string) api.HeartbeatResponse {
	h.t.Helper()
	if state == "" {
		state = api.AgentReady
	}
	r, err := h.e.Heartbeat(h.ctx, id, api.Heartbeat{State: state, Telemetry: tel, Runtimes: []string{"sh"}, MaxTasks: h.max[id], Running: running})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) tick() {
	h.t.Helper()
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) poll(id string) []api.Assignment {
	h.t.Helper()
	as, err := h.e.Poll(h.ctx, id, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return as
}

func (h *harness) task(id string) *TaskDetail {
	h.t.Helper()
	d, err := h.e.Task(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) submit(spec api.JobSpec) *api.Job {
	h.t.Helper()
	j, err := h.e.SubmitJob(h.ctx, "tester", spec)
	if err != nil {
		h.t.Fatal(err)
	}
	full, _ := h.e.Job(h.ctx, j.ID, true)
	return full
}

func ip(v int) *int { return &v }

func simpleTask(key string) api.TaskSpec {
	return api.TaskSpec{Key: key, Runtime: "sh", Args: []string{"-c", "true"}}
}

func TestPlacementPrefersQuietMacAndSpreads(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("quiet", 2)
	b := h.enrol("busy-user", 2)
	h.hb(a, goodTel(), "")
	h.hb(b, activeUserTel(), "")
	j := h.submit(api.JobSpec{Name: "spread", Tasks: []api.TaskSpec{simpleTask("t1"), simpleTask("t2"), simpleTask("t3"), simpleTask("t4")}})
	h.tick()
	per := map[string]int{}
	for _, tk := range j.Tasks {
		d := h.task(tk.ID)
		if d.State != api.StateAssigned {
			t.Fatalf("%s not assigned: %s", tk.Key, d.State)
		}
		per[d.AgentID]++
		if tk.Key == "t1" && d.AgentID != a {
			t.Fatalf("first task should land on the quiet Mac")
		}
		if len(d.Placement) != 2 {
			t.Fatalf("placement explanation missing")
		}
	}
	if per[a] != 2 || per[b] != 2 {
		t.Fatalf("want 2/2 spread, got %v", per)
	}
	if as := h.poll(a); len(as) != 2 || as[0].Attempt != 1 {
		t.Fatalf("poll a: %+v", as)
	}
	if as := h.poll(a); len(as) != 0 {
		t.Fatal("tasks must be dispatched once")
	}
}

func TestYieldReassignsToAnotherMac(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("long")}})
	h.tick()
	as := h.poll(a)
	if len(as) != 1 {
		t.Fatal("expected assignment on a")
	}
	b := h.enrol("b", 1)
	h.hb(b, activeUserTel(), "")
	// The user on Mac a goes heavy: the agent yields and reports.
	h.hb(a, goodTel(), api.AgentBusyUser, as[0].TaskID)
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeYielded, Error: "user CPU 90%"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d := h.task(j.Tasks[0].ID)
	if d.State != api.StateAssigned || d.AgentID != b {
		t.Fatalf("want reassigned to b, got %s on %s", d.State, d.AgentID)
	}
	if d.Failures != 0 {
		t.Fatal("a yield must not count as a failure")
	}
	as = h.poll(b)
	if len(as) != 1 || as[0].Attempt != 2 {
		t.Fatalf("b assignment: %+v", as)
	}
	if err := h.e.TaskUpdate(h.ctx, b, as[0].TaskID, api.TaskUpdate{Attempt: 2, Outcome: api.OutcomeSucceeded, ExitCode: ip(0)}); err != nil {
		t.Fatal(err)
	}
	d = h.task(j.Tasks[0].ID)
	if d.State != api.StateSucceeded || len(d.Attempts) != 2 || d.Attempts[0].Outcome != api.OutcomeYielded || d.Attempts[1].AgentID != b {
		t.Fatalf("attempt history wrong: %+v", d.Attempts)
	}
	if jj, _ := h.e.Job(h.ctx, j.ID, false); jj.State != api.StateSucceeded {
		t.Fatalf("job state %s", jj.State)
	}
}

func TestFailureBackoffExclusionThenFinalFailure(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	b := h.enrol("b", 1)
	h.hb(a, goodTel(), "")
	h.hb(b, activeUserTel(), "")
	spec := simpleTask("flaky")
	spec.MaxRetries = ip(1)
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	as := h.poll(a)
	if len(as) != 1 {
		t.Fatal("expected a")
	}
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed, ExitCode: ip(2), Error: "exit 2"}); err != nil {
		t.Fatal(err)
	}
	d := h.task(j.Tasks[0].ID)
	if d.State != api.StatePending || d.NotBefore == nil || d.Failures != 1 {
		t.Fatalf("want pending with backoff, got %+v", d.Task)
	}
	h.tick()
	if h.task(d.ID).State != api.StatePending {
		t.Fatal("must wait for backoff")
	}
	h.advance(11 * time.Second)
	h.hb(a, goodTel(), "")
	h.hb(b, activeUserTel(), "")
	h.tick()
	d = h.task(d.ID)
	if d.AgentID != b {
		t.Fatalf("failed Mac must be excluded; got %q (excluded %v)", d.AgentID, d.Excluded)
	}
	as = h.poll(b)
	if err := h.e.TaskUpdate(h.ctx, b, as[0].TaskID, api.TaskUpdate{Attempt: 2, Outcome: api.OutcomeFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if d = h.task(d.ID); d.State != api.StateFailed || d.Failures != 2 {
		t.Fatalf("want failed after retries, got %s/%d", d.State, d.Failures)
	}
	if jj, _ := h.e.Job(h.ctx, j.ID, false); jj.State != api.StateFailed {
		t.Fatalf("job state %s", jj.State)
	}
}

func TestQuarantineAfterConsecutiveFailures(t *testing.T) {
	h := newHarness(t)
	h.e.cfg.QuarantineAfter = 2
	a := h.enrol("a", 2)
	h.hb(a, goodTel(), "")
	s1, s2, s3 := simpleTask("x1"), simpleTask("x2"), simpleTask("x3")
	s1.MaxRetries, s2.MaxRetries = ip(0), ip(0)
	h.submit(api.JobSpec{Tasks: []api.TaskSpec{s1, s2}})
	h.tick()
	for _, as := range h.poll(a) {
		if err := h.e.TaskUpdate(h.ctx, a, as.TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed}); err != nil {
			t.Fatal(err)
		}
	}
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{s3}})
	h.tick()
	d := h.task(j.Tasks[0].ID)
	if d.State != api.StatePending || !strings.Contains(d.Placement[0].Reason, "quarantined") {
		t.Fatalf("Mac should be quarantined: %+v %+v", d.Task, d.Placement)
	}
	macs, _ := h.e.Macs(h.ctx)
	if macs[0].Quarantined == nil {
		t.Fatal("quarantine not visible on Mac")
	}
	h.advance(11 * time.Minute)
	h.hb(a, goodTel(), "")
	h.tick()
	if h.task(d.ID).State != api.StateAssigned {
		t.Fatal("quarantine should expire")
	}
}

func TestHeartbeatLossReassigns(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	b := h.enrol("b", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	if len(h.poll(a)) != 1 {
		t.Fatal("expected a")
	}
	// a goes silent; b keeps heartbeating.
	for i := 0; i < 4; i++ {
		h.advance(10 * time.Second)
		h.hb(b, activeUserTel(), "")
		h.tick()
	}
	d := h.task(j.Tasks[0].ID)
	if d.AgentID != b || d.State != api.StateAssigned || d.Failures != 0 {
		t.Fatalf("want reassigned to b without failure, got %+v", d.Task)
	}
	if d.Attempts[0].Outcome != api.OutcomeLost {
		t.Fatalf("attempt 1 outcome %s", d.Attempts[0].Outcome)
	}
	// a comes back and still reports the task: it must be told to stop.
	r := h.hb(a, goodTel(), "", d.ID)
	if len(r.Cancel) != 1 || r.Cancel[0] != d.ID {
		t.Fatalf("stale agent should get cancel, got %+v", r)
	}
	// Stale update from a is rejected.
	if err := h.e.TaskUpdate(h.ctx, a, d.ID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded}); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
}

func TestNotReadyAgentLosesUnclaimedAssignment(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	h.hb(a, goodTel(), api.AgentBusyUser)
	h.tick()
	if d := h.task(j.Tasks[0].ID); d.State != api.StatePending {
		t.Fatalf("assignment to a yielding Mac must be withdrawn, got %s", d.State)
	}
}

func TestMapReduceDAG(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 4)
	h.hb(a, goodTel(), "")
	var tasks []api.TaskSpec
	var reduceIn []api.Input
	for _, k := range []string{"m1", "m2", "m3"} {
		ts := simpleTask(k)
		ts.Outputs = []api.Output{{Name: "part"}}
		tasks = append(tasks, ts)
		reduceIn = append(reduceIn, api.Input{Name: k, From: &api.OutputRef{Task: k, Output: "part"}})
	}
	tasks = append(tasks, api.TaskSpec{Key: "reduce", Runtime: "sh", Inputs: reduceIn, Outputs: []api.Output{{Name: "all"}}})
	j := h.submit(api.JobSpec{Name: "mr", Tasks: tasks})
	byKey := map[string]api.Task{}
	for _, tk := range j.Tasks {
		byKey[tk.Key] = tk
	}
	if byKey["reduce"].State != api.StateBlocked || len(byKey["reduce"].Spec.DependsOn) != 3 {
		t.Fatalf("reduce should be blocked on 3 deps: %+v", byKey["reduce"])
	}
	h.tick()
	as := h.poll(a)
	if len(as) != 3 {
		t.Fatalf("want 3 map tasks, got %d", len(as))
	}
	for _, x := range as {
		if len(x.Outputs) != 1 || !strings.Contains(x.Outputs[0].URL, "m=PUT") {
			t.Fatalf("map output url: %+v", x.Outputs)
		}
	}
	// Two maps finish: reduce stays blocked.
	for _, x := range as[:2] {
		if err := h.e.TaskUpdate(h.ctx, a, x.TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded, Outputs: []string{"part"}}); err != nil {
			t.Fatal(err)
		}
	}
	h.tick()
	if h.task(byKey["reduce"].ID).State != api.StateBlocked {
		t.Fatal("reduce must wait for all maps")
	}
	// Missing output counts as a failure.
	if err := h.e.TaskUpdate(h.ctx, a, as[2].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	if d := h.task(as[2].TaskID); d.State != api.StatePending || d.Failures != 1 {
		t.Fatalf("missing output should fail the attempt: %+v", d.Task)
	}
	h.advance(15 * time.Second)
	h.hb(a, goodTel(), "")
	h.tick()
	as2 := h.poll(a)
	if len(as2) != 0 {
		t.Fatal("the only Mac is excluded for this task after a failure")
	}
	// Clear the exclusion window to let the single Mac retry.
	h.advance(2 * time.Hour)
	h.hb(a, goodTel(), "")
	h.tick()
	as2 = h.poll(a)
	if len(as2) != 1 || as2[0].Attempt != 2 {
		t.Fatalf("retry: %+v", as2)
	}
	if err := h.e.TaskUpdate(h.ctx, a, as2[0].TaskID, api.TaskUpdate{Attempt: 2, Outcome: api.OutcomeSucceeded, Outputs: []string{"part"}}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	red := h.poll(a)
	if len(red) != 1 || red[0].Key != "reduce" || len(red[0].Inputs) != 3 {
		t.Fatalf("reduce assignment: %+v", red)
	}
	if !strings.Contains(red[0].Inputs[0].URL, "jobs%2F"+j.ID+"%2Fm1%2Fpart") {
		t.Fatalf("reduce input should point at m1's output: %s", red[0].Inputs[0].URL)
	}
	if err := h.e.TaskUpdate(h.ctx, a, red[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded, Outputs: []string{"all"}}); err != nil {
		t.Fatal(err)
	}
	if jj, _ := h.e.Job(h.ctx, j.ID, false); jj.State != api.StateSucceeded {
		t.Fatalf("job %s", jj.State)
	}
}

func TestUpstreamFailureCancelsReduce(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 2)
	h.hb(a, goodTel(), "")
	m := simpleTask("m")
	m.MaxRetries = ip(0)
	m.Outputs = []api.Output{{Name: "o"}}
	r := api.TaskSpec{Key: "r", Runtime: "sh", Inputs: []api.Input{{Name: "i", From: &api.OutputRef{Task: "m", Output: "o"}}}}
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{m, r}})
	h.tick()
	as := h.poll(a)
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	jj, _ := h.e.Job(h.ctx, j.ID, true)
	if jj.State != api.StateFailed || jj.Tasks[1].State != api.StateCancelled {
		t.Fatalf("want job failed and reduce cancelled: %s %s", jj.State, jj.Tasks[1].State)
	}
}

func TestCancelJobStopsRunningTasks(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t"), simpleTask("u")}})
	h.tick()
	as := h.poll(a)
	jj, err := h.e.CancelJob(h.ctx, j.ID)
	if err != nil || jj.State != api.StateCancelled {
		t.Fatalf("cancel: %v %+v", err, jj)
	}
	r := h.hb(a, goodTel(), "", as[0].TaskID)
	if len(r.Cancel) != 1 {
		t.Fatal("agent must be told to stop the cancelled task")
	}
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeCancelled}); !errors.Is(err, ErrStale) {
		t.Fatalf("late update after cancel: %v", err)
	}
}

func TestOptOutStopsWorkAndDrainOnlyBlocksNew(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 2)
	h.hb(a, goodTel(), "")
	h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	as := h.poll(a)
	yes := true
	if err := h.e.PatchMac(h.ctx, a, api.MacPatch{Drain: &yes}); err != nil {
		t.Fatal(err)
	}
	r := h.hb(a, goodTel(), "", as[0].TaskID)
	if !r.Drain || len(r.Cancel) != 0 {
		t.Fatalf("drain: keep running task, take no new: %+v", r)
	}
	j2 := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("new")}})
	h.tick()
	if h.task(j2.Tasks[0].ID).State != api.StatePending {
		t.Fatal("drained Mac must not get new work")
	}
	if err := h.e.PatchMac(h.ctx, a, api.MacPatch{OptedOut: &yes}); err != nil {
		t.Fatal(err)
	}
	if r := h.hb(a, goodTel(), "", as[0].TaskID); len(r.Cancel) != 1 {
		t.Fatal("opt-out must stop running work")
	}
}

func TestValidateJob(t *testing.T) {
	bad := []api.JobSpec{
		{},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh"}, {Key: "a", Runtime: "sh"}}},
		{Tasks: []api.TaskSpec{{Key: "a b", Runtime: "sh"}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "../bin/sh"}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", DependsOn: []string{"b"}}, {Key: "b", Runtime: "sh", DependsOn: []string{"a"}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", DependsOn: []string{"zzz"}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", Inputs: []api.Input{{Name: "i"}}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", Inputs: []api.Input{{Name: "i", URL: "http://x/y", Blob: "k"}}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", Inputs: []api.Input{{Name: "i", URL: "file:///etc/passwd"}}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", Inputs: []api.Input{{Name: "i", Blob: "../x"}}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh"}, {Key: "b", Runtime: "sh", Inputs: []api.Input{{Name: "i", From: &api.OutputRef{Task: "a", Output: "nope"}}}}}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", Outputs: []api.Output{{Name: "o"}, {Name: "o"}}}}},
	}
	for i, s := range bad {
		if err := ValidateJob(&s); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
	ok := api.JobSpec{Tasks: []api.TaskSpec{
		{Key: "a", Runtime: "sh", Outputs: []api.Output{{Name: "o"}}},
		{Key: "b", Runtime: "sh", Inputs: []api.Input{{Name: "i", From: &api.OutputRef{Task: "a", Output: "o"}}}},
	}}
	if err := ValidateJob(&ok); err != nil {
		t.Fatal(err)
	}
	if len(ok.Tasks[1].DependsOn) != 1 || ok.Tasks[1].DependsOn[0] != "a" || ok.Name != "job" {
		t.Fatalf("implicit dependency not added: %+v", ok.Tasks[1])
	}
}

func TestJobStateFromCounts(t *testing.T) {
	cases := []struct {
		c      map[string]int
		cancel bool
		want   string
	}{
		{map[string]int{"pending": 2}, false, JobQueued},
		{map[string]int{"running": 1, "pending": 1}, false, JobRunning},
		{map[string]int{"succeeded": 1, "blocked": 1}, false, JobRunning},
		{map[string]int{"succeeded": 2}, false, api.StateSucceeded},
		{map[string]int{"succeeded": 1, "failed": 1}, false, api.StateFailed},
		{map[string]int{"cancelled": 2}, true, api.StateCancelled},
	}
	for _, tc := range cases {
		if got := jobStateFromCounts(tc.c, tc.cancel); got != tc.want {
			t.Errorf("%v: got %s want %s", tc.c, got, tc.want)
		}
	}
}

func TestAPIKeysAndSettings(t *testing.T) {
	h := newHarness(t)
	k, err := h.e.CreateAPIKey(h.ctx, "ci", api.RoleApp)
	if err != nil || k.Secret == "" {
		t.Fatal(err)
	}
	if got, err := h.e.AuthAPIKey(h.ctx, k.Secret); err != nil || got.Role != api.RoleApp {
		t.Fatalf("auth: %v", err)
	}
	if err := h.e.RevokeAPIKey(h.ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AuthAPIKey(h.ctx, k.Secret); err == nil {
		t.Fatal("revoked key must fail")
	}
	s := h.e.Settings()
	s.Weights.Presence = 9
	if err := h.e.SetSettings(h.ctx, s); err != nil {
		t.Fatal(err)
	}
	if h.e.Settings().Weights.Presence != 9 {
		t.Fatal("settings not applied")
	}
	s.Weights.Load = -1
	if err := h.e.SetSettings(h.ctx, s); err == nil {
		t.Fatal("negative weight must fail")
	}
}

func TestAgentRestartRequeuesForgottenTasks(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	h.tick()
	as := h.poll(a)
	// Right after dispatch the agent may not list the task yet: keep it.
	h.hb(a, goodTel(), "")
	if d := h.task(j.Tasks[0].ID); d.State != api.StateRunning {
		t.Fatalf("grace period: %s", d.State)
	}
	// The agent restarted and no longer runs it.
	h.advance(20 * time.Second)
	h.hb(a, goodTel(), "")
	d := h.task(as[0].TaskID)
	if d.State != api.StatePending || d.Failures != 0 || d.Attempts[0].Outcome != api.OutcomeLost {
		t.Fatalf("forgotten task should be requeued without failure: %+v", d.Task)
	}
}
