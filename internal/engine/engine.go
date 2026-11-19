// Package engine is the router's brain: it accepts jobs, keeps track of Macs,
// places runnable tasks on the best Mac, hands assignments to agents that
// long-poll for work, and handles every way an attempt can end (success,
// failure with retry/backoff, user yield, lost agent, timeout, cancel).
//
// The database is the source of truth; live agent state (latest heartbeat and
// wake-up channels) is kept in memory. One engine instance should own a
// database at a time (run the server as a single replica).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/metrics"
	"github.com/alee-ibrahim/use-my-mac/internal/sched"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Config holds scheduling and failure-handling parameters.
type Config struct {
	Weights sched.Weights `json:"weights" yaml:"weights"`
	Limits  sched.Limits  `json:"limits" yaml:"limits"`

	TickInterval      time.Duration `json:"tick_interval" yaml:"tick_interval"`
	HeartbeatTimeout  time.Duration `json:"heartbeat_timeout" yaml:"heartbeat_timeout"`
	AssignTimeout     time.Duration `json:"assign_timeout" yaml:"assign_timeout"`
	DefaultTimeout    time.Duration `json:"default_timeout" yaml:"default_timeout"`
	TimeoutGrace      time.Duration `json:"timeout_grace" yaml:"timeout_grace"`
	DefaultMaxRetries int           `json:"default_max_retries" yaml:"default_max_retries"`
	BackoffBase       time.Duration `json:"backoff_base" yaml:"backoff_base"`
	BackoffMax        time.Duration `json:"backoff_max" yaml:"backoff_max"`
	FailExclude       time.Duration `json:"fail_exclude" yaml:"fail_exclude"`
	YieldExclude      time.Duration `json:"yield_exclude" yaml:"yield_exclude"`
	QuarantineAfter   int           `json:"quarantine_after" yaml:"quarantine_after"`
	QuarantineFor     time.Duration `json:"quarantine_for" yaml:"quarantine_for"`
	MaxLogBytes       int           `json:"max_log_bytes" yaml:"max_log_bytes"`
	EnrolTokenTTL     time.Duration `json:"enrol_token_ttl" yaml:"enrol_token_ttl"`
	// MaxMacFaults is how many times one task is rescheduled for free
	// because a Mac could not execute its runtime (program missing, exit
	// 127). Further such attempts count against the task's max_retries, so
	// a task cannot bounce forever.
	MaxMacFaults int `json:"max_mac_faults" yaml:"max_mac_faults"`
	// LegacyFaultTTL: agents without runtime preflight never confirm a
	// runtime is back, so the router retries it after this long.
	LegacyFaultTTL time.Duration `json:"legacy_fault_ttl" yaml:"legacy_fault_ttl"`
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Weights:           sched.DefaultWeights(),
		Limits:            sched.DefaultLimits(),
		TickInterval:      time.Second,
		HeartbeatTimeout:  30 * time.Second,
		AssignTimeout:     60 * time.Second,
		DefaultTimeout:    time.Hour,
		TimeoutGrace:      2 * time.Minute,
		DefaultMaxRetries: 3,
		BackoffBase:       5 * time.Second,
		BackoffMax:        5 * time.Minute,
		FailExclude:       time.Hour,
		YieldExclude:      5 * time.Minute,
		QuarantineAfter:   3,
		QuarantineFor:     10 * time.Minute,
		MaxLogBytes:       1 << 20,
		EnrolTokenTTL:     24 * time.Hour,
		MaxMacFaults:      5,
		LegacyFaultTTL:    30 * time.Minute,
	}
}

// Errors returned to the API layer.
var (
	ErrStale     = errors.New("stale update: task is not assigned to this agent/attempt")
	ErrNotFound  = store.ErrNotFound
	ErrForbidden = errors.New("forbidden")
)

// ValidationError is a bad job spec.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type live struct {
	seen time.Time
	hb   api.Heartbeat
	wake chan struct{}
	// updDrainUntil: the updater has the Mac drained until then.
	updDrainUntil time.Time
	// Runtime availability (see runtimes.go): faults are runtimes the
	// router marked unavailable after an exec failure; runtimes/unavail are
	// the effective view the scheduler and the API use.
	faults       map[string]runtimeFault
	faultsLoaded bool
	runtimes     []string
	unavail      []api.UnavailableRuntime
	savedUnavail string
	savedFaults  string
}

