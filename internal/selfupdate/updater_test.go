package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Fake binaries are "BIN:<version>"; "BIN:<version>:broken" reports its
// version but never heartbeats once started.
func fakeBin(v string) []byte { return []byte("BIN:" + v) }

func fakeVersionOf(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s, ok := strings.CutPrefix(string(b), "BIN:")
	if !ok {
		return "", errors.New("not executable")
	}
	return strings.TrimSuffix(s, ":broken"), nil
}

// fakeRouter simulates the router's view of one Mac. Restarting the service
// "starts" whatever binary is on disk.
type fakeRouter struct {
	mu       sync.Mutex
	now      int64
	check    api.UpdateCheck // policy part
	agent    api.UpdateAgentView
	reports  []api.UpdateReport
	checkErr error
	// runningFor: checks left until the running tasks finish (-1 never).
	runningFor int
}

func (r *fakeRouter) Check(context.Context) (api.UpdateCheck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkErr != nil {
		return api.UpdateCheck{}, r.checkErr
	}
	r.now += 1000
	if r.agent.Running > 0 && r.runningFor >= 0 {
		if r.runningFor == 0 {
			r.agent.Running = 0
		} else {
			r.runningFor--
		}
	}
	c := r.check
	c.Agent, c.NowMS = r.agent, r.now
	return c, nil
}

func (r *fakeRouter) Report(_ context.Context, rep api.UpdateReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, rep)
	return nil
}

func (r *fakeRouter) states() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, x := range r.reports {
		out = append(out, x.State)
	}
	return out
}

type fakeService struct {
	r        *fakeRouter
	bin      string
	restarts int
	// jobs is what Jobs returns; reloads records Reregister calls.
	jobs    []Job
	reloads []string
}

func (s *fakeService) Jobs(context.Context) ([]Job, error) { return append([]Job(nil), s.jobs...), nil }

func (s *fakeService) Reregister(_ context.Context, j Job) error {
	s.reloads = append(s.reloads, j.Role)
	for i := range s.jobs {
		if s.jobs[i].Label == j.Label {
			s.jobs[i].Stale, s.jobs[i].Why = false, ""
		}
	}
	return nil
}

func (s *fakeService) Restart(context.Context) error {
	s.restarts++
	b, _ := os.ReadFile(s.bin)
	v, _ := fakeVersionOf(context.Background(), s.bin)
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if bytes.HasSuffix(b, []byte(":broken")) {
		s.r.agent.Online = false
		return nil
	}
	s.r.now += 500
	s.r.agent = api.UpdateAgentView{Version: v, Online: true, LastSeenMS: s.r.now}
	return nil
}

type harness struct {
	t       *testing.T
	u       *Updater
	r       *fakeRouter
	svc     *fakeService
	rel     map[string]map[string][]byte // version -> artifacts
	clock   time.Time
	fetched []string
}

