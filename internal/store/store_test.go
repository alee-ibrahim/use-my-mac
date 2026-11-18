package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"crypto/rand"
	"encoding/hex"
)

func rid() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// TestStore runs against SQLite always, and against Postgres when
// UMM_TEST_POSTGRES_URL is set (CI starts a Postgres service).
func TestStore(t *testing.T) {
	urls := map[string]string{"sqlite": "sqlite://" + t.TempDir() + "/t.db"}
	if pg := os.Getenv("UMM_TEST_POSTGRES_URL"); pg != "" {
		urls["postgres"] = pg
	}
	for name, url := range urls {
		t.Run(name, func(t *testing.T) { exercise(t, url) })
	}
}

func exercise(t *testing.T, url string) {
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Re-open: migrations are idempotent.
	s2, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()

	if s.rebind("a=? AND b=?") != map[bool]string{true: "a=$1 AND b=$2", false: "a=? AND b=?"}[s.postgres] {
		t.Fatal("rebind")
	}

	// Settings upsert.
	k := "k" + rid()
	if _, err := s.GetSetting(ctx, k); !errors.Is(err, ErrNotFound) {
		t.Fatal("want not found")
	}
	s.PutSetting(ctx, k, "1")
	s.PutSetting(ctx, k, "2")
	if v, _ := s.GetSetting(ctx, k); v != "2" {
		t.Fatalf("setting %q", v)
	}

	// Enrolment tokens are single use and expire.
	h := "h" + rid()
	if err := s.CreateEnrolToken(ctx, h, 100, 200, "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeEnrolToken(ctx, h, 300); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired token consumed")
	}
	h2 := "h" + rid()
	s.CreateEnrolToken(ctx, h2, 100, 200, "me")
	if err := s.ConsumeEnrolToken(ctx, h2, 150); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeEnrolToken(ctx, h2, 151); !errors.Is(err, ErrNotFound) {
		t.Fatal("token reused")
	}

	// Agents.
	a := &Agent{ID: "a" + rid(), Name: "mac", CredHash: "c" + rid(), CreatedAt: 1, Runtimes: "[]", Labels: "{}", MaxTasks: 2}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentByCredential(ctx, a.CredHash)
	if err != nil || got.ID != a.ID || got.MaxTasks != 2 {
		t.Fatalf("agent %v %+v", err, got)
	}
	s.SetAgentFlags(ctx, a.ID, true, false)
	s.SetAgentHealth(ctx, a.ID, 2, 99)
	got.LastSeen, got.State, got.Telemetry = 5, "ready", `{"cpu_count":8}`
	s.UpdateAgentHeartbeat(ctx, got)
	got, _ = s.GetAgent(ctx, a.ID)
	if !got.Drain || got.ConsecFailures != 2 || got.QuarantinedUntil != 99 || got.State != "ready" || got.LastSeen != 5 {
		t.Fatalf("agent update %+v", got)
	}

	// API keys.
	key := &APIKey{ID: "k" + rid(), Name: "n", Hash: "x" + rid(), Prefix: "p", Role: "admin", CreatedAt: 1}
	s.CreateAPIKey(ctx, key)
	if _, err := s.APIKeyByHash(ctx, key.Hash); err != nil {
		t.Fatal(err)
	}
	s.RevokeAPIKey(ctx, key.ID, 9)
	if _, err := s.APIKeyByHash(ctx, key.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked key still valid")
	}

	// Jobs, tasks, attempts, logs.
	j := &Job{ID: "j" + rid(), Name: "job", Owner: "o", Labels: "{}", State: "queued", CreatedAt: 10}
	exit := 3
	ts := []*Task{
		{ID: "t1" + rid(), JobID: j.ID, Key: "a", Spec: "{}", State: "pending", Excluded: "{}", Outputs: "[]", CreatedAt: 10},
		{ID: "t2" + rid(), JobID: j.ID, Key: "b", Spec: "{}", State: "blocked", Excluded: "{}", Outputs: "[]", CreatedAt: 11, ExitCode: &exit},
	}
	if err := s.CreateJob(ctx, j, ts); err != nil {
		t.Fatal(err)
	}
	dup := &Job{ID: "j" + rid(), Name: "dup", Owner: "o", Labels: "{}", State: "queued", CreatedAt: 10}
	bad := []*Task{ts[0]} // duplicate task ID: whole job must roll back
	if err := s.CreateJob(ctx, dup, bad); err == nil {
		t.Fatal("duplicate task id accepted")
	}
	if _, err := s.GetJob(ctx, dup.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("job insert not rolled back")
	}
	all, _ := s.TasksByJob(ctx, j.ID)
	if len(all) != 2 || all[0].Key != "a" || all[1].ExitCode == nil || *all[1].ExitCode != 3 {
		t.Fatalf("tasks %+v", all)
	}
	all[0].State, all[0].Attempt, all[0].AgentID = "running", 1, a.ID
	if err := s.UpdateTask(ctx, all[0]); err != nil {
		t.Fatal(err)
	}
	counts, _ := s.TaskCounts(ctx, j.ID)
	if counts["running"] != 1 || counts["blocked"] != 1 {
		t.Fatalf("counts %v", counts)
	}
	if err := s.InsertAttempt(ctx, &Attempt{TaskID: all[0].ID, Attempt: 1, AgentID: a.ID, StartedAt: 1, Outcome: "running", Score: 88.5}); err != nil {
		t.Fatal(err)
	}
	s.FinishAttempt(ctx, all[0].ID, 1, "yielded", "user busy", 2)
	att, _ := s.Attempts(ctx, all[0].ID)
	if len(att) != 1 || att[0].Outcome != "yielded" || att[0].Score != 88.5 {
		t.Fatalf("attempts %+v", att[0])
	}
	s.AppendLog(ctx, all[0].ID, 1, 1, 1, "hello ")
	s.AppendLog(ctx, all[0].ID, 1, 1, 1, "hello ") // duplicate chunk ignored
	s.AppendLog(ctx, all[0].ID, 1, 2, 1, "world\n")
	s.AppendLog(ctx, all[0].ID, 2, 1, 1, "again\n")
	logs, _ := s.Logs(ctx, all[0].ID, 0)
	if !strings.Contains(logs, "=== attempt 1 ===\nhello world\n=== attempt 2 ===\nagain\n") {
		t.Fatalf("logs %q", logs)
	}
	if n, _ := s.LogSize(ctx, all[0].ID); n != len("hello world\nagain\n") {
		t.Fatalf("log size %d", n)
	}
	js, _ := s.ListJobs(ctx, "queued", "o", 10)
	if len(js) == 0 {
		t.Fatal("list jobs")
	}
}