// Engine is safe for concurrent use.
type Engine struct {
	st    *store.Store
	blobs storage.Backend
	m     *metrics.M
	log   *slog.Logger
	now   func() time.Time

	mu        sync.Mutex
	cfg       Config
	live      map[string]*live
	placement map[string][]sched.Result
	started   time.Time
	kick      chan struct{}
	// version is the router's own version; updates is the agent update policy.
	version string
	updates api.UpdateSettings
	// h accumulates metrics history between samples; retention is the
	// effective retention policy (retentionDefault comes from the config).
	h                hist
	retention        api.Retention
	retentionDefault api.Retention

	// Live progress of running attempts (see progress.go).
	progMu sync.Mutex
	prog   map[string]*taskProg
	// Change subscribers (SSE streams).
	subMu sync.Mutex
	subs  map[chan string]struct{}
}

// Options for New.
type Options struct {
	Store   *store.Store
	Blobs   storage.Backend
	Metrics *metrics.M
	Logger  *slog.Logger
	Now     func() time.Time
	Config  Config
	// Version is the router's version, the default target for agent updates.
	Version string
	// Retention is the default retention policy (zero fields: built-in
	// defaults). Admins can change it at runtime.
	Retention api.Retention
}

const settingScheduler = "scheduler"

// New creates an engine. Scheduler weights/limits saved via the settings API
// override those in opts.Config.
func New(ctx context.Context, o Options) (*Engine, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	e := &Engine{
		st: o.Store, blobs: o.Blobs, m: o.Metrics, log: o.Logger, now: o.Now, cfg: o.Config,
		live: map[string]*live{}, placement: map[string][]sched.Result{}, kick: make(chan struct{}, 1),
		version: o.Version, retentionDefault: o.Retention,
		prog: map[string]*taskProg{}, subs: map[chan string]struct{}{},
	}
	e.h.last = o.Now()
	if e.version == "" {
		e.version = "dev"
	}
	e.loadUpdateSettings(ctx)
	e.loadRetention(ctx)
	e.started = e.now()
	if v, err := e.st.GetSetting(ctx, settingScheduler); err == nil {
		var s SchedulerSettings
		if json.Unmarshal([]byte(v), &s) == nil {
			e.cfg.Weights, e.cfg.Limits = s.Weights, s.Limits
		}
	}
	return e, nil
}

// Blobs returns the storage backend.
func (e *Engine) Blobs() storage.Backend { return e.blobs }

// Store returns the store.
func (e *Engine) Store() *store.Store { return e.st }

