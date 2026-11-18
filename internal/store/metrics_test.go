package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestMetricsAndCleanup runs the history, events and cleanup SQL against
// SQLite, and Postgres when UMM_TEST_POSTGRES_URL is set.
func TestMetricsAndCleanup(t *testing.T) {
	urls := map[string]string{"sqlite": "sqlite://" + t.TempDir() + "/m.db"}
	if pg := os.Getenv("UMM_TEST_POSTGRES_URL"); pg != "" {
		urls["postgres"] = pg
	}
	for name, url := range urls {
		t.Run(name, func(t *testing.T) { exerciseMetrics(t, url) })
	}
}

func exerciseMetrics(t *testing.T, url string) {
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A private time window so reruns against one Postgres don't collide.
	var base int64 = 4_000_000_000_000
	for _, c := range rid()[:4] {
		base += int64(c) * 3_600_000 * 24 * 400
	}
	base = base / 900_000 * 900_000
	mac := "mac_" + rid()

	for i := 0; i < 3; i++ {
		fleet := Sample{"macs_online": float64(i + 1), "succeeded": 2, "task_seconds": 60}
		macs := map[string]Sample{mac: {"user_cpu": float64(10 * (i + 1)), "task_seconds": 60, "online": 1}}
		if err := s.PutSamples(ctx, base+int64(i)*60_000, fleet, macs); err != nil {
			t.Fatal(err)
		}
	}
	// Rewriting a bucket replaces it.
	if err := s.PutSamples(ctx, base+120_000, Sample{"macs_online": 3, "succeeded": 2, "task_seconds": 60},
		map[string]Sample{mac: {"user_cpu": 30, "task_seconds": 60, "online": 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, base); err != nil { // idempotent
		t.Fatal(err)
	}

	raw, err := s.FleetSeries(ctx, ResRaw, base, base+900_000, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw[0].TS != base || raw[0].Values["macs_online"] != 1.5 || raw[0].Values["succeeded"] != 4 {
		t.Fatalf("raw merged into 2-minute steps: %+v", raw)
	}
	roll, err := s.FleetSeries(ctx, ResRollup, base, base+900_000, ResRollup)
	if err != nil {
		t.Fatal(err)
	}
	if len(roll) != 1 || roll[0].Values["macs_online"] != 2 || roll[0].Values["succeeded"] != 6 || roll[0].Values["task_seconds"] != 180 {
		t.Fatalf("rollup: %+v", roll)
	}
	ms, err := s.MacSeries(ctx, mac, ResRollup, base, base+900_000, ResRollup)
	if err != nil || len(ms) != 1 || ms[0].Values["user_cpu"] != 20 {
		t.Fatalf("mac rollup: %+v %v", ms, err)
	}
	all, err := s.AllMacSeries(ctx, ResRaw, base, base+900_000, 60)
	if err != nil || len(all[mac]) != 3 {
		t.Fatalf("all macs: %+v %v", all, err)
	}
	tot, err := s.FleetTotals(ctx, ResRaw, base, base+900_000)
	if err != nil || tot["succeeded"] != 6 || tot["task_seconds"] != 180 {
		t.Fatalf("totals: %v %v", tot, err)
	}
	if n, err := s.PruneSamples(ctx, ResRaw, base+60_000); err != nil || n != 2 { // fleet + mac row
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := s.DeleteMacSamples(ctx, mac); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.AllMacSeries(ctx, ResRaw, base, base+900_000, 60); len(all[mac]) != 0 {
		t.Fatal("mac samples should be gone")
	}

	// Events.
	tgt := "t_" + rid()
	for i, k := range []string{"mac.enrolled", "mac.deleted", "key.deleted"} {
		if err := s.AddEvent(ctx, &Event{ID: rid(), TS: base + int64(i), Kind: k, Target: tgt, Message: k}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.Events(ctx, []string{"mac."}, tgt, 0, 10)
	if err != nil || len(evs) != 2 || evs[0].Kind != "mac.deleted" {
		t.Fatalf("events: %+v %v", evs, err)
	}
	if evs, _ := s.Events(ctx, nil, tgt, base+2, 10); len(evs) != 2 {
		t.Fatal("before filter")
	}

	// Keys.
	k := &APIKey{ID: "key_" + rid(), Name: "x", Hash: rid(), Prefix: "p", Role: "app", CreatedAt: 1}
	if err := s.CreateAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAPIKey(ctx, k.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("active key: %v", err)
	}
	_ = s.RevokeAPIKey(ctx, k.ID, 5)
	if _, err := s.DeleteAPIKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}

	// Install tokens.
	live := &InstallToken{ID: "ins_" + rid(), Hash: rid(), Prefix: "p", Profile: "default", Spec: "{}", CreatedAt: 1,
		ExpiresAt: base * 2, MaxUses: 1}
	dead := &InstallToken{ID: "ins_" + rid(), Hash: rid(), Prefix: "p", Profile: "default", Spec: "{}", CreatedAt: 1,
		ExpiresAt: 2, MaxUses: 1}
	for _, it := range []*InstallToken{live, dead} {
		if err := s.CreateInstallToken(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteInstallToken(ctx, live.ID, base); !errors.Is(err, ErrActive) {
		t.Fatalf("live link: %v", err)
	}
	ts, err := s.DeleteDeadInstallTokens(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	gone := false
	for _, x := range ts {
		if x.ID == live.ID {
			t.Fatal("live link purged")
		}
		gone = gone || x.ID == dead.ID
	}
	if !gone {
		t.Fatal("dead link not purged")
	}

	// Jobs and timelines.
	j := &Job{ID: "job_" + rid(), Name: "n", Owner: "o", Labels: "{}", State: "succeeded", CreatedAt: 1, FinishedAt: 10}
	tk := &Task{ID: "tsk_" + rid(), JobID: j.ID, Key: "a", Spec: "{}", State: "succeeded", Excluded: "{}", Outputs: "[]", CreatedAt: 1}
	if err := s.CreateJob(ctx, j, []*Task{tk}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAttempt(ctx, &Attempt{TaskID: tk.ID, Attempt: 1, AgentID: mac, StartedAt: 3, Outcome: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	_ = s.AppendLog(ctx, tk.ID, 1, 1, 3, "hi")
	if tl, err := s.JobAttempts(ctx, j.ID); err != nil || len(tl) != 1 || tl[0].TaskKey != "a" || tl[0].JobName != "n" {
		t.Fatalf("timeline: %+v %v", tl, err)
	}
	if tl, err := s.AgentAttempts(ctx, mac, 5); err != nil || len(tl) != 1 {
		t.Fatalf("agent attempts: %+v %v", tl, err)
	}
	ids, err := s.FinishedJobs(ctx, 11, []string{"succeeded"}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		found = found || id == j.ID
	}
	if !found {
		t.Fatal("finished job not listed")
	}
	if err := s.DeleteJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(ctx, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("task must be deleted with its job")
	}
	if err := s.DeleteJob(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleting twice: not found")
	}
	if _, err := s.PruneEnrolTokens(ctx, base); err != nil {
		t.Fatal(err)
	}
}
