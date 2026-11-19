package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Idempotent submits: a job's ClientID is unique per owner, and so is each
// task's. Submitting again returns what exists instead of duplicating it,
// so a feeder that crashed or timed out can simply re-send its batch.

var clientIDRE = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)

func validateClientIDs(spec *api.JobSpec) error {
	if spec.ClientID != "" && !clientIDRE.MatchString(spec.ClientID) {
		return vErr("client_id must be 1-200 printable characters without spaces")
	}
	seen := map[string]bool{}
	for _, t := range spec.Tasks {
		if t.ClientID == "" {
			continue
		}
		if !clientIDRE.MatchString(t.ClientID) {
			return vErr("task %s: client_id must be 1-200 printable characters without spaces", t.Key)
		}
		if seen[t.ClientID] {
			return vErr("task %s: duplicate client_id %q", t.Key, t.ClientID)
		}
		seen[t.ClientID] = true
	}
	return nil
}

func clientKey(owner, clientID string) string { return owner + "\n" + clientID }

func specHash(spec *api.JobSpec) string {
	s := sha256.Sum256([]byte(jsonStr(spec)))
	return hex.EncodeToString(s[:])
}

// dedupe applies client IDs before a submit. It returns the existing job
// when the job's ClientID was used before (an error if with another spec),
// and otherwise drops tasks whose ClientID exists, listing them. Tasks
// left must not depend on dropped ones.
func (e *Engine) dedupe(ctx context.Context, owner string, spec *api.JobSpec, hash string) (*api.Job, []api.ExistingTask, error) {
	if spec.ClientID != "" {
		j, err := e.st.JobByClientID(ctx, owner, spec.ClientID)
		switch {
		case err == nil && j.ClientHash != hash:
			return nil, nil, activeErr("client_id " + spec.ClientID + " was already used for a different job (" + j.ID + ")")
		case err == nil:
			v, err := e.Job(ctx, j.ID, false)
			if err != nil {
				return nil, nil, err
			}
			v.Existing = true
			return v, nil, nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, nil, err
		}
	}
	var keys []string
	for _, t := range spec.Tasks {
		if t.ClientID != "" {
			keys = append(keys, clientKey(owner, t.ClientID))
		}
	}
	if len(keys) == 0 {
		return nil, nil, nil
	}
	found, err := e.st.TasksByClientKeys(ctx, keys)
	if err != nil || len(found) == 0 {
		return nil, nil, err
	}
	byKey := map[string]*store.Task{}
	for _, t := range found {
		byKey[t.ClientKey] = t
	}
	var keep []api.TaskSpec
	var existing []api.ExistingTask
	dropped := map[string]bool{}
	for _, t := range spec.Tasks {
		if t.ClientID != "" {
			if x := byKey[clientKey(owner, t.ClientID)]; x != nil {
				existing = append(existing, api.ExistingTask{ClientID: t.ClientID, TaskID: x.ID, JobID: x.JobID, State: x.State})
				dropped[t.Key] = true
				continue
			}
		}
		keep = append(keep, t)
	}
	for _, t := range keep {
		for _, d := range t.DependsOn {
			if dropped[d] {
				return nil, nil, vErr("task %s depends on %s, which already exists in job %s (client_id): tasks cannot depend on another job's tasks",
					t.Key, d, byKey[clientKey(owner, taskClientID(spec, d))].JobID)
			}
		}
	}
	if len(keep) == 0 {
		v, err := e.Job(ctx, existing[0].JobID, false)
		if err != nil {
			return nil, nil, err
		}
		v.Existing, v.ExistingTasks = true, existing
		return v, existing, nil
	}
	spec.Tasks = keep
	return nil, existing, nil
}

func taskClientID(spec *api.JobSpec, key string) string {
	for _, t := range spec.Tasks {
		if t.Key == key {
			return t.ClientID
		}
	}
	return ""
}
