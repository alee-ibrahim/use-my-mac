// Package agent is the Mac-side worker. It only ever connects outbound to
// the router: it sends heartbeats with telemetry, long-polls for
// assignments, runs allowlisted native processes at low priority in a
// private work dir, streams logs, uploads outputs to short-lived URLs, and
// yields (stops work promptly) when the Mac's own user needs the machine.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Version is set at build time.
var Version = "dev"

// Agent runs on one Mac.
type Agent struct {
	cfg       *Config
	cred      string
	collector telemetry.Collector
	log       *slog.Logger
	http      *http.Client // for the router API (bearer)
	blobHTTP  *http.Client // for signed/presigned URLs (no bearer)
	goos      string

	mu     sync.Mutex
	yield  *Yield
	state  string
	reason string
	drain  bool
	// drainReason is the router's reason for a drain ("" = admin).
	drainReason string
	running     map[string]*run
	capacity    chan struct{}
	// lastTel is the latest telemetry (for adaptive priority).
	lastTel api.Telemetry
	haveTel bool
	// applyQoS moves a task's process group into or out of background QoS.
	applyQoS func(pgid int, background bool) error

	// Runtime preflight (see preflight.go). runtimes is the allowlist
	// (replaced on reload); avail/unavail are the last preflight's result.
	runtimes      map[string][]string
	avail         []string
	unavail       map[string]api.UnavailableRuntime
	faults        map[string]execFault
	preflightID   int64
	lastPreflight time.Time
}

type run struct {
	as     api.Assignment
	cancel context.CancelCauseFunc
	pgid   int
	qos    string // effective QoS under adaptive priority
}

// Stop causes.
var (
	errYield  = errors.New("yielded to the Mac's user")
	errCancel = errors.New("cancelled by router")
	errPause  = errors.New("paused on this Mac")
	errStop   = errors.New("agent shutting down")
)

// New creates an agent. collector may be nil to use the platform default.
func New(cfg *Config, cred string, collector telemetry.Collector, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	a := &Agent{
		cfg: cfg, cred: cred, log: log, goos: runtime.GOOS,
		http:     &http.Client{Timeout: cfg.PollWait + 30*time.Second},
		blobHTTP: &http.Client{},
		yield:    NewYield(cfg.Yield),
		state:    api.AgentReady,
		running:  map[string]*run{},
		capacity: make(chan struct{}, 1),
		applyQoS: setGroupBackground,
		unavail:  map[string]api.UnavailableRuntime{},
		faults:   map[string]execFault{},
	}
	a.ReloadRuntimes(cfg.Runtimes)
	if collector == nil {
		collector = telemetry.NewDefault(telemetry.Options{
			WorkDir: cfg.WorkDir, UserLoadScope: cfg.UserLoadScope, OurUID: os.Getuid(),
			OwnProcessGroups: a.ownGroups,
		})
	}
	a.collector = collector
	return a
}

