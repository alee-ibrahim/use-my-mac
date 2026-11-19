package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestLiveProgressFoldsIntoTaskAndJob(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 2)
	h.hb(a, goodTel(), "")
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t1"), simpleTask("t2")}})
	sub, cancel := h.e.Subscribe()
	defer cancel()
	h.tick()
	as := h.poll(a)
	if len(as) != 2 {
		t.Fatalf("want 2 assignments, got %d", len(as))
	}
	t1 := as[0]

	// Phase and percent before any report.
	if d := h.task(t1.TaskID); d.Phase != api.PhaseStarting || d.Percent != 0 || d.Progress != nil {
		t.Fatalf("before report: %s %v %+v", d.Phase, d.Percent, d.Progress)
	}

	// Running at 50% maps to 5 + 90*0.5 = 50 overall.
	if err := h.e.ReportProgress(h.ctx, a, t1.TaskID, api.TaskProgress{Attempt: t1.Attempt, Phase: api.PhaseRunning, Percent: 50}); err != nil {
		t.Fatal(err)
	}
	d := h.task(t1.TaskID)
	if d.Phase != api.PhaseRunning || d.Percent != 50 || d.Progress == nil || d.Progress.UpdatedAt == nil {
		t.Fatalf("after report: %s %v %+v", d.Phase, d.Percent, d.Progress)
	}
	jv, err := h.e.Job(h.ctx, j.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Percent != 25 || jv.Phase != api.PhaseRunning {
		t.Fatalf("job: %v %s", jv.Percent, jv.Phase)
	}
	select {
	case id := <-sub:
		if id != j.ID {
			t.Fatalf("notified %s, want %s", id, j.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("no change notification")
	}

	// Uploading bytes: 95 + 5 * 0.5.
	if err := h.e.ReportProgress(h.ctx, a, t1.TaskID, api.TaskProgress{Attempt: t1.Attempt, Phase: api.PhaseUploading, Percent: -1,
		BytesDone: 5, BytesTotal: 10}); err != nil {
		t.Fatal(err)
	}
	if d := h.task(t1.TaskID); d.Percent != 97.5 {
		t.Fatalf("uploading percent %v", d.Percent)
	}

	// Stale attempt, wrong agent and bad phase are rejected.
	if err := h.e.ReportProgress(h.ctx, a, t1.TaskID, api.TaskProgress{Attempt: t1.Attempt + 1, Phase: api.PhaseRunning}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale attempt: %v", err)
	}
	if err := h.e.ReportProgress(h.ctx, "agt_other", t1.TaskID, api.TaskProgress{Attempt: t1.Attempt, Phase: api.PhaseRunning}); !errors.Is(err, ErrStale) {
		t.Fatalf("other agent: %v", err)
	}
	var ve *ValidationError
	if err := h.e.ReportProgress(h.ctx, a, t1.TaskID, api.TaskProgress{Attempt: t1.Attempt, Phase: "dancing"}); !errors.As(err, &ve) {
		t.Fatalf("bad phase: %v", err)
	}

	// Finishing drops the live report; the task is done at 100%.
	if err := h.e.TaskUpdate(h.ctx, a, t1.TaskID, api.TaskUpdate{Attempt: t1.Attempt, Outcome: api.OutcomeSucceeded, ExitCode: ip(0)}); err != nil {
		t.Fatal(err)
	}
	if d := h.task(t1.TaskID); d.Phase != api.PhaseDone || d.Percent != 100 || d.Progress != nil {
		t.Fatalf("finished: %s %v %+v", d.Phase, d.Percent, d.Progress)
	}
	if h.e.progressOf(t1.TaskID, t1.Attempt) != nil {
		t.Fatal("progress kept after the attempt ended")
	}
	if jv, _ := h.e.Job(h.ctx, j.ID, false); jv.Percent != 50 {
		t.Fatalf("job after one of two done: %v", jv.Percent)
	}
	if err := h.e.ReportProgress(h.ctx, a, t1.TaskID, api.TaskProgress{Attempt: t1.Attempt, Phase: api.PhaseRunning}); !errors.Is(err, ErrStale) {
		t.Fatalf("report after finish: %v", err)
	}
}

func TestJobProgressNeverReports100UntilDone(t *testing.T) {
	pct, phase := jobProgress(api.StateRunning, map[string]int{api.StateSucceeded: 1, api.StateRunning: 1}, 100, map[string]int{api.PhaseUploading: 1})
	if pct != 99.9 || phase != api.PhaseUploading {
		t.Fatalf("got %v %s", pct, phase)
	}
	if pct, phase := jobProgress(api.StateSucceeded, map[string]int{api.StateSucceeded: 2}, 0, nil); pct != 100 || phase != api.PhaseDone {
		t.Fatalf("done: %v %s", pct, phase)
	}
	if pct, phase := jobProgress(api.StatePending, map[string]int{api.StatePending: 3}, 0, map[string]int{}); pct != 0 || phase != api.PhaseQueued {
		t.Fatalf("queued: %v %s", pct, phase)
	}
}
