package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/metrics"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/internal/server"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
	"github.com/alee-ibrahim/use-my-mac/pkg/client"
)

// fakeAgentScript is a stand-in agent binary: `version` prints the version,
// `run` prints it too (the fake launchctl records that as "running") or, for
// a broken build, fails so no heartbeat ever comes.
func fakeAgentScript(version string, broken bool) []byte {
	run := "echo " + version
	if broken {
		run = "exit 1"
	}
	return []byte("#!/bin/sh\ncase \"$1\" in\n  version) echo " + version + " ;;\n  run) " + run + " ;;\nesac\n")
}

func releaseArtifacts(t *testing.T, sk *selfupdate.SecretKey, version string, bin []byte) map[string][]byte {
	t.Helper()
	bundle := selfupdate.BundleName("darwin", "arm64")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: bundle + "/umm-agent", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	tw.Write(bin)
	tw.Close()
	zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + selfupdate.BundleFile("darwin", "arm64") + "\n")
	return map[string][]byte{
		selfupdate.SumsFile: sums, selfupdate.SigFile: sk.Sign(sums, selfupdate.TrustedComment(version)),
		selfupdate.BundleFile("darwin", "arm64"): buf.Bytes(),
	}
}

// TestSelfUpdate runs the real router (update policy, check/report, artifact
// relay fetching from a fake release host), the real updater with HTTP
// clients, real exec of the binaries and a fake launchctl that "restarts"
// the agent. A simulated agent heartbeats with whatever version launchctl
// last started.
func TestSelfUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub, sk, _ := selfupdate.GenerateKey()

	// Release host.
	releases := map[string]map[string][]byte{
		"v0.2.0": releaseArtifacts(t, sk, "v0.2.0", fakeAgentScript("v0.2.0", false)),
		"v0.3.0": releaseArtifacts(t, sk, "v0.3.0", fakeAgentScript("v0.3.0", true)),
	}
	var releaseHits atomic.Int32
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releaseHits.Add(1)
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) == 2 {
			if b, ok := releases[parts[0]][parts[1]]; ok {
				w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer rel.Close()

	// Router at v0.2.0, relaying from the release host.
	st, err := store.Open(ctx, "sqlite://"+t.TempDir()+"/umm.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	defer ts.Close()
	blobs, _ := storage.NewLocal(t.TempDir(), ts.URL, []byte("integration-test-secret"))
	cfg := engine.DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.HeartbeatTimeout = 2 * time.Second
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Config: cfg, Metrics: metrics.New(), Logger: logger, Version: "v0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	srv := &server.Server{E: eng, Log: logger, SessionKey: []byte("session-key-session-key-session!"), PollWaitMax: time.Second,
		Updates: server.UpdatesConfig{ReleaseURL: rel.URL}}
	h = srv.Handler()
	go eng.Run(ctx)
	ak, _ := eng.CreateAPIKey(ctx, "admin", api.RoleAdmin)
	admin := client.New(ts.URL, ak.Secret)
	tok, _ := eng.CreateEnrolToken(ctx, "test", time.Hour)
	enr, err := eng.Enrol(ctx, api.EnrolRequest{Token: tok.Token, Name: "studio", Runtimes: []string{"x"}, MaxTasks: 1, Version: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	// The Mac: live binary, state dir, fake launchctl.
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "umm-agent")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, fakeAgentScript("v0.1.0", false), 0o755)
	running := filepath.Join(dir, "running-version")
	os.WriteFile(running, []byte("v0.1.0\n"), 0o644)
	launchctl := filepath.Join(dir, "launchctl")
	os.WriteFile(launchctl, []byte(fmt.Sprintf(`#!/bin/sh
echo "$@" >> %[1]q.log
[ "$1" = kickstart ] && [ "$2" = -k ] && [ "$3" = system/test.umm-agent ] || exit 3
if %[2]q run > %[1]q.tmp 2>/dev/null; then mv %[1]q.tmp %[1]q; else rm -f %[1]q %[1]q.tmp; fi
`, running, bin)), 0o755)

	// Simulated agent: heartbeats with the version launchctl last started.
	agentHTTP := &selfupdate.HTTPRouter{Server: ts.URL, Credential: enr.Credential}
	go func() {
		for ctx.Err() == nil {
			if b, err := os.ReadFile(running); err == nil {
				hb := api.Heartbeat{State: api.AgentReady, Version: strings.TrimSpace(string(b)), Running: []string{}, MaxTasks: 1}
				if _, err := eng.Heartbeat(ctx, enr.AgentID, hb); err != nil {
					t.Log(err)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	u := &selfupdate.Updater{
		Router: agentHTTP, Sources: []selfupdate.Source{agentHTTP.RelaySource(), selfupdate.DirectSource(rel.URL, nil)},
		Service: selfupdate.Launchctl{Path: launchctl, Label: "test.umm-agent"}, PublicKey: pub,
		BinPath: bin, StateDir: filepath.Join(dir, "updater"), GOOS: "darwin", GOARCH: "arm64",
		VerifyTimeout: 3 * time.Second, PollEvery: 100 * time.Millisecond, DrainPoll: 100 * time.Millisecond, Log: logger,
	}
	waitVersion := func(v string) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := agentHTTP.Check(ctx); err == nil && c.Agent.Version == v {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("agent never reported %s", v)
	}
	waitVersion("v0.1.0")

	// 1. Default target = router version: v0.1.0 -> v0.2.0 through the relay.
	res, err := u.RunOnce(ctx)
	if err != nil || res.Action != "installed" || res.To != "v0.2.0" {
		t.Fatalf("update: %+v %v", res, err)
	}
	if v, _ := selfupdate.ExecVersion(ctx, bin); v != "v0.2.0" {
		t.Fatalf("installed %q", v)
	}
	if releaseHits.Load() != 3 {
		t.Fatalf("release host hit %d times, want 3 (relay fetch + cache)", releaseHits.Load())
	}
	// The relay serves the second request from its cache.
	if _, err := agentHTTP.RelaySource().Fetch(ctx, "v0.2.0", selfupdate.SumsFile); err != nil || releaseHits.Load() != 3 {
		t.Fatalf("relay cache: %v hits %d", err, releaseHits.Load())
	}
	// Not a release artifact: refused by the relay.
	if _, err := agentHTTP.RelaySource().Fetch(ctx, "v0.2.0", "umm-server-linux-amd64.tar.gz"); err == nil {
		t.Fatal("relay served an unexpected file")
	}

	macs, err := admin.Macs(ctx)
	if err != nil || len(macs) != 1 || macs[0].Version != "v0.2.0" || macs[0].Update == nil ||
		macs[0].Update.State != api.UpdateSucceeded || macs[0].Update.Desired != "v0.2.0" {
		t.Fatalf("dashboard view: %+v %v", macs, err)
	}

	// 2. Canary pinned to a broken v0.3.0: rolled back to v0.2.0.
	if err := eng.SetUpdateSettings(ctx, api.UpdateSettings{CanaryVersion: "v0.3.0", CanaryMacs: []string{enr.AgentID}}); err != nil {
		t.Fatal(err)
	}
	res, err = u.RunOnce(ctx)
	if err != nil || res.Action != "rolled_back" {
		t.Fatalf("broken update: %+v %v", res, err)
	}
	if v, _ := selfupdate.ExecVersion(ctx, bin); v != "v0.2.0" {
		t.Fatalf("after rollback installed %q", v)
	}
	waitVersion("v0.2.0")

	evs, err := eng.UpdateEvents(ctx, enr.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for i := len(evs) - 1; i >= 0; i-- {
		states = append(states, evs[i].State)
	}
	want := "downloading,applying,succeeded,downloading,applying,rolled_back"
	if strings.Join(states, ",") != want {
		t.Fatalf("history %v, want %s", states, want)
	}
	logb, _ := os.ReadFile(running + ".log")
	if n := strings.Count(string(logb), "kickstart -k system/test.umm-agent"); n != 3 {
		t.Fatalf("launchctl calls:\n%s", logb)
	}

	// 3. Paused: nothing happens, even with a newer target.
	eng.SetUpdateSettings(ctx, api.UpdateSettings{Paused: true, TargetVersion: "v0.3.0"})
	if res, _ := u.RunOnce(ctx); res.Action != "nothing" {
		t.Fatalf("paused: %+v", res)
	}
}
