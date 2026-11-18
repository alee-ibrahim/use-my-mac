package integration

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// The one-line installer, end to end on Linux: the real router renders the
// script, `curl ... | sh` runs it against a fake Mac (a temp root plus fake
// uname/id/sw_vers/dscl/install/chown/sudo/scutil/xattr/shasum/launchctl,
// and osascript → node running the same verify.js), the release comes
// through the router's relay from a fake release host, the real umm-agent
// (built for this OS) enrols and the fake launchctl starts it so it really
// heartbeats.

const installVersion = "v9.9.9"

var fakeTools = map[string]string{
	"uname":   `case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; *) /bin/uname "$@" ;; esac`,
	"id":      `if [ "${1:-}" = -u ]; then echo 0; else exec /usr/bin/id "$@"; fi`,
	"sw_vers": `echo 14.5`,
	"dscl":    `echo "dscl $*" >> "$FAKE_STATE/calls"; exit 0`,
	"chown":   `exit 0`,
	"xattr":   `exit 0`,
	"scutil":  `echo "Lab Mac's \"2\" \$(x)"`,
	"install": `skip=0; first=1
for a do
  if [ $first = 1 ]; then set --; first=0; fi
  if [ $skip = 1 ]; then skip=0; continue; fi
  case "$a" in -o|-g) skip=1; continue ;; esac
  set -- "$@" "$a"
done
exec /usr/bin/install "$@"`,
	"sudo":      `[ "$1" = -u ] && shift 2; exec "$@"`,
	"shasum":    `[ "$1" = -a ] && shift 2; exec sha256sum "$@"`,
	"osascript": `[ "$1" = -l ] && shift 2; exec node "$@"`,
	"launchctl": `echo "launchctl $*" >> "$FAKE_STATE/calls"
label() { basename "$1" .plist; }
case "$1" in
bootstrap)
  l="$(label "$3")"
  case "$l" in *.updater) touch "$FAKE_STATE/$l.loaded"; exit 0 ;; esac
  nohup "$FAKE_ROOT/usr/local/bin/umm-agent" run -config "$FAKE_ROOT/usr/local/etc/umm/agent.yaml" >> "$FAKE_STATE/agent.log" 2>&1 < /dev/null &
  echo $! > "$FAKE_STATE/$l.pid" ;;
bootout)
  l="${2#system/}"
  rm -f "$FAKE_STATE/$l.loaded"
  [ -f "$FAKE_STATE/$l.pid" ] && kill "$(cat "$FAKE_STATE/$l.pid")" 2>/dev/null
  rm -f "$FAKE_STATE/$l.pid"; exit 0 ;;
print)
  l="${2#system/}"
  [ -f "$FAKE_STATE/$l.pid" ] || exit 1
  p="$(cat "$FAKE_STATE/$l.pid")"
  [ -r "/proc/$p/stat" ] && ! grep -q '^[0-9]* ([^)]*) Z' "/proc/$p/stat" ;;
esac`,
}

type fakeMacEnv struct {
	root, state, path string
}

func newFakeMacEnv(t *testing.T) *fakeMacEnv {
	t.Helper()
	dir := t.TempDir()
	f := &fakeMacEnv{root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state")}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{f.root, f.state, bin} {
		os.MkdirAll(d, 0o755)
	}
	for name, body := range fakeTools {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	f.path = bin + ":" + os.Getenv("PATH")
	t.Cleanup(func() {
		pids, _ := filepath.Glob(filepath.Join(f.state, "*.pid"))
		for _, p := range pids {
			var pid int
			if b, err := os.ReadFile(p); err == nil {
				fmt.Sscan(string(b), &pid)
				if proc, err := os.FindProcess(pid); err == nil && pid > 0 {
					proc.Kill()
				}
			}
		}
	})
	return f
}

// sh runs `curl -fsSL <url> | sh [args]` on the fake Mac.
func (f *fakeMacEnv) sh(t *testing.T, url string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	line := "curl -fsSL " + url + " | sh"
	if len(args) > 0 {
		line += " -s -- " + strings.Join(args, " ")
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Env = append(os.Environ(), "PATH="+f.path, "FAKE_ROOT="+f.root, "FAKE_STATE="+f.state)
	out, err := cmd.CombinedOutput()
	t.Logf("$ %s\n%s", strings.Replace(line, url, "<url>", 1), out)
	return string(out), err
}

func buildAgent(t *testing.T, version string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "umm-agent")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out,
		"-ldflags", "-X github.com/alee-ibrahim/use-my-mac/internal/agent.Version="+version,
		"github.com/alee-ibrahim/use-my-mac/cmd/umm-agent")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, b)
	}
	b, _ := os.ReadFile(out)
	return b
}

