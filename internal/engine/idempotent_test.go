package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestIdempotentSubmit(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	spec := api.JobSpec{Name: "batch-1", ClientID: "feeder/batch-1", Tasks: []api.TaskSpec{
		{Key: "a", Runtime: "sh", ClientID: "clip-a"},
		{Key: "b", Runtime: "sh", ClientID: "clip-b"},
	}}
	first, err := h.e.SubmitJobAs(ctx, ownerA, spec)
	if err != nil || first.Existing || first.ClientID != "feeder/batch-1" {
		t.Fatalf("first submit: %+v %v", first, err)
	}
	again, err := h.e.SubmitJobAs(ctx, ownerA, spec)
	if err != nil || !again.Existing || again.ID != first.ID {
		t.Fatalf("re-submit: %+v %v", again, err)
	}
	// Another owner's client IDs are separate.
	other, err := h.e.SubmitJobAs(ctx, ownerB, spec)
	if err != nil || other.Existing || other.ID == first.ID {
		t.Fatalf("other owner: %+v %v", other, err)
	}
	// Same job client ID, different spec: refused.
	changed := spec
	changed.Name = "batch-1-changed"
	if _, err := h.e.SubmitJobAs(ctx, ownerA, changed); !errors.Is(err, ErrActive) {
		t.Fatalf("changed spec: %v", err)
	}

	// Task client IDs: a new batch repeating clip-b only creates clip-c.
	next, err := h.e.SubmitJobAs(ctx, ownerA, api.JobSpec{Name: "batch-2", Tasks: []api.TaskSpec{
		{Key: "b", Runtime: "sh", ClientID: "clip-b"},
		{Key: "c", Runtime: "sh", ClientID: "clip-c"},
	}})
	if err != nil || next.Existing || len(next.ExistingTasks) != 1 || next.ExistingTasks[0].JobID != first.ID ||
		next.ExistingTasks[0].ClientID != "clip-b" {
		t.Fatalf("batch-2: %+v %v", next, err)
	}
	full, _ := h.e.Job(ctx, next.ID, true)
	if len(full.Tasks) != 1 || full.Tasks[0].Key != "c" {
		t.Fatalf("batch-2 tasks: %+v", full.Tasks)
	}
	// Every task exists: nothing is created, the existing job comes back.
	all, err := h.e.SubmitJobAs(ctx, ownerA, api.JobSpec{Tasks: []api.TaskSpec{{Key: "x", Runtime: "sh", ClientID: "clip-a"}}})
	if err != nil || !all.Existing || all.ID != first.ID || len(all.ExistingTasks) != 1 {
		t.Fatalf("all existing: %+v %v", all, err)
	}
	// A new task may not depend on one that already exists elsewhere.
	_, err = h.e.SubmitJobAs(ctx, ownerA, api.JobSpec{Tasks: []api.TaskSpec{
		{Key: "a", Runtime: "sh", ClientID: "clip-a", Outputs: []api.Output{{Name: "o"}}},
		{Key: "r", Runtime: "sh", Inputs: []api.Input{{Name: "i", From: &api.OutputRef{Task: "a", Output: "o"}}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "depends on a") {
		t.Fatalf("cross-job dependency: %v", err)
	}
	for _, bad := range []api.JobSpec{
		{ClientID: "has space", Tasks: []api.TaskSpec{simpleTask("t")}},
		{Tasks: []api.TaskSpec{{Key: "a", Runtime: "sh", ClientID: "x"}, {Key: "b", Runtime: "sh", ClientID: "x"}}},
	} {
		if _, err := h.e.SubmitJobAs(ctx, ownerA, bad); err == nil {
			t.Fatalf("invalid client IDs accepted: %+v", bad)
		}
	}
	// Deleting the job frees its client IDs.
	if err := h.e.RemoveJob(ctx, ownerA, first.ID, false); err != nil {
		t.Fatal(err)
	}
	again, err = h.e.SubmitJobAs(ctx, ownerA, spec)
	if err != nil || again.Existing || again.ID == first.ID {
		t.Fatalf("after delete: %+v %v", again, err)
	}
}

func TestOutputChecksums(t *testing.T) {
	h := newHarness(t)
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submitAs(ownerA, api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh",
		Outputs: []api.Output{{Name: "o"}, {Name: "p"}}}}})
	h.tick()
	as := h.poll(a)
	for _, o := range []string{"o", "p"} {
		_ = h.e.blobs.Put(h.ctx, OutputKey(j.ID, "t", o), strings.NewReader(o), -1)
	}
	good := strings.Repeat("ab", 32)
	if err := h.e.TaskUpdate(h.ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeSucceeded, ExitCode: ip(0),
		Outputs: []string{"o", "p"}, OutputSHA256: map[string]string{"o": strings.ToUpper(good), "p": "not-a-digest", "q": good}}); err != nil {
		t.Fatal(err)
	}
	tk := h.task(as[0].TaskID)
	if len(tk.OutputSHA256) != 1 || tk.OutputSHA256["o"] != good {
		t.Fatalf("digests: %+v", tk.OutputSHA256)
	}
	fs, _ := h.e.Files(h.ctx, ownerA, FileFilter{JobID: j.ID})
	for _, f := range fs {
		if (f.TaskID == as[0].TaskID && strings.HasSuffix(f.Key, "/o")) != (f.SHA256 == good) {
			t.Fatalf("file digest: %+v", f)
		}
	}
}
