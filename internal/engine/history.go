package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Metrics history: the router samples the fleet once a minute (Mac states,
// task counts, attempt outcomes, average telemetry per Mac) into the
// database, rolls samples up into 15-minute buckets and prunes both by age.
// The dashboard's charts read it through FleetSeries / MacSeries / Summary.

// DefaultRetention is used for unset retention fields.
func DefaultRetention() api.Retention {
	return api.Retention{JobsDays: 0, EventsDays: 90, MetricsRawHours: 24, MetricsRollupDays: 30}
}

const settingRetention = "retention"

// hist accumulates between samples. It has its own lock and never takes
// e.mu, so it can be fed from code that holds e.mu.
type hist struct {
	mu       sync.Mutex
	counters map[string]float64
	macs     map[string]*macAcc
	last     time.Time
}

type macAcc struct {
	userCPU, ourCPU, gpu, memUsed float64
	n, gpuN                       int
}

func (h *hist) count(name string, v float64) {
	h.mu.Lock()
	if h.counters == nil {
		h.counters = map[string]float64{}
	}
	h.counters[name] += v
	h.mu.Unlock()
}

func memUsedPct(t api.Telemetry) float64 {
	if t.MemTotalBytes == 0 || t.MemAvailableBytes > t.MemTotalBytes {
		return 0
	}
	return float64(t.MemTotalBytes-t.MemAvailableBytes) / float64(t.MemTotalBytes) * 100
}

func (h *hist) telemetry(agentID string, t api.Telemetry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.macs == nil {
		h.macs = map[string]*macAcc{}
	}
	a := h.macs[agentID]
	if a == nil {
		a = &macAcc{}
		h.macs[agentID] = a
	}
	a.userCPU += t.UserCPUPercent
	a.ourCPU += t.OurCPUPercent
	a.memUsed += memUsedPct(t)
	a.n++
	if t.GPUUtilPercent >= 0 {
		a.gpu += t.GPUUtilPercent
		a.gpuN++
	}
}

func (h *hist) take() (map[string]float64, map[string]*macAcc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, m := h.counters, h.macs
	h.counters, h.macs = map[string]float64{}, map[string]*macAcc{}
	return c, m
}

func (h *hist) peek() map[string]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]float64{}
	for k, v := range h.counters {
		out[k] = v
	}
	return out
}

// outcome counts a finished attempt in Prometheus and in the history.
func (e *Engine) outcome(outcome string, seconds float64) {
	e.m.Outcome(outcome, seconds)
	e.h.count(outcome, 1)
}

// ---------------------------------------------------------------------------
// Live fleet state
// ---------------------------------------------------------------------------

// Mac classes for metrics. Every Mac is in exactly one.
const (
	classOffline  = "offline"
	classDraining = "draining"
	classYielded  = "yielded"
	classBusy     = "busy"
	classIdle     = "idle"
)

type macNow struct {
	agent   *store.Agent
	class   string
	online  bool
	running int
	tel     *api.Telemetry
}

type fleetNow struct {
	macs                     []macNow
	running, queued, blocked int
}

