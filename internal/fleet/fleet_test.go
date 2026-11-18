package fleet

import (
	"bytes"
	"context"
	"fmt"
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

// --- fakes -----------------------------------------------------------------

type ranCmd struct {
	Cmd   string
	Stdin string
}

type upFile struct {
	Path string
	Data []byte
	Mode os.FileMode
}

type fakeTarget struct {
	mu      sync.Mutex
	tmpN    int
	runs    []ranCmd
	uploads []upFile
	// respond lets a test shape exit codes/output per command substring.
	respond func(cmd string) Result
}

func (f *fakeTarget) Run(ctx context.Context, cmd string, stdin io.Reader) (Result, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.mu.Lock()
	f.runs = append(f.runs, ranCmd{Cmd: cmd, Stdin: in})
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(cmd), nil
	}
	return Result{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (f *fakeTarget) Upload(ctx context.Context, p string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, upFile{p, append([]byte(nil), data...), mode})
	return nil
}

func (f *fakeTarget) MkdirTemp(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tmpN++
	return fmt.Sprintf("/tmp/umm.%d", f.tmpN), nil
}

func (f *fakeTarget) Close() error { return nil }

type fakeRouter struct {
	token    string
	macName  string
	lastSeen *time.Time
	// tick advances lastSeen by a second on every Macs call, like a live
	// agent heartbeating; without it lastSeen stays put (a stale record).
	tick  bool
	calls int
}

func (r *fakeRouter) CreateEnrolmentToken(ctx context.Context, ttl int) (*api.EnrolmentToken, error) {
	return &api.EnrolmentToken{Token: r.token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r *fakeRouter) Macs(ctx context.Context) ([]api.Mac, error) {
	r.calls++
	if r.tick && r.lastSeen != nil {
		t := r.lastSeen.Add(time.Second)
		r.lastSeen = &t
	}
	return []api.Mac{{Name: r.macName, Online: true, LastSeen: r.lastSeen}}, nil
}

func testHost() ResolvedHost {
	return ResolvedHost{
		Host: "mac-a.example.com", Port: 22, User: "fleetadmin", Name: "studio-a",
		MaxTasks: 1, Priority: "background", ServiceLabel: DefaultServiceLabel,
		Runtimes: map[string][]string{"ffmpeg": {"/opt/homebrew/bin/ffmpeg"}},
	}
}

func testBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "umm-agent"), []byte("#!fake-macho-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newRunner builds a Runner whose dial returns the given fake target and whose
// log goes to buf.
func newRunner(t *testing.T, ft *fakeTarget, buf *bytes.Buffer, opt Options) *Runner {
	opt.Dial = func(ctx context.Context, h ResolvedHost, o SSHOptions) (Target, error) { return ft, nil }
	opt.Log = slog.New(slog.NewTextHandler(buf, nil))
	opt.Now = time.Now
	if opt.Sleep == nil {
		opt.Sleep = func(ctx context.Context, d time.Duration) {}
	}
	return NewRunner(opt)
}

// assertNoSecret fails if secret appears in any command argv, command stdin
// other than the allowed one, or the log buffer.
func assertNoSecretInArgv(t *testing.T, ft *fakeTarget, secret string) {
	t.Helper()
	for _, r := range ft.runs {
		if strings.Contains(r.Cmd, secret) {
			t.Errorf("secret leaked into argv: %q", r.Cmd)
		}
	}
}

// --- tests -----------------------------------------------------------------

func TestBootstrapPasswordViaStdinNotArgv(t *testing.T) {
	ft := &fakeTarget{}
	var log bytes.Buffer
	const pw = "sup3r-s3cret-sudo"
	r := newRunner(t, ft, &log, Options{SudoPassword: NewSecret([]byte(pw))})
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionBootstrap)
	if len(res) != 1 || !res[0].OK() {
		t.Fatalf("bootstrap result: %+v", res)
	}
	// The password must never be in argv or the log; it must be in exactly one
	// command's stdin (the sudo -S invocation).
	assertNoSecretInArgv(t, ft, pw)
	if strings.Contains(log.String(), pw) {
		t.Error("password leaked into log")
	}
	stdinHits := 0
	var bootstrapCmd string
	for _, rc := range ft.runs {
		if strings.Contains(rc.Stdin, pw) {
			stdinHits++
		}
		if strings.Contains(rc.Cmd, "bootstrap.sh") {
			bootstrapCmd = rc.Cmd
		}
	}
	if stdinHits != 1 {
		t.Errorf("password should be in exactly one stdin, got %d", stdinHits)
	}
	if !strings.Contains(bootstrapCmd, "sudo -S") || !strings.Contains(bootstrapCmd, "fleetadmin") {
		t.Errorf("bootstrap cmd unexpected: %q", bootstrapCmd)
	}
	// The managed scripts + bootstrap must be staged.
	names := map[string]os.FileMode{}
	for _, u := range ft.uploads {
		names[filepath.Base(u.Path)] = u.Mode
	}
	for _, want := range []string{"bootstrap.sh", "install", "upgrade", "uninstall"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing staged %s", want)
		}
	}
}

