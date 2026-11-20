package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Ownership of stored files and jobs: owners (the API key or user that
// uploaded a blob or submitted a job) list and delete their own; admins can
// act on anyone's. Every deletion goes to the audit log.

// cleanupGrace is the quiet period after the last download before an
// after_download cleanup runs (parallel range downloads finish).
const cleanupGrace = 2 * time.Minute

// tombstoneDelay: outputs of a deleted job are deleted again after this
// long, in case an agent was still uploading when the job went.
const tombstoneDelay = 10 * time.Minute

// cleanupSweepEvery is how often cleanup policies and tombstones are checked.
const cleanupSweepEvery = time.Minute

func (e *Engine) sizeOf(ctx context.Context, key string) int64 {
	if sz, ok := e.blobs.(storage.Sizer); ok {
		if n, err := sz.Size(ctx, key); err == nil {
			return n
		}
	}
	return 0
}

// blobAccess: a caller may read, overwrite or use a blob it owns, an admin
// any blob, and anyone a blob stored before ownership was tracked.
func (e *Engine) blobAccess(ctx context.Context, c Caller, key string) (*store.Blob, bool, error) {
	b, err := e.st.GetBlob(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, c.Admin || b.Owner == c.Subject, nil
}

// PutBlob stores an app upload (PUT /v1/blobs/KEY) and records its owner.
// A key another owner uploaded cannot be overwritten (admins can; the
// owner stays).
func (e *Engine) PutBlob(ctx context.Context, c Caller, key string, r io.Reader, size int64) error {
	if err := storage.ValidKey(key); err != nil {
		return err
	}
	full := UploadKey(key)
	b, ok, err := e.blobAccess(ctx, c, full)
	if err != nil {
		return err
	}
	if !ok {
		return forbidden("blob %s belongs to another owner", key)
	}
	if err := e.blobs.Put(ctx, full, r, size); err != nil {
		return err
	}
	owner := c.Subject
	if b != nil {
		owner = b.Owner
	}
	return e.recordUpload(ctx, full, owner, size)
}

func (e *Engine) recordUpload(ctx context.Context, full, owner string, size int64) error {
	if n := e.sizeOf(ctx, full); n > 0 || size < 0 {
		size = n
	}
	return e.st.PutBlob(ctx, &store.Blob{Key: full, Owner: owner, Kind: store.BlobUpload, Size: size, CreatedAt: ms(e.now())})
}

// OpenBlob opens an app upload the caller may read.
func (e *Engine) OpenBlob(ctx context.Context, c Caller, key string) (io.ReadCloser, error) {
	if err := storage.ValidKey(key); err != nil {
		return nil, err
	}
	if _, ok, err := e.blobAccess(ctx, c, UploadKey(key)); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotFound
	}
	return e.blobs.Get(ctx, UploadKey(key))
}

// checkInputOwnership refuses inputs that are another owner's blobs.
func (e *Engine) checkInputOwnership(ctx context.Context, c Caller, spec *api.JobSpec) error {
	if c.Admin {
		return nil
	}
	seen := map[string]bool{}
	for _, t := range spec.Tasks {
		for _, in := range t.Inputs {
			if in.Blob == "" || seen[in.Blob] {
				continue
			}
			seen[in.Blob] = true
			if _, ok, err := e.blobAccess(ctx, c, UploadKey(in.Blob)); err != nil {
				return err
			} else if !ok {
				return forbidden("task %s: input %s: blob %s belongs to another owner", t.Key, in.Name, in.Blob)
			}
		}
	}
	return nil
}

// recordOutputsLocked notes the stored outputs of a task that succeeded.
func (e *Engine) recordOutputsLocked(ctx context.Context, t *store.Task, spec api.TaskSpec, done []string) {
	digests := map[string]string{}
	_ = json.Unmarshal([]byte(t.OutputSHA256), &digests)
	j, err := e.st.GetJob(ctx, t.JobID)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, o := range done {
		have[o] = true
	}
	now := ms(e.now())
	for _, o := range spec.Outputs {
		if o.URL != "" || !have[o.Name] {
			continue
		}
		key := OutputKey(t.JobID, t.Key, o.Name)
		if err := e.st.PutBlob(ctx, &store.Blob{Key: key, Owner: j.Owner, Kind: store.BlobOutput, JobID: t.JobID,
			TaskID: t.ID, Size: e.sizeOf(ctx, key), CreatedAt: now, SHA256: digests[o.Name]}); err != nil {
			e.log.Warn("record output", "key", key, "err", err)
		}
	}
}

// inUse returns the upload keys unfinished tasks read and the unfinished
// jobs (whose outputs may still be needed).
func (e *Engine) inUse(ctx context.Context) (uploads, jobs map[string]bool, err error) {
	tasks, err := e.st.ActiveTasks(ctx)
	if err != nil {
		return nil, nil, err
	}
	uploads, jobs = map[string]bool{}, map[string]bool{}
	for _, t := range tasks {
		jobs[t.JobID] = true
		var spec api.TaskSpec
		if json.Unmarshal([]byte(t.Spec), &spec) != nil {
			continue
		}
		for _, in := range spec.Inputs {
			if in.Blob != "" {
				uploads[UploadKey(in.Blob)] = true
			}
		}
	}
	// A job whose tasks just finished may not be marked finished yet; its
	// state is what counts.
	js, err := e.st.ListJobs(ctx, JobRunning, "", 1000)
	if err == nil {
		for _, j := range js {
			jobs[j.ID] = true
		}
	}
	return uploads, jobs, nil
}

func fileInUse(b *store.Blob, uploads, jobs map[string]bool) bool {
	if b.Kind == store.BlobOutput {
		return jobs[b.JobID]
	}
	return uploads[b.Key]
}

func fileView(b *store.Blob, uploads, jobs map[string]bool) api.StoredFile {
	f := api.StoredFile{Key: b.Key, Kind: b.Kind, Owner: b.Owner, JobID: b.JobID, TaskID: b.TaskID, Size: b.Size,
		CreatedAt: time.UnixMilli(b.CreatedAt).UTC(), DownloadedAt: tptr(b.DownloadedAt), InUse: fileInUse(b, uploads, jobs),
		SHA256: b.SHA256}
	if k, ok := cutUploadKey(b.Key); ok && b.Kind == store.BlobUpload {
		f.Blob = k
	}
	return f
}

// FileFilter selects stored files.
type FileFilter struct {
	// Owner: "" = the caller's own; admins may name another owner or "*".
	Owner, Kind, JobID, Prefix string
	Limit                      int
}

func (e *Engine) fileOwner(c Caller, owner string) (string, error) {
	switch {
	case owner == "" || owner == c.Subject:
		return c.Subject, nil
	case !c.Admin:
		return "", forbidden("only admins can see other owners' files")
	case owner == "*":
		return "", nil
	}
	return owner, nil
}

// Files lists stored files.
func (e *Engine) Files(ctx context.Context, c Caller, f FileFilter) ([]api.StoredFile, error) {
	owner, err := e.fileOwner(c, f.Owner)
	if err != nil {
		return nil, err
	}
	if f.Kind != "" && f.Kind != api.FileUpload && f.Kind != api.FileOutput {
		return nil, vErr("kind must be upload or output")
	}
	bs, err := e.st.ListBlobs(ctx, store.BlobFilter{Owner: owner, Kind: f.Kind, JobID: f.JobID, Prefix: f.Prefix, Limit: f.Limit})
	if err != nil {
		return nil, err
	}
	uploads, jobs, err := e.inUse(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.StoredFile{}
	for _, b := range bs {
		out = append(out, fileView(b, uploads, jobs))
	}
	return out, nil
}

// fileKey accepts a storage key (uploads/..., jobs/...) or a bare blob key.
func fileKey(k string) string {
	if strings.HasPrefix(k, "uploads/") || strings.HasPrefix(k, "jobs/") {
		return k
	}
	return UploadKey(k)
}

// DeleteFile deletes one stored file the caller owns (admins: any, also
// untracked ones). Files an unfinished job still needs are refused.
func (e *Engine) DeleteFile(ctx context.Context, c Caller, key string) error {
	res, err := e.deleteFiles(ctx, c, []string{fileKey(key)}, true)
	if err != nil {
		return err
	}
	if len(res.Skipped) > 0 {
		if res.Skipped[0].Reason == "not found" {
			return ErrNotFound
		}
		return activeErr(res.Skipped[0].Reason)
	}
	return nil
}

// DeleteFiles deletes several files: req.Keys, or every file matching the
// filter when req.All is set. Files in use are skipped.
func (e *Engine) DeleteFiles(ctx context.Context, c Caller, req api.DeleteFilesRequest) (*api.DeleteFilesResult, error) {
	var keys []string
	for _, k := range req.Keys {
		keys = append(keys, fileKey(k))
	}
	if len(keys) > 10000 {
		return nil, vErr("at most 10000 keys at once")
	}
	if len(keys) == 0 {
		if !req.All {
			return nil, vErr("give keys, or a filter with all: true")
		}
		bs, err := e.Files(ctx, c, FileFilter{Owner: req.Owner, Kind: req.Kind, JobID: req.JobID, Prefix: req.Prefix, Limit: 10000})
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			keys = append(keys, b.Key)
		}
	}
	return e.deleteFiles(ctx, c, keys, true)
}

func (e *Engine) deleteFiles(ctx context.Context, c Caller, keys []string, audit bool) (*api.DeleteFilesResult, error) {
	res := &api.DeleteFilesResult{}
	if len(keys) == 0 {
		return res, nil
	}
	uploads, jobs, err := e.inUse(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if storage.ValidKey(k) != nil {
			res.Skipped = append(res.Skipped, api.SkippedFile{Key: k, Reason: "invalid key"})
			continue
		}
		b, err := e.st.GetBlob(ctx, k)
		switch {
		case errors.Is(err, store.ErrNotFound) && c.Admin:
			// Untracked (stored before ownership): admins only.
			b = &store.Blob{Key: k, Kind: store.BlobUpload}
			if strings.HasPrefix(k, "jobs/") {
				b.Kind = store.BlobOutput
				if parts := strings.SplitN(k, "/", 3); len(parts) == 3 {
					b.JobID = parts[1]
				}
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return res, err
		case err != nil || (!c.Admin && b.Owner != c.Subject):
			res.Skipped = append(res.Skipped, api.SkippedFile{Key: k, Reason: "not found"})
			continue
		}
		if fileInUse(b, uploads, jobs) {
			res.Skipped = append(res.Skipped, api.SkippedFile{Key: k, Reason: "an unfinished job still needs it"})
			continue
		}
		if err := e.blobs.Delete(ctx, k); err != nil {
			return res, err
		}
		_ = e.st.DeleteBlobRow(ctx, k)
		res.Deleted++
		res.Bytes += b.Size
	}
	if audit && res.Deleted > 0 {
		msg := "deleted " + strconv.Itoa(res.Deleted) + " stored files (" + strconv.FormatInt(res.Bytes, 10) + " bytes)"
		if res.Deleted == 1 && len(keys) == 1 {
			msg = "deleted stored file " + keys[0]
		}
		e.Audit(ctx, api.EventFilesDeleted, c.actor(), "", msg)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// RemoveJob deletes a job for its owner or an admin: an unfinished job is
// cancelled first (running tasks are stopped), then the job, its tasks,
// logs and stored outputs go. With inputs, the input blobs the job's owner
// uploaded go too, unless another unfinished job uses them.
func (e *Engine) RemoveJob(ctx context.Context, c Caller, id string, inputs bool) error {
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if !c.Admin && j.Owner != c.Subject {
		return ErrNotFound
	}
	what := j.State
	if !api.IsTerminal(j.State) {
		if _, err := e.CancelJob(ctx, id); err != nil {
			return err
		}
		what = "cancelled " + j.State
	}
	n, err := e.deleteJobFiles(ctx, j, inputs)
	if err != nil {
		return err
	}
	msg := "deleted " + what + " job " + j.Name
	if n > 0 {
		msg += " and " + strconv.Itoa(n) + " of its input files"
	}
	e.Audit(ctx, api.EventJobDeleted, c.actor(), id, msg)
	return nil
}

// deleteJobFiles deletes a job with its outputs and, with inputs, the
// input blobs its owner uploaded that no unfinished job uses. It returns
// the number of inputs deleted.
func (e *Engine) deleteJobFiles(ctx context.Context, j *store.Job, inputs bool) (int, error) {
	var keys []string
	if inputs {
		var err error
		if keys, err = e.jobInputKeys(ctx, j); err != nil {
			return 0, err
		}
	}
	if err := e.deleteJob(ctx, j.ID); err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}
	res, err := e.deleteFiles(ctx, Caller{Subject: j.Owner}, keys, false)
	if err != nil {
		return 0, err
	}
	return res.Deleted, nil
}

// jobInputKeys lists the upload keys a job's tasks read that its owner
// uploaded (tracked blobs only).
func (e *Engine) jobInputKeys(ctx context.Context, j *store.Job) ([]string, error) {
	tasks, err := e.st.TasksByJob(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var keys []string
	for _, t := range tasks {
		var spec api.TaskSpec
		if json.Unmarshal([]byte(t.Spec), &spec) != nil {
			continue
		}
		for _, in := range spec.Inputs {
			k := UploadKey(in.Blob)
			if in.Blob == "" || seen[k] {
				continue
			}
			seen[k] = true
			if b, err := e.st.GetBlob(ctx, k); err == nil && b.Owner == j.Owner {
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

// jobOutputKeys lists a job's outputs in router storage.
func jobOutputKeys(jobID string, tasks []*store.Task) []string {
	var keys []string
	for _, t := range tasks {
		var spec api.TaskSpec
		if json.Unmarshal([]byte(t.Spec), &spec) != nil {
			continue
		}
		for _, o := range spec.Outputs {
			if o.URL == "" {
				keys = append(keys, OutputKey(jobID, t.Key, o.Name))
			}
		}
	}
	return keys
}

// OutputKeyLive reports whether a jobs/<job>/... key belongs to a job that
// still exists (signed uploads to a deleted job's outputs are refused).
func (e *Engine) OutputKeyLive(ctx context.Context, key string) bool {
	rest, ok := strings.CutPrefix(key, "jobs/")
	if !ok {
		return true
	}
	id, _, _ := strings.Cut(rest, "/")
	_, err := e.st.GetJob(ctx, id)
	return !errors.Is(err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Downloads and cleanup policies
// ---------------------------------------------------------------------------

// RecordDownload notes bytes of a task output served to a client. status
// is the HTTP status (200 = the whole file, 206 = a range). Once a file's
// bytes have all been served it counts as downloaded.
func (e *Engine) RecordDownload(ctx context.Context, taskID, name string, status int, n int64) {
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	key := OutputKey(t.JobID, t.Key, name)
	b, err := e.st.GetBlob(ctx, key)
	if err != nil {
		return
	}
	e.dlMu.Lock()
	e.lastDownload[t.JobID] = e.now()
	full := false
	switch {
	case status == 200:
		full = b.Size == 0 || n >= b.Size
	case status == 206 && b.Size > 0:
		e.dlBytes[key] += n
		full = e.dlBytes[key] >= b.Size
	}
	if full {
		delete(e.dlBytes, key)
	}
	e.dlMu.Unlock()
	if full && b.DownloadedAt == 0 {
		_ = e.st.MarkBlobDownloaded(ctx, key, ms(e.now()))
	}
}

// validateCleanup checks a job's cleanup policy.
func validateCleanup(c *api.Cleanup) error {
	if c == nil {
		return nil
	}
	switch {
	case !c.AfterDownload && c.AfterHours == 0:
		return vErr("cleanup needs after_download and/or after_hours")
	case c.AfterHours < 0 || c.AfterHours > 24*3650:
		return vErr("cleanup.after_hours must be between 0 and 87600")
	case c.DeleteJob && c.KeepOutputs:
		return vErr("cleanup: delete_job also deletes the outputs; drop keep_outputs")
	case c.KeepInputs && c.KeepOutputs && !c.DeleteJob:
		return vErr("cleanup keeps everything: drop keep_inputs or keep_outputs")
	}
	return nil
}

// cleanupDue reports whether a finished job's cleanup policy should run.
func (e *Engine) cleanupDue(ctx context.Context, j *store.Job, p *api.Cleanup, now time.Time) bool {
	fin := time.UnixMilli(j.FinishedAt)
	if p.AfterHours > 0 && !now.Before(fin.Add(time.Duration(p.AfterHours)*time.Hour)) {
		return true
	}
	if !p.AfterDownload || j.State != api.StateSucceeded || now.Sub(fin) < cleanupGrace {
		return false
	}
	tasks, err := e.st.TasksByJob(ctx, j.ID)
	if err != nil {
		return false
	}
	consumed := map[string]bool{}
	for _, t := range tasks {
		var spec api.TaskSpec
		if json.Unmarshal([]byte(t.Spec), &spec) != nil {
			continue
		}
		for _, in := range spec.Inputs {
			if in.From != nil {
				consumed[OutputKey(j.ID, in.From.Task, in.From.Output)] = true
			}
		}
	}
	bs, err := e.st.ListBlobs(ctx, store.BlobFilter{JobID: j.ID, Kind: store.BlobOutput, Limit: 10000})
	if err != nil {
		return false
	}
	var last int64
	for _, b := range bs {
		if consumed[b.Key] {
			continue
		}
		if b.DownloadedAt == 0 {
			return false
		}
		last = max(last, b.DownloadedAt)
	}
	e.dlMu.Lock()
	if t, ok := e.lastDownload[j.ID]; ok {
		last = max(last, ms(t))
	}
	e.dlMu.Unlock()
	return now.Sub(time.UnixMilli(last)) >= cleanupGrace
}

// SweepCleanup runs due cleanup policies and re-deletes the outputs of
// recently deleted jobs.
func (e *Engine) SweepCleanup(ctx context.Context) {
	now := e.now()
	e.dlMu.Lock()
	var redo []string
	for id, t := range e.tombstones {
		if now.Sub(t.at) >= tombstoneDelay {
			redo = append(redo, t.keys...)
			delete(e.tombstones, id)
		}
	}
	e.dlMu.Unlock()
	for _, k := range redo {
		_ = e.blobs.Delete(ctx, k)
	}
	js, err := e.st.JobsDueCleanup(ctx, 500)
	if err != nil {
		e.log.Warn("cleanup sweep", "err", err)
		return
	}
	for _, j := range js {
		var p api.Cleanup
		if json.Unmarshal([]byte(j.Cleanup), &p) != nil {
			_ = e.st.SetJobCleaned(ctx, j.ID, ms(now))
			continue
		}
		if !e.cleanupDue(ctx, j, &p, now) {
			continue
		}
		if err := e.runCleanup(ctx, j, &p, now); err != nil {
			e.log.Warn("job cleanup", "job", j.ID, "err", err)
		}
	}
}

func (e *Engine) runCleanup(ctx context.Context, j *store.Job, p *api.Cleanup, now time.Time) error {
	const actor = "cleanup policy"
	if p.DeleteJob {
		n, err := e.deleteJobFiles(ctx, j, !p.KeepInputs)
		if err != nil {
			return err
		}
		e.Audit(ctx, api.EventJobCleaned, actor, j.ID, "deleted "+j.State+" job "+j.Name+" with its outputs and "+
			strconv.Itoa(n)+" input files")
		return nil
	}
	var keys []string
	if !p.KeepInputs {
		in, err := e.jobInputKeys(ctx, j)
		if err != nil {
			return err
		}
		keys = append(keys, in...)
	}
	if !p.KeepOutputs {
		bs, err := e.st.ListBlobs(ctx, store.BlobFilter{JobID: j.ID, Kind: store.BlobOutput, Limit: 10000})
		if err != nil {
			return err
		}
		for _, b := range bs {
			keys = append(keys, b.Key)
		}
	}
	res, err := e.deleteFiles(ctx, Caller{Subject: j.Owner}, keys, false)
	if err != nil {
		return err
	}
	if err := e.st.SetJobCleaned(ctx, j.ID, ms(now)); err != nil {
		return err
	}
	e.notify(j.ID)
	e.Audit(ctx, api.EventJobCleaned, actor, j.ID, "job "+j.Name+": deleted "+strconv.Itoa(res.Deleted)+
		" stored files ("+strconv.FormatInt(res.Bytes, 10)+" bytes)")
	return nil
}