func (a *Agent) ownGroups() map[int]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := map[int]bool{}
	for _, r := range a.running {
		if r.pgid > 0 {
			m[r.pgid] = true
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// HTTP to the router
// ---------------------------------------------------------------------------

func (a *Agent) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cred)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Enrol exchanges a one-time token for a credential. The token must come
// from the environment or stdin, never argv.
func Enrol(ctx context.Context, cfg *Config, token string) (*api.EnrolResponse, error) {
	// Only runtimes that pass the preflight; heartbeats keep this current.
	rts := []string{}
	for _, n := range cfg.RuntimeNames() {
		if CheckProgram(cfg.Runtimes[n][0]) == "" {
			rts = append(rts, n)
		}
	}
	req := api.EnrolRequest{Token: token, Name: cfg.Name, Runtimes: rts, Labels: cfg.Labels,
		MaxTasks: cfg.MaxTasks, Version: Version}
	b, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+"/v1/agent/enrol", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("enrol failed: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out api.EnrolResponse
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// ---------------------------------------------------------------------------
// Main loops
// ---------------------------------------------------------------------------

// Run blocks until ctx is cancelled. Running tasks are stopped and reported
// as cancelled on shutdown so the router can reassign them immediately.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.cfg.WorkDir, 0o700); err != nil {
		return err
	}
	a.cleanStaleWorkDirs()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	<-ctx.Done()
	a.stopAll(errStop)
	wg.Wait()
	a.waitIdle(a.cfg.StopGrace + 5*time.Second)
	return nil
}

func (a *Agent) cleanStaleWorkDirs() {
	entries, _ := os.ReadDir(a.cfg.WorkDir)
	for _, e := range entries {
		if e.IsDir() {
			_ = os.RemoveAll(filepath.Join(a.cfg.WorkDir, e.Name()))
		}
	}
}

func (a *Agent) waitIdle(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		n := len(a.running)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *Agent) signalCapacity() {
	select {
	case a.capacity <- struct{}{}:
	default:
	}
}

func (a *Agent) stopAll(cause error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.running {
		r.cancel(cause)
	}
}

// heartbeatOnce collects telemetry, advances the yield state machine and
// reports to the router.
func (a *Agent) heartbeatOnce(ctx context.Context) {
	tel, err := a.collector.Collect(ctx)
	if err != nil {
		a.log.Warn("telemetry", "err", err)
	}
	now := time.Now()
	a.mu.Lock()
	if now.Sub(a.lastPreflight) >= a.cfg.PreflightInterval {
		a.preflightLocked(now)
	}
	avail, unavail := a.runtimeStatusLocked()
	preflightID := a.preflightID
	a.lastTel, a.haveTel = tel, true
	state, changed := a.yield.Update(now, tel)
	_, reason := a.yield.State()
	paused := false
	if a.cfg.PauseFile != "" {
		if _, err := os.Stat(a.cfg.PauseFile); err == nil {
			paused = true
		}
	}
	if paused {
		state, reason = api.AgentDraining, "paused locally"
	} else if a.drain && state == api.AgentReady {
		state, reason = api.AgentDraining, "drained by admin"
		if a.drainReason != "" {
			reason = a.drainReason
		}
	}
	prev := a.state
	a.state, a.reason = state, reason
	var running []string
	for id := range a.running {
		running = append(running, id)
	}
	a.mu.Unlock()

	if changed && state == api.AgentBusyUser {
		a.log.Info("yielding to the Mac's user", "reason", reason)
		a.stopAll(errYield)
	}
	if paused && prev != api.AgentDraining {
		a.log.Info("paused locally; stopping tasks")
		a.stopAll(errPause)
	}
	if state == api.AgentReady && prev != api.AgentReady {
		a.signalCapacity()
	}
	a.adjustQoS()

	hb := api.Heartbeat{State: state, Reason: reason, Telemetry: tel, Running: nonNilS(running),
		Runtimes: avail, UnavailableRuntimes: unavail, PreflightID: preflightID,
		Labels: a.cfg.Labels, MaxTasks: a.cfg.MaxTasks, Version: Version}
	var resp api.HeartbeatResponse
	if err := a.call(ctx, http.MethodPost, "/v1/agent/heartbeat", hb, &resp); err != nil {
		if ctx.Err() == nil {
			a.log.Warn("heartbeat failed", "err", err)
		}
		return
	}
	a.mu.Lock()
	wasDrain := a.drain
	a.drain, a.drainReason = resp.Drain, resp.DrainReason
	for _, id := range resp.Cancel {
		if r := a.running[id]; r != nil {
			a.log.Info("router cancelled task", "task", id)
			r.cancel(errCancel)
		}
	}
	a.mu.Unlock()
	if wasDrain && !resp.Drain {
		a.signalCapacity()
	}
}

// currentQoS is the QoS a task started now should run at.
func (a *Agent) currentQoS() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.qosLocked()
}

func (a *Agent) qosLocked() string {
	if a.cfg.Priority != PriorityAdaptive {
		return a.cfg.Priority
	}
	if !a.haveTel {
		return QoSBackground // assume someone is there until we know
	}
	return AdaptiveQoS(a.lastTel, a.cfg.PriorityIdle)
}

// adjustQoS demotes running tasks to background QoS when someone starts
// using the Mac, and promotes them back to utility when they leave. Tasks
// keep running either way; stopping them is the yield policy's job. While at
// background it is re-applied each heartbeat so children forked in between
// are covered too.
func (a *Agent) adjustQoS() {
	if a.cfg.Priority != PriorityAdaptive {
		return
	}
	type change struct {
		r    *run
		from string
	}
	a.mu.Lock()
	want := a.qosLocked()
	var todo []change
	for _, r := range a.running {
		if r.pgid > 0 && (r.qos != want || want == QoSBackground) {
			todo = append(todo, change{r, r.qos})
		}
	}
	a.mu.Unlock()
	for _, c := range todo {
		if err := a.applyQoS(c.r.pgid, want == QoSBackground); err != nil {
			a.log.Warn("set task priority", "task", c.r.as.TaskID, "qos", want, "err", err)
			continue
		}
		a.mu.Lock()
		c.r.qos = want
		a.mu.Unlock()
		if c.from != want {
			a.log.Info("task priority changed", "task", c.r.as.TaskID, "from", c.from, "to", want)
		}
	}
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		a.heartbeatOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) canTakeWork() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state == api.AgentReady && !a.drain && len(a.running) < a.cfg.MaxTasks
}