func TestInstallTokenHandling(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result {
		return Result{ExitCode: 0, Stdout: "installed: running\n"}
	}}
	var log bytes.Buffer
	now := time.Now()
	fr := &fakeRouter{token: "umm_enrol_SECRETTOKEN", macName: "studio-a", lastSeen: &now, tick: true}
	r := newRunner(t, ft, &log, Options{Router: "https://umm.example.com", Bundle: testBundle(t), Router_: fr})
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionInstall)
	if len(res) != 1 || !res[0].OK() {
		t.Fatalf("install result: %+v", res)
	}
	if !strings.Contains(res[0].Message, "telemetry: online") {
		t.Errorf("expected telemetry verification, got %q", res[0].Message)
	}
	// Token must never be in argv or logs...
	assertNoSecretInArgv(t, ft, fr.token)
	if strings.Contains(log.String(), fr.token) {
		t.Error("token leaked into log")
	}
	// ...and must never be passed over stdin either (it goes in a file).
	for _, rc := range ft.runs {
		if strings.Contains(rc.Stdin, fr.token) {
			t.Error("token leaked into stdin")
		}
	}
	// It must be uploaded as a 0600 file named enrol-token.
	found := false
	for _, u := range ft.uploads {
		if filepath.Base(u.Path) == "enrol-token" {
			found = true
			if u.Mode != 0o600 {
				t.Errorf("enrol-token mode = %o, want 0600", u.Mode)
			}
			if string(u.Data) != fr.token {
				t.Errorf("enrol-token data = %q", u.Data)
			}
		}
	}
	if !found {
		t.Error("enrol-token not staged")
	}
	// The install must run the root-owned script with sudo -n (no password).
	ok := false
	for _, rc := range ft.runs {
		if strings.Contains(rc.Cmd, "sudo -n") && strings.Contains(rc.Cmd, scriptPath("install")) {
			ok = true
		}
	}
	if !ok {
		t.Error("install did not run the root-owned script via sudo -n")
	}
}

func TestInstallWithoutRouterSkipsEnrol(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result {
		// install.sh exits 2 when installed but not enrolled.
		return Result{ExitCode: 2, Stdout: "installed: NOT started (no credential)\n"}
	}}
	var log bytes.Buffer
	r := newRunner(t, ft, &log, Options{Router: "https://umm.example.com", Bundle: testBundle(t)})
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionInstall)
	if !res[0].OK() {
		t.Fatalf("expected ok (installed, not enrolled), got %+v", res[0])
	}
	for _, u := range ft.uploads {
		if filepath.Base(u.Path) == "enrol-token" {
			t.Error("no token should be staged without a router client")
		}
	}
}

func TestIdempotentReinstall(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result { return Result{ExitCode: 0, Stdout: "installed: running\n"} }}
	var log bytes.Buffer
	now := time.Now()
	fr := &fakeRouter{token: "tok", macName: "studio-a", lastSeen: &now, tick: true}
	r := newRunner(t, ft, &log, Options{Router: "https://umm.example.com", Bundle: testBundle(t), Router_: fr})
	for i := 0; i < 2; i++ {
		res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionInstall)
		if !res[0].OK() {
			t.Fatalf("run %d not ok: %+v", i, res[0])
		}
	}
}

