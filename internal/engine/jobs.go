package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/sched"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// MaxTasksPerJob bounds job size.
const MaxTasksPerJob = 10000

func vErr(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ValidateJob checks a spec and normalises it (adds implicit dependencies for
// inputs that come from other tasks). It rejects cycles.
func ValidateJob(spec *api.JobSpec) error {
	if spec.Name == "" {
		spec.Name = "job"
	}
	if len(spec.Name) > 200 {
		return vErr("job name too long")
	}
	if len(spec.Tasks) == 0 || len(spec.Tasks) > MaxTasksPerJob {
		return vErr("a job needs 1..%d tasks", MaxTasksPerJob)
	}
	byKey := map[string]*api.TaskSpec{}
	for i := range spec.Tasks {
		t := &spec.Tasks[i]
		if !nameRe.MatchString(t.Key) {
			return vErr("task %d: key %q must match %s", i, t.Key, nameRe)
		}
		if byKey[t.Key] != nil {
			return vErr("duplicate task key %q", t.Key)
		}
		byKey[t.Key] = t
		if !nameRe.MatchString(t.Runtime) {
			return vErr("task %s: runtime %q is invalid", t.Key, t.Runtime)
		}
		if t.TimeoutSeconds < 0 || (t.MaxRetries != nil && *t.MaxRetries < 0) {
			return vErr("task %s: negative timeout/retries", t.Key)
		}
		if t.Resources.RAMMB < 0 || t.Resources.DiskMB < 0 || t.Resources.ExpectedSeconds < 0 {
			return vErr("task %s: negative resources", t.Key)
		}
		outs := map[string]bool{}
		for _, o := range t.Outputs {
			if !nameRe.MatchString(o.Name) || outs[o.Name] {
				return vErr("task %s: output name %q invalid or duplicate", t.Key, o.Name)
			}
			outs[o.Name] = true
			if o.URL != "" && !validURL(o.URL) {
				return vErr("task %s: output %s url must be http(s)", t.Key, o.Name)
			}
		}
	}
	for i := range spec.Tasks {
		t := &spec.Tasks[i]
		ins := map[string]bool{}
		deps := map[string]bool{}
		for _, d := range t.DependsOn {
			if byKey[d] == nil || d == t.Key {
				return vErr("task %s: bad dependency %q", t.Key, d)
			}
			deps[d] = true
		}
		for _, in := range t.Inputs {
			if !nameRe.MatchString(in.Name) || ins[in.Name] {
				return vErr("task %s: input name %q invalid or duplicate", t.Key, in.Name)
			}
			ins[in.Name] = true
			n := 0
			if in.URL != "" {
				n++
				if !validURL(in.URL) {
					return vErr("task %s: input %s url must be http(s)", t.Key, in.Name)
				}
			}
			if in.Blob != "" {
				n++
				if storage.ValidKey(in.Blob) != nil {
					return vErr("task %s: input %s blob key invalid", t.Key, in.Name)
				}
			}
			if in.From != nil {
				n++
				src := byKey[in.From.Task]
				if src == nil || in.From.Task == t.Key {
					return vErr("task %s: input %s refers to unknown task %q", t.Key, in.Name, in.From.Task)
				}
				found := false
				for _, o := range src.Outputs {
					found = found || o.Name == in.From.Output
				}
				if !found {
					return vErr("task %s: input %s refers to unknown output %s.%s", t.Key, in.Name, in.From.Task, in.From.Output)
				}
				if !deps[in.From.Task] {
					deps[in.From.Task] = true
					t.DependsOn = append(t.DependsOn, in.From.Task)
				}
			}
			if n != 1 {
				return vErr("task %s: input %s needs exactly one of url, blob, from", t.Key, in.Name)
			}
		}
	}
	// Cycle check (DFS).
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var visit func(k string) error
	visit = func(k string) error {
		color[k] = grey
		for _, d := range byKey[k].DependsOn {
			switch color[d] {
			case grey:
				return vErr("dependency cycle through %s -> %s", k, d)
			case white:
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		color[k] = black
		return nil
	}
	for k := range byKey {
		if color[k] == white {
			if err := visit(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// SubmitJob validates and stores a job.
func (e *Engine) SubmitJob(ctx context.Context, owner string, spec api.JobSpec) (*api.Job, error) {
	if err := ValidateJob(&spec); err != nil {
		return nil, err
	}
	now := e.now()
	j := &store.Job{
		ID: "job_" + auth.NewID(), Name: spec.Name, Owner: owner, Labels: jsonStr(nonNilMap(spec.Labels)),
		State: JobQueued, CreatedAt: ms(now),
	}
	var rows []*store.Task
	for i, ts := range spec.Tasks {
		st := api.StatePending
		if len(ts.DependsOn) > 0 {
			st = api.StateBlocked
		}
		rows = append(rows, &store.Task{
			ID: "tsk_" + auth.NewID(), JobID: j.ID, Key: ts.Key, Spec: jsonStr(ts), State: st,
			Excluded: "{}", Outputs: "[]", CreatedAt: ms(now) + int64(i), // keep submission order
		})
	}
	if err := e.st.CreateJob(ctx, j, rows); err != nil {
		return nil, err
	}
	e.log.Info("job submitted", "job", j.ID, "name", j.Name, "tasks", len(rows), "owner", owner)
	e.Kick()
	return e.Job(ctx, j.ID, false)
}

// CancelJob cancels every unfinished task; running ones are stopped by their
// agents on the next heartbeat.
func (e *Engine) CancelJob(ctx context.Context, id string) (*api.Job, error) {
	e.mu.Lock()
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	now := e.now()
	if !api.IsTerminal(j.State) {
		tasks, err := e.st.TasksByJob(ctx, id)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		for _, t := range tasks {
			if api.IsTerminal(t.State) {
				continue
			}
			if t.State == api.StateRunning {
				_ = e.st.FinishAttempt(ctx, t.ID, t.Attempt, api.OutcomeCancelled, "job cancelled", ms(now))
				e.wakeAgent(t.AgentID)
			}
			t.State, t.FinishedAt, t.Error = api.StateCancelled, ms(now), "job cancelled"
			if err := e.st.UpdateTask(ctx, t); err != nil {
				e.mu.Unlock()
				return nil, err
			}
		}
		j.CancelRequested = true
		if err := e.st.UpdateJob(ctx, j); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.updateJobLocked(ctx, id, now)
	}
	e.mu.Unlock()
	return e.Job(ctx, id, false)
}

func (e *Engine) agentNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	if as, err := e.st.ListAgents(ctx); err == nil {
		for _, a := range as {
			out[a.ID] = a.Name
		}
	}
	return out
}

func taskView(t *store.Task) api.Task {
	v := api.Task{
		ID: t.ID, JobID: t.JobID, Key: t.Key, State: t.State, Attempt: t.Attempt, Failures: t.Failures,
		AgentID: t.AgentID, NotBefore: tptr(t.NotBefore), StartedAt: tptr(t.StartedAt), FinishedAt: tptr(t.FinishedAt),
		ExitCode: t.ExitCode, Error: t.Error, CreatedAt: time.UnixMilli(t.CreatedAt).UTC(),
	}
	_ = json.Unmarshal([]byte(t.Spec), &v.Spec)
	_ = json.Unmarshal([]byte(t.Outputs), &v.OutputsDone)
	now := time.Now().UnixMilli()
	for id, until := range excludedMap(t) {
		if until > now {
			v.Excluded = append(v.Excluded, id)
		}
	}
	return v
}

// Job returns a job, optionally with its tasks.
func (e *Engine) Job(ctx context.Context, id string, withTasks bool) (*api.Job, error) {
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &api.Job{ID: j.ID, Name: j.Name, Owner: j.Owner, State: j.State,
		CreatedAt: time.UnixMilli(j.CreatedAt).UTC(), FinishedAt: tptr(j.FinishedAt)}
	_ = json.Unmarshal([]byte(j.Labels), &v.Labels)
	if v.Counts, err = e.st.TaskCounts(ctx, id); err != nil {
		return nil, err
	}
	if withTasks {
		ts, err := e.st.TasksByJob(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			v.Tasks = append(v.Tasks, taskView(t))
		}
	}
	return v, nil
}

// Jobs lists jobs.
func (e *Engine) Jobs(ctx context.Context, state, owner string, limit int) ([]*api.Job, error) {
	js, err := e.st.ListJobs(ctx, state, owner, limit)
	if err != nil {
		return nil, err
	}
	out := []*api.Job{}
	for _, j := range js {
		v, err := e.Job(ctx, j.ID, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// TaskDetail is a task plus its attempts and the last placement decision.
type TaskDetail struct {
	api.Task
	Placement []sched.Result `json:"placement,omitempty"`
}

// Task returns a task with attempt history.
func (e *Engine) Task(ctx context.Context, id string) (*TaskDetail, error) {
	t, err := e.st.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &TaskDetail{Task: taskView(t)}
	att, err := e.st.Attempts(ctx, id)
	if err != nil {
		return nil, err
	}
	names := e.agentNames(ctx)
	for _, a := range att {
		d.Attempts = append(d.Attempts, api.Attempt{Attempt: a.Attempt, AgentID: a.AgentID, AgentName: names[a.AgentID],
			StartedAt: time.UnixMilli(a.StartedAt).UTC(), FinishedAt: tptr(a.Finished), Outcome: a.Outcome,
			Error: a.Error, Score: a.Score})
	}
	e.mu.Lock()
	d.Placement = append([]sched.Result(nil), e.placement[id]...)
	e.mu.Unlock()
	return d, nil
}

// Logs returns a task's logs (attempt 0 = all).
func (e *Engine) Logs(ctx context.Context, id string, attempt int) (string, error) {
	if _, err := e.st.GetTask(ctx, id); err != nil {
		return "", err
	}
	return e.st.Logs(ctx, id, attempt)
}

// OpenOutput streams a task output kept in router storage.
func (e *Engine) OpenOutput(ctx context.Context, taskID, name string) (io.ReadCloser, error) {
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var spec api.TaskSpec
	_ = json.Unmarshal([]byte(t.Spec), &spec)
	for _, o := range spec.Outputs {
		if o.Name == name {
			if o.URL != "" {
				return nil, vErr("output %s was sent to an app-supplied URL", name)
			}
			return e.blobs.Get(ctx, OutputKey(t.JobID, t.Key, name))
		}
	}
	return nil, ErrNotFound
}

// ---------------------------------------------------------------------------
// Macs
// ---------------------------------------------------------------------------

// Macs lists enrolled Macs with live state.
func (e *Engine) Macs(ctx context.Context) ([]api.Mac, error) {
	agents, err := e.st.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	tasks, err := e.st.ActiveTasks(ctx)
	if err != nil {
		return nil, err
	}
	running := map[string][]string{}
	for _, t := range tasks {
		if t.State == api.StateRunning || t.State == api.StateAssigned {
			running[t.AgentID] = append(running[t.AgentID], t.ID)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := []api.Mac{}
	for _, a := range agents {
		m := api.Mac{ID: a.ID, Name: a.Name, Online: e.online(a.ID, now), LastSeen: tptr(a.LastSeen),
			AgentState: a.State, Drain: a.Drain, OptedOut: a.OptedOut, MaxTasks: a.MaxTasks,
			Running: nonNil(running[a.ID]), Version: a.Version, CreatedAt: time.UnixMilli(a.CreatedAt).UTC()}
		if a.QuarantinedUntil > ms(now) {
			m.Quarantined = tptr(a.QuarantinedUntil)
		}
		if !m.Online {
			m.AgentState = "offline"
		}
		_ = json.Unmarshal([]byte(a.Runtimes), &m.Runtimes)
		_ = json.Unmarshal([]byte(a.Labels), &m.Labels)
		if a.Telemetry != "" {
			var t api.Telemetry
			if json.Unmarshal([]byte(a.Telemetry), &t) == nil {
				m.Telemetry = &t
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// PatchMac drains / opts out a Mac.
func (e *Engine) PatchMac(ctx context.Context, id string, p api.MacPatch) error {
	a, err := e.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	drain, opt := a.Drain, a.OptedOut
	if p.Drain != nil {
		drain = *p.Drain
	}
	if p.OptedOut != nil {
		opt = *p.OptedOut
	}
	if err := e.st.SetAgentFlags(ctx, id, drain, opt); err != nil {
		return err
	}
	e.Kick()
	return nil
}

// DeleteMac removes a Mac; its credential stops working and its tasks are
// requeued once its heartbeats stop.
func (e *Engine) DeleteMac(ctx context.Context, id string) error {
	if err := e.st.DeleteAgent(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	if l := e.live[id]; l != nil {
		l.seen = time.Time{}
	}
	e.mu.Unlock()
	e.Kick()
	return nil
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// CreateAPIKey mints an API key; the secret is only returned here.
func (e *Engine) CreateAPIKey(ctx context.Context, name, role string) (api.APIKey, error) {
	if role != api.RoleApp && role != api.RoleAdmin && role != api.RoleViewer {
		return api.APIKey{}, vErr("role must be app, viewer or admin")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return api.APIKey{}, vErr("name is required")
	}
	secret := auth.NewSecret(auth.PrefixAPIKey)
	now := e.now()
	k := &store.APIKey{ID: "key_" + auth.NewID(), Name: name, Hash: auth.Hash(secret),
		Prefix: secret[:len(auth.PrefixAPIKey)+4], Role: role, CreatedAt: ms(now)}
	if err := e.st.CreateAPIKey(ctx, k); err != nil {
		return api.APIKey{}, err
	}
	return api.APIKey{ID: k.ID, Name: k.Name, Role: k.Role, Prefix: k.Prefix, CreatedAt: now.UTC(), Secret: secret}, nil
}

// ImportAPIKey stores a key whose secret was supplied out of band (bootstrap).
func (e *Engine) ImportAPIKey(ctx context.Context, name, role, secret string) error {
	if !strings.HasPrefix(secret, auth.PrefixAPIKey) || len(secret) < len(auth.PrefixAPIKey)+32 {
		return vErr("bootstrap key must start with %s and be at least 32 chars after the prefix", auth.PrefixAPIKey)
	}
	if _, err := e.st.APIKeyByHash(ctx, auth.Hash(secret)); err == nil {
		return nil
	}
	return e.st.CreateAPIKey(ctx, &store.APIKey{ID: "key_" + auth.NewID(), Name: name, Hash: auth.Hash(secret),
		Prefix: secret[:len(auth.PrefixAPIKey)+4], Role: role, CreatedAt: ms(e.now())})
}

// APIKeys lists keys (without secrets).
func (e *Engine) APIKeys(ctx context.Context) ([]api.APIKey, error) {
	ks, err := e.st.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.APIKey{}
	for _, k := range ks {
		out = append(out, api.APIKey{ID: k.ID, Name: k.Name, Role: k.Role, Prefix: k.Prefix,
			CreatedAt: time.UnixMilli(k.CreatedAt).UTC(), LastUsedAt: tptr(k.LastUsedAt), RevokedAt: tptr(k.RevokedAt)})
	}
	return out, nil
}

// AuthAPIKey resolves an API key secret.
func (e *Engine) AuthAPIKey(ctx context.Context, secret string) (*store.APIKey, error) {
	k, err := e.st.APIKeyByHash(ctx, auth.Hash(secret))
	if err != nil {
		return nil, ErrForbidden
	}
	now := ms(e.now())
	if now-k.LastUsedAt > 60_000 {
		_ = e.st.TouchAPIKey(ctx, k.ID, now)
	}
	return k, nil
}

// RevokeAPIKey revokes a key.
func (e *Engine) RevokeAPIKey(ctx context.Context, id string) error {
	return e.st.RevokeAPIKey(ctx, id, ms(e.now()))
}
