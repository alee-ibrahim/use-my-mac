// Package agent is the Mac-side worker. It only ever connects outbound to
// the router: it sends heartbeats with telemetry, long-polls for
// assignments, runs allowlisted native processes at low priority in a
// private work dir, streams logs, uploads outputs to short-lived URLs, and
// yields (stops work promptly) when the Mac's own user needs the machine.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
	"github.com/alee-ibrahim/use-my-mac/internal/hostprep"
	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/internal/transfer"
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
	// routerMax is the concurrency an admin set on the router (0 = use
	// cfg.MaxTasks); it is capped at cfg.MaxTasksLimit.
	routerMax int
	running   map[string]*run
	capacity  chan struct{}
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

	// Mac health (see health.go).
	health    healthState
	statfs    func(path string) (free, total uint64, err error)
	homeProbe HomeProbe
	isMount   func(dir string) bool
	// Managed runtimes (nil when disabled). managedReady maps commands
	// served by installed packages to their argv; installable and
	// installablePkgs can be installed on demand.
	managed         *runtimes.Manager
	managedReady    map[string][]string
	installable     []string
	installablePkgs []string
	packages        []api.RuntimePackageStatus
	catalogBusy     atomic.Bool
	prewarmBusy     atomic.Bool
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

		statfs:    statfsDefault,
		homeProbe: probeHome,
		isMount:   hostprep.IsMountPoint,
	}
	if cfg.ManagedRuntimes.On() {
		m, err := newManager(cfg, log)
		if err != nil {
			log.Warn("managed runtimes disabled", "err", err)
		} else {
			a.managed = m
		}
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

// ManagedKeys returns the keys managed runtime packages must be signed
// with: managed_runtimes.public_keys, else the release key.
func ManagedKeys(cfg *Config) ([]*selfupdate.PublicKey, error) {
	var keys []*selfupdate.PublicKey
	for _, k := range cfg.ManagedRuntimes.PublicKeys {
		pk, err := selfupdate.ParsePublicKey(k)
		if err != nil {
			return nil, fmt.Errorf("managed_runtimes.public_keys: %w", err)
		}
		keys = append(keys, pk)
	}
	if len(keys) > 0 {
		return keys, nil
	}
	pk, err := selfupdate.ReleasePublicKey()
	if cfg.Update.PublicKey != "" {
		pk, err = selfupdate.ParsePublicKey(cfg.Update.PublicKey)
	}
	if err != nil {
		return nil, fmt.Errorf("release public key: %w", err)
	}
	return []*selfupdate.PublicKey{pk}, nil
}

func newManager(cfg *Config, log *slog.Logger) (*runtimes.Manager, error) {
	keys, err := ManagedKeys(cfg)
	if err != nil {
		return nil, err
	}
	mr := cfg.ManagedRuntimes
	rlog := log.With("component", "runtimes")
	return runtimes.New(runtimes.Options{Role: runtimes.RoleAgent, Dir: mr.Dir, Keys: keys, Allow: mr.Allow,
		RetryAfter: mr.RetryAfter, RequestTimeout: mr.RequestTimeout, InUseFile: mr.InUseFile, Log: rlog,
		Request: func(install, reinstall []string) {
			// The root helper's LaunchDaemon watches this file.
			err := runtimes.WriteRequest(mr.RequestFile, runtimes.Request{Install: install, Reinstall: reinstall,
				At: time.Now().UTC()})
			if err != nil {
				rlog.Warn("could not ask the root helper for runtimes", "file", mr.RequestFile, "err", err)
				return
			}
			rlog.Info("asked the root helper for runtimes", "install", install, "reinstall", reinstall)
			if mr.OnRequest != nil {
				mr.OnRequest()
			}
		}})
}

// NewInstaller returns the root helper's manager: it installs packages
// into the root-owned prefix (see `umm-agent runtimes-sync`).
func NewInstaller(cfg *Config, log *slog.Logger) (*runtimes.Manager, error) {
	keys, err := ManagedKeys(cfg)
	if err != nil {
		return nil, err
	}
	mr := cfg.ManagedRuntimes
	return runtimes.New(runtimes.Options{Role: runtimes.RoleInstaller, Dir: mr.Dir, Keys: keys, Allow: mr.Allow,
		RetryAfter: mr.RetryAfter, InUseFile: mr.InUseFile, Log: log})
}

// SyncRuntimes is one root helper pass: fetch the catalog from the router
// with the agent's credential, then install, reinstall, prewarm and clean
// up as root (runtimes.Manager.Sync). The caller serialises passes.
func SyncRuntimes(ctx context.Context, cfg *Config, log *slog.Logger) error {
	if !cfg.ManagedRuntimes.On() {
		return nil
	}
	cred, err := cfg.LoadCredential()
	if err != nil || cred == "" {
		log.Info("agent not enrolled yet; no runtimes to install")
		return nil
	}
	m, err := NewInstaller(cfg, log)
	if err != nil {
		return err
	}
	var c api.AgentRuntimeCatalog
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	a := &Agent{cfg: cfg, cred: cred, http: &http.Client{}}
	err = a.call(cctx, http.MethodGet, "/v1/agent/runtimes", nil, &c)
	cancel()
	if err != nil {
		return fmt.Errorf("runtime catalog: %w", err)
	}
	return m.Sync(ctx, c, cfg.ManagedRuntimes.RequestFile)
}

// Managed returns the managed runtime manager (nil when disabled).
func (a *Agent) Managed() *runtimes.Manager { return a.managed }

// syncCatalog fetches the router's runtime catalog, loads it, re-runs the
// preflight, removes unused versions and starts prewarming.
func (a *Agent) syncCatalog(ctx context.Context) {
	if a.managed == nil || !a.catalogBusy.CompareAndSwap(false, true) {
		return
	}
	defer a.catalogBusy.Store(false)
	var c api.AgentRuntimeCatalog
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := a.call(cctx, http.MethodGet, "/v1/agent/runtimes", nil, &c)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("fetch runtime catalog", "err", err)
		}
		return
	}
	a.managed.SetCatalog(c)
	a.log.Info("runtime catalog loaded", "revision", c.Revision, "packages", len(c.Packages))
	a.managed.Check()
	a.managed.RequestSync() // root prewarms and cleans up for the new catalog
	a.Preflight()
	a.managed.GC()
	a.startPrewarm(ctx)
}

