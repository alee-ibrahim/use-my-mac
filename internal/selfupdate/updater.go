package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// RouterClient is the updater's view of the router.
type RouterClient interface {
	Check(ctx context.Context) (api.UpdateCheck, error)
	Report(ctx context.Context, r api.UpdateReport) error
}

// Source fetches release artifacts (router relay or the release host).
type Source struct {
	Name  string
	Fetch func(ctx context.Context, version, file string) ([]byte, error)
	// RelayOnly sources are used only when the router says it relays.
	RelayOnly bool
}

// Updater performs one update cycle. It runs as root.
type Updater struct {
	Router    RouterClient
	Sources   []Source
	Service   Service
	PublicKey *PublicKey
	// BinPath is the live agent binary (the path in the agent's plist).
	BinPath string
	// StateDir is a root-only directory for staging, the previous binary
	// and the state file.
	StateDir string
	GOOS     string
	GOARCH   string
	// VersionOf runs `<binary> version`.
	VersionOf func(ctx context.Context, path string) (string, error)
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration)
	// VerifyTimeout: how long a new agent has to heartbeat with its version.
	VerifyTimeout time.Duration
	// PollEvery: router check interval while verifying.
	PollEvery time.Duration
	// DrainPoll: router check interval while waiting for tasks to finish.
	DrainPoll time.Duration
	// DryRun downloads and verifies but does not install.
	DryRun bool
	Log    *slog.Logger

	// selfJob is set when this run should reload the updater's own job
	// once it exits (see Service.Reregister).
	selfJob *Job
}

// Result summarises a cycle (for logs and tests).
type Result struct {
	Action  string // nothing, refused, skipped, installed, rolled_back, failed, dry_run
	From    string
	To      string
	Message string
}

type pendingUpdate struct {
	From        string    `json:"from"`
	To          string    `json:"to"`
	SHA256      string    `json:"sha256"`
	RouterNowMS int64     `json:"router_now_ms"`
	AppliedAt   time.Time `json:"applied_at"`
}

type failure struct {
	Count int       `json:"count"`
	At    time.Time `json:"at"`
}

type updaterState struct {
	Pending    *pendingUpdate     `json:"pending,omitempty"`
	Failed     map[string]failure `json:"failed,omitempty"`
	LastReport string             `json:"last_report,omitempty"`
	// Reloaded: when the updater last re-registered each launchd job, so
	// a job whose constraint stays stale is not reloaded every cycle.
	Reloaded map[string]time.Time `json:"reloaded,omitempty"`
}

// reloadEvery limits how often one job is re-registered by the self-heal.
const reloadEvery = time.Hour

