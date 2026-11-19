package engine

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

var (
	ownerA = Caller{Subject: "key:key_a", Actor: "app-a"}
	ownerB = Caller{Subject: "key:key_b", Actor: "app-b"}
	admin  = Caller{Subject: "key:key_admin", Admin: true, Actor: "admin"}
)

func (h *harness) put(c Caller, key, body string) {
	h.t.Helper()
	if err := h.e.PutBlob(h.ctx, c, key, strings.NewReader(body), int64(len(body))); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) submitAs(c Caller, spec api.JobSpec) *api.Job {
	h.t.Helper()
	j, err := h.e.SubmitJobAs(h.ctx, c, spec)
	if err != nil {
		h.t.Fatal(err)
	}
	full, _ := h.e.Job(h.ctx, j.ID, true)
	return full
}

func (h *harness) exists(key string) bool {
	rc, err := h.e.blobs.Get(h.ctx, key)
	if err != nil {
		return false
	}
	rc.Close()
	return true
}

// finish runs the assignment to success, storing its outputs.
func (h *harness) finish(mac string, as api.Assignment) {
	h.t.Helper()
	var outs []string
	for _, o := range as.Outputs {
		key := OutputKey(as.JobID, as.Key, o.Name)
		if err := h.e.blobs.Put(h.ctx, key, strings.NewReader("result-"+o.Name), -1); err != nil {
			h.t.Fatal(err)
		}
		outs = append(outs, o.Name)
	}
	if err := h.e.TaskUpdate(h.ctx, mac, as.TaskID, api.TaskUpdate{Attempt: as.Attempt, Outcome: api.OutcomeSucceeded,
		ExitCode: ip(0), Outputs: outs}); err != nil {
		h.t.Fatal(err)
	}
}

func encodeSpec(blob string) api.JobSpec {
	return api.JobSpec{Name: "encode", Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh",
		Inputs: []api.Input{{Name: "src", Blob: blob}}, Outputs: []api.Output{{Name: "out.mp4"}}}}}
}

