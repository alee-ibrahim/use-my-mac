package engine

import (
	"context"
	"math"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Live progress. Agents report the phase of a running attempt (downloading
// inputs, running, uploading outputs) and a percentage; the router keeps the
// latest report in memory only (it is cheap to lose: the next report a
// second later restores it) and folds it into task and job views. Every
// change is published to subscribers (the SSE streams).

type taskProg struct {
	jobID string
	p     api.TaskProgress
}

// Overall task percentage bands per phase.
const (
	bandDownload = 5.0  // 0-5: downloading inputs
	bandUpload   = 95.0 // 95-100: uploading outputs
)

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// overall maps a phase and its in-phase percent to the task's 0-100.
func overall(p *api.TaskProgress) float64 {
	frac := -1.0
	if p.Percent >= 0 {
		frac = clampPct(p.Percent) / 100
	} else if p.BytesTotal > 0 {
		frac = math.Min(1, float64(p.BytesDone)/float64(p.BytesTotal))
	}
	switch p.Phase {
	case api.PhaseDownloading:
		return bandDownload * math.Max(frac, 0)
	case api.PhaseRunning:
		return bandDownload + (bandUpload-bandDownload)*math.Max(frac, 0)
	case api.PhaseUploading:
		return bandUpload + (100-bandUpload)*math.Max(frac, 0)
	}
	return 0
}

// phaseOf returns the task's phase and overall percent.
func phaseOf(state string, p *api.TaskProgress) (string, float64) {
	switch {
	case api.IsTerminal(state):
		return api.PhaseDone, 100
	case state == api.StateRunning && p != nil:
		return p.Phase, math.Round(overall(p)*10) / 10
	case state == api.StateRunning, state == api.StateAssigned:
		return api.PhaseStarting, 0
	}
	return api.PhaseQueued, 0
}

func validPhase(p string) bool {
	switch p {
	case api.PhaseDownloading, api.PhaseRunning, api.PhaseUploading:
		return true
	}
	return false
}

// ReportProgress records a live progress report from the agent running a
// task. Reports for attempts that are not current are rejected as stale.
func (e *Engine) ReportProgress(ctx context.Context, agentID, taskID string, p api.TaskProgress) error {
	if !validPhase(p.Phase) {
		return &ValidationError{"unknown phase " + p.Phase}
	}
	if math.IsNaN(p.Percent) || p.Percent > 100 {
		p.Percent = 100
	}
	if p.Percent < 0 {
		p.Percent = -1
	}
	if len(p.Message) > 200 {
		p.Message = p.Message[:200]
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.AgentID != agentID || t.Attempt != p.Attempt || t.State != api.StateRunning {
		return ErrStale
	}
	now := e.now().UTC()
	p.UpdatedAt = &now
	e.progMu.Lock()
	e.prog[taskID] = &taskProg{jobID: t.JobID, p: p}
	e.progMu.Unlock()
	e.notify(t.JobID)
	return nil
}

// dropProgress forgets a task's live progress (its attempt ended).
func (e *Engine) dropProgress(taskID string) {
	e.progMu.Lock()
	delete(e.prog, taskID)
	e.progMu.Unlock()
}

func (e *Engine) progressOf(taskID string, attempt int) *api.TaskProgress {
	e.progMu.Lock()
	defer e.progMu.Unlock()
	if tp := e.prog[taskID]; tp != nil && tp.p.Attempt == attempt {
		p := tp.p
		return &p
	}
	return nil
}

// jobRunningPercents returns the overall percent of every running task of
// a job that has a live report.
func (e *Engine) jobRunning(jobID string) (sum float64, phases map[string]int) {
	e.progMu.Lock()
	defer e.progMu.Unlock()
	phases = map[string]int{}
	for _, tp := range e.prog {
		if tp.jobID == jobID {
			sum += overall(&tp.p)
			phases[tp.p.Phase]++
		}
	}
	return sum, phases
}

// jobProgress computes a job's percent and phase from its task counts and
// the live reports of its running tasks.
func jobProgress(state string, counts map[string]int, runningSum float64, phases map[string]int) (float64, string) {
	total, terminal := 0, 0
	for s, n := range counts {
		total += n
		if api.IsTerminal(s) {
			terminal += n
		}
	}
	if api.IsTerminal(state) {
		return 100, api.PhaseDone
	}
	pct := 0.0
	if total > 0 {
		pct = math.Round((100*float64(terminal)+runningSum)/float64(total)*10) / 10
		pct = math.Min(pct, 99.9) // 100 only once the job has finished
	}
	switch {
	case phases[api.PhaseRunning] > 0:
		return pct, api.PhaseRunning
	case phases[api.PhaseUploading] > 0:
		return pct, api.PhaseUploading
	case phases[api.PhaseDownloading] > 0:
		return pct, api.PhaseDownloading
	case counts[api.StateRunning]+counts[api.StateAssigned] > 0:
		return pct, api.PhaseStarting
	}
	return pct, api.PhaseQueued
}

// ---------------------------------------------------------------------------
// Change notifications
// ---------------------------------------------------------------------------

// Subscribe returns a channel that receives the ID of every job that
// changed (state, tasks or live progress). Slow subscribers miss
// notifications rather than block the engine, so they should also resync
// now and then. Call cancel when done.
func (e *Engine) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 256)
	e.subMu.Lock()
	e.subs[ch] = struct{}{}
	e.subMu.Unlock()
	return ch, func() {
		e.subMu.Lock()
		delete(e.subs, ch)
		e.subMu.Unlock()
	}
}

func (e *Engine) notify(jobID string) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- jobID:
		default:
		}
	}
}

// uploadSweepEvery is how often expired multipart uploads are aborted.
const uploadSweepEvery = 10 * time.Minute