func (u *Updater) defaults() {
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Sleep == nil {
		u.Sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if u.VersionOf == nil {
		u.VersionOf = ExecVersion
	}
	if u.VerifyTimeout <= 0 {
		u.VerifyTimeout = 2 * time.Minute
	}
	if u.PollEvery <= 0 {
		u.PollEvery = 5 * time.Second
	}
	if u.DrainPoll <= 0 {
		u.DrainPoll = 15 * time.Second
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
}

// ExecVersion runs `<path> version` with a short timeout.
func ExecVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s version: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (u *Updater) statePath() string    { return filepath.Join(u.StateDir, "state.json") }
func (u *Updater) previousPath() string { return filepath.Join(u.StateDir, "previous", "umm-agent") }
func (u *Updater) stagedPath() string   { return filepath.Join(u.StateDir, "staging", "umm-agent") }

func (u *Updater) load() updaterState {
	var st updaterState
	if b, err := os.ReadFile(u.statePath()); err == nil {
		if json.Unmarshal(b, &st) != nil {
			u.Log.Warn("ignoring unreadable updater state")
			st = updaterState{}
		}
	}
	if st.Failed == nil {
		st.Failed = map[string]failure{}
	}
	if st.Reloaded == nil {
		st.Reloaded = map[string]time.Time{}
	}
	return st
}

func (u *Updater) save(st updaterState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(u.statePath(), b, 0o600)
}

// report sends a report. Steady states (up to date, paused, refused) are sent
// only when they change so a quiet fleet does not fill the history.
func (u *Updater) report(ctx context.Context, st *updaterState, r api.UpdateReport) {
	key := r.State + "|" + r.ToVersion + "|" + r.FromVersion
	steady := r.State == api.UpdateUpToDate || r.State == api.UpdatePaused || r.State == api.UpdateRefused
	if steady && st.LastReport == key {
		return
	}
	if err := u.Router.Report(ctx, r); err != nil {
		u.Log.Warn("update report failed", "state", r.State, "err", err)
		return
	}
	st.LastReport = key
}

// backoff after a failed version: 1h, 2h, 4h ... capped at 24h.
func backoff(n int) time.Duration {
	d := time.Hour
	for i := 1; i < n && d < 24*time.Hour; i++ {
		d *= 2
	}
	return min(d, 24*time.Hour)
}

// RunOnce performs one update cycle.
func (u *Updater) RunOnce(ctx context.Context) (Result, error) {
	u.defaults()
	if err := ensurePrivateDir(u.StateDir); err != nil {
		return Result{Action: "failed"}, err
	}
	st := u.load()
	u.selfJob = nil
	defer func() {
		u.reloadSelf(ctx)
		if err := u.save(st); err != nil {
			u.Log.Warn("save updater state", "err", err)
		}
	}()

	// A previous run applied an update and did not see it through (crash,
	// reboot): finish verifying it, or roll back.
	if st.Pending != nil {
		if res, done := u.resume(ctx, &st); done {
			return res, nil
		}
	}

	check, err := u.Router.Check(ctx)
	if err != nil {
		return Result{Action: "failed"}, fmt.Errorf("router check: %w", err)
	}
	if !u.DryRun {
		u.heal(ctx, &st, check)
	}
	if _, err := os.Stat(u.BinPath); err != nil {
		return Result{Action: "failed"}, fmt.Errorf("agent binary: %w", err)
	}
	current, err := u.VersionOf(ctx, u.BinPath)
	if err != nil {
		return Result{Action: "failed"}, err
	}
	action, why := Decide(current, check)
	switch action {
	case Nothing:
		state := api.UpdateUpToDate
		if check.Paused {
			state = api.UpdatePaused
		}
		if check.TargetVersion != "" || check.Paused {
			u.report(ctx, &st, api.UpdateReport{State: state, FromVersion: current, ToVersion: check.TargetVersion, Message: why})
		}
		return Result{Action: "nothing", From: current, To: check.TargetVersion, Message: why}, nil
	case Refuse:
		u.report(ctx, &st, api.UpdateReport{State: api.UpdateRefused, FromVersion: current, ToVersion: check.TargetVersion, Message: why})
		u.Log.Warn("update refused", "reason", why)
		return Result{Action: "refused", From: current, To: check.TargetVersion, Message: why}, nil
	}
	target := check.TargetVersion
	if f, ok := st.Failed[target]; ok && u.Now().Before(f.At.Add(backoff(f.Count))) {
		msg := fmt.Sprintf("%s failed %d time(s); next try after %s", target, f.Count, f.At.Add(backoff(f.Count)).Format(time.RFC3339))
		return Result{Action: "skipped", From: current, To: target, Message: msg}, nil
	}

	u.Log.Info("update available", "from", current, "to", target, "why", why)
	u.report(ctx, &st, api.UpdateReport{State: api.UpdateDownloading, FromVersion: current, ToVersion: target, Message: why})
	bin, source, err := u.download(ctx, target, check.Relay)
	if err != nil {
		msg := "download/verify: " + err.Error()
		if !u.DryRun {
			// Back off like a rollback, so a bad or missing release is not
			// fetched (and reported) every cycle.
			f := st.Failed[target]
			f.Count++
			f.At = u.Now()
			st.Failed[target] = f
			msg += fmt.Sprintf("; retry after %s", backoff(f.Count))
		}
		u.report(ctx, &st, api.UpdateReport{State: api.UpdateFailed, FromVersion: current, ToVersion: target, Message: msg})
		return Result{Action: "failed", From: current, To: target, Message: msg}, err
	}
	if u.DryRun {
		return Result{Action: "dry_run", From: current, To: target, Message: "verified from " + source + "; not installed (dry run)"}, nil
	}

	// Wait for running tasks to finish (the router drains the Mac meanwhile).
	if check, err = u.drain(ctx, &st, check, current, target); err != nil {
		return Result{Action: "nothing", From: current, To: target, Message: err.Error()}, nil
	}
	return u.apply(ctx, &st, current, target, bin, check.NowMS, source)
}

// download fetches and verifies the bundle, returning the agent binary.
func (u *Updater) download(ctx context.Context, version string, relay bool) ([]byte, string, error) {
	var errs []string
	for _, src := range u.Sources {
		if src.RelayOnly && !relay {
			continue
		}
		bin, err := u.fetchVerified(ctx, src, version)
		if err == nil {
			return bin, src.Name, nil
		}
		u.Log.Warn("update source failed", "source", src.Name, "err", err)
		errs = append(errs, src.Name+": "+err.Error())
	}
	if len(errs) == 0 {
		return nil, "", errors.New("no update source available")
	}
	return nil, "", errors.New(strings.Join(errs, "; "))
}

func (u *Updater) fetchVerified(ctx context.Context, src Source, version string) ([]byte, error) {
	if u.PublicKey == nil {
		return nil, errors.New("no release public key")
	}
	sums, err := src.Fetch(ctx, version, SumsFile)
	if err != nil {
		return nil, err
	}
	sig, err := src.Fetch(ctx, version, SigFile)
	if err != nil {
		return nil, fmt.Errorf("release is not signed (cannot get %s: %w)", SigFile, err)
	}
	table, err := VerifySums(u.PublicKey, version, sums, sig)
	if err != nil {
		return nil, err
	}
	name := BundleFile(u.GOOS, u.GOARCH)
	if _, ok := table[name]; !ok {
		return nil, fmt.Errorf("%s is not in this release", name)
	}
	tgz, err := src.Fetch(ctx, version, name)
	if err != nil {
		return nil, err
	}
	if err := CheckSum(table, name, tgz); err != nil {
		return nil, err
	}
	bin, err := ExtractBinary(tgz, BundleName(u.GOOS, u.GOARCH))
	if err != nil {
		return nil, err
	}
	// Stage it and make sure it is the version we were promised.
	if err := ensurePrivateDir(filepath.Dir(u.stagedPath())); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(u.stagedPath(), bin, 0o755); err != nil {
		return nil, err
	}
	got, err := u.VersionOf(ctx, u.stagedPath())
	if err != nil {
		return nil, fmt.Errorf("staged binary does not run: %w", err)
	}
	if got != version {
		return nil, fmt.Errorf("staged binary reports %q, expected %q", got, version)
	}
	return bin, nil
}

// drain waits until the agent runs no tasks, or the drain timeout passes. It
// returns an error when the update should not go ahead after all.
func (u *Updater) drain(ctx context.Context, st *updaterState, c api.UpdateCheck, current, target string) (api.UpdateCheck, error) {
	timeout := time.Duration(c.DrainTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultDrainTimeoutSeconds * time.Second
	}
	deadline := u.Now().Add(timeout)
	for c.Agent.Online && c.Agent.Running > 0 {
		if !u.Now().Before(deadline) {
			u.Log.Warn("drain timeout; restarting the agent with tasks running (the router requeues them)", "running", c.Agent.Running)
			break
		}
		u.report(ctx, st, api.UpdateReport{State: api.UpdateDraining, FromVersion: current, ToVersion: target,
			Message: fmt.Sprintf("waiting for %d running task(s)", c.Agent.Running)})
		u.Sleep(ctx, u.DrainPoll)
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		next, err := u.Router.Check(ctx)
		if err != nil {
			u.Log.Warn("router check while draining", "err", err)
			continue
		}
		if a, _ := Decide(current, next); a != Install || next.TargetVersion != target {
			return next, errors.New("target changed while draining; not updating")
		}
		c = next
	}
	return c, nil
}

func (u *Updater) apply(ctx context.Context, st *updaterState, current, target string, bin []byte, routerNow int64, source string) (Result, error) {
	fail := func(msg string, err error) (Result, error) {
		u.report(ctx, st, api.UpdateReport{State: api.UpdateFailed, FromVersion: current, ToVersion: target, Message: msg})
		return Result{Action: "failed", From: current, To: target, Message: msg}, err
	}
	// Keep the running binary as the one previous version.
	old, err := os.ReadFile(u.BinPath)
	if err != nil {
		return fail("read current binary", err)
	}
	if err := ensurePrivateDir(filepath.Dir(u.previousPath())); err != nil {
		return fail("previous dir", err)
	}
	if err := writeFileAtomic(u.previousPath(), old, 0o755); err != nil {
		return fail("save previous binary", err)
	}
	sum := sha256.Sum256(bin)
	st.Pending = &pendingUpdate{From: current, To: target, SHA256: hex.EncodeToString(sum[:]), RouterNowMS: routerNow, AppliedAt: u.Now()}
	if err := u.save(*st); err != nil {
		st.Pending = nil
		return fail("save state", err)
	}
	u.report(ctx, st, api.UpdateReport{State: api.UpdateApplying, FromVersion: current, ToVersion: target, Message: "verified from " + source})
	if err := installBinary(u.BinPath, bin); err != nil {
		st.Pending = nil
		return fail("install binary: "+err.Error(), err)
	}
	u.Log.Info("installed new agent binary; re-registering its launchd jobs", "version", target)
	if err := u.Service.Restart(ctx); err != nil {
		u.Log.Warn("restart failed", "err", err)
	}
	u.refreshJobs(ctx, st)
	if u.waitFor(ctx, target, routerNow) {
		st.Pending = nil
		delete(st.Failed, target)
		_ = os.Remove(u.stagedPath())
		msg := fmt.Sprintf("agent heartbeating with %s", target)
		u.report(ctx, st, api.UpdateReport{State: api.UpdateSucceeded, FromVersion: current, ToVersion: target, Message: msg})
		u.Log.Info("update succeeded", "from", current, "to", target)
		return Result{Action: "installed", From: current, To: target, Message: msg}, nil
	}
	return u.rollback(ctx, st, current, target, old)
}

// waitFor waits for a heartbeat with version that the router received after
// sinceMS (router clock).
func (u *Updater) waitFor(ctx context.Context, version string, sinceMS int64) bool {
	deadline := u.Now().Add(u.VerifyTimeout)
	for {
		c, err := u.Router.Check(ctx)
		if err == nil && c.Agent.Version == version && c.Agent.LastSeenMS > sinceMS {
			return true
		}
		if ctx.Err() != nil || !u.Now().Before(deadline) {
			return false
		}
		u.Sleep(ctx, u.PollEvery)
	}
}

func (u *Updater) rollback(ctx context.Context, st *updaterState, from, to string, old []byte) (Result, error) {
	u.Log.Error("new agent did not heartbeat; rolling back", "to", to, "back_to", from)
	f := st.Failed[to]
	f.Count++
	f.At = u.Now()
	st.Failed[to] = f
	st.Pending = nil
	if old == nil {
		b, err := os.ReadFile(u.previousPath())
		if err != nil {
			msg := "rollback impossible, previous binary missing: " + err.Error()
			u.report(ctx, st, api.UpdateReport{State: api.UpdateFailed, FromVersion: from, ToVersion: to, Message: msg})
			return Result{Action: "failed", From: from, To: to, Message: msg}, err
		}
		old = b
	}
	var routerNow int64
	if c, err := u.Router.Check(ctx); err == nil {
		routerNow = c.NowMS
	}
	if err := installBinary(u.BinPath, old); err != nil {
		msg := "rollback install failed: " + err.Error()
		u.report(ctx, st, api.UpdateReport{State: api.UpdateFailed, FromVersion: from, ToVersion: to, Message: msg})
		return Result{Action: "failed", From: from, To: to, Message: msg}, err
	}
	if err := u.Service.Restart(ctx); err != nil {
		u.Log.Warn("restart after rollback failed", "err", err)
	}
	u.refreshJobs(ctx, st)
	back := u.waitFor(ctx, from, routerNow)
	msg := fmt.Sprintf("%s did not heartbeat within %s; restored %s", to, u.VerifyTimeout, from)
	if !back {
		msg += " (the restored agent has not heartbeated yet either)"
	}
	msg += fmt.Sprintf("; retry after %s", backoff(f.Count))
	u.report(ctx, st, api.UpdateReport{State: api.UpdateRolledBack, FromVersion: from, ToVersion: to, Message: msg})
	return Result{Action: "rolled_back", From: from, To: to, Message: msg}, nil
}

// resume handles an update applied by an earlier run that did not finish.
func (u *Updater) resume(ctx context.Context, st *updaterState) (Result, bool) {
	p := st.Pending
	b, err := os.ReadFile(u.BinPath)
	sum := sha256.Sum256(b)
	if err != nil || hex.EncodeToString(sum[:]) != p.SHA256 {
		// Something else (an admin's fleet upgrade) replaced the binary.
		u.Log.Info("discarding unfinished update: binary was replaced since", "to", p.To)
		st.Pending = nil
		return Result{}, false
	}
	u.Log.Info("verifying an update applied by an earlier run", "to", p.To)
	if u.waitFor(ctx, p.To, p.RouterNowMS) {
		st.Pending = nil
		delete(st.Failed, p.To)
		msg := fmt.Sprintf("agent heartbeating with %s", p.To)
		u.report(ctx, st, api.UpdateReport{State: api.UpdateSucceeded, FromVersion: p.From, ToVersion: p.To, Message: msg})
		return Result{Action: "installed", From: p.From, To: p.To, Message: msg}, true
	}
	res, _ := u.rollback(ctx, st, p.From, p.To, nil)
	return res, true
}

// ---------------------------------------------------------------------------
// launchd jobs
// ---------------------------------------------------------------------------

// refreshJobs runs after the binary was replaced (the agent's job is
// re-registered by Restart): it re-registers the runtimes helper (unless a
// pass is running; the next cycle's heal catches it) and schedules the
// reload of the updater's own job for when this run exits. Otherwise
// launchd may keep a launch constraint pinned to the old binary's cdhash
// and refuse to start the updater again.
func (u *Updater) refreshJobs(ctx context.Context, st *updaterState) {
	jobs, err := u.Service.Jobs(ctx)
	if err != nil {
		u.Log.Warn("inspect launchd jobs", "err", err)
		return
	}
	for _, j := range jobs {
		switch {
		case j.Role == JobUpdater && j.Loaded:
			u.selfJob = &j
			st.Reloaded[j.Label] = u.Now()
		case j.Role == JobRuntimes && j.Loaded && !j.Running:
			if err := u.Service.Reregister(ctx, j); err != nil {
				u.Log.Warn("re-register launchd job", "label", j.Label, "err", err)
				continue
			}
			st.Reloaded[j.Label] = u.Now()
		}
	}
}

// heal re-registers jobs whose launch constraint went stale, e.g. after an
// update by a version without refreshJobs, or a binary replaced by hand.
// A job is reloaded at most once an hour; the agent's job is left alone
// while it runs tasks.
func (u *Updater) heal(ctx context.Context, st *updaterState, c api.UpdateCheck) {
	jobs, err := u.Service.Jobs(ctx)
	if err != nil {
		u.Log.Warn("inspect launchd jobs", "err", err)
		return
	}
	var fixed []string
	for _, j := range jobs {
		if !j.Stale {
			continue
		}
		if at, ok := st.Reloaded[j.Label]; ok && u.Now().Sub(at) < reloadEvery {
			u.Log.Warn("launchd job still stale after a recent reload", "label", j.Label, "why", j.Why)
			continue
		}
		switch j.Role {
		case JobAgent:
			if j.Running && c.Agent.Online && c.Agent.Running > 0 {
				u.Log.Info("agent job has a stale launch constraint; reloading it once its tasks finish", "why", j.Why)
				continue
			}
		case JobRuntimes:
			if j.Running {
				continue
			}
		case JobUpdater:
			u.selfJob = &j
			st.Reloaded[j.Label] = u.Now()
			fixed = append(fixed, j.Label+" (after this run; "+j.Why+")")
			continue
		}
		if err := u.Service.Reregister(ctx, j); err != nil {
			u.Log.Warn("re-register launchd job", "label", j.Label, "err", err)
			continue
		}
		st.Reloaded[j.Label] = u.Now()
		fixed = append(fixed, j.Label+" ("+j.Why+")")
	}
	if len(fixed) == 0 {
		return
	}
	msg := "re-registered launchd job(s): " + strings.Join(fixed, "; ")
	u.Log.Info(msg)
	u.report(ctx, st, api.UpdateReport{State: api.UpdateRepaired, FromVersion: c.Agent.Version, Message: msg})
}

// reloadSelf hands the reload of the updater's own job to a detached
// process (it can only happen once this one has exited).
func (u *Updater) reloadSelf(ctx context.Context) {
	if u.selfJob == nil {
		return
	}
	j := *u.selfJob
	u.selfJob = nil
	if err := u.Service.Reregister(ctx, j); err != nil {
		u.Log.Warn("schedule reload of the updater job", "label", j.Label, "err", err)
		return
	}
	u.Log.Info("the updater's launchd job is reloaded after this run (refreshes its launch constraint)", "label", j.Label)
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

// ensurePrivateDir creates dir (0700) and refuses a symlink or, as root, a
// directory not owned by root.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkRootOwned(dir, fi); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// writeFileAtomic writes a temp file in the same directory and renames it.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	chownRoot(tmp)
	return os.Rename(tmp, path)
}

// installBinary atomically replaces the live binary: a running process keeps
// its old inode, launchd starts the new file on restart.
func installBinary(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := writeFileAtomic(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// readAllLimit reads at most MaxArtifactBytes.
func readAllLimit(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxArtifactBytes {
		return nil, errors.New("artifact too large")
	}
	return b, nil
}