// TestOwnerPermissions: blobs and jobs belong to their owner; other app
// keys can neither read, overwrite, use nor delete them; admins can.
func TestOwnerPermissions(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	h.put(ownerA, "a/in.mov", "movie")

	if _, err := h.e.OpenBlob(ctx, ownerB, "a/in.mov"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner read: %v", err)
	}
	var fe *ForbiddenError
	if err := h.e.PutBlob(ctx, ownerB, "a/in.mov", strings.NewReader("x"), 1); !errors.As(err, &fe) {
		t.Fatalf("other owner overwrite: %v", err)
	}
	if _, err := h.e.CreateUploadAs(ctx, ownerB, api.UploadRequest{Key: "a/in.mov", Size: 10}); !errors.As(err, &fe) {
		t.Fatalf("other owner multipart overwrite: %v", err)
	}
	if _, err := h.e.SubmitJobAs(ctx, ownerB, encodeSpec("a/in.mov")); !errors.As(err, &fe) {
		t.Fatalf("other owner input: %v", err)
	}
	if rc, err := h.e.OpenBlob(ctx, admin, "a/in.mov"); err != nil {
		t.Fatalf("admin read: %v", err)
	} else {
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != "movie" {
			t.Fatalf("content %q", b)
		}
	}
	// An admin overwrite keeps the owner.
	h.put(admin, "a/in.mov", "movie2")
	if b, _ := h.e.st.GetBlob(ctx, UploadKey("a/in.mov")); b.Owner != ownerA.Subject {
		t.Fatalf("owner after admin overwrite: %+v", b)
	}

	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	j := h.submitAs(ownerA, encodeSpec("a/in.mov"))
	h.tick()
	as := h.poll(a)
	if len(as) != 1 {
		t.Fatal("no assignment")
	}

	// Files: each owner sees only their own.
	if fs, _ := h.e.Files(ctx, ownerB, FileFilter{}); len(fs) != 0 {
		t.Fatalf("B sees %+v", fs)
	}
	if _, err := h.e.Files(ctx, ownerB, FileFilter{Owner: ownerA.Subject}); !errors.As(err, &fe) {
		t.Fatalf("B listing A's files: %v", err)
	}
	fs, _ := h.e.Files(ctx, ownerA, FileFilter{})
	if len(fs) != 1 || fs[0].Blob != "a/in.mov" || !fs[0].InUse {
		t.Fatalf("A's files: %+v", fs)
	}
	if err := h.e.DeleteFile(ctx, ownerB, "a/in.mov"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B deletes A's file: %v", err)
	}
	if err := h.e.DeleteFile(ctx, ownerA, "a/in.mov"); !errors.Is(err, ErrActive) {
		t.Fatalf("delete in-use input: %v", err)
	}

	// Jobs: B cannot delete A's job; A can, while it runs.
	if err := h.e.RemoveJob(ctx, ownerB, j.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B deletes A's job: %v", err)
	}
	if err := h.e.RemoveJob(ctx, ownerA, j.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := h.hb(a, goodTel(), "", as[0].TaskID); len(r.Cancel) != 1 {
		t.Fatal("the running task must be stopped")
	}
	if _, err := h.e.Job(ctx, j.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("job must be gone")
	}
	if h.exists(UploadKey("a/in.mov")) {
		t.Fatal("inputs=true must delete the input")
	}
	// A late signed upload to the deleted job's outputs is refused.
	if h.e.OutputKeyLive(ctx, OutputKey(j.ID, "t", "out.mp4")) {
		t.Fatal("deleted job's output key must not be live")
	}

	// Admins can delete anyone's job.
	h.put(ownerB, "b/in.mov", "b")
	jb := h.submitAs(ownerB, encodeSpec("b/in.mov"))
	if err := h.e.RemoveJob(ctx, admin, jb.ID, false); err != nil {
		t.Fatal(err)
	}
	if !h.exists(UploadKey("b/in.mov")) {
		t.Fatal("inputs=false keeps the input")
	}

	evs, _ := h.e.Events(ctx, []string{api.EventJobDeleted}, "", time.Time{}, 10)
	byActor := map[string]string{}
	for _, ev := range evs {
		byActor[ev.Actor] = ev.Message
	}
	if len(evs) != 2 || !strings.Contains(byActor["app-a"], "cancelled running") || byActor["admin"] == "" {
		t.Fatalf("audit: %+v", evs)
	}
}

// TestCascadeDelete: deleting a job removes its outputs from storage and
// the file list; inputs shared with an unfinished job stay.
func TestCascadeDelete(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	h.put(ownerA, "shared.mov", "s")
	h.put(ownerA, "own.mov", "o")
	spec := api.JobSpec{Name: "mr", Tasks: []api.TaskSpec{
		{Key: "m", Runtime: "sh", Inputs: []api.Input{{Name: "a", Blob: "shared.mov"}, {Name: "b", Blob: "own.mov"}},
			Outputs: []api.Output{{Name: "part"}}},
		{Key: "r", Runtime: "sh", Inputs: []api.Input{{Name: "p", From: &api.OutputRef{Task: "m", Output: "part"}}},
			Outputs: []api.Output{{Name: "final"}}},
	}}
	j := h.submitAs(ownerA, spec)
	for range 2 {
		h.tick()
		as := h.poll(a)
		if len(as) != 1 {
			t.Fatal("no assignment")
		}
		h.finish(a, as[0])
		h.hb(a, goodTel(), "")
	}
	h.tick()
	if jj, _ := h.e.Job(ctx, j.ID, false); jj.State != api.StateSucceeded {
		t.Fatalf("job %s", jj.State)
	}
	outs, _ := h.e.Files(ctx, ownerA, FileFilter{JobID: j.ID})
	if len(outs) != 2 || outs[0].Kind != api.FileOutput || outs[0].Owner != ownerA.Subject || outs[0].Size == 0 {
		t.Fatalf("outputs: %+v", outs)
	}
	// Another queued job still needs shared.mov.
	other := h.submitAs(ownerA, encodeSpec("shared.mov"))

	if err := h.e.RemoveJob(ctx, ownerA, j.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{OutputKey(j.ID, "m", "part"), OutputKey(j.ID, "r", "final"), UploadKey("own.mov")} {
		if h.exists(k) {
			t.Fatalf("%s must be deleted", k)
		}
	}
	if !h.exists(UploadKey("shared.mov")) {
		t.Fatal("an input an unfinished job uses must stay")
	}
	fs, _ := h.e.Files(ctx, ownerA, FileFilter{})
	if len(fs) != 1 || fs[0].Blob != "shared.mov" {
		t.Fatalf("files left: %+v", fs)
	}

	// Bulk: every file not in use; the in-use one is reported.
	h.put(ownerA, "x/1", "1")
	h.put(ownerA, "x/2", "22")
	res, err := h.e.DeleteFiles(ctx, ownerA, api.DeleteFilesRequest{All: true})
	if err != nil || res.Deleted != 2 || res.Bytes != 3 || len(res.Skipped) != 1 || res.Skipped[0].Key != UploadKey("shared.mov") {
		t.Fatalf("bulk: %+v %v", res, err)
	}
	if _, err := h.e.DeleteFiles(ctx, ownerA, api.DeleteFilesRequest{Prefix: "uploads/"}); err == nil {
		t.Fatal("a filter without all must be refused")
	}
	if _, err := h.e.CancelJob(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.e.DeleteFile(ctx, ownerA, "shared.mov"); err != nil {
		t.Fatalf("delete after the job finished: %v", err)
	}
	evs, _ := h.e.Events(ctx, []string{api.EventFilesDeleted}, "", time.Time{}, 10)
	if len(evs) != 2 || evs[0].Actor != "app-a" {
		t.Fatalf("file audit: %+v", evs)
	}

	// Purge by owner only touches that owner's jobs.
	jb := h.submitAs(ownerB, api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	_, _ = h.e.CancelJob(ctx, jb.ID)
	ja := h.submitAs(ownerA, api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")}})
	_, _ = h.e.CancelJob(ctx, ja.ID)
	if n, err := h.e.PurgeJobs(ctx, api.PurgeJobsRequest{Owner: ownerA.Subject}, "app-a"); err != nil || n != 2 {
		t.Fatalf("purge A: %d %v", n, err)
	}
	if _, err := h.e.Job(ctx, jb.ID, false); err != nil {
		t.Fatal("B's job must stay")
	}
}

// TestCleanupPolicy: after_download deletes inputs and outputs once every
// result was downloaded; after_hours cleans failed jobs.
func TestCleanupPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	a := h.enrol("a", 1)
	h.hb(a, goodTel(), "")
	if _, err := h.e.SubmitJobAs(ctx, ownerA, api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t")},
		Cleanup: &api.Cleanup{KeepInputs: true}}); err == nil {
		t.Fatal("a cleanup without trigger must be refused")
	}
	h.put(ownerA, "in.mov", "in")
	spec := encodeSpec("in.mov")
	spec.Cleanup = &api.Cleanup{AfterDownload: true}
	j := h.submitAs(ownerA, spec)
	h.tick()
	as := h.poll(a)
	h.finish(a, as[0])
	h.tick()
	out := OutputKey(j.ID, "t", "out.mp4")

	h.advance(5 * time.Minute)
	h.e.SweepCleanup(ctx)
	if !h.exists(out) {
		t.Fatal("nothing was downloaded yet")
	}
	// Half the file as a range, then the rest: downloaded.
	size := int64(len("result-out.mp4"))
	h.e.RecordDownload(ctx, as[0].TaskID, "out.mp4", 206, size/2)
	h.e.SweepCleanup(ctx)
	if !h.exists(out) {
		t.Fatal("a partial download must not trigger cleanup")
	}
	h.e.RecordDownload(ctx, as[0].TaskID, "out.mp4", 206, size-size/2)
	h.e.SweepCleanup(ctx)
	if !h.exists(out) {
		t.Fatal("cleanup waits for the quiet period")
	}
	h.advance(cleanupGrace)
	h.e.SweepCleanup(ctx)
	if h.exists(out) || h.exists(UploadKey("in.mov")) {
		t.Fatal("inputs and outputs must be deleted")
	}
	jj, _ := h.e.Job(ctx, j.ID, false)
	if jj.CleanedAt == nil || jj.Cleanup == nil || !jj.Cleanup.AfterDownload {
		t.Fatalf("job after cleanup: %+v", jj)
	}

	// after_hours on a failed job, deleting the job itself.
	h.hb(a, goodTel(), "")
	h.put(ownerA, "in2.mov", "in")
	spec = encodeSpec("in2.mov")
	spec.Tasks[0].MaxRetries = ip(0)
	spec.Cleanup = &api.Cleanup{AfterDownload: true, AfterHours: 2, DeleteJob: true}
	f := h.submitAs(ownerA, spec)
	h.tick()
	as = h.poll(a)
	if err := h.e.TaskUpdate(ctx, a, as[0].TaskID, api.TaskUpdate{Attempt: 1, Outcome: api.OutcomeFailed, ExitCode: ip(1)}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Hour)
	h.e.SweepCleanup(ctx)
	if _, err := h.e.Job(ctx, f.ID, false); err != nil {
		t.Fatal("failed jobs are not cleaned by after_download")
	}
	h.advance(time.Hour)
	h.e.SweepCleanup(ctx)
	if _, err := h.e.Job(ctx, f.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("after_hours + delete_job must delete the job")
	}
	if h.exists(UploadKey("in2.mov")) {
		t.Fatal("its input must be deleted")
	}
	evs, _ := h.e.Events(ctx, []string{api.EventJobCleaned}, "", time.Time{}, 10)
	if len(evs) != 2 {
		t.Fatalf("cleanup audit: %+v", evs)
	}
}

func (h *harness) hbFull(id string, labels map[string]string, maxTasks, limit int, running ...string) api.HeartbeatResponse {
	h.t.Helper()
	r, err := h.e.Heartbeat(h.ctx, id, api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), Runtimes: []string{"sh"},
		Labels: labels, MaxTasks: maxTasks, MaxTasksLimit: limit, Running: running})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// TestReservationScheduling: a reserved Mac only runs its owners' tasks;
// they can pin to it by label or by name; others wait with a reason.
func TestReservationScheduling(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	studio := h.enrol("studio", 1)
	lab := h.enrol("lab", 1)
	h.hbFull(studio, map[string]string{"room": "1"}, 1, 0)
	h.hbFull(lab, nil, 1, 0)
	ka, _ := h.e.CreateAPIKey(ctx, "a", api.RoleApp)
	a := Caller{Subject: "key:" + ka.ID, Actor: "a"}

	if err := h.e.SetMacReservation(ctx, studio, api.MacReservationRequest{Owners: []string{"key_nope"}}, "admin"); err == nil {
		t.Fatal("unknown key must be refused")
	}
	if err := h.e.SetMacReservation(ctx, studio, api.MacReservationRequest{Owners: []string{ka.ID}, Hours: 24,
		Note: "bulk encode"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.SetMacLabels(ctx, studio, map[string]string{"class": "studio"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.SetMacLabels(ctx, studio, map[string]string{"bad key": "x"}, "admin"); err == nil {
		t.Fatal("bad label key must be refused")
	}
	macs, _ := h.e.Macs(ctx)
	for _, m := range macs {
		if m.ID == studio && (m.Labels["class"] != "studio" || m.Labels["room"] != "1" || m.AgentLabels["class"] != "" ||
			m.Reservation == nil || m.Reservation.Owners[0] != "key:"+ka.ID || m.Reservation.OwnerNames["key:"+ka.ID] != "a" ||
			m.Reservation.Until == nil) {
			t.Fatalf("studio view: %+v %+v", m, m.Reservation)
		}
	}

	// Another owner cannot pin to it.
	var fe *ForbiddenError
	if _, err := h.e.SubmitJobAs(ctx, ownerB, api.JobSpec{Macs: []string{"studio"}, Tasks: []api.TaskSpec{simpleTask("t")}}); !errors.As(err, &fe) {
		t.Fatalf("pin to someone else's Mac: %v", err)
	}
	if _, err := h.e.SubmitJobAs(ctx, ownerB, api.JobSpec{Macs: []string{"nowhere"}, Tasks: []api.TaskSpec{simpleTask("t")}}); err == nil {
		t.Fatal("unknown Mac must be refused")
	}

	// The owner pins by router label and by name.
	byLabel := api.TaskSpec{Key: "l", Runtime: "sh", Resources: api.Resources{Labels: map[string]string{"class": "studio"}}}
	ja := h.submitAs(a, api.JobSpec{Tasks: []api.TaskSpec{byLabel}})
	h.tick()
	if got := h.task(ja.Tasks[0].ID); got.AgentID != studio {
		t.Fatalf("label pin went to %s", got.AgentID)
	}
	h.poll(studio)

	// B's task fits only the reserved studio (by label): it waits, and says why.
	jb := h.submitAs(ownerB, api.JobSpec{Tasks: []api.TaskSpec{{Key: "l", Runtime: "sh",
		Resources: api.Resources{Labels: map[string]string{"room": "1"}}}}})
	h.hbFull(studio, map[string]string{"room": "1"}, 2, 0, ja.Tasks[0].ID)
	h.tick()
	tb := h.task(jb.Tasks[0].ID)
	if tb.State != api.StatePending || !strings.Contains(tb.Waiting, "reserved for other owners") || !strings.Contains(tb.Waiting, "studio") {
		t.Fatalf("B waiting: %s %q", tb.State, tb.Waiting)
	}
	// A plain B task goes to the free lab Mac.
	jb2 := h.submitAs(ownerB, api.JobSpec{Tasks: []api.TaskSpec{simpleTask("p")}})
	h.tick()
	if got := h.task(jb2.Tasks[0].ID); got.AgentID != lab {
		t.Fatalf("B's plain task on %s", got.AgentID)
	}

	// A's task pinned by name lands on studio even though lab exists.
	h.poll(lab)
	jp := h.submitAs(a, api.JobSpec{Macs: []string{"studio"}, Tasks: []api.TaskSpec{simpleTask("n")}})
	h.tick()
	if got := h.task(jp.Tasks[0].ID); got.AgentID != studio {
		t.Fatalf("name pin went to %q (%s)", got.AgentID, got.Waiting)
	}

	// When the reservation ends, B's waiting task can use studio.
	h.advance(25 * time.Hour)
	h.hbFull(studio, map[string]string{"room": "1"}, 3, 0, ja.Tasks[0].ID)
	h.hbFull(lab, nil, 1, 0, jb2.Tasks[0].ID)
	h.tick()
	if got := h.task(jb.Tasks[0].ID); got.AgentID != studio || got.Waiting != "" {
		t.Fatalf("after expiry: %q %q", got.AgentID, got.Waiting)
	}
	evs, _ := h.e.Events(ctx, []string{api.EventMacReserved, api.EventMacLabels}, "", time.Time{}, 10)
	if len(evs) != 2 {
		t.Fatalf("audit: %+v", evs)
	}
}

// TestPreferAndRouterMaxTasks: a soft preference wins ties without draining
// other Macs; router-set concurrency is capped by the agent's hard limit.
func TestPreferAndRouterMaxTasks(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	big := h.enrol("big", 1)
	small := h.enrol("small", 1)
	if err := h.e.SetMacLabels(ctx, small, map[string]string{"class": "studio"}, "admin"); err != nil {
		t.Fatal(err)
	}
	h.hbFull(big, nil, 1, 0)
	h.hbFull(small, nil, 1, 0)
	prefer := &api.Prefer{Labels: map[string]string{"class": "studio"}}
	j := h.submitAs(ownerA, api.JobSpec{Prefer: prefer, Tasks: []api.TaskSpec{simpleTask("a"), simpleTask("b")}})
	h.tick()
	t0, t1 := h.task(j.Tasks[0].ID), h.task(j.Tasks[1].ID)
	if t0.AgentID != small {
		t.Fatalf("first task should prefer the studio Mac, went to %s", t0.AgentID)
	}
	if t1.AgentID != big {
		t.Fatalf("second task must still use the other Mac, went to %q", t1.AgentID)
	}
	if _, err := h.e.SubmitJobAs(ctx, ownerA, api.JobSpec{Prefer: &api.Prefer{}, Tasks: []api.TaskSpec{simpleTask("a")}}); err == nil {
		t.Fatal("empty prefer must be refused")
	}

	// Router max_tasks 3, agent hard limit 2: two tasks run at once.
	if err := h.e.PatchMacBy(ctx, big, api.MacPatch{MaxTasks: ip(3)}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.e.PatchMacBy(ctx, big, api.MacPatch{MaxTasks: ip(65)}, "admin"); err == nil {
		t.Fatal("max_tasks above 64 must be refused")
	}
	if r := h.hbFull(big, nil, 2, 2); r.MaxTasks != 3 {
		t.Fatalf("heartbeat response max_tasks %d", r.MaxTasks)
	}
	h.poll(big)
	h.poll(small)
	j2 := h.submitAs(ownerB, api.JobSpec{Macs: []string{big}, Tasks: []api.TaskSpec{simpleTask("x"), simpleTask("y")}})
	h.hbFull(big, nil, 2, 2, t1.ID)
	h.tick()
	placed := 0
	for _, tk := range j2.Tasks {
		if h.task(tk.ID).AgentID == big {
			placed++
		}
	}
	if placed != 1 {
		t.Fatalf("with one running and a cap of 2, one more fits; placed %d", placed)
	}
	macs, _ := h.e.Macs(ctx)
	for _, m := range macs {
		if m.ID == big && (m.MaxTasks != 2 || m.MaxTasksSet != 3 || m.AgentMaxTasks != 2) {
			t.Fatalf("big view: max %d set %d limit %d", m.MaxTasks, m.MaxTasksSet, m.AgentMaxTasks)
		}
	}
	if w := h.task(j2.Tasks[1].ID).Waiting; !strings.Contains(w, "waiting for its macs") {
		t.Fatalf("pinned waiting: %q", w)
	}
}