func newHarness(t *testing.T, installed string) *harness {
	t.Helper()
	pub, sk, _ := GenerateKey()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "umm-agent")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, fakeBin(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, rel: map[string]map[string][]byte{}, clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, v := range []string{"v0.1.0", "v0.2.0", "v0.3.0"} {
		h.rel[v] = testRelease(t, sk, v, "darwin", "arm64", fakeBin(v))
	}
	h.rel["v0.9.0"] = testRelease(t, sk, "v0.9.0", "darwin", "arm64", fakeBin("v0.9.0:broken"))
	h.r = &fakeRouter{now: 1_000_000, agent: api.UpdateAgentView{Version: installed, Online: true, LastSeenMS: 1_000_000}}
	h.svc = &fakeService{r: h.r, bin: bin}
	src := func(name string, relay bool) Source {
		return Source{Name: name, RelayOnly: relay, Fetch: func(_ context.Context, v, f string) ([]byte, error) {
			h.fetched = append(h.fetched, name+":"+f)
			b, ok := h.rel[v][f]
			if !ok {
				return nil, errors.New("404")
			}
			return b, nil
		}}
	}
	h.u = &Updater{
		Router: h.r, Sources: []Source{src("router", true), src("direct", false)}, Service: h.svc, PublicKey: pub,
		BinPath: bin, StateDir: filepath.Join(dir, "state"), GOOS: "darwin", GOARCH: "arm64",
		VersionOf: fakeVersionOf, VerifyTimeout: 30 * time.Second, PollEvery: 5 * time.Second, DrainPoll: 15 * time.Second,
		Now: func() time.Time { return h.clock }, Sleep: func(_ context.Context, d time.Duration) { h.clock = h.clock.Add(d) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

func (h *harness) target(v string, explicit bool) {
	h.r.check = api.UpdateCheck{TargetVersion: v, AllowDowngrade: explicit, Relay: true, DrainTimeoutSeconds: 600}
}

func (h *harness) run() Result {
	h.t.Helper()
	res, err := h.u.RunOnce(context.Background())
	if err != nil && res.Action != "failed" {
		h.t.Fatalf("RunOnce: %v", err)
	}
	return res
}

func (h *harness) installed() string {
	v, _ := fakeVersionOf(context.Background(), h.u.BinPath)
	return v
}

func TestUpdaterInstalls(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	res := h.run()
	if res.Action != "installed" || h.installed() != "v0.2.0" {
		t.Fatalf("got %+v, installed %s", res, h.installed())
	}
	if got := strings.Join(h.r.states(), ","); got != "downloading,applying,succeeded" {
		t.Fatalf("reports %s", got)
	}
	prev, _ := os.ReadFile(h.u.previousPath())
	if string(prev) != "BIN:v0.1.0" {
		t.Fatalf("previous binary not kept: %q", prev)
	}
	if !strings.HasPrefix(h.fetched[0], "router:") {
		t.Fatalf("relay not preferred: %v", h.fetched)
	}
	// Next cycle: up to date, reported once, then quiet.
	h.run()
	h.run()
	if got := strings.Join(h.r.states(), ","); got != "downloading,applying,succeeded,up_to_date" {
		t.Fatalf("reports after idle cycles %s", got)
	}
}

func TestUpdaterRefusesDowngradeAndUnsigned(t *testing.T) {
	h := newHarness(t, "v0.3.0")
	h.target("v0.2.0", false)
	if res := h.run(); res.Action != "refused" || h.installed() != "v0.3.0" {
		t.Fatalf("downgrade not refused: %+v", res)
	}
	h.target("v0.2.0", true)
	if res := h.run(); res.Action != "installed" || h.installed() != "v0.2.0" {
		t.Fatalf("pinned downgrade: %+v", res)
	}

	// Unsigned release: refused from every source, binary untouched.
	h.target("v0.3.0", false)
	delete(h.rel["v0.3.0"], SigFile)
	res := h.run()
	if res.Action != "failed" || h.installed() != "v0.2.0" || !strings.Contains(res.Message, "not signed") {
		t.Fatalf("unsigned: %+v installed %s", res, h.installed())
	}
	// A failed download backs off instead of retrying every cycle.
	n := len(h.r.reports)
	if res := h.run(); res.Action != "skipped" || len(h.r.reports) != n {
		t.Fatalf("no backoff after failed download: %+v", res)
	}
}

func TestUpdaterTamperedBundle(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	// Keep the genuine signed SHA256SUMS, swap the tarball.
	h.rel["v0.2.0"][BundleFile("darwin", "arm64")] = testRelease(t, nil, "v0.2.0", "darwin", "arm64", fakeBin("v0.2.0"))[BundleFile("darwin", "arm64")][:10]
	res := h.run()
	if res.Action != "failed" || !strings.Contains(res.Message, "sha256 mismatch") || h.installed() != "v0.1.0" {
		t.Fatalf("tampered bundle: %+v", res)
	}
	if h.svc.restarts != 0 {
		t.Fatal("restarted for a failed download")
	}
}

func TestUpdaterRollsBack(t *testing.T) {
	h := newHarness(t, "v0.2.0")
	h.target("v0.9.0", false) // its binary never heartbeats
	res := h.run()
	if res.Action != "rolled_back" || h.installed() != "v0.2.0" {
		t.Fatalf("got %+v, installed %s", res, h.installed())
	}
	if h.svc.restarts != 2 {
		t.Fatalf("restarts %d", h.svc.restarts)
	}
	if got := strings.Join(h.r.states(), ","); got != "downloading,applying,rolled_back" {
		t.Fatalf("reports %s", got)
	}
	// Backoff: the next cycle does not retry the bad version.
	if res := h.run(); res.Action != "skipped" {
		t.Fatalf("retried at once: %+v", res)
	}
	h.clock = h.clock.Add(2 * time.Hour)
	if res := h.run(); res.Action != "rolled_back" {
		t.Fatalf("not retried after backoff: %+v", res)
	}
	st := h.u.load()
	if st.Failed["v0.9.0"].Count != 2 || st.Pending != nil {
		t.Fatalf("state %+v", st)
	}
	// A good version still goes through.
	h.target("v0.3.0", false)
	if res := h.run(); res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
}

func TestUpdaterDrains(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.r.agent.Running, h.r.runningFor = 2, 3
	res := h.run()
	if res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
	st := h.r.states()
	if st[1] != "draining" {
		t.Fatalf("no drain: %v", st)
	}

	// Tasks that never finish: restart after the drain timeout.
	h = newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.r.agent.Running, h.r.runningFor = 1, -1
	start := h.clock
	if res := h.run(); res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
	if h.clock.Sub(start) < 10*time.Minute {
		t.Fatalf("did not wait for the drain timeout: %s", h.clock.Sub(start))
	}

	// Target withdrawn while draining: nothing installed.
	h = newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.r.agent.Running, h.r.runningFor = 1, -1
	h.u.Sleep = func(context.Context, time.Duration) { h.r.check.Paused = true }
	if res := h.run(); res.Action != "nothing" || h.installed() != "v0.1.0" {
		t.Fatalf("%+v", res)
	}
}

func TestUpdaterResumesAfterCrash(t *testing.T) {
	// Simulate a crash right after the swap: pending state on disk, new
	// binary in place, agent heartbeating with it.
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.u.defaults()
	if err := ensurePrivateDir(h.u.StateDir); err != nil {
		t.Fatal(err)
	}
	st := h.u.load()
	old, _ := os.ReadFile(h.u.BinPath)
	os.MkdirAll(filepath.Dir(h.u.previousPath()), 0o700)
	os.WriteFile(h.u.previousPath(), old, 0o755)
	newBin := fakeBin("v0.2.0")
	if err := installBinary(h.u.BinPath, newBin); err != nil {
		t.Fatal(err)
	}
	st.Pending = &pendingUpdate{From: "v0.1.0", To: "v0.2.0", SHA256: sha256Hex(newBin), RouterNowMS: h.r.now}
	h.u.save(st)

	// Reboot: the new agent comes up and heartbeats.
	h.svc.Restart(context.Background())
	if res := h.run(); res.Action != "installed" || res.To != "v0.2.0" {
		t.Fatalf("resume: %+v", res)
	}
	if h.u.load().Pending != nil {
		t.Fatal("pending not cleared")
	}

	// Pending update whose agent never comes back: rolled back on resume.
	h2 := newHarness(t, "v0.2.0")
	h2.target("v0.2.0", false)
	old2, _ := os.ReadFile(h2.u.BinPath)
	ensurePrivateDir(h2.u.StateDir)
	os.MkdirAll(filepath.Dir(h2.u.previousPath()), 0o700)
	os.WriteFile(h2.u.previousPath(), old2, 0o755)
	bad := fakeBin("v0.9.0:broken")
	installBinary(h2.u.BinPath, bad)
	h2.svc.Restart(context.Background())
	st2 := h2.u.load()
	st2.Pending = &pendingUpdate{From: "v0.2.0", To: "v0.9.0", SHA256: sha256Hex(bad), RouterNowMS: h2.r.now}
	h2.u.save(st2)
	if res := h2.run(); res.Action != "rolled_back" || h2.installed() != "v0.2.0" {
		t.Fatalf("resume rollback: %+v installed %s", res, h2.installed())
	}

	// Pending update but an admin replaced the binary meanwhile: discarded.
	h3 := newHarness(t, "v0.3.0")
	h3.target("v0.3.0", false)
	st3 := h3.u.load()
	ensurePrivateDir(h3.u.StateDir)
	st3.Pending = &pendingUpdate{From: "v0.1.0", To: "v0.2.0", SHA256: "deadbeef"}
	h3.u.save(st3)
	if res := h3.run(); res.Action != "nothing" || h3.installed() != "v0.3.0" {
		t.Fatalf("stale pending: %+v", res)
	}
}

func TestUpdaterRelayFallback(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.r.check.Relay = false
	if res := h.run(); res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
	for _, f := range h.fetched {
		if strings.HasPrefix(f, "router:") {
			t.Fatalf("used relay although the router does not relay: %v", h.fetched)
		}
	}
	// Relay errors fall back to direct.
	h = newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.u.Sources[0].Fetch = func(context.Context, string, string) ([]byte, error) { return nil, errors.New("502") }
	if res := h.run(); res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
}

func TestUpdaterPausedAndRouterDown(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.r.check.Paused = true
	if res := h.run(); res.Action != "nothing" || h.installed() != "v0.1.0" {
		t.Fatalf("%+v", res)
	}
	if got := h.r.states(); len(got) != 1 || got[0] != "paused" {
		t.Fatalf("%v", got)
	}
	h.r.checkErr = errors.New("connection refused")
	if _, err := h.u.RunOnce(context.Background()); err == nil {
		t.Fatal("router down should be an error")
	}
	if h.installed() != "v0.1.0" {
		t.Fatal("changed binary without the router")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func healthyJobs() []Job {
	return []Job{
		{Role: JobAgent, Label: "l", Loaded: true, Running: true, State: "running"},
		{Role: JobUpdater, Label: "l.updater", Loaded: true, Running: true, State: "running"},
		{Role: JobRuntimes, Label: "l.runtimes", Loaded: true, State: "not running"},
	}
}

func TestUpdaterReregistersJobsAfterSwap(t *testing.T) {
	h := newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.svc.jobs = healthyJobs()
	if res := h.run(); res.Action != "installed" {
		t.Fatalf("%+v", res)
	}
	// The agent through Restart; the helper at once; the updater's own job
	// last, handed to a detached process when the run ends.
	if got := strings.Join(h.svc.reloads, ","); got != "runtimes,updater" {
		t.Fatalf("reloads %s", got)
	}
	if h.svc.restarts != 1 {
		t.Fatalf("restarts %d", h.svc.restarts)
	}
	// A busy runtimes helper is left for the next cycle.
	h = newHarness(t, "v0.1.0")
	h.target("v0.2.0", false)
	h.svc.jobs = healthyJobs()
	h.svc.jobs[2].Running = true
	h.run()
	if got := strings.Join(h.svc.reloads, ","); got != "updater" {
		t.Fatalf("reloads %s", got)
	}
	// Next cycle (the reloaded updater, RunAtLoad): nothing stale, nothing done.
	h.svc.reloads = nil
	if res := h.run(); res.Action != "nothing" || len(h.svc.reloads) != 0 {
		t.Fatalf("%+v reloads %v", res, h.svc.reloads)
	}
}

func TestUpdaterHealsStaleJobs(t *testing.T) {
	h := newHarness(t, "v0.2.0")
	h.target("v0.2.0", false)
	h.svc.jobs = healthyJobs()
	// The state seen in the field: the updater's job pinned to the old
	// cdhash (here it runs, e.g. after a reboot), the helper too.
	h.svc.jobs[1].Stale, h.svc.jobs[1].Why = true, "launchd: needs LWCR update"
	h.svc.jobs[2].Stale, h.svc.jobs[2].Why = true, "launch constraint pins another cdhash"
	// The agent's job too, but it is running tasks: left alone for now.
	h.svc.jobs[0].Stale, h.svc.jobs[0].Why = true, "launchd: needs LWCR update"
	h.r.agent.Running, h.r.runningFor = 1, -1
	if res := h.run(); res.Action != "nothing" {
		t.Fatalf("%+v", res)
	}
	if got := strings.Join(h.svc.reloads, ","); got != "runtimes,updater" {
		t.Fatalf("reloads %s", got)
	}
	st := h.r.states()
	if len(st) == 0 || st[0] != api.UpdateRepaired || !strings.Contains(h.r.reports[0].Message, "l.updater") {
		t.Fatalf("reports %v %+v", st, h.r.reports)
	}
	// Tasks done: the agent's job is reloaded on the next cycle.
	h.r.agent.Running = 0
	h.svc.reloads = nil
	h.run()
	if got := strings.Join(h.svc.reloads, ","); got != "agent" {
		t.Fatalf("reloads %s", got)
	}
	// A job that stays stale is not reloaded again within the hour.
	h.svc.jobs[1].Stale = true
	h.svc.reloads = nil
	h.run()
	if len(h.svc.reloads) != 0 {
		t.Fatalf("reloaded again at once: %v", h.svc.reloads)
	}
	h.clock = h.clock.Add(2 * time.Hour)
	h.run()
	if got := strings.Join(h.svc.reloads, ","); got != "updater" {
		t.Fatalf("reloads after an hour %s", got)
	}
	// An agent job that is not loaded (failed bootstrap) is loaded again.
	h.svc.jobs[0] = Job{Role: JobAgent, Label: "l", Stale: true, Why: "not loaded"}
	h.svc.reloads = nil
	h.run()
	if got := strings.Join(h.svc.reloads, ","); got != "agent" {
		t.Fatalf("reloads %s", got)
	}
	// Dry runs change nothing.
	h.svc.jobs[2].Stale = true
	h.clock = h.clock.Add(2 * time.Hour)
	h.svc.reloads = nil
	h.u.DryRun = true
	h.run()
	if len(h.svc.reloads) != 0 {
		t.Fatalf("dry run reloaded %v", h.svc.reloads)
	}
}