func (a *Agent) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if !a.canTakeWork() {
			select {
			case <-ctx.Done():
				return
			case <-a.capacity:
			case <-time.After(a.cfg.HeartbeatInterval):
			}
			continue
		}
		var resp api.PollResponse
		q := url.Values{"wait": {a.cfg.PollWait.String()}}
		if err := a.call(ctx, http.MethodGet, "/v1/agent/poll?"+q.Encode(), nil, &resp); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("poll failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, as := range resp.Assignments {
			a.start(ctx, as)
		}
	}
}

// ---------------------------------------------------------------------------
// Running a task
// ---------------------------------------------------------------------------

func (a *Agent) start(parent context.Context, as api.Assignment) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	r := &run{as: as, cancel: cancel}
	a.mu.Lock()
	a.running[as.TaskID] = r
	a.mu.Unlock()
	a.log.Info("task started", "task", as.TaskID, "key", as.Key, "attempt", as.Attempt, "runtime", as.Runtime)
	go func() {
		defer func() {
			cancel(nil)
			a.mu.Lock()
			delete(a.running, as.TaskID)
			a.mu.Unlock()
			a.signalCapacity()
		}()
		upd := a.execute(ctx, r)
		a.log.Info("task finished", "task", as.TaskID, "outcome", upd.Outcome, "error", upd.Error)
		a.report(as.TaskID, upd)
	}()
}

// report sends the final status with a few retries (it matters).
func (a *Agent) report(taskID string, upd api.TaskUpdate) {
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := a.call(ctx, http.MethodPost, "/v1/agent/tasks/"+url.PathEscape(taskID)+"/status", upd, nil)
		cancel()
		if err == nil {
			return
		}
		a.log.Warn("status report failed", "task", taskID, "err", err)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
}

func (a *Agent) execute(ctx context.Context, r *run) api.TaskUpdate {
	as := r.as
	upd := api.TaskUpdate{Attempt: as.Attempt}
	prefix, why := a.runtimeFor(as.Runtime)
	if why != "" {
		// The router placed it before it learned; it is the Mac's fault.
		upd.Outcome, upd.Error, upd.RuntimeUnavailable = api.OutcomeFailed, why, why
		return upd
	}
	workdir := filepath.Join(a.cfg.WorkDir, fmt.Sprintf("%s-%d", as.TaskID, as.Attempt))
	defer os.RemoveAll(workdir)
	for _, d := range []string{"in", "out", "tmp"} {
		if err := os.MkdirAll(filepath.Join(workdir, d), 0o700); err != nil {
			upd.Outcome, upd.Error = api.OutcomeFailed, err.Error()
			return upd
		}
	}
	ins, outs := map[string]bool{}, map[string]bool{}
	for _, in := range as.Inputs {
		ins[in.Name] = true
	}
	for _, o := range as.Outputs {
		outs[o.Name] = true
	}
	args, err := SubstituteArgs(as.Args, workdir, ins, outs)
	if err != nil {
		upd.Outcome, upd.Error = api.OutcomeFailed, err.Error()
		return upd
	}
	env, err := TaskEnv(workdir, as.TaskID, as.Attempt, a.cfg.TaskEnv, as.Env)
	if err != nil {
		upd.Outcome, upd.Error = api.OutcomeFailed, err.Error()
		return upd
	}

	logs := newLogStreamer(a, as.TaskID, as.Attempt, a.cfg.MaxLogBytes)
	defer logs.close()

	// Inputs.
	for _, in := range as.Inputs {
		if err := a.download(ctx, in.URL, filepath.Join(workdir, "in", in.Name)); err != nil {
			return a.stopped(ctx, upd, "download input "+in.Name+": "+err.Error())
		}
	}

	// Run.
	timeout := time.Duration(as.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	defer cancelRun()
	qos := a.currentQoS()
	argv := BuildArgv(a.goos, launchPriority(a.cfg.Priority, qos), a.cfg.Sandbox, workdir, prefix, args)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logs, logs
	cmd.WaitDelay = a.cfg.StopGrace
	setupProc(cmd)
	if err := cmd.Start(); err != nil {
		upd.Outcome, upd.Error = api.OutcomeFailed, "start: "+err.Error()
		if why := execFailureReason(prefix[0], err, 0); why != "" {
			upd.RuntimeUnavailable = why
			a.markExecFault(as.Runtime, why)
		}
		return upd
	}
	a.mu.Lock()
	r.pgid, r.qos = cmd.Process.Pid, qos
	a.mu.Unlock()
	werr := cmd.Wait()
	killGroup(cmd.Process.Pid)
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		upd.ExitCode = &code
	}
	if ctx.Err() != nil {
		return a.stopped(ctx, upd, "")
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		upd.Outcome, upd.Error = api.OutcomeTimeout, fmt.Sprintf("exceeded %s", timeout)
		return upd
	}
	if werr != nil {
		upd.Outcome, upd.Error = api.OutcomeFailed, werr.Error()
		if upd.ExitCode != nil {
			if why := execFailureReason(prefix[0], nil, *upd.ExitCode); why != "" {
				upd.RuntimeUnavailable = why
				a.markExecFault(as.Runtime, why)
			}
		}
		return upd
	}

	// Outputs.
	for _, o := range as.Outputs {
		p := filepath.Join(workdir, "out", o.Name)
		if _, err := os.Stat(p); err != nil {
			if o.Optional {
				continue
			}
			upd.Outcome, upd.Error = api.OutcomeFailed, "output "+o.Name+" was not written"
			return upd
		}
		if err := a.upload(ctx, o.URL, p); err != nil {
			if ctx.Err() != nil {
				return a.stopped(ctx, upd, "")
			}
			upd.Outcome, upd.Error = api.OutcomeFailed, "upload "+o.Name+": "+err.Error()
			return upd
		}
		upd.Outputs = append(upd.Outputs, o.Name)
	}
	upd.Outcome = api.OutcomeSucceeded
	return upd
}