// A heartbeat recorded before the restart must not count as the new agent
// being online: install waits for a newer one and reports when none arrives.
func TestInstallIgnoresStaleHeartbeat(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result { return Result{ExitCode: 0, Stdout: "installed: running\n"} }}
	var log bytes.Buffer
	old := time.Now().Add(-10 * time.Second)
	fr := &fakeRouter{token: "tok", macName: "studio-a", lastSeen: &old}
	clock := time.Now()
	r := newRunner(t, ft, &log, Options{Router: "https://umm.example.com", Bundle: testBundle(t), Router_: fr})
	r.opt.Now = func() time.Time { return clock }
	r.opt.Sleep = func(ctx context.Context, d time.Duration) { clock = clock.Add(d) }
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionInstall)
	if strings.Contains(res[0].Message, "telemetry: online") {
		t.Fatalf("stale heartbeat counted as online: %q", res[0].Message)
	}
	if !strings.Contains(res[0].Message, "no heartbeat since restart") {
		t.Errorf("message = %q", res[0].Message)
	}
	if fr.calls < 3 {
		t.Errorf("expected repeated router checks, got %d", fr.calls)
	}
}

// -replace-config stages a marker file; install warnings on stderr surface.
func TestInstallReplaceConfigAndWarnings(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result {
		return Result{ExitCode: 0, Stdout: "installed: running\n", Stderr: "warning: kept /usr/local/etc/umm/agent.yaml\n"}
	}}
	var log bytes.Buffer
	r := newRunner(t, ft, &log, Options{Router: "https://umm.example.com", Bundle: testBundle(t), ReplaceConfig: true})
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionInstall)
	if !res[0].OK() || !strings.Contains(res[0].Message, "warning: kept") {
		t.Fatalf("result: %+v", res[0])
	}
	found := false
	for _, u := range ft.uploads {
		if filepath.Base(u.Path) == "replace-config" {
			found = true
		}
	}
	if !found {
		t.Error("replace-config marker not staged")
	}
}

func TestParallelAcrossHosts(t *testing.T) {
	ft := &fakeTarget{}
	var log bytes.Buffer
	r := newRunner(t, ft, &log, Options{SudoPassword: NewSecret([]byte("pw")), Concurrency: 3})
	var hosts []ResolvedHost
	for i := 0; i < 7; i++ {
		h := testHost()
		h.Host = fmt.Sprintf("mac-%d.example.com", i)
		h.Name = fmt.Sprintf("studio-%d", i)
		hosts = append(hosts, h)
	}
	res := r.Run(context.Background(), hosts, ActionBootstrap)
	if len(res) != 7 {
		t.Fatalf("got %d results", len(res))
	}
	for _, rr := range res {
		if !rr.OK() {
			t.Errorf("%s not ok: %v", rr.Host, rr.Err)
		}
	}
	// Results are sorted by host.
	for i := 1; i < len(res); i++ {
		if res[i-1].Host > res[i].Host {
			t.Error("results not sorted by host")
		}
	}
}

func TestUninstallPurge(t *testing.T) {
	ft := &fakeTarget{respond: func(cmd string) Result { return Result{ExitCode: 0, Stdout: "purged\n"} }}
	var log bytes.Buffer
	r := newRunner(t, ft, &log, Options{Purge: true})
	res := r.Run(context.Background(), []ResolvedHost{testHost()}, ActionUninstall)
	if !res[0].OK() {
		t.Fatalf("uninstall: %+v", res[0])
	}
	ok := false
	for _, rc := range ft.runs {
		if strings.Contains(rc.Cmd, scriptPath("uninstall")) && strings.Contains(rc.Cmd, "--purge") {
			ok = true
		}
	}
	if !ok {
		t.Error("uninstall --purge not invoked")
	}
}
