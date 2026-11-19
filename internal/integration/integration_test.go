// Package integration runs the real router (engine + HTTP server + SQLite +
// local storage) with real agents that execute real processes. Only the
// telemetry is simulated, so tests can make "the Mac's user" go heavy.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/metrics"
	"github.com/alee-ibrahim/use-my-mac/internal/server"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// TestMain doubles as the "workload": when GO_HELPER_MODE is set the test
// binary behaves like a small CLI tool that agents run as a native process.
func TestMain(m *testing.M) {
	if os.Getenv("GO_HELPER_MODE") == "1" {
		os.Exit(helper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helper(args []string) int {
	switch args[0] {
	case "work": // work <seconds> <out> <content>
		secs, _ := strconv.ParseFloat(args[1], 64)
		fmt.Println("working on", os.Getenv("AGENT_NAME"), "for", secs, "s")
		deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if err := os.WriteFile(args[2], []byte(args[3]), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("done")
		return 0
	case "concat": // concat <out> <in...>
		var buf bytes.Buffer
		for _, p := range args[2:] {
			b, err := os.ReadFile(p)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			buf.Write(b)
		}
		fmt.Println("concatenated", len(args)-2, "parts")
		return writeOut(args[1], buf.Bytes())
	case "exit127": // exit127 <bad-agent> <out>: like a launcher whose program is missing
		if os.Getenv("AGENT_NAME") == args[1] {
			fmt.Fprintln(os.Stderr, "nice: /opt/homebrew/bin/tool: No such file or directory")
			return 127
		}
		return writeOut(args[2], []byte("ok from "+os.Getenv("AGENT_NAME")))
	case "big": // big <in> <out>: reports progress, writes in reversed
		b, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("UMM_PROGRESS=40")
		time.Sleep(1500 * time.Millisecond)
		os.WriteFile(os.Getenv("UMM_PROGRESS_FILE"), []byte("80"), 0o600)
		time.Sleep(2500 * time.Millisecond) // the agent reads the file every 0.5 s and reports every 1 s
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
		return writeOut(args[2], b)
	case "which": // which <program> <out>: where PATH finds a program
		p, err := exec.LookPath(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return writeOut(args[2], []byte(p))
	case "flaky": // flaky <bad-agent> <out>
		if os.Getenv("AGENT_NAME") == args[1] {
			fmt.Fprintln(os.Stderr, "simulated crash on", args[1])
			return 3
		}
		return writeOut(args[2], []byte("ok from "+os.Getenv("AGENT_NAME")))
	}
	return 2
}

func writeOut(p string, b []byte) int {
	if err := os.WriteFile(p, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------

type env struct {
	t      *testing.T
	ts     *httptest.Server
	eng    *engine.Engine
	app    *client.Client
	admin  *client.Client
	cancel context.CancelFunc
	ctx    context.Context
}

type fakeMac struct {
	name string
	tel  atomic.Pointer[api.Telemetry]
	id   string
}

func (f *fakeMac) set(mut func(*api.Telemetry)) {
	t := *f.tel.Load()
	mut(&t)
	f.tel.Store(&t)
}

func quietTel() api.Telemetry {
	return api.Telemetry{CPUCount: 10, MemTotalBytes: 32 << 30, MemAvailableBytes: 24 << 30, MemoryPressure: "normal",
		Thermal: "nominal", OnAC: true, BatteryPercent: -1, GPUUtilPercent: 0, DiskFreeBytes: 500 << 30, Chip: "Apple M2 Pro"}
}

func activeTel() api.Telemetry {
	t := quietTel()
	t.ConsoleUser, t.UserIdleSeconds, t.UserCPUPercent = "staff", 3, 8
	return t
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, "sqlite://"+t.TempDir()+"/umm.db")
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	blobs, err := storage.NewLocal(t.TempDir(), ts.URL, []byte("integration-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.HeartbeatTimeout = 2 * time.Second
	cfg.AssignTimeout = 5 * time.Second
	cfg.BackoffBase = 100 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m := metrics.New()
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Config: cfg, Metrics: m, Logger: logger.With("c", "router")})
	if err != nil {
		t.Fatal(err)
	}
	srv := &server.Server{E: eng, Log: logger, Metrics: m, SessionKey: []byte("session-key-session-key-session!"), PollWaitMax: time.Second,
		APIKeyLogin: true}
	h = srv.Handler()
	go eng.Run(ctx)

	ak, err := eng.CreateAPIKey(ctx, "admin", api.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	admin := client.New(ts.URL, ak.Secret)
	appKey, err := admin.CreateAPIKey(ctx, "test-app", api.RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ts: ts, eng: eng, app: client.New(ts.URL, appKey.Secret), admin: admin, cancel: cancel, ctx: ctx}
	t.Cleanup(func() {
		cancel()
		time.Sleep(200 * time.Millisecond)
		ts.Close()
		st.Close()
	})
	return e
}

func (e *env) mac(name string, maxTasks int, tel api.Telemetry) *fakeMac {
	e.t.Helper()
	return e.macWith(name, maxTasks, tel, nil)
}

// macWith starts an agent with extra runtimes besides "helper".
func (e *env) macWith(name string, maxTasks int, tel api.Telemetry, extra map[string][]string) *fakeMac {
	e.t.Helper()
	return e.macCfg(name, maxTasks, tel, extra, nil)
}

// macCfg is macWith plus a hook to adjust the agent config before it starts.
func (e *env) macCfg(name string, maxTasks int, tel api.Telemetry, extra map[string][]string, mut func(*agent.Config)) *fakeMac {
	e.t.Helper()
	f := &fakeMac{name: name}
	f.tel.Store(&tel)
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	rts := map[string][]string{"helper": {exe}}
	for k, v := range extra {
		rts[k] = v
	}
	cfg := &agent.Config{
		Server: e.ts.URL, Name: name, WorkDir: e.t.TempDir(), MaxTasks: maxTasks,
		Runtimes:          rts,
		TaskEnv:           map[string]string{"GO_HELPER_MODE": "1", "AGENT_NAME": name},
		HeartbeatInterval: 100 * time.Millisecond, PollWait: time.Second, StopGrace: 2 * time.Second,
		Priority: "none",
		Transfer: agent.TransferConfig{PartSizeMB: 5},
		Yield: agent.YieldConfig{UserCPUHigh: 50, UserCPULow: 25, SustainSeconds: 0.3, CooldownSeconds: 60,
			MemoryPressure: "critical", Thermal: "critical", YieldOnBattery: true},
	}
	off := false
	cfg.ManagedRuntimes.Enabled = &off
	if mut != nil {
		mut(cfg)
	}
	if err := cfg.Normalize(); err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.admin.CreateEnrolmentToken(e.ctx, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := agent.Enrol(e.ctx, cfg, tok.Token)
	if err != nil {
		e.t.Fatal(err)
	}
	f.id = res.AgentID
	if cfg.CredentialFile != "" { // read by the in-process root helper
		if err := os.WriteFile(cfg.CredentialFile, []byte(res.Credential+"\n"), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	coll := telemetry.Func(func(context.Context) (api.Telemetry, error) {
		t := *f.tel.Load()
		t.CollectedAt = time.Now()
		return t, nil
	})
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("c", "agent:"+name)
	a := agent.New(cfg, res.Credential, coll, lg)
	go a.Run(e.ctx)
	return f
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func names(e *env, macs ...*fakeMac) map[string]string {
	m := map[string]string{}
	for _, f := range macs {
		m[f.id] = f.name
	}
	return m
}

// A user on the chosen Mac starts heavy work mid-task: the agent stops the
// task, reports a yield, and the router finishes it on another Mac.
func TestUserGoesHeavyTaskMovesToAnotherMac(t *testing.T) {
	e := newEnv(t)
	idle := e.mac("idle-mac", 1, quietTel())    // nobody logged in: preferred
	staff := e.mac("staff-mac", 1, activeTel()) // logged-in user, light load
	nm := names(e, idle, staff)
	waitFor(t, "both Macs online", 5*time.Second, func() bool {
		ms, _ := e.admin.Macs(e.ctx)
		n := 0
		for _, m := range ms {
			if m.Online && m.AgentState == api.AgentReady {
				n++
			}
		}
		return n == 2
	})

	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "yield-demo", Tasks: []api.TaskSpec{{
		Key: "encode", Runtime: "helper", Args: []string{"work", "3", "{out:result.txt}", "hello from a Mac"},
		Outputs: []api.Output{{Name: "result.txt"}}, Resources: api.Resources{RAMMB: 512, ExpectedSeconds: 3},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var taskID string
	waitFor(t, "task running on idle-mac", 5*time.Second, func() bool {
		j, _ := e.app.Job(e.ctx, job.ID)
		taskID = j.Tasks[0].ID
		return j.Tasks[0].State == api.StateRunning && j.Tasks[0].AgentID == idle.id
	})
	waitFor(t, "task output streaming", 5*time.Second, func() bool {
		l, _ := e.app.Logs(e.ctx, taskID, 1)
		return strings.Contains(l, "working on idle-mac")
	})

	t.Logf("user on idle-mac starts heavy work (95%% CPU) mid-task")
	idle.set(func(t *api.Telemetry) { t.ConsoleUser, t.UserIdleSeconds, t.UserCPUPercent = "owner", 0, 95 })

	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != api.StateSucceeded {
		t.Fatalf("job state %s", final.State)
	}
	td, err := e.app.Task(e.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range td.Attempts {
		dur := time.Duration(0)
		if a.FinishedAt != nil {
			dur = a.FinishedAt.Sub(a.StartedAt).Round(10 * time.Millisecond)
		}
		t.Logf("attempt %d on %s: %s after %s (%s)", a.Attempt, nm[a.AgentID], a.Outcome, dur, a.Error)
	}
	if len(td.Attempts) != 2 || td.Attempts[0].AgentID != idle.id || td.Attempts[0].Outcome != api.OutcomeYielded ||
		td.Attempts[1].AgentID != staff.id || td.Attempts[1].Outcome != api.OutcomeSucceeded {
		t.Fatalf("unexpected attempts: %+v", td.Attempts)
	}
	if y := td.Attempts[0]; y.FinishedAt.Sub(y.StartedAt) > 3*time.Second {
		t.Fatalf("yield was not prompt")
	}
	if td.Failures != 0 {
		t.Fatal("yield must not count as a failure")
	}
	var out bytes.Buffer
	if _, err := e.app.DownloadOutput(e.ctx, taskID, "result.txt", &out); err != nil || out.String() != "hello from a Mac" {
		t.Fatalf("output %q %v", out.String(), err)
	}
	ms, _ := e.admin.Macs(e.ctx)
	for _, m := range ms {
		t.Logf("mac %s: state=%s user_cpu=%.0f%%", m.Name, m.AgentState, m.Telemetry.UserCPUPercent)
		if m.ID == idle.id && m.AgentState != api.AgentBusyUser && m.AgentState != api.AgentCooldown {
			t.Fatalf("idle-mac should be yielding, is %s", m.AgentState)
		}
	}
	logs, _ := e.app.Logs(e.ctx, taskID, 0)
	t.Logf("task logs:\n%s", strings.TrimSpace(logs))
}

// A map/reduce job: 4 map tasks spread over two Macs, then a reduce task that
// consumes every map output.
func TestMapReduceJobCompletes(t *testing.T) {
	e := newEnv(t)
	m1 := e.mac("mac-one", 2, quietTel())
	m2 := e.mac("mac-two", 2, quietTel())
	nm := names(e, m1, m2)
	waitFor(t, "Macs online", 5*time.Second, func() bool {
		ms, _ := e.admin.Macs(e.ctx)
		return len(ms) == 2 && ms[0].Online && ms[1].Online
	})
	var tasks []api.TaskSpec
	reduce := api.TaskSpec{Key: "reduce", Runtime: "helper", Args: []string{"concat", "{out:all.txt}"},
		Outputs: []api.Output{{Name: "all.txt"}}}
	var want strings.Builder
	for i := 1; i <= 4; i++ {
		k := fmt.Sprintf("map-%d", i)
		tasks = append(tasks, api.TaskSpec{Key: k, Runtime: "helper",
			Args:    []string{"work", "0.5", "{out:part}", fmt.Sprintf("chunk %d\n", i)},
			Outputs: []api.Output{{Name: "part"}}})
		reduce.Inputs = append(reduce.Inputs, api.Input{Name: k, From: &api.OutputRef{Task: k, Output: "part"}})
		reduce.Args = append(reduce.Args, "{in:"+k+"}")
		want.WriteString(fmt.Sprintf("chunk %d\n", i))
	}
	tasks = append(tasks, reduce)
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "map-reduce", Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded {
		t.Fatalf("job %v %v", final.State, err)
	}
	full, _ := e.app.Job(e.ctx, job.ID)
	used := map[string]int{}
	var reduceID string
	for _, tk := range full.Tasks {
		d, _ := e.app.Task(e.ctx, tk.ID)
		last := d.Attempts[len(d.Attempts)-1]
		used[nm[last.AgentID]]++
		t.Logf("%-7s %s on %s (score %.1f)", tk.Key, tk.State, nm[last.AgentID], last.Score)
		if tk.Key == "reduce" {
			reduceID = tk.ID
		}
	}
	if len(used) != 2 {
		t.Fatalf("work should be spread across both Macs: %v", used)
	}
	var out bytes.Buffer
	if _, err := e.app.DownloadOutput(e.ctx, reduceID, "all.txt", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != want.String() {
		t.Fatalf("reduce output %q want %q", out.String(), want.String())
	}
	t.Logf("reduce output:\n%s", strings.TrimSpace(out.String()))
}

// A task crashes on one Mac: it is retried with backoff on another Mac and
// the failing Mac is excluded for that task.
func TestFailureRetriesOnAnotherMac(t *testing.T) {
	e := newEnv(t)
	bad := e.mac("bad-mac", 1, quietTel())
	good := e.mac("good-mac", 1, activeTel())
	nm := names(e, bad, good)
	waitFor(t, "Macs online", 5*time.Second, func() bool {
		ms, _ := e.admin.Macs(e.ctx)
		return len(ms) == 2 && ms[0].Online && ms[1].Online
	})
	retries := 2
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "helper",
		Args: []string{"flaky", "bad-mac", "{out:o}"}, Outputs: []api.Output{{Name: "o"}}, MaxRetries: &retries}}})
	if err != nil {
		t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded {
		t.Fatalf("job %v %v", final.State, err)
	}
	full, _ := e.app.Job(e.ctx, job.ID)
	d, _ := e.app.Task(e.ctx, full.Tasks[0].ID)
	for _, a := range d.Attempts {
		t.Logf("attempt %d on %s: %s %s", a.Attempt, nm[a.AgentID], a.Outcome, a.Error)
	}
	if len(d.Attempts) != 2 || d.Attempts[0].AgentID != bad.id || d.Attempts[0].Outcome != api.OutcomeFailed ||
		d.Attempts[1].AgentID != good.id {
		t.Fatalf("attempts: %+v", d.Attempts)
	}
	logs, _ := e.app.Logs(e.ctx, d.ID, 1)
	if !strings.Contains(logs, "simulated crash") {
		t.Fatalf("stderr of failed attempt not captured: %q", logs)
	}
	// Metrics are exported.
	resp, err := http.Get(e.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `umm_task_attempt_outcomes_total{outcome="failed"} 1`) {
		t.Fatalf("metrics missing failure count")
	}
}

// A runtime whose program is missing is never advertised, so no task needing
// it lands there; a Mac where the runtime exits 127 loses it and the task
// moves on without using a retry.
func TestRuntimePreflightAndExit127(t *testing.T) {
	e := newEnv(t)
	missing := e.macWith("no-tool-mac", 1, activeTel(), map[string][]string{"tool": {"/nonexistent/bin/tool"}})
	bad := e.mac("exit127-mac", 1, quietTel()) // preferred: nobody logged in
	good := e.mac("good-mac", 1, activeTel())
	nm := names(e, missing, bad, good)
	waitFor(t, "Macs online", 5*time.Second, func() bool {
		ms, _ := e.admin.Macs(e.ctx)
		return len(ms) == 3 && ms[0].Online && ms[1].Online && ms[2].Online
	})
	ms, _ := e.admin.Macs(e.ctx)
	for _, m := range ms {
		if m.ID != missing.id {
			continue
		}
		if strings.Join(m.Runtimes, ",") != "helper" || len(m.UnavailableRuntimes) != 1 ||
			m.UnavailableRuntimes[0].Name != "tool" || !strings.Contains(m.UnavailableRuntimes[0].Reason, "program not found") {
			t.Fatalf("no-tool-mac: runtimes %v unavailable %+v", m.Runtimes, m.UnavailableRuntimes)
		}
	}

	// Nobody can run "tool": the task waits instead of failing on no-tool-mac.
	toolJob, err := e.app.SubmitJob(e.ctx, api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "tool"}}})
	if err != nil {
		t.Fatal(err)
	}

	retries := 0
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "helper",
		Args: []string{"exit127", "exit127-mac", "{out:o}"}, Outputs: []api.Output{{Name: "o"}}, MaxRetries: &retries}}})
	if err != nil {
		t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded {
		t.Fatalf("job %v %v", final.State, err)
	}
	full, _ := e.app.Job(e.ctx, job.ID)
	d, _ := e.app.Task(e.ctx, full.Tasks[0].ID)
	for _, a := range d.Attempts {
		t.Logf("attempt %d on %s: %s %s", a.Attempt, nm[a.AgentID], a.Outcome, a.Error)
	}
	if len(d.Attempts) != 2 || d.Attempts[0].AgentID != bad.id || d.Attempts[0].Outcome != api.OutcomeUnavailable ||
		d.Attempts[1].AgentID == bad.id || d.Failures != 0 {
		t.Fatalf("attempts: %+v failures %d", d.Attempts, d.Failures)
	}
	waitFor(t, "exit127-mac reports helper unavailable", 5*time.Second, func() bool {
		ms, _ := e.admin.Macs(e.ctx)
		for _, m := range ms {
			if m.ID == bad.id {
				return len(m.Runtimes) == 0 && len(m.UnavailableRuntimes) == 1 &&
					m.UnavailableRuntimes[0].Source == api.RuntimeSourceExec
			}
		}
		return false
	})

	tj, _ := e.app.Job(e.ctx, toolJob.ID)
	if tk := tj.Tasks[0]; tk.State != api.StatePending || tk.Attempt != 0 {
		t.Fatalf("tool task must wait unplaced: %s attempt %d", tk.State, tk.Attempt)
	}
}

// App keys cannot reach admin endpoints or other apps' jobs; agents cannot
// use app endpoints.
func TestAuthBoundaries(t *testing.T) {
	e := newEnv(t)
	if _, err := e.app.CreateAPIKey(e.ctx, "x", api.RoleAdmin); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("app key must not create keys: %v", err)
	}
	if _, err := client.New(e.ts.URL, "umm_key_bogus").Macs(e.ctx); err == nil {
		t.Fatal("bogus key accepted")
	}
	other, _ := e.admin.CreateAPIKey(e.ctx, "other-app", api.RoleApp)
	oc := client.New(e.ts.URL, other.Secret)
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "helper"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oc.Job(e.ctx, job.ID); err == nil {
		t.Fatal("other app must not see the job")
	}
	if _, err := e.admin.Job(e.ctx, job.ID); err != nil {
		t.Fatal("admin must see all jobs")
	}
	// Signed blob URL tampering is rejected.
	resp, _ := http.Get(e.ts.URL + storage.SignedPath + "?k=uploads/x&m=GET&e=99999999999&s=00")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered signed url: %d", resp.StatusCode)
	}
	// Agent endpoints need an agent credential.
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/v1/agent/poll", nil)
	req.Header.Set("Authorization", "Bearer "+other.Secret)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("app key on agent endpoint: %d", resp.StatusCode)
	}
	// Cookie sessions need the CSRF header for writes.
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/auth/apikey", strings.NewReader(`{"key":"`+e.admin.APIKey+`"}`))
	resp, _ = http.DefaultClient.Do(req)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "umm_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/v1/api-keys", strings.NewReader(`{"name":"n","role":"app"}`))
	req.AddCookie(cookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie write without CSRF header: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/v1/api-keys", strings.NewReader(`{"name":"n","role":"app"}`))
	req.AddCookie(cookie)
	req.Header.Set("X-UMM-CSRF", "1")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("cookie write with CSRF header: %d", resp.StatusCode)
	}
}