// startPrewarm installs the catalog's prewarm packages in the background.
func (a *Agent) startPrewarm(ctx context.Context) {
	if a.managed == nil || !a.prewarmBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.prewarmBusy.Store(false)
		a.managed.RequestPrewarm() // the root helper installs them
		a.Preflight()
	}()
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
	periodic := false
	if a.managed != nil {
		a.mu.Lock()
		periodic = now.Sub(a.lastPreflight) >= a.cfg.PreflightInterval
		a.mu.Unlock()
		if periodic {
			a.managed.Check()
		}
	}
	a.mu.Lock()
	full := now.Sub(a.health.checkedAt) >= a.cfg.PreflightInterval
	prevHealth := a.health.reason()
	a.mu.Unlock()
	if why := a.checkHealth(now, full); why != "" {
		a.log.Warn("disk space ran out; stopping tasks", "reason", why)
		a.stopAll(errDiskFull)
	}
	a.mu.Lock()
	health, iso := a.health.reason(), a.health.iso
	if health != prevHealth {
		if health != "" {
			a.log.Warn("Mac unhealthy; taking no new work", "reason", health)
		} else {
			a.log.Info("Mac healthy again")
		}
	}
	if now.Sub(a.lastPreflight) >= a.cfg.PreflightInterval {
		a.preflightLocked(now)
	}
	avail, unavail := a.runtimeStatusLocked()
	preflightID := a.preflightID
	installable := append([]string(nil), a.installable...)
	installablePkgs := append([]string(nil), a.installablePkgs...)
	packages := append([]api.RuntimePackageStatus(nil), a.packages...)
	a.lastTel, a.haveTel = tel, true
	state, changed := a.yield.Update(now, tel)
	_, reason := a.yield.State()
	yieldNow := changed && state == api.AgentBusyUser
	paused := false
	if a.cfg.PauseFile != "" {
		if _, err := os.Stat(a.cfg.PauseFile); err == nil {
			paused = true
		}
	}
	if paused {
		state, reason = api.AgentDraining, "paused locally"
	} else if health != "" {
		state, reason = api.AgentUnhealthy, health
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

	if yieldNow {
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
		Labels: a.cfg.Labels, MaxTasks: a.maxTasks(), MaxTasksLimit: a.cfg.MaxTasksLimit, Version: Version, Isolation: &iso,
		InstallableRuntimes: installable, InstallablePackages: installablePkgs, Packages: packages}
	if a.managed != nil {
		hb.CatalogRevision = a.managed.Revision()
	}
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
	oldMax := a.maxTasksLocked()
	a.routerMax = resp.MaxTasks
	if newMax := a.maxTasksLocked(); newMax != oldMax {
		a.log.Info("concurrency changed by the router", "max_tasks", newMax, "limit", a.cfg.MaxTasksLimit)
		if newMax > oldMax {
			defer a.signalCapacity()
		}
	}
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
	if a.managed != nil {
		if resp.CatalogRevision != a.managed.Revision() {
			go a.syncCatalog(ctx)
		} else if periodic {
			a.startPrewarm(ctx) // retries prewarms after a failure backoff
		}
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
	return a.state == api.AgentReady && !a.drain && len(a.running) < a.maxTasksLocked()
}

// maxTasks is how many tasks the agent runs at once: the router's setting
// capped at max_tasks_limit, else max_tasks from agent.yaml. Lowering it
// never stops running tasks; the agent just takes no more until it is
// below the new value.
func (a *Agent) maxTasks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxTasksLocked()
}