type installEnv struct {
	ts    *httptest.Server
	eng   *engine.Engine
	admin *client.Client
	mac   *fakeMacEnv
}

func newInstallEnv(t *testing.T, tamper bool) *installEnv {
	t.Helper()
	if runtime.GOOS == "windows" || testing.Short() {
		t.Skip("needs /bin/sh; slow")
	}
	for _, tool := range []string{"curl", "node", "sha256sum", "od"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pub, sk, _ := selfupdate.GenerateKey()
	arts := releaseArtifacts(t, sk, installVersion, buildAgent(t, installVersion))
	if tamper {
		// A signed SHA256SUMS changed after signing: must fail closed.
		arts[selfupdate.SumsFile] = append(arts[selfupdate.SumsFile], []byte("0000  extra\n")...)
	}
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := arts[strings.TrimPrefix(r.URL.Path, "/"+installVersion+"/")]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(rel.Close)

	st, err := store.Open(ctx, "sqlite://"+t.TempDir()+"/umm.db")
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	blobs, _ := storage.NewLocal(t.TempDir(), ts.URL, []byte("integration-test-secret"))
	cfg := engine.DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Config: cfg, Metrics: metrics.New(), Logger: logger,
		Version: installVersion})
	if err != nil {
		t.Fatal(err)
	}
	mac := newFakeMacEnv(t)
	keyLines := strings.Split(strings.TrimSpace(pub.String()), "\n")
	srv := &server.Server{E: eng, Log: logger, SessionKey: []byte("session-key-session-key-session!"), PollWaitMax: time.Second,
		Updates: server.UpdatesConfig{ReleaseURL: rel.URL}, PublicURL: ts.URL, ReleaseKey: keyLines[len(keyLines)-1],
		InstallRoot: mac.root,
		Install: server.InstallConfig{AllowHTTP: true, Resolved: map[string]api.InstallProfile{
			"lab": {Name: "lab", Labels: map[string]string{"site": "hq"}, Runtimes: map[string][]string{"sh": {"/bin/sh"}},
				MaxTasks: 1, Priority: "none", ServiceLabel: "com.example.helper"}}}}
	h = srv.Handler()
	go eng.Run(ctx)
	t.Cleanup(func() { cancel(); ts.Close(); st.Close() })
	ak, _ := eng.CreateAPIKey(ctx, "admin", api.RoleAdmin)
	return &installEnv{ts: ts, eng: eng, admin: client.New(ts.URL, ak.Secret), mac: mac}
}