// Kick asks the scheduler to run soon.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run ticks the scheduler until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.TickInterval)
	defer t.Stop()
	lastSweep := time.Time{}
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.log.Error("scheduler tick", "err", err)
		}
		if now := e.now(); now.Sub(lastSweep) > uploadSweepEvery {
			lastSweep = now
			e.ExpireUploads(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
	}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func tptr(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := time.UnixMilli(v).UTC()
	return &t
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (e *Engine) wakeAgent(id string) {
	if l := e.live[id]; l != nil {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

func (e *Engine) liveFor(id string) *live {
	l := e.live[id]
	if l == nil {
		l = &live{wake: make(chan struct{}, 1)}
		e.live[id] = l
	}
	return l
}

func (e *Engine) online(id string, now time.Time) bool {
	l := e.live[id]
	return l != nil && !l.seen.IsZero() && now.Sub(l.seen) <= e.cfg.HeartbeatTimeout
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// SchedulerSettings are editable at runtime by admins.
type SchedulerSettings struct {
	Weights sched.Weights `json:"weights"`
	Limits  sched.Limits  `json:"limits"`
}

// Settings returns the current scheduler settings.
func (e *Engine) Settings() SchedulerSettings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return SchedulerSettings{Weights: e.cfg.Weights, Limits: e.cfg.Limits}
}

// SetSettings validates and persists scheduler settings.
func (e *Engine) SetSettings(ctx context.Context, s SchedulerSettings) error {
	w := s.Weights
	for _, v := range []float64{w.Presence, w.Load, w.Spread, w.Memory, w.Power, w.Thermal, w.GPU, w.Capacity} {
		if v < 0 || v > 100 {
			return &ValidationError{"weights must be between 0 and 100"}
		}
	}
	if s.Limits.IdleFullSeconds <= 0 || s.Limits.MinDiskFreeMB < 0 || s.Limits.MinBatteryPercent < 0 || s.Limits.MinBatteryPercent > 100 {
		return &ValidationError{"invalid limits"}
	}
	if err := e.st.PutSetting(ctx, settingScheduler, jsonStr(s)); err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg.Weights, e.cfg.Limits = s.Weights, s.Limits
	e.mu.Unlock()
	e.Kick()
	return nil
}

// ---------------------------------------------------------------------------
// Enrolment and agent auth
// ---------------------------------------------------------------------------

// CreateEnrolToken mints a one-time enrolment token.
func (e *Engine) CreateEnrolToken(ctx context.Context, by string, ttl time.Duration) (api.EnrolmentToken, error) {
	if ttl <= 0 || ttl > 30*24*time.Hour {
		ttl = e.cfg.EnrolTokenTTL
	}
	tok := auth.NewSecret(auth.PrefixEnrol)
	now := e.now()
	exp := now.Add(ttl)
	if err := e.st.CreateEnrolToken(ctx, auth.Hash(tok), ms(now), ms(exp), by); err != nil {
		return api.EnrolmentToken{}, err
	}
	return api.EnrolmentToken{Token: tok, ExpiresAt: exp.UTC()}, nil
}

// Enrol exchanges a one-time token for a long-lived agent credential.
func (e *Engine) Enrol(ctx context.Context, req api.EnrolRequest) (api.EnrolResponse, error) {
	return e.EnrolFrom(ctx, req, "")
}

// EnrolFrom is Enrol with the caller's IP for the install audit log. It
// accepts one-time enrolment tokens and install tokens (one use each).
func (e *Engine) EnrolFrom(ctx context.Context, req api.EnrolRequest, ip string) (api.EnrolResponse, error) {
	if req.Name == "" || len(req.Name) > 128 {
		return api.EnrolResponse{}, &ValidationError{"name is required (max 128 chars)"}
	}
	now := e.now()
	installID := ""
	if strings.HasPrefix(req.Token, auth.PrefixInstall) {
		t, err := e.st.ConsumeInstallToken(ctx, auth.Hash(req.Token), ms(now))
		if err != nil {
			if old, lerr := e.st.InstallTokenByHash(ctx, auth.Hash(req.Token)); lerr == nil {
				e.RecordInstallEvent(ctx, old.ID, InstallEventDenied, ip, "", "enrol refused: link expired, revoked or used up")
			}
			return api.EnrolResponse{}, ErrForbidden
		}
		installID = t.ID
	} else if err := e.st.ConsumeEnrolToken(ctx, auth.Hash(req.Token), ms(now)); err != nil {
		return api.EnrolResponse{}, ErrForbidden
	}
	cred := auth.NewSecret(auth.PrefixAgent)
	if req.MaxTasks <= 0 {
		req.MaxTasks = 1
	}
	a := &store.Agent{
		ID: "mac_" + auth.NewID(), Name: req.Name, CredHash: auth.Hash(cred), CreatedAt: ms(now),
		Runtimes: jsonStr(nonNil(req.Runtimes)), Labels: jsonStr(nonNilMap(req.Labels)), MaxTasks: req.MaxTasks,
		Version: req.Version, Telemetry: "",
	}
	if err := e.st.CreateAgent(ctx, a); err != nil {
		return api.EnrolResponse{}, err
	}
	e.log.Info("agent enrolled", "agent", a.ID, "name", a.Name, "install_token", installID)
	e.RecordInstallEvent(ctx, installID, InstallEventEnrol, ip, a.ID, "enrolled "+truncate(a.Name, 128))
	how := "with a one-time token"
	if installID != "" {
		how = "with install link " + installID
	}
	e.Audit(ctx, api.EventMacEnrolled, "", a.ID, a.Name+" enrolled "+how)
	return api.EnrolResponse{AgentID: a.ID, Credential: cred}, nil
}

// AuthAgent resolves an agent credential.
func (e *Engine) AuthAgent(ctx context.Context, cred string) (string, error) {
	a, err := e.st.AgentByCredential(ctx, auth.Hash(cred))
	if err != nil {
		return "", ErrForbidden
	}
	return a.ID, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// ---------------------------------------------------------------------------
// Heartbeats and polling
// ---------------------------------------------------------------------------

// Heartbeat records an agent's telemetry and tells it which tasks to stop.
func (e *Engine) Heartbeat(ctx context.Context, agentID string, hb api.Heartbeat) (api.HeartbeatResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.st.GetAgent(ctx, agentID)
	if err != nil {
		return api.HeartbeatResponse{}, err
	}
	now := e.now()
	l := e.liveFor(agentID)
	wasReady := e.online(agentID, now) && l.hb.State == api.AgentReady
	l.seen, l.hb = now, hb
	if hb.MaxTasks <= 0 {
		hb.MaxTasks = 1
	}
	a.LastSeen, a.State, a.Reason = ms(now), hb.State, hb.Reason
	a.Telemetry = jsonStr(hb.Telemetry)
	a.Runtimes, a.Labels = jsonStr(nonNil(e.applyRuntimesLocked(ctx, agentID, l, now))), jsonStr(nonNilMap(hb.Labels))
	a.MaxTasks, a.Version = hb.MaxTasks, hb.Version
	if err := e.st.UpdateAgentHeartbeat(ctx, a); err != nil {
		return api.HeartbeatResponse{}, err
	}
	e.h.telemetry(agentID, hb.Telemetry)
	e.m.SetTelemetry(a.Name, map[string]float64{
		"user_cpu": hb.Telemetry.UserCPUPercent, "our_cpu": hb.Telemetry.OurCPUPercent,
		"gpu": hb.Telemetry.GPUUtilPercent, "mem_available_bytes": float64(hb.Telemetry.MemAvailableBytes),
		"idle_seconds": hb.Telemetry.UserIdleSeconds,
	})

	resp := api.HeartbeatResponse{Drain: a.Drain || a.OptedOut}
	if !resp.Drain && e.updateDrainingLocked(agentID, now) {
		resp.Drain, resp.DrainReason = true, "updating"
	}
	for _, id := range hb.Running {
		t, err := e.st.GetTask(ctx, id)
		if err != nil || t.AgentID != agentID || t.State != api.StateRunning || a.OptedOut {
			resp.Cancel = append(resp.Cancel, id)
		}
	}
	// Tasks we think run here but the agent doesn't report (e.g. the agent
	// restarted) are requeued. The grace period covers the race between a
	// poll handing out a task and the next heartbeat listing it.
	reported := map[string]bool{}
	for _, id := range hb.Running {
		reported[id] = true
	}
	if mine, err := e.st.RunningTasksForAgent(ctx, agentID); err == nil {
		for _, t := range mine {
			if !reported[t.ID] && now.Sub(time.UnixMilli(t.StartedAt)) > e.cfg.HeartbeatTimeout/2 {
				e.requeueLocked(ctx, t, api.OutcomeLost, "agent no longer runs it (restarted?)", 0, now)
				e.updateJobLocked(ctx, t.JobID, now)
				e.Kick()
			}
		}
	}
	if !wasReady && hb.State == api.AgentReady {
		e.Kick()
	}
	return resp, nil
}

// Poll waits up to `wait` for assignments for this agent.
func (e *Engine) Poll(ctx context.Context, agentID string, wait time.Duration) ([]api.Assignment, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		as, wake, err := e.dispatch(ctx, agentID)
		if err != nil || len(as) > 0 {
			return as, err
		}
		select {
		case <-wake:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, nil
		}
	}
}

func (e *Engine) dispatch(ctx context.Context, agentID string) ([]api.Assignment, chan struct{}, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.liveFor(agentID)
	tasks, err := e.st.ActiveTasks(ctx)
	if err != nil {
		return nil, l.wake, err
	}
	now := e.now()
	var out []api.Assignment
	for _, t := range tasks {
		if t.State != api.StateAssigned || t.AgentID != agentID {
			continue
		}
		var spec api.TaskSpec
		if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
			return nil, l.wake, err
		}
		timeout := time.Duration(spec.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = e.cfg.DefaultTimeout
		}
		t.Attempt++
		t.State = api.StateRunning
		t.StartedAt = ms(now)
		t.DeadlineAt = ms(now.Add(timeout))
		if err := e.st.UpdateTask(ctx, t); err != nil {
			return nil, l.wake, err
		}
		if err := e.st.InsertAttempt(ctx, &store.Attempt{TaskID: t.ID, Attempt: t.Attempt, AgentID: agentID,
			StartedAt: ms(now), Outcome: api.OutcomeRunning, Score: t.Score}); err != nil {
			return nil, l.wake, err
		}
		as, err := e.resolve(ctx, t, spec, timeout)
		if err != nil {
			// Inputs cannot be resolved: this attempt fails without running.
			e.failLocked(ctx, t, api.OutcomeFailed, "resolve inputs/outputs: "+err.Error(), nil, now)
			continue
		}
		out = append(out, as)
		e.updateJobLocked(ctx, t.JobID, now)
	}
	return out, l.wake, nil
}

// OutputKey is where router storage keeps a task output.
func OutputKey(jobID, taskKey, output string) string {
	return "jobs/" + jobID + "/" + taskKey + "/" + output
}

// UploadKey is where app uploads live.
func UploadKey(key string) string { return "uploads/" + key }

func (e *Engine) resolve(ctx context.Context, t *store.Task, spec api.TaskSpec, timeout time.Duration) (api.Assignment, error) {
	ttl := timeout + 30*time.Minute
	if ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour
	}
	as := api.Assignment{
		TaskID: t.ID, JobID: t.JobID, Key: t.Key, Attempt: t.Attempt, Runtime: spec.Runtime, Args: spec.Args,
		Env: spec.Env, TimeoutSeconds: int(timeout / time.Second), Resources: spec.Resources,
		Inputs: []api.AssignedInput{}, Outputs: []api.AssignedOutput{},
	}
	var siblings map[string]api.TaskSpec
	for _, in := range spec.Inputs {
		var u string
		var err error
		switch {
		case in.URL != "":
			u = in.URL
		case in.Blob != "":
			u, err = e.blobs.DownloadURL(ctx, UploadKey(in.Blob), ttl)
		case in.From != nil:
			if siblings == nil {
				siblings, err = e.jobSpecs(ctx, t.JobID)
				if err != nil {
					return as, err
				}
			}
			src, ok := siblings[in.From.Task]
			if !ok {
				return as, fmt.Errorf("input %s: unknown task %s", in.Name, in.From.Task)
			}
			u, err = "", nil
			for _, o := range src.Outputs {
				if o.Name == in.From.Output && o.URL != "" {
					u = o.URL
				}
			}
			if u == "" {
				u, err = e.blobs.DownloadURL(ctx, OutputKey(t.JobID, in.From.Task, in.From.Output), ttl)
			}
		}
		if err != nil {
			return as, err
		}
		as.Inputs = append(as.Inputs, api.AssignedInput{Name: in.Name, URL: u})
	}
	_, multipart := storage.MultipartOf(e.blobs)
	for _, o := range spec.Outputs {
		u := o.URL
		if u == "" {
			var err error
			u, err = e.blobs.UploadURL(ctx, OutputKey(t.JobID, t.Key, o.Name), ttl)
			if err != nil {
				return as, err
			}
		}
		as.Outputs = append(as.Outputs, api.AssignedOutput{Name: o.Name, URL: u, Optional: o.Optional,
			Multipart: multipart && o.URL == ""})
	}
	return as, nil
}

func (e *Engine) jobSpecs(ctx context.Context, jobID string) (map[string]api.TaskSpec, error) {
	ts, err := e.st.TasksByJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := map[string]api.TaskSpec{}
	for _, t := range ts {
		var s api.TaskSpec
		if err := json.Unmarshal([]byte(t.Spec), &s); err != nil {
			return nil, err
		}
		out[t.Key] = s
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Task updates from agents
// ---------------------------------------------------------------------------

// TaskUpdate handles progress / completion of an attempt.
func (e *Engine) TaskUpdate(ctx context.Context, agentID, taskID string, u api.TaskUpdate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.AgentID != agentID || t.Attempt != u.Attempt || t.State != api.StateRunning {
		return ErrStale
	}
	now := e.now()
	var spec api.TaskSpec
	if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
		return err
	}
	switch u.Outcome {
	case api.OutcomeRunning:
		return nil
	case api.OutcomeSucceeded:
		have := map[string]bool{}
		for _, o := range u.Outputs {
			have[o] = true
		}
		for _, o := range spec.Outputs {
			if !o.Optional && !have[o.Name] {
				e.failLocked(ctx, t, api.OutcomeFailed, "missing output "+o.Name, u.ExitCode, now)
				e.updateJobLocked(ctx, t.JobID, now)
				e.Kick()
				return nil
			}
		}
		if err := e.st.FinishAttempt(ctx, t.ID, t.Attempt, api.OutcomeSucceeded, "", ms(now)); err != nil {
			return err
		}
		e.dropProgress(t.ID)
		t.State, t.FinishedAt, t.ExitCode, t.Error = api.StateSucceeded, ms(now), u.ExitCode, ""
		t.Outputs = jsonStr(nonNil(u.Outputs))
		if err := e.st.UpdateTask(ctx, t); err != nil {
			return err
		}
		if a, err := e.st.GetAgent(ctx, agentID); err == nil && a.ConsecFailures > 0 {
			_ = e.st.SetAgentHealth(ctx, agentID, 0, a.QuarantinedUntil)
		}
		e.outcome(api.OutcomeSucceeded, now.Sub(time.UnixMilli(t.StartedAt)).Seconds())
	case api.OutcomeFailed, api.OutcomeTimeout:
		if why := macFault(u); why != "" {
			e.runtimeFaultLocked(ctx, t, agentID, spec.Runtime, why, u.ExitCode, now)
			break
		}
		msg := u.Error
		if msg == "" {
			msg = u.Outcome
		}
		e.failLocked(ctx, t, u.Outcome, msg, u.ExitCode, now)
	case api.OutcomeYielded:
		e.requeueLocked(ctx, t, api.OutcomeYielded, nz(u.Error, "Mac's user needed it"), e.cfg.YieldExclude, now)
	case api.OutcomeCancelled:
		e.requeueLocked(ctx, t, api.OutcomeCancelled, nz(u.Error, "stopped by agent"), 0, now)
	default:
		return &ValidationError{"unknown outcome " + u.Outcome}
	}
	e.updateJobLocked(ctx, t.JobID, now)
	e.Kick()
	return nil
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func excludedMap(t *store.Task) map[string]int64 {
	m := map[string]int64{}
	_ = json.Unmarshal([]byte(t.Excluded), &m)
	return m
}

// requeueLocked ends the current attempt without counting a failure.
func (e *Engine) requeueLocked(ctx context.Context, t *store.Task, outcome, msg string, exclude time.Duration, now time.Time) {
	e.dropProgress(t.ID)
	if t.State == api.StateRunning && t.Attempt > 0 {
		_ = e.st.FinishAttempt(ctx, t.ID, t.Attempt, outcome, msg, ms(now))
		e.outcome(outcome, now.Sub(time.UnixMilli(t.StartedAt)).Seconds())
	}
	if exclude > 0 && t.AgentID != "" {
		ex := excludedMap(t)
		ex[t.AgentID] = ms(now.Add(exclude))
		t.Excluded = jsonStr(ex)
	}
	prev := t.AgentID
	if outcome == api.OutcomeYielded && prev != "" {
		name := prev
		if a, err := e.st.GetAgent(ctx, prev); err == nil {
			name = a.Name
		}
		e.Audit(ctx, api.EventTaskYielded, "", prev, "task "+t.Key+" yielded on "+name+": "+msg)
	}
	t.State, t.AgentID, t.AssignedAt, t.StartedAt, t.DeadlineAt, t.NotBefore = api.StatePending, "", 0, 0, 0, 0
	t.Error = outcome + ": " + msg
	if err := e.st.UpdateTask(ctx, t); err != nil {
		e.log.Error("requeue", "task", t.ID, "err", err)
	}
	e.log.Info("task requeued", "task", t.ID, "agent", prev, "outcome", outcome, "reason", msg)
}

// failLocked counts a failure: retry with backoff on another Mac, or give up.
func (e *Engine) failLocked(ctx context.Context, t *store.Task, outcome, msg string, exit *int, now time.Time) {
	e.dropProgress(t.ID)
	if t.State == api.StateRunning && t.Attempt > 0 {
		_ = e.st.FinishAttempt(ctx, t.ID, t.Attempt, outcome, msg, ms(now))
		e.outcome(outcome, now.Sub(time.UnixMilli(t.StartedAt)).Seconds())
	}
	if t.AgentID != "" {
		ex := excludedMap(t)
		ex[t.AgentID] = ms(now.Add(e.cfg.FailExclude))
		t.Excluded = jsonStr(ex)
		if a, err := e.st.GetAgent(ctx, t.AgentID); err == nil {
			consec := a.ConsecFailures + 1
			q := a.QuarantinedUntil
			if e.cfg.QuarantineAfter > 0 && consec >= e.cfg.QuarantineAfter {
				q = ms(now.Add(e.cfg.QuarantineFor))
				consec = 0
				e.log.Warn("agent quarantined after consecutive failures", "agent", a.ID, "until", tptr(q))
				e.Audit(ctx, api.EventMacQuarantined, "", a.ID, a.Name+" quarantined for "+e.cfg.QuarantineFor.String()+
					" after "+strconv.Itoa(e.cfg.QuarantineAfter)+" failures in a row")
			}
			_ = e.st.SetAgentHealth(ctx, a.ID, consec, q)
		}
	}
	var spec api.TaskSpec
	_ = json.Unmarshal([]byte(t.Spec), &spec)
	maxRetries := e.cfg.DefaultMaxRetries
	if spec.MaxRetries != nil {
		maxRetries = *spec.MaxRetries
	}
	t.Failures++
	t.ExitCode, t.Error = exit, outcome+": "+msg
	t.AgentID, t.AssignedAt, t.StartedAt, t.DeadlineAt = "", 0, 0, 0
	if t.Failures > maxRetries {
		t.State, t.FinishedAt = api.StateFailed, ms(now)
	} else {
		t.State = api.StatePending
		t.NotBefore = ms(now.Add(sched.Backoff(t.Failures, e.cfg.BackoffBase, e.cfg.BackoffMax)))
	}
	if err := e.st.UpdateTask(ctx, t); err != nil {
		e.log.Error("fail task", "task", t.ID, "err", err)
	}
	e.log.Info("task attempt failed", "task", t.ID, "failures", t.Failures, "state", t.State, "reason", msg)
}

// AppendLog stores a log chunk from the agent running the task.
func (e *Engine) AppendLog(ctx context.Context, agentID, taskID string, c api.LogChunk) error {
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if c.Attempt < 1 || c.Attempt > t.Attempt {
		return ErrStale
	}
	att, err := e.st.Attempts(ctx, taskID)
	if err != nil {
		return err
	}
	owner := ""
	for _, a := range att {
		if a.Attempt == c.Attempt {
			owner = a.AgentID
		}
	}
	if owner != agentID {
		return ErrStale
	}
	size, err := e.st.LogSize(ctx, taskID)
	if err != nil {
		return err
	}
	if size >= e.cfg.MaxLogBytes {
		return nil
	}
	data := c.Data
	if size+len(data) > e.cfg.MaxLogBytes {
		data = data[:e.cfg.MaxLogBytes-size] + "\n[log truncated]\n"
	}
	return e.st.AppendLog(ctx, taskID, c.Attempt, c.Seq, ms(e.now()), data)
}

// ---------------------------------------------------------------------------
// The scheduler tick
// ---------------------------------------------------------------------------

// Tick runs one scheduling round: detect lost agents and overdue tasks,
// unblock tasks whose dependencies finished, then place runnable tasks.
func (e *Engine) Tick(ctx context.Context) error {
	start := time.Now()
	defer func() { e.m.Tick(time.Since(start).Seconds()) }()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	tasks, err := e.st.ActiveTasks(ctx)
	if err != nil {
		return err
	}
	agents, err := e.st.ListAgents(ctx)
	if err != nil {
		return err
	}
	jobs := map[string]bool{}
	settled := now.Sub(e.started) > e.cfg.HeartbeatTimeout

	// 1. Lost agents, unclaimed assignments, overdue attempts.
	for _, t := range tasks {
		switch t.State {
		case api.StateAssigned, api.StateRunning:
			switch {
			case !e.online(t.AgentID, now) && (settled || e.live[t.AgentID] != nil):
				e.requeueLocked(ctx, t, api.OutcomeLost, "agent stopped sending heartbeats", 0, now)
				jobs[t.JobID] = true
			case t.State == api.StateAssigned && e.live[t.AgentID].hb.State != api.AgentReady:
				e.requeueLocked(ctx, t, api.OutcomeLost, "Mac stopped accepting work before pick-up", 0, now)
				jobs[t.JobID] = true
			case t.State == api.StateAssigned && now.Sub(time.UnixMilli(t.AssignedAt)) > e.cfg.AssignTimeout:
				e.requeueLocked(ctx, t, api.OutcomeLost, "agent did not pick up the task", e.cfg.YieldExclude, now)
				jobs[t.JobID] = true
			case t.State == api.StateRunning && t.DeadlineAt > 0 && now.After(time.UnixMilli(t.DeadlineAt).Add(e.cfg.TimeoutGrace)):
				e.failLocked(ctx, t, api.OutcomeTimeout, "deadline exceeded and agent did not report", nil, now)
				jobs[t.JobID] = true
			}
		}
	}

	// 2. Dependencies.
	depState := map[string]map[string]string{} // job -> key -> state
	for _, t := range tasks {
		if t.State != api.StateBlocked {
			continue
		}
		states, ok := depState[t.JobID]
		if !ok {
			all, err := e.st.TasksByJob(ctx, t.JobID)
			if err != nil {
				return err
			}
			states = map[string]string{}
			for _, x := range all {
				states[x.Key] = x.State
			}
			depState[t.JobID] = states
		}
		var spec api.TaskSpec
		_ = json.Unmarshal([]byte(t.Spec), &spec)
		ready, broken := true, ""
		for _, d := range spec.DependsOn {
			switch states[d] {
			case api.StateSucceeded:
			case api.StateFailed, api.StateCancelled:
				broken = d
			default:
				ready = false
			}
		}
		switch {
		case broken != "":
			t.State, t.FinishedAt, t.Error = api.StateCancelled, ms(now), "dependency "+broken+" did not succeed"
		case ready:
			t.State = api.StatePending
		default:
			continue
		}
		if err := e.st.UpdateTask(ctx, t); err != nil {
			return err
		}
		states[t.Key] = t.State
		jobs[t.JobID] = true
	}

	// 3. Placement.
	if err := e.placeLocked(ctx, tasks, agents, now, jobs); err != nil {
		return err
	}

	for j := range jobs {
		e.updateJobLocked(ctx, j, now)
	}

	// Metrics.
	states := map[string]int{}
	for _, a := range agents {
		switch {
		case !e.online(a.ID, now):
			states["offline"]++
		case a.Drain || a.OptedOut:
			states[api.AgentDraining]++
		default:
			states[nz(e.live[a.ID].hb.State, "unknown")]++
		}
	}
	e.m.SetAgents(states)
	if counts, err := e.st.CountTasksByState(ctx); err == nil {
		e.m.SetTasks(counts)
	}
	return nil
}

func (e *Engine) candidatesLocked(agents []*store.Agent, tasks []*store.Task, now time.Time) []sched.Candidate {
	active := map[string]int{}
	reserved := map[string]int{}
	for _, t := range tasks {
		if t.State == api.StateAssigned || t.State == api.StateRunning {
			active[t.AgentID]++
			var s api.TaskSpec
			_ = json.Unmarshal([]byte(t.Spec), &s)
			reserved[t.AgentID] += s.Resources.RAMMB
		}
	}
	maxCPU := 0
	var out []sched.Candidate
	for _, a := range agents {
		c := sched.Candidate{
			ID: a.ID, Online: e.online(a.ID, now), Drain: a.Drain || e.updateDrainingLocked(a.ID, now), OptedOut: a.OptedOut,
			Quarantined: a.QuarantinedUntil > ms(now), MaxTasks: a.MaxTasks,
			Active: active[a.ID], ReservedMB: reserved[a.ID],
		}
		if l := e.live[a.ID]; l != nil {
			c.AgentState, c.Telemetry, c.Runtimes, c.Labels = l.hb.State, l.hb.Telemetry, l.runtimes, l.hb.Labels
			if len(l.unavail) > 0 {
				c.Unavailable = map[string]string{}
				for _, u := range l.unavail {
					c.Unavailable[u.Name] = u.Reason
				}
			}
			if l.hb.MaxTasks > 0 {
				c.MaxTasks = l.hb.MaxTasks
			}
		}
		if c.Telemetry.CPUCount > maxCPU {
			maxCPU = c.Telemetry.CPUCount
		}
		out = append(out, c)
	}
	for i := range out {
		out[i].FleetMaxCPU = maxCPU
	}
	return out
}

func (e *Engine) placeLocked(ctx context.Context, tasks []*store.Task, agents []*store.Agent, now time.Time, jobs map[string]bool) error {
	var pending []*store.Task
	for _, t := range tasks {
		if t.State == api.StatePending && t.NotBefore <= ms(now) {
			pending = append(pending, t)
		}
	}
	if len(pending) == 0 || len(agents) == 0 {
		return nil
	}
	cands := e.candidatesLocked(agents, tasks, now)
	idx := map[string]int{}
	for i, c := range cands {
		idx[c.ID] = i
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].CreatedAt < pending[j].CreatedAt })
	for _, t := range pending {
		var spec api.TaskSpec
		if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
			return err
		}
		ex := excludedMap(t)
		cs := make([]sched.Candidate, len(cands))
		copy(cs, cands)
		for i := range cs {
			cs[i].Excluded = ex[cs[i].ID] > ms(now)
		}
		best, all := sched.Pick(cs, sched.Need{Runtime: spec.Runtime, Resources: spec.Resources}, e.cfg.Weights, e.cfg.Limits)
		e.placement[t.ID] = all
		if best == nil {
			continue
		}
		t.State, t.AgentID, t.AssignedAt, t.Score = api.StateAssigned, best.ID, ms(now), best.Score
		if err := e.st.UpdateTask(ctx, t); err != nil {
			return err
		}
		c := &cands[idx[best.ID]]
		c.Active++
		c.ReservedMB += spec.Resources.RAMMB
		jobs[t.JobID] = true
		e.m.Assigned(best.ID)
		e.h.count("assigned", 1)
		e.wakeAgent(best.ID)
	}
	return nil
}

// updateJobLocked recomputes a job's state from its tasks.
func (e *Engine) updateJobLocked(ctx context.Context, jobID string, now time.Time) {
	defer e.notify(jobID)
	j, err := e.st.GetJob(ctx, jobID)
	if err != nil || api.IsTerminal(j.State) {
		return
	}
	counts, err := e.st.TaskCounts(ctx, jobID)
	if err != nil {
		return
	}
	state := jobStateFromCounts(counts, j.CancelRequested)
	if state != j.State {
		j.State = state
		if api.IsTerminal(state) {
			j.FinishedAt = ms(now)
			if ts, err := e.st.TasksByJob(ctx, jobID); err == nil {
				for _, t := range ts {
					delete(e.placement, t.ID)
				}
			}
		}
		if err := e.st.UpdateJob(ctx, j); err != nil {
			e.log.Error("update job", "job", jobID, "err", err)
		}
		e.log.Info("job state", "job", jobID, "state", state)
		if state == api.StateFailed || state == api.StateCancelled {
			e.Audit(ctx, api.EventJobFinished, "", jobID, "job "+j.Name+" "+state)
		}
	}
}

// Job states.
const (
	JobQueued  = "queued"
	JobRunning = "running"
)

func jobStateFromCounts(c map[string]int, cancel bool) string {
	total, terminal := 0, 0
	for s, n := range c {
		total += n
		if api.IsTerminal(s) {
			terminal += n
		}
	}
	if total > 0 && total == terminal {
		switch {
		case cancel:
			return api.StateCancelled
		case c[api.StateSucceeded] == total:
			return api.StateSucceeded
		default:
			return api.StateFailed
		}
	}
	if c[api.StateRunning]+c[api.StateAssigned] > 0 || c[api.StateSucceeded]+c[api.StateFailed] > 0 {
		return JobRunning
	}
	return JobQueued
}