func (a *Agent) maxTasksLocked() int {
	return EffectiveMaxTasks(a.cfg.MaxTasks, a.cfg.MaxTasksLimit, a.routerMax)
}

// EffectiveMaxTasks combines agent.yaml's max_tasks and max_tasks_limit with
// the router's setting (0 = none).
func EffectiveMaxTasks(own, limit, router int) int {
	if router <= 0 {
		return max(own, 1)
	}
	if limit > 0 && router > limit {
		return limit
	}
	return router
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
	if why := a.healthReason(); why != "" {
		// It became unhealthy after the router placed the task.
		upd.Outcome, upd.Error, upd.MacFault = api.OutcomeFailed, "Mac unhealthy: "+why, "Mac unhealthy: "+why
		return upd
	}
	prefix, install, why := a.runtimeFor(as.Runtime)
	if why == "" {
		why = a.requiresOK(as.Requires)
	}
	if why != "" {
		// The router placed it before it learned; it is the Mac's fault.
		upd.Outcome, upd.Error, upd.RuntimeUnavailable = api.OutcomeFailed, why, why
		return upd
	}
	// A mise task brings its own toolchain (see mise.go).
	useMise := as.Runtime == MiseRuntime && a.cfg.Mise.Enabled
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
	if err == nil && useMise {
		err = CheckMiseTaskEnv(as.Env)
	}
	if err != nil {
		upd.Outcome, upd.Error = api.OutcomeFailed, err.Error()
		return upd
	}

	logs := newLogStreamer(a, as.TaskID, as.Attempt, a.cfg.MaxLogBytes)
	defer logs.close()
	prog := newProgressReporter(a, as.TaskID, as.Attempt)
	defer prog.close()

	// Managed runtimes: install what the task needs (a Mac fault if that
	// fails, so the router reassigns the task without counting a failure).
	var pathDirs []string
	if install != "" || len(as.Requires) > 0 {
		logf := func(s string) { fmt.Fprintf(logs, "[agent] %s\n", s) }
		pkgs := append([]string(nil), as.Requires...)
		if install != "" {
			pkgs = append([]string{install}, pkgs...)
		}
		seen := map[string]bool{}
		for _, name := range pkgs {
			if seen[name] {
				continue
			}
			seen[name] = true
			inst, release, err := a.managed.Acquire(ctx, name, logf)
			if err != nil {
				a.Preflight()
				if ctx.Err() != nil {
					return a.stopped(ctx, upd, "")
				}
				why := "managed runtime " + name + ": " + err.Error()
				upd.Outcome, upd.Error, upd.RuntimeUnavailable = api.OutcomeFailed, why, why
				return upd
			}
			defer release()
			if name == install {
				prefix = inst.Argv(as.Runtime)
			}
			pathDirs = append(pathDirs, inst.PathDirs()...)
		}
		a.Preflight()
	}
	if len(pathDirs) > 0 {
		env = prependPath(env, pathDirs)
	}

	// Inputs.
	if len(as.Inputs) > 0 {
		m := transfer.NewMeter(0)
		stop := m.Report(progressInterval/2, prog.bytes)
		for _, in := range as.Inputs {
			if err := a.download(ctx, in.URL, filepath.Join(workdir, "in", in.Name), m); err != nil {
				stop()
				upd = a.stopped(ctx, upd, "download input "+in.Name+": "+err.Error())
				if upd.Outcome == api.OutcomeFailed && isDiskFull(err) {
					upd.MacFault = "work volume full while downloading input " + in.Name
				}
				return upd
			}
		}
		stop()
		prog.flush(true)
	}

	// The attempt's timeout covers installing a mise toolchain too.
	timeout := time.Duration(as.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	deadline := time.Now().Add(timeout)
	parserArgv := prefix
	if useMise {
		mr, fail := a.prepareMise(ctx, r, deadline, workdir, prefix, args, env, logs, upd)
		if fail != nil {
			return *fail
		}
		prefix, args, env, parserArgv = mr.prefix, mr.args, mr.env, []string{mr.prog}
	}
	prog.phase(api.PhaseRunning)
	prog.flush(true)

	// Run.
	runCtx, cancelRun := context.WithDeadline(ctx, deadline)
	defer cancelRun()
	qos := a.currentQoS()
	argv := BuildArgv(a.goos, launchPriority(a.cfg.Priority, qos), prefix, args)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = env
	tap := &progressTap{p: a.cfg.parserFor(as.Runtime, parserArgv), report: prog.percent}
	full := &diskFullTap{}
	out := io.MultiWriter(logs, tap, full)
	cmd.Stdout, cmd.Stderr = out, out
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
	watchCtx, stopWatch := context.WithCancel(runCtx)
	go watchProgressFile(watchCtx, filepath.Join(workdir, ProgressFileName), prog.percent)
	werr := cmd.Wait()
	stopWatch()
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
		if full.seen {
			// The task ran out of work space (quota or disk): the Mac's
			// fault, it may fit elsewhere.
			upd.MacFault = "task ran out of disk space on this Mac (" + werr.Error() + ")"
		}
		if upd.ExitCode != nil && useMise {
			// mise ran the task's own program, which the agent resolved
			// before: exit 127 is the task's, unless mise itself is gone.
			upd.ProgramExit = true
			if why := CheckProgram(prefix[0]); why != "" {
				upd.RuntimeUnavailable = why
				a.markExecFault(as.Runtime, why)
			}
		} else if upd.ExitCode != nil {
			if why := execFailureReason(prefix[0], nil, *upd.ExitCode); why != "" {
				upd.RuntimeUnavailable = why
				a.markExecFault(as.Runtime, why)
			}
		}
		return upd
	}

	// Outputs.
	var sizes []int64
	m := transfer.NewMeter(0)
	for _, o := range as.Outputs {
		st, err := os.Stat(filepath.Join(workdir, "out", o.Name))
		if err != nil {
			if o.Optional {
				sizes = append(sizes, -1)
				continue
			}
			upd.Outcome, upd.Error = api.OutcomeFailed, "output "+o.Name+" was not written"
			return upd
		}
		sizes = append(sizes, st.Size())
		m.AddTotal(st.Size())
	}
	prog.phase(api.PhaseUploading)
	prog.flush(true)
	stop := m.Report(progressInterval/2, prog.bytes)
	defer stop()
	for i, o := range as.Outputs {
		if sizes[i] < 0 {
			continue
		}
		p := filepath.Join(workdir, "out", o.Name)
		// Hash the file while it uploads (a second read, in parallel).
		sum := make(chan hashResult, 1)
		go func() { d, err := fileSHA256(ctx, p); sum <- hashResult{d, err} }()
		err := a.upload(ctx, as, o, p, sizes[i], m)
		h := <-sum
		if err != nil {
			if ctx.Err() != nil {
				return a.stopped(ctx, upd, "")
			}
			upd.Outcome, upd.Error = api.OutcomeFailed, "upload "+o.Name+": "+err.Error()
			return upd
		}
		upd.Outputs = append(upd.Outputs, o.Name)
		if h.err == nil {
			if upd.OutputSHA256 == nil {
				upd.OutputSHA256 = map[string]string{}
			}
			upd.OutputSHA256[o.Name] = h.digest
		} else {
			a.log.Warn("hash output", "task", as.TaskID, "output", o.Name, "err", h.err)
		}
	}
	upd.Outcome = api.OutcomeSucceeded
	return upd
}