// stopped maps a cancellation cause to an outcome.
func (a *Agent) stopped(ctx context.Context, upd api.TaskUpdate, fallback string) api.TaskUpdate {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errYield):
		a.mu.Lock()
		_, why := a.yield.State()
		a.mu.Unlock()
		upd.Outcome, upd.Error = api.OutcomeYielded, why
	case errors.Is(cause, errCancel), errors.Is(cause, errPause), errors.Is(cause, errStop):
		upd.Outcome, upd.Error = api.OutcomeCancelled, cause.Error()
	case ctx.Err() == nil && fallback != "":
		upd.Outcome, upd.Error = api.OutcomeFailed, fallback
	default:
		upd.Outcome, upd.Error = api.OutcomeCancelled, "stopped"
	}
	return upd
}

func (a *Agent) download(ctx context.Context, u, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.blobHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET: %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (a *Agent) upload(ctx context.Context, u, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	resp, err := a.blobHTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT: %s", resp.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Log streaming
// ---------------------------------------------------------------------------

type logStreamer struct {
	a       *Agent
	taskID  string
	attempt int
	max     int

	mu    sync.Mutex
	buf   bytes.Buffer
	sent  int
	seq   int
	done  chan struct{}
	wg    sync.WaitGroup
	trunc bool
}

func newLogStreamer(a *Agent, taskID string, attempt, max int) *logStreamer {
	l := &logStreamer{a: a, taskID: taskID, attempt: attempt, max: max, done: make(chan struct{})}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-l.done:
				l.flush()
				return
			case <-t.C:
				l.flush()
			}
		}
	}()
	return l
}

func (l *logStreamer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := l.max - l.sent - l.buf.Len()
	if room <= 0 {
		if !l.trunc {
			l.trunc = true
			l.buf.WriteString("\n[agent: log limit reached]\n")
		}
		return len(p), nil
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *logStreamer) flush() {
	l.mu.Lock()
	if l.buf.Len() == 0 {
		l.mu.Unlock()
		return
	}
	data := l.buf.String()
	l.buf.Reset()
	l.sent += len(data)
	l.seq++
	seq := l.seq
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := l.a.call(ctx, http.MethodPost, "/v1/agent/tasks/"+url.PathEscape(l.taskID)+"/logs",
		api.LogChunk{Attempt: l.attempt, Seq: seq, Data: data}, nil)
	if err != nil {
		l.a.log.Debug("log upload failed", "task", l.taskID, "err", err)
	}
}

func (l *logStreamer) close() {
	close(l.done)
	l.wg.Wait()
}