func (e *Engine) fleetState(ctx context.Context) (fleetNow, error) {
	var f fleetNow
	agents, err := e.st.ListAgents(ctx)
	if err != nil {
		return f, err
	}
	tasks, err := e.st.ActiveTasks(ctx)
	if err != nil {
		return f, err
	}
	running := map[string]int{}
	for _, t := range tasks {
		switch t.State {
		case api.StateRunning:
			running[t.AgentID]++
			f.running++
		case api.StatePending, api.StateAssigned:
			f.queued++
		case api.StateBlocked:
			f.blocked++
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, a := range agents {
		m := macNow{agent: a, online: e.online(a.ID, now), running: running[a.ID]}
		l := e.live[a.ID]
		switch {
		case !m.online:
			m.class = classOffline
		case a.Drain || a.OptedOut || e.updateDrainingLocked(a.ID, now) || l.hb.State == api.AgentDraining:
			m.class = classDraining
		case l.hb.State == api.AgentBusyUser || l.hb.State == api.AgentCooldown:
			m.class = classYielded
		case m.running > 0:
			m.class = classBusy
		default:
			m.class = classIdle
		}
		if m.online {
			t := l.hb.Telemetry
			m.tel = &t
		}
		f.macs = append(f.macs, m)
	}
	return f, nil
}

func (f fleetNow) classes() map[string]int {
	out := map[string]int{"total": len(f.macs)}
	for _, c := range []string{classOffline, classDraining, classYielded, classBusy, classIdle} {
		out[c] = 0
	}
	for _, m := range f.macs {
		out[m.class]++
		if m.online {
			out["online"]++
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Sampling, rollup, pruning
// ---------------------------------------------------------------------------

// SampleInterval is how often RunHistory samples (and the raw resolution).
const SampleInterval = store.ResRaw * time.Second

// RunHistory samples every minute and prunes every hour until ctx is done.
func (e *Engine) RunHistory(ctx context.Context) {
	t := time.NewTicker(SampleInterval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := e.SampleHistory(ctx); err != nil && ctx.Err() == nil {
			e.log.Error("metrics sample", "err", err)
		}
		if time.Since(lastPrune) >= time.Hour {
			lastPrune = time.Now()
			if err := e.PruneHistory(ctx); err != nil && ctx.Err() == nil {
				e.log.Error("retention prune", "err", err)
			}
		}
	}
}

// SampleHistory writes one raw sample for the current minute and refreshes
// its 15-minute rollup.
func (e *Engine) SampleHistory(ctx context.Context) error {
	f, err := e.fleetState(ctx)
	if err != nil {
		return err
	}
	now := e.now()
	counters, accs := e.h.take()
	e.h.mu.Lock()
	elapsed := now.Sub(e.h.last).Seconds()
	e.h.last = now
	e.h.mu.Unlock()
	if elapsed <= 0 || elapsed > 5*SampleInterval.Seconds() {
		elapsed = SampleInterval.Seconds()
	}

	cls := f.classes()
	fleet := store.Sample{
		"macs_online": float64(cls["online"]), "macs_busy": float64(cls[classBusy]), "macs_idle": float64(cls[classIdle]),
		"macs_yielded": float64(cls[classYielded]), "macs_draining": float64(cls[classDraining]),
		"macs_offline": float64(cls[classOffline]), "tasks_running": float64(f.running), "tasks_queued": float64(f.queued),
		"task_seconds": float64(f.running) * elapsed,
	}
	for _, c := range store.FleetCounters {
		if c != "task_seconds" {
			fleet[c] = counters[c]
		}
	}
	macs := map[string]store.Sample{}
	for _, m := range f.macs {
		s := store.Sample{"running": float64(m.running), "task_seconds": float64(m.running) * elapsed}
		if m.online {
			s["online"] = 1
		}
		if m.class == classYielded {
			s["yielded"] = 1
		}
		if a := accs[m.agent.ID]; a != nil && a.n > 0 {
			s["user_cpu"], s["our_cpu"], s["mem_used"] = a.userCPU/float64(a.n), a.ourCPU/float64(a.n), a.memUsed/float64(a.n)
			if a.gpuN > 0 {
				s["gpu"] = a.gpu / float64(a.gpuN)
			}
		} else if m.tel != nil {
			s["user_cpu"], s["our_cpu"], s["mem_used"] = m.tel.UserCPUPercent, m.tel.OurCPUPercent, memUsedPct(*m.tel)
			s["gpu"] = math.Max(0, m.tel.GPUUtilPercent)
		}
		macs[m.agent.ID] = s
	}
	ts := bucket(ms(now), store.ResRaw)
	if err := e.st.PutSamples(ctx, ts, fleet, macs); err != nil {
		return err
	}
	return e.st.Rollup(ctx, bucket(ts, store.ResRollup))
}

func bucket(tsMS int64, seconds int) int64 {
	step := int64(seconds) * 1000
	return tsMS / step * step
}

// PruneHistory applies the retention settings: old metrics, old events,
// spent enrolment tokens and (if enabled) old finished jobs.
func (e *Engine) PruneHistory(ctx context.Context) error {
	r := e.Retention()
	now := e.now()
	var errs []error
	if _, err := e.st.PruneSamples(ctx, store.ResRaw, ms(now.Add(-time.Duration(r.MetricsRawHours)*time.Hour))); err != nil {
		errs = append(errs, err)
	}
	if _, err := e.st.PruneSamples(ctx, store.ResRollup, ms(now.AddDate(0, 0, -r.MetricsRollupDays))); err != nil {
		errs = append(errs, err)
	}
	if _, err := e.st.PruneEvents(ctx, ms(now.AddDate(0, 0, -r.EventsDays))); err != nil {
		errs = append(errs, err)
	}
	if _, err := e.st.PruneEnrolTokens(ctx, ms(now)); err != nil {
		errs = append(errs, err)
	}
	if r.JobsDays > 0 {
		if _, err := e.PurgeJobs(ctx, api.PurgeJobsRequest{OlderThanDays: r.JobsDays}, "retention"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Retention settings
// ---------------------------------------------------------------------------

func normRetention(r api.Retention, def api.Retention) api.Retention {
	if r.EventsDays <= 0 {
		r.EventsDays = def.EventsDays
	}
	if r.MetricsRawHours <= 0 {
		r.MetricsRawHours = def.MetricsRawHours
	}
	if r.MetricsRollupDays <= 0 {
		r.MetricsRollupDays = def.MetricsRollupDays
	}
	if r.JobsDays < 0 {
		r.JobsDays = 0
	}
	return r
}

func validRetention(r api.Retention) error {
	switch {
	case r.MetricsRawHours < 24 || r.MetricsRawHours > 168:
		return &ValidationError{"metrics_raw_hours must be between 24 and 168"}
	case r.MetricsRollupDays < 1 || r.MetricsRollupDays > 400:
		return &ValidationError{"metrics_rollup_days must be between 1 and 400"}
	case r.EventsDays < 1 || r.EventsDays > 3650:
		return &ValidationError{"events_days must be between 1 and 3650"}
	case r.JobsDays < 0 || r.JobsDays > 3650:
		return &ValidationError{"jobs_days must be between 0 (keep) and 3650"}
	}
	return nil
}

func (e *Engine) loadRetention(ctx context.Context) {
	e.retention = normRetention(e.retentionDefault, DefaultRetention())
	if v, err := e.st.GetSetting(ctx, settingRetention); err == nil {
		var r api.Retention
		if json.Unmarshal([]byte(v), &r) == nil {
			e.retention = normRetention(r, e.retention)
		}
	}
}

// Retention returns the effective retention settings.
func (e *Engine) Retention() api.Retention {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.retention
}

// SetRetention validates and stores retention settings (zero fields take
// the defaults).
func (e *Engine) SetRetention(ctx context.Context, r api.Retention, actor string) (api.Retention, error) {
	r = normRetention(r, normRetention(e.retentionDefault, DefaultRetention()))
	if err := validRetention(r); err != nil {
		return r, err
	}
	if err := e.st.PutSetting(ctx, settingRetention, jsonStr(r)); err != nil {
		return r, err
	}
	e.mu.Lock()
	e.retention = r
	e.mu.Unlock()
	msg := "jobs kept forever"
	if r.JobsDays > 0 {
		msg = "finished jobs deleted after " + itoa(r.JobsDays) + " days"
	}
	e.Audit(ctx, api.EventRetention, actor, "", "retention: "+msg+", events "+itoa(r.EventsDays)+" days, metrics "+
		itoa(r.MetricsRawHours)+" h (1 min) / "+itoa(r.MetricsRollupDays)+" days (15 min)")
	return r, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// Audit appends to the activity / audit log (best effort).
func (e *Engine) Audit(ctx context.Context, kind, actor, target, msg string) {
	err := e.st.AddEvent(ctx, &store.Event{ID: "ev_" + auth.NewID(), TS: ms(e.now()), Kind: kind,
		Actor: truncate(actor, 200), Target: truncate(target, 200), Message: truncate(msg, 1000)})
	if err != nil {
		e.log.Warn("event log", "kind", kind, "err", err)
	}
}

// Events lists the activity log, newest first.
func (e *Engine) Events(ctx context.Context, kinds []string, target string, before time.Time, limit int) ([]api.Event, error) {
	var b int64
	if !before.IsZero() {
		b = ms(before)
	}
	evs, err := e.st.Events(ctx, kinds, target, b, limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.Event, 0, len(evs))
	for _, ev := range evs {
		out = append(out, api.Event{ID: ev.ID, At: time.UnixMilli(ev.TS).UTC(), Kind: ev.Kind, Actor: ev.Actor,
			Target: ev.Target, Message: ev.Message})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// maxPoints bounds a series answer.
const maxPoints = 1500

// pickRes chooses the resolution and step for a range: raw minutes while the
// range is inside raw retention, else 15-minute rollups. step <= 0 = auto
// (about 120 points).
func (e *Engine) pickRes(from, to time.Time, step int) (res, outStep int) {
	r := e.Retention()
	res = store.ResRollup
	// A minute of slack: "range=24h" is computed a moment before this runs.
	if !from.Before(e.now().Add(-time.Duration(r.MetricsRawHours)*time.Hour - time.Minute)) {
		res = store.ResRaw
	}
	span := to.Sub(from).Seconds()
	if step <= 0 {
		step = int(math.Ceil(span / 120))
	}
	if minStep := int(math.Ceil(span / maxPoints)); step < minStep {
		step = minStep
	}
	if step < res {
		step = res
	}
	step = (step + res - 1) / res * res
	return res, step
}

func fill(points []store.Point, from, to time.Time, step int) []map[string]float64 {
	stepMS := int64(step) * 1000
	have := make(map[int64]store.Point, len(points))
	for _, p := range points {
		have[p.TS] = p
	}
	var out []map[string]float64
	for b := ms(from) / stepMS * stepMS; b < ms(to); b += stepMS {
		m := map[string]float64{"t": float64(b)}
		if p, ok := have[b]; ok {
			for k, v := range p.Values {
				m[k] = round(v)
			}
		}
		out = append(out, m)
	}
	if out == nil {
		out = []map[string]float64{}
	}
	return out
}

func round(v float64) float64 { return math.Round(v*100) / 100 }

func (e *Engine) clampRange(from, to time.Time) (time.Time, time.Time, error) {
	now := e.now()
	if to.IsZero() || to.After(now) {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	if !from.Before(to) {
		return from, to, &ValidationError{"from must be before to"}
	}
	if to.Sub(from) > 400*24*time.Hour {
		return from, to, &ValidationError{"range too long (max 400 days)"}
	}
	return from, to, nil
}

// FleetSeries returns fleet metrics for [from, to).
func (e *Engine) FleetSeries(ctx context.Context, from, to time.Time, step int) (api.Series, error) {
	from, to, err := e.clampRange(from, to)
	if err != nil {
		return api.Series{}, err
	}
	res, step := e.pickRes(from, to, step)
	stepMS := int64(step) * 1000
	pts, err := e.st.FleetSeries(ctx, res, ms(from)/stepMS*stepMS, ms(to), step)
	if err != nil {
		return api.Series{}, err
	}
	return api.Series{From: from.UTC(), To: to.UTC(), Step: step, Res: res, Points: fill(pts, from, to, step)}, nil
}

// MacSeries returns one Mac's metrics for [from, to).
func (e *Engine) MacSeries(ctx context.Context, agentID string, from, to time.Time, step int) (api.Series, error) {
	if _, err := e.st.GetAgent(ctx, agentID); err != nil {
		return api.Series{}, err
	}
	from, to, err := e.clampRange(from, to)
	if err != nil {
		return api.Series{}, err
	}
	res, step := e.pickRes(from, to, step)
	stepMS := int64(step) * 1000
	pts, err := e.st.MacSeries(ctx, agentID, res, ms(from)/stepMS*stepMS, ms(to), step)
	if err != nil {
		return api.Series{}, err
	}
	return api.Series{From: from.UTC(), To: to.UTC(), Step: step, Res: res, Points: fill(pts, from, to, step)}, nil
}

// MacSparks returns a short series per Mac (default: last hour, 2-minute
// steps) for the Overview's sparklines.
func (e *Engine) MacSparks(ctx context.Context, window time.Duration, step int) (api.MacSparks, error) {
	if window <= 0 || window > 24*time.Hour {
		window = time.Hour
	}
	to := e.now()
	from := to.Add(-window)
	if step <= 0 {
		step = int(window.Seconds() / 30)
	}
	res, step := e.pickRes(from, to, step)
	stepMS := int64(step) * 1000
	all, err := e.st.AllMacSeries(ctx, res, ms(from)/stepMS*stepMS, ms(to), step)
	if err != nil {
		return api.MacSparks{}, err
	}
	out := api.MacSparks{From: from.UTC(), To: to.UTC(), Step: step, Macs: map[string][]map[string]float64{}}
	for id, pts := range all {
		out.Macs[id] = fill(pts, from, to, step)
	}
	return out, nil
}

func window(t map[string]float64) api.Window {
	w := api.Window{
		Succeeded: int64(t["succeeded"]), Failed: int64(t["failed"]), Timeout: int64(t["timeout"]),
		Yielded: int64(t["yielded"]), Lost: int64(t["lost"]), Cancelled: int64(t["cancelled"]),
		Assigned: int64(t["assigned"]), ComputeHours: round(t["task_seconds"] / 3600), SuccessRate: -1,
	}
	if done := w.Succeeded + w.Failed + w.Timeout; done > 0 {
		w.SuccessRate = math.Round(float64(w.Succeeded)/float64(done)*10000) / 10000
	}
	return w
}

// Summary is the Overview's headline numbers.
func (e *Engine) Summary(ctx context.Context) (api.Summary, error) {
	f, err := e.fleetState(ctx)
	if err != nil {
		return api.Summary{}, err
	}
	now := e.now()
	jobs, err := e.st.CountJobsByState(ctx)
	if err != nil {
		return api.Summary{}, err
	}
	pending := e.h.peek()
	d1, err := e.st.FleetTotals(ctx, store.ResRaw, ms(now.Add(-24*time.Hour)), ms(now)+1)
	if err != nil {
		return api.Summary{}, err
	}
	d7, err := e.st.FleetTotals(ctx, store.ResRollup, ms(now.AddDate(0, 0, -7)), ms(now)+1)
	if err != nil {
		return api.Summary{}, err
	}
	for k, v := range pending {
		d1[k] += v
		d7[k] += v
	}
	s := api.Summary{
		At: now.UTC(), Macs: f.classes(), RouterVer: e.version,
		Tasks:   map[string]int{"running": f.running, "queued": f.queued, "blocked": f.blocked},
		Jobs:    map[string]int{"queued": jobs[JobQueued], "running": jobs[JobRunning]},
		Last24h: window(d1), Last7d: window(d7),
	}
	if old, err := e.st.OldestSample(ctx, store.ResRollup); err == nil && old > 0 {
		s.HistoryStart = tptr(old)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Timelines
// ---------------------------------------------------------------------------

func (e *Engine) timelineView(ctx context.Context, rows []store.TimelineRow) []api.TimelineAttempt {
	names := e.agentNames(ctx)
	out := make([]api.TimelineAttempt, 0, len(rows))
	for _, r := range rows {
		a := api.TimelineAttempt{TaskID: r.TaskID, TaskKey: r.TaskKey, Attempt: r.Attempt.Attempt, AgentID: r.AgentID,
			AgentName: names[r.AgentID], StartedAt: time.UnixMilli(r.StartedAt).UTC(), FinishedAt: tptr(r.Finished),
			Outcome: r.Outcome, Error: r.Error, Score: r.Score}
		out = append(out, a)
	}
	return out
}

// JobTimeline returns every attempt of a job's tasks, by start time.
func (e *Engine) JobTimeline(ctx context.Context, jobID string) ([]api.TimelineAttempt, error) {
	rows, err := e.st.JobAttempts(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return e.timelineView(ctx, rows), nil
}

// MacAttempts returns a Mac's recent attempts, newest first.
func (e *Engine) MacAttempts(ctx context.Context, agentID string, limit int) ([]api.MacAttempt, error) {
	if _, err := e.st.GetAgent(ctx, agentID); err != nil {
		return nil, err
	}
	rows, err := e.st.AgentAttempts(ctx, agentID, limit)
	if err != nil {
		return nil, err
	}
	view := e.timelineView(ctx, rows)
	out := make([]api.MacAttempt, len(rows))
	for i, r := range rows {
		out[i] = api.MacAttempt{TimelineAttempt: view[i], JobID: r.JobID, JobName: r.JobName}
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