// requiresOK checks that every required managed package can be had here.
func (a *Agent) requiresOK(req []string) string {
	if len(req) == 0 {
		return ""
	}
	if a.managed == nil {
		return "managed runtimes are disabled on this Mac (task requires " + strings.Join(req, ", ") + ")"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ok := map[string]bool{}
	for _, n := range a.installablePkgs {
		ok[n] = true
	}
	for _, p := range a.packages {
		if p.State == api.PackageInstalled {
			ok[p.Name] = true
		}
	}
	for _, n := range req {
		if !ok[n] {
			return "required managed runtime " + n + " is not available on this Mac"
		}
	}
	return ""
}

// prependPath puts dirs first on the environment's PATH.
func prependPath(env []string, dirs []string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + strings.Join(append(append([]string(nil), dirs...), v), string(os.PathListSeparator))
			found = true
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
	}
	return out
}

// stopped maps a cancellation cause to an outcome.
func (a *Agent) stopped(ctx context.Context, upd api.TaskUpdate, fallback string) api.TaskUpdate {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errYield):
		a.mu.Lock()
		_, why := a.yield.State()
		a.mu.Unlock()
		upd.Outcome, upd.Error = api.OutcomeYielded, why
	case errors.Is(cause, errDiskFull):
		upd.Outcome, upd.Error, upd.MacFault = api.OutcomeFailed, cause.Error(), cause.Error()
	case errors.Is(cause, errCancel), errors.Is(cause, errPause), errors.Is(cause, errStop):
		upd.Outcome, upd.Error = api.OutcomeCancelled, cause.Error()
	case ctx.Err() == nil && fallback != "":
		upd.Outcome, upd.Error = api.OutcomeFailed, fallback
	default:
		upd.Outcome, upd.Error = api.OutcomeCancelled, "stopped"
	}
	return upd
}

