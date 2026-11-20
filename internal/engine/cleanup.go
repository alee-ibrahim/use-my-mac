package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Admin cleanup: delete revoked API keys, dead install links, removed Macs
// and finished jobs. Every deletion is written to the audit log with who
// did it. Records that are still in use are refused with ErrActive.

// ErrActive: the record is still in use (unrevoked key, live link,
// unfinished job).
var ErrActive = store.ErrActive

func activeErr(msg string) error { return fmt.Errorf("%w: %s", ErrActive, msg) }

// DeleteAPIKey deletes a revoked key.
func (e *Engine) DeleteAPIKey(ctx context.Context, id, actor string) error {
	k, err := e.st.DeleteAPIKey(ctx, id)
	if errors.Is(err, store.ErrActive) {
		return activeErr("revoke the key before deleting it")
	}
	if err != nil {
		return err
	}
	e.Audit(ctx, api.EventKeyDeleted, actor, id, "deleted revoked API key "+k.Name+" ("+k.Role+")")
	return nil
}

// PurgeAPIKeys deletes every revoked key.
func (e *Engine) PurgeAPIKeys(ctx context.Context, actor string) (int, error) {
	ks, err := e.st.DeleteRevokedAPIKeys(ctx)
	if err != nil {
		return 0, err
	}
	if len(ks) > 0 {
		e.Audit(ctx, api.EventKeyDeleted, actor, "", "deleted "+strconv.Itoa(len(ks))+" revoked API keys")
	}
	return len(ks), nil
}

// DeleteInstallToken deletes a revoked, expired or used-up install link.
func (e *Engine) DeleteInstallToken(ctx context.Context, id, actor string) error {
	t, err := e.st.DeleteInstallToken(ctx, id, ms(e.now()))
	if errors.Is(err, store.ErrActive) {
		return activeErr("the link still works; revoke it first")
	}
	if err != nil {
		return err
	}
	e.Audit(ctx, api.EventInstallDeleted, actor, id, "deleted install link "+t.Prefix+"… ("+t.Profile+")")
	return nil
}

// PurgeInstallTokens deletes every link that can no longer enrol a Mac.
func (e *Engine) PurgeInstallTokens(ctx context.Context, actor string) (int, error) {
	ts, err := e.st.DeleteDeadInstallTokens(ctx, ms(e.now()))
	if err != nil {
		return 0, err
	}
	if len(ts) > 0 {
		e.Audit(ctx, api.EventInstallDeleted, actor, "", "deleted "+strconv.Itoa(len(ts))+" expired, used or revoked install links")
	}
	return len(ts), nil
}

// DeleteMacBy deletes a Mac (revoking its credential) and its metrics
// history, and records who did it.
func (e *Engine) DeleteMacBy(ctx context.Context, id, actor string) error {
	a, err := e.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if err := e.DeleteMac(ctx, id); err != nil {
		return err
	}
	_ = e.st.DeleteMacSamples(ctx, id)
	e.Audit(ctx, api.EventMacDeleted, actor, id, "deleted Mac "+a.Name+"; its credential no longer works")
	return nil
}

// DeleteJob deletes a finished job with its tasks, attempts, logs and the
// outputs kept in router storage.
func (e *Engine) DeleteJob(ctx context.Context, id, actor string) error {
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if !api.IsTerminal(j.State) {
		return activeErr("the job has not finished; cancel it first")
	}
	if err := e.deleteJob(ctx, j.ID); err != nil {
		return err
	}
	e.Audit(ctx, api.EventJobDeleted, actor, id, "deleted "+j.State+" job "+j.Name)
	return nil
}

func (e *Engine) deleteJob(ctx context.Context, id string) error {
	tasks, err := e.st.TasksByJob(ctx, id)
	if err != nil {
		return err
	}
	if err := e.st.DeleteJob(ctx, id); err != nil {
		return err
	}
	// Outputs in router storage (best effort; app-provided URLs are not ours),
	// unfinished output uploads, and the ownership rows.
	keys := jobOutputKeys(id, tasks)
	if e.blobs != nil {
		for _, k := range keys {
			if err := e.blobs.Delete(ctx, k); err != nil {
				e.log.Warn("delete job output", "job", id, "key", k, "err", err)
			}
		}
		if us, err := e.st.UploadsUnder(ctx, "jobs/"+id+"/"); err == nil {
			for _, u := range us {
				_ = e.abort(ctx, u)
			}
		}
	}
	_ = e.st.DeleteJobBlobRows(ctx, id)
	e.dlMu.Lock()
	if len(keys) > 0 {
		// An agent stopped mid-upload may still finish a PUT: delete again later.
		e.tombstones[id] = tombstone{at: e.now(), keys: keys}
	}
	delete(e.lastDownload, id)
	e.dlMu.Unlock()
	e.mu.Lock()
	for _, t := range tasks {
		delete(e.placement, t.ID)
		e.setWaiting(t.ID, "")
		delete(e.diskNeed, t.ID)
	}
	delete(e.jobOwner, id)
	e.mu.Unlock()
	e.notify(id)
	return nil
}

// PurgeJobs deletes finished jobs older than req.OlderThanDays.
func (e *Engine) PurgeJobs(ctx context.Context, req api.PurgeJobsRequest, actor string) (int, error) {
	if req.OlderThanDays < 0 || req.OlderThanDays > 3650 {
		return 0, &ValidationError{"older_than_days must be between 0 and 3650"}
	}
	for _, s := range req.States {
		if !api.IsTerminal(s) {
			return 0, &ValidationError{"states may only be succeeded, failed or cancelled"}
		}
	}
	before := ms(e.now().Add(-time.Duration(req.OlderThanDays) * 24 * time.Hour))
	if req.OlderThanDays == 0 {
		before = ms(e.now()) + 1
	}
	n := 0
	for {
		ids, err := e.st.FinishedJobsOf(ctx, req.Owner, before, req.States, 200)
		if err != nil {
			return n, err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if req.Inputs {
				j, err := e.st.GetJob(ctx, id)
				if err == nil {
					_, err = e.deleteJobFiles(ctx, j, true)
				}
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return n, err
				}
			} else if err := e.deleteJob(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				return n, err
			}
			n++
		}
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
	}
	if n > 0 {
		what := "finished"
		if len(req.States) > 0 {
			what = fmt.Sprint(req.States)
		}
		msg := "deleted " + strconv.Itoa(n) + " " + what + " jobs older than " + strconv.Itoa(req.OlderThanDays) + " days"
		if req.Owner != "" {
			msg += " of " + req.Owner
		}
		if req.Inputs {
			msg += ", with their input files"
		}
		e.Audit(ctx, api.EventJobsPurged, actor, "", msg)
	}
	return n, nil
}