func TestOneLineInstall(t *testing.T) {
	e := newInstallEnv(t, false)
	ctx := context.Background()
	tok, err := e.admin.CreateInstallToken(ctx, api.InstallTokenRequest{Profile: "lab", MaxUses: 2,
		Labels: map[string]string{"room": "r1"}})
	if err != nil {
		t.Fatal(err)
	}
	url := e.ts.URL + "/i/" + tok.Token
	if tok.Command != "curl -fsSL "+url+" | sudo sh" {
		t.Fatalf("command %q", tok.Command)
	}

	// An existing parent that _umm cannot traverse only gets o+x; missing
	// parents are created 0755 even under the script's umask 077.
	os.MkdirAll(e.mac.root+"/usr/local", 0o755)
	os.Mkdir(e.mac.root+"/usr/local/var", 0o700)
	os.Chmod(e.mac.root+"/usr/local/var", 0o700)

	out, err := e.mac.sh(t, url)
	if err != nil || !strings.Contains(out, `is online (agent `+installVersion+`)`) {
		b, _ := os.ReadFile(filepath.Join(e.mac.state, "agent.log"))
		t.Fatalf("install failed: %v\nagent log:\n%s", err, b)
	}
	if strings.Contains(out, tok.Token) {
		t.Error("installer output shows the token")
	}
	macs, _ := e.admin.Macs(ctx)
	if len(macs) != 1 || macs[0].Name != "Lab Macs 2 x" || macs[0].Labels["room"] != "r1" || macs[0].Labels["site"] != "hq" ||
		macs[0].Version != installVersion {
		t.Fatalf("macs %+v", macs)
	}
	// Installed like install.sh: binary, root-only credential, config, both daemons.
	r := e.mac.root
	for _, p := range []string{"/usr/local/bin/umm-agent", "/usr/local/etc/umm/agent.yaml",
		"/Library/LaunchDaemons/com.example.helper.plist", "/Library/LaunchDaemons/com.example.helper.updater.plist"} {
		if _, err := os.Stat(r + p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	for p, want := range map[string]os.FileMode{"/usr/local/etc": 0o755, "/usr/local/var/log": 0o755,
		"/usr/local/bin": 0o755, "/usr/local/var": 0o701} {
		if fi, err := os.Stat(r + p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, fi.Mode().Perm(), err, want)
		}
	}
	if fi, err := os.Stat(r + "/usr/local/etc/umm/credential"); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		t.Errorf("credential file: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(e.mac.state, "com.example.helper.updater.loaded")); err != nil {
		t.Error("updater daemon not loaded")
	}
	cfg, _ := os.ReadFile(r + "/usr/local/etc/umm/agent.yaml")
	if !bytes.Contains(cfg, []byte(`name: "Lab Macs 2 x"`)) || !bytes.Contains(cfg, []byte("room: r1")) {
		t.Errorf("agent.yaml:\n%s", cfg)
	}
	if bytes.Contains(cfg, []byte("umm_ins_")) {
		t.Error("agent.yaml holds the token")
	}

	// Re-running repairs/upgrades without using the token up.
	out, err = e.mac.sh(t, url)
	if err != nil || !strings.Contains(out, "already enrolled") || !strings.Contains(out, "is online") {
		t.Fatalf("re-run: %v", err)
	}
	list, _ := e.admin.InstallTokens(ctx)
	if list[0].Uses != 1 || list[0].State != api.InstallActive {
		t.Fatalf("after re-run: uses %d state %s", list[0].Uses, list[0].State)
	}
	evs, _ := e.admin.InstallTokenEvents(ctx, tok.ID)
	seen := map[string]int{}
	for _, ev := range evs {
		seen[ev.Kind]++
		if ev.Kind != engine.InstallEventCreate && ev.IP == "" {
			t.Errorf("event without IP: %+v", ev)
		}
	}
	if seen["script"] != 2 || seen["download"] != 2 || seen["enrol"] != 1 {
		t.Errorf("audit log %v", seen)
	}

	// Revoked: the script prints a clear error and exits non-zero.
	if err := e.admin.RevokeInstallToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	out, err = e.mac.sh(t, url)
	if err == nil || !strings.Contains(out, "install link is invalid, expired, revoked or used up") {
		t.Fatalf("revoked link: err=%v", err)
	}

	// Uninstall (purge) finds the daemons by what they run and stops the agent.
	out, err = e.mac.sh(t, e.ts.URL+"/i/uninstall", "--purge")
	if err != nil || !strings.Contains(out, "purged") {
		t.Fatalf("uninstall: %v", err)
	}
	for _, p := range []string{"/usr/local/bin/umm-agent", "/usr/local/etc/umm", "/Library/LaunchDaemons/com.example.helper.plist",
		"/Library/LaunchDaemons/com.example.helper.updater.plist"} {
		if _, err := os.Stat(r + p); err == nil {
			t.Errorf("%s still there", p)
		}
	}
	if pids, _ := filepath.Glob(filepath.Join(e.mac.state, "*.pid")); len(pids) != 0 {
		t.Errorf("agent still running: %v", pids)
	}
}

func TestOneLineInstallBadSignature(t *testing.T) {
	e := newInstallEnv(t, true)
	ctx := context.Background()
	tok, err := e.admin.CreateInstallToken(ctx, api.InstallTokenRequest{Profile: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.mac.sh(t, e.ts.URL+"/i/"+tok.Token)
	if err == nil || !strings.Contains(out, "release signature is not valid") {
		t.Fatalf("tampered release: err=%v", err)
	}
	// Fail closed: nothing installed, token not used.
	for _, p := range []string{"/usr/local/bin/umm-agent", "/usr/local/etc/umm", "/Library/LaunchDaemons"} {
		if _, err := os.Stat(e.mac.root + p); err == nil {
			t.Errorf("%s exists after a failed verification", p)
		}
	}
	list, _ := e.admin.InstallTokens(ctx)
	if list[0].Uses != 0 {
		t.Error("token used by a failed install")
	}
	if macs, _ := e.admin.Macs(ctx); len(macs) != 0 {
		t.Error("a Mac enrolled")
	}
	// Unknown tokens get an error script too.
	out, err = e.mac.sh(t, e.ts.URL+"/i/umm_ins_doesnotexistdoesnotexist")
	if err == nil || !strings.Contains(out, "use-my-mac: error:") {
		t.Fatalf("unknown token: err=%v", err)
	}
}

func TestInstallEndpointGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, _ := store.Open(ctx, "sqlite://"+t.TempDir()+"/umm.db")
	defer st.Close()
	blobs, _ := storage.NewLocal(t.TempDir(), "http://x", []byte("integration-test-secret"))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, _ := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Config: engine.DefaultConfig(), Logger: logger, Version: "v1.0.0"})
	prof := api.InstallProfile{Name: "default", Runtimes: map[string][]string{"sh": {"/bin/sh"}}}
	srv := &server.Server{E: eng, Log: logger, SessionKey: []byte("session-key-session-key-session!"),
		PublicURL: "http://umm.example.com", Install: server.InstallConfig{Resolved: map[string]api.InstallProfile{"default": prof}}}
	h := srv.Handler()
	tok, _ := eng.CreateInstallToken(ctx, "t", prof, api.InstallTokenRequest{})
	get := func(path string, hdr map[string]string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	// Plain http public URL without dev mode: refused (as a script).
	if _, body := get("/i/"+tok.Token, nil); !strings.Contains(body, "public_url to be https") {
		t.Errorf("http public_url served: %s", body)
	}
	srv.PublicURL = "https://umm.example.com"
	h = srv.Handler()
	if _, body := get("/i/"+tok.Token, map[string]string{"X-Forwarded-Proto": "http"}); !strings.Contains(body, "use https") {
		t.Errorf("plain-http request served: %s", body)
	}
	code, body := get("/i/"+tok.Token, map[string]string{"X-Forwarded-Proto": "https"})
	if code != 200 || !strings.Contains(body, "TOKEN='"+tok.Token+"'") || !strings.Contains(body, "PROTO='=https'") {
		t.Fatalf("script: %d %.200s", code, body)
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(body)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, b)
	}
	// Rate limit per client IP.
	limited := false
	for range 40 {
		if _, b := get("/i/umm_ins_nopenopenopenopenopenope", map[string]string{"X-Forwarded-For": "198.51.100.9"}); strings.Contains(b, "too many requests") {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no rate limit")
	}
	if _, b := get("/i/umm_ins_nopenopenopenopenopenope", map[string]string{"X-Forwarded-For": "198.51.100.10"}); strings.Contains(b, "too many") {
		t.Error("rate limit is not per IP")
	}
	// Artifacts need a live token.
	if code, _ := get("/i/umm_ins_nopenopenopenopenopenope/releases/v1.0.0/SHA256SUMS", nil); code != http.StatusForbidden {
		t.Errorf("artifact with a bad token: %d", code)
	}
}