// download fetches an input, in parallel byte ranges when the server
// supports them.
func (a *Agent) download(ctx context.Context, u, dst string, m *transfer.Meter) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = transfer.Download(ctx, a.blobHTTP, u, f, transfer.DownloadOptions{
		Chunk: a.cfg.Transfer.PartSize(), Concurrency: a.cfg.Transfer.Concurrency, Meter: m})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// upload pushes an output: in parallel parts through a multipart upload
// when the router offers one and the file is bigger than one part,
// otherwise with a single PUT to the assigned URL.
func (a *Agent) upload(ctx context.Context, as api.Assignment, o api.AssignedOutput, src string, size int64, m *transfer.Meter) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	partSize := a.cfg.Transfer.PartSize()
	if o.Multipart && size > partSize {
		return a.uploadParts(ctx, as, o.Name, f, size, partSize, m)
	}
	return transfer.Put(ctx, a.blobHTTP, o.URL, nil, transfer.CountingReader(f, m), size)
}

type hashResult struct {
	digest string
	err    error
}

// fileSHA256 returns the hex SHA-256 of a file.
func fileSHA256(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (a *Agent) uploadParts(ctx context.Context, as api.Assignment, name string, f *os.File, size, partSize int64, m *transfer.Meter) error {
	var up api.Upload
	path := "/v1/agent/tasks/" + url.PathEscape(as.TaskID) + "/outputs/" + url.PathEscape(name) + "/upload"
	if err := a.call(ctx, http.MethodPost, path, api.OutputUploadRequest{Attempt: as.Attempt, Size: size, PartSize: partSize}, &up); err != nil {
		return err
	}
	// Part URLs are presigned (S3) or signed by the router and live for
	// hours; the agent never sends its bearer credential with them.
	err := transfer.UploadParts(ctx, f, size, up.PartSize, a.cfg.Transfer.Concurrency, transfer.DefaultRetries, nil, m,
		func(ctx context.Context, n, _ int, r io.Reader, sz int64) error {
			return transfer.Put(ctx, a.blobHTTP, up.URLs[n], nil, r, sz)
		})
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		_ = a.call(cctx, http.MethodDelete, "/v1/agent/uploads/"+url.PathEscape(up.ID), nil, nil)
		cancel()
		return err
	}
	return a.call(ctx, http.MethodPost, "/v1/agent/uploads/"+url.PathEscape(up.ID)+"/complete", nil, nil)
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
