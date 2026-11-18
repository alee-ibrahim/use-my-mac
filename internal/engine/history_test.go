package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func (h *harness) sample() {
	h.t.Helper()
	if err := h.e.SampleHistory(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// runOne runs a one-task job on mac and reports outcome.
func (h *harness) runOne(mac, outcome string) *api.Job {
	h.t.Helper()
	j := h.submit(api.JobSpec{Name: "j-" + outcome, Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh", MaxRetries: ip(0)}}})
	h.tick()
	as := h.poll(mac)
	if len(as) != 1 {
		h.t.Fatalf("no assignment for %s", outcome)
	}
	if outcome != "" {
		if err := h.e.TaskUpdate(h.ctx, mac, as[0].TaskID, api.TaskUpdate{Attempt: as[0].Attempt, Outcome: outcome,
			ExitCode: ip(0)}); err != nil {
			h.t.Fatal(err)
		}
	}
	return j
}

func TestHistorySampleRollupAndQueries(t *testing.T) {
	h := newHarness(t)
	// Align to a 15-minute boundary so the rollup bucket is predictable.
	h.now = time.UnixMilli(bucket(h.now.UnixMilli(), store.ResRollup)).Add(30 * time.Second)
	start := h.now
	a := h.enrol("a", 1)
	b := h.enrol("b", 1)
	tel := goodTel()
	tel.UserCPUPercent, tel.GPUUtilPercent = 20, 50
	h.hb(a, tel, "")
	tel.UserCPUPercent = 40
	h.hb(a, tel, "")
	h.hb(b, goodTel(), api.AgentBusyUser)

	h.runOne(a, api.OutcomeSucceeded)
	h.runOne(a, api.OutcomeFailed)
	h.runOne(a, "") // stays running
	h.sample()

	for i := 0; i < 4; i++ {
		h.advance(time.Minute)
		h.hb(a, goodTel(), "", h.runningOn(a)...)
		h.hb(b, goodTel(), api.AgentBusyUser)
		h.sample()
	}

	s, err := h.e.FleetSeries(h.ctx, start.Add(-time.Minute), h.now.Add(time.Second), 60)
	if err != nil {
		t.Fatal(err)
	}
	if s.Res != store.ResRaw || s.Step != 60 {
		t.Fatalf("res/step %d/%d", s.Res, s.Step)
	}
	var withData []map[string]float64
	for _, p := range s.Points {
		if _, ok := p["macs_online"]; ok {
			withData = append(withData, p)
		}
	}
	if len(withData) != 5 {
		t.Fatalf("want 5 raw points, got %d of %d", len(withData), len(s.Points))
	}
	first := withData[0]
	if first["succeeded"] != 1 || first["failed"] != 1 || first["assigned"] != 3 {
		t.Fatalf("counters in first sample: %v", first)
	}
	if first["macs_online"] != 2 || first["macs_busy"] != 1 || first["macs_yielded"] != 1 || first["tasks_running"] != 1 {
		t.Fatalf("gauges: %v", first)
	}
	if withData[1]["succeeded"] != 0 {
		t.Fatal("counters must reset after a sample")
	}
	if withData[1]["task_seconds"] != 60 {
		t.Fatalf("task_seconds: %v", withData[1]["task_seconds"])
	}

	// Per-Mac averages: the first sample averages two heartbeats (20, 40).
	ms, err := h.e.MacSeries(h.ctx, a, start.Add(-time.Minute), h.now.Add(time.Second), 60)
	if err != nil {
		t.Fatal(err)
	}
	var macFirst map[string]float64
	for _, p := range ms.Points {
		if _, ok := p["online"]; ok {
			macFirst = p
			break
		}
	}
	if macFirst["user_cpu"] != 30 || macFirst["gpu"] != 50 || macFirst["online"] != 1 || macFirst["running"] != 1 {
		t.Fatalf("mac sample: %v", macFirst)
	}

	// Rollup: one 15-minute bucket summing counters, averaging gauges (the first
	// sample covers 30 s: the engine started on the bucket boundary).
	pts, err := h.e.st.FleetSeries(h.ctx, store.ResRollup, start.Add(-time.Hour).UnixMilli(), h.now.Add(time.Hour).UnixMilli(), store.ResRollup)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Values["succeeded"] != 1 || pts[0].Values["task_seconds"] != 270 || pts[0].Values["macs_online"] != 2 {
		t.Fatalf("rollup: %+v", pts)
	}

	// Older ranges come from rollups.
	h.advance(48 * time.Hour)
	s, err = h.e.FleetSeries(h.ctx, start.Add(-time.Hour), h.now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Res != store.ResRollup || s.Step%store.ResRollup != 0 {
		t.Fatalf("old range should use rollups, got res %d step %d", s.Res, s.Step)
	}

	sum, err := h.e.Summary(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Last7d.Succeeded != 1 || sum.Last7d.Failed != 1 || sum.Last7d.SuccessRate != 0.5 {
		t.Fatalf("7d window: %+v", sum.Last7d)
	}
	if sum.Last24h.Succeeded != 0 || sum.Last24h.SuccessRate != -1 {
		t.Fatalf("24h window should be empty now: %+v", sum.Last24h)
	}
	if sum.Macs["total"] != 2 || sum.Macs["offline"] != 2 {
		t.Fatalf("macs: %v", sum.Macs)
	}

	sp, err := h.e.MacSparks(h.ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Step <= 0 || sp.Macs == nil {
		t.Fatalf("sparks: %+v", sp)
	}
}

func (h *harness) runningOn(mac string) []string {
	ts, _ := h.e.st.RunningTasksForAgent(h.ctx, mac)
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestRetentionPrunes(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	h.sample()
	old := h.runOne(a, api.OutcomeSucceeded)
	if _, err := h.e.SetRetention(h.ctx, api.Retention{JobsDays: 2}, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.SetRetention(h.ctx, api.Retention{MetricsRawHours: 2}, "admin"); err == nil {
		t.Fatal("raw retention below 24h must be refused")
	}
	h.advance(3 * 24 * time.Hour)
	h.hb(a, goodTel(), "")
	fresh := h.runOne(a, api.OutcomeSucceeded)
	h.sample()
	if err := h.e.PruneHistory(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Job(h.ctx, old.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old job should be pruned: %v", err)
	}
	if _, err := h.e.Job(h.ctx, fresh.ID, false); err != nil {
		t.Fatalf("fresh job must stay: %v", err)
	}
	if n, _ := h.e.st.OldestSample(h.ctx, store.ResRaw); n < h.now.Add(-25*time.Hour).UnixMilli() {
		t.Fatal("raw samples older than retention must be pruned")
	}
	evs, _ := h.e.Events(h.ctx, []string{api.EventJobsPurged}, "", time.Time{}, 10)
	if len(evs) != 1 || evs[0].Actor != "retention" {
		t.Fatalf("purge must be audited: %+v", evs)
	}
}

func TestCleanupDeletes(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	// API keys: active keys are refused, revoked ones go.
	k, _ := h.e.CreateAPIKey(ctx, "ci", api.RoleApp)
	if err := h.e.DeleteAPIKey(ctx, k.ID, "admin"); !errors.Is(err, ErrActive) {
		t.Fatalf("active key delete: %v", err)
	}
	if err := h.e.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.e.DeleteAPIKey(ctx, k.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.DeleteAPIKey(ctx, k.ID, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	k2, _ := h.e.CreateAPIKey(ctx, "old", api.RoleApp)
	keep, _ := h.e.CreateAPIKey(ctx, "keep", api.RoleApp)
	_ = h.e.RevokeAPIKey(ctx, k2.ID)
	if n, err := h.e.PurgeAPIKeys(ctx, "admin"); err != nil || n != 1 {
		t.Fatalf("purge keys: %d %v", n, err)
	}
	ks, _ := h.e.APIKeys(ctx)
	if len(ks) != 1 || ks[0].ID != keep.ID {
		t.Fatalf("keys left: %+v", ks)
	}

	// Jobs: only finished ones.
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	running := h.runOne(a, "")
	if err := h.e.DeleteJob(ctx, running.ID, "admin"); !errors.Is(err, ErrActive) {
		t.Fatalf("running job delete: %v", err)
	}
	if err := h.e.TaskUpdate(ctx, a, running.Tasks[0].ID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded, ExitCode: ip(0)}); err != nil {
		t.Fatal(err)
	}
	if tl, err := h.e.JobTimeline(ctx, running.ID); err != nil || len(tl) != 1 || tl[0].AgentName != "a" || tl[0].Outcome != api.OutcomeSucceeded {
		t.Fatalf("timeline: %+v %v", tl, err)
	}
	if as, err := h.e.MacAttempts(ctx, a, 10); err != nil || len(as) != 1 || as[0].JobID != running.ID {
		t.Fatalf("mac attempts: %+v %v", as, err)
	}
	if err := h.e.DeleteJob(ctx, running.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Task(ctx, running.Tasks[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("tasks must go with the job")
	}

	// Bulk purge by age and state.
	h.hb(a, goodTel(), "")
	f := h.runOne(a, api.OutcomeFailed)
	h.hb(a, goodTel(), "")
	s := h.runOne(a, api.OutcomeSucceeded)
	if _, err := h.e.PurgeJobs(ctx, api.PurgeJobsRequest{States: []string{"running"}}, "admin"); err == nil {
		t.Fatal("non-terminal states must be refused")
	}
	if n, _ := h.e.PurgeJobs(ctx, api.PurgeJobsRequest{OlderThanDays: 1}, "admin"); n != 0 {
		t.Fatal("nothing is a day old yet")
	}
	if n, _ := h.e.PurgeJobs(ctx, api.PurgeJobsRequest{States: []string{api.StateFailed}}, "admin"); n != 1 {
		t.Fatal("want the failed job purged")
	}
	if _, err := h.e.Job(ctx, f.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed job should be gone")
	}
	if _, err := h.e.Job(ctx, s.ID, false); err != nil {
		t.Fatal("succeeded job should stay")
	}

	// Macs: deleting revokes the credential and removes history.
	h.sample()
	if err := h.e.DeleteMacBy(ctx, a, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.MacSeries(ctx, a, time.Time{}, time.Time{}, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted Mac has no series")
	}
	evs, _ := h.e.Events(ctx, []string{"mac.", "key.", "job."}, "", time.Time{}, 50)
	kinds := map[string]bool{}
	for _, e := range evs {
		kinds[e.Kind] = true
		if strings.HasSuffix(e.Kind, ".deleted") && e.Actor != "admin" {
			t.Fatalf("deletion without actor: %+v", e)
		}
	}
	for _, k := range []string{api.EventKeyDeleted, api.EventJobDeleted, api.EventMacDeleted, api.EventMacEnrolled} {
		if !kinds[k] {
			t.Fatalf("missing event %s in %v", k, kinds)
		}
	}
}
