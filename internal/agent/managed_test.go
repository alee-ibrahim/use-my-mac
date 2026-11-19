package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// managedFixture is an agent whose managed runtime catalog offers "tool"
// and "lib" (signed by a key in its config), served from a test server, and
// a root helper (installer) for the same config.
type managedFixture struct {
	a   *Agent
	cfg *Config
	cat api.AgentRuntimeCatalog
}

func managedAgent(t *testing.T, static map[string][]string) *managedFixture {
	t.Helper()
	pub, sk, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := "#!/bin/sh\nexit 0\n"
	for _, n := range []string{"bin/tool", "lib/bin/lib"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	data := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	t.Cleanup(srv.Close)
	h := sha256.Sum256(data)
	var pkgs []api.AgentRuntimePackage
	for _, p := range []api.RuntimePackage{
		{Name: "tool", Version: "1", Commands: map[string][]string{"tool": {"bin/tool"}, "ffmpeg": {"bin/tool", "-hide_banner"}}},
		{Name: "lib", Version: "1", Commands: map[string][]string{"lib": {"lib/bin/lib"}}},
	} {
		p.Artifacts = []api.RuntimeArtifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL + "/p.tgz",
			SHA256: hex.EncodeToString(h[:]), Format: api.FormatTarGz}}
		m, _ := json.Marshal(p)
		sig, err := runtimes.Sign(sk, m)
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, api.AgentRuntimePackage{Manifest: string(m), Signature: string(sig)})
	}
	on := true
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "umm", "work"), 0o700)
	cfg := &Config{Server: "http://127.0.0.1:1", WorkDir: filepath.Join(base, "umm", "work"), Runtimes: static, Priority: "none",
		ManagedRuntimes: ManagedRuntimesConfig{Enabled: &on, PublicKeys: []string{pub.String()}}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	f := &managedFixture{cfg: cfg, cat: api.AgentRuntimeCatalog{Revision: 1, Packages: pkgs}}
	f.rootSync(t) // creates the root-owned prefix, installs nothing yet
	f.a = New(cfg, "cred", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if f.a.Managed() == nil {
		t.Fatal("managed runtimes not enabled")
	}
	f.a.Managed().SetCatalog(f.cat)
	f.a.Preflight()
	return f
}

// rootSync runs one root helper pass, as its LaunchDaemon does when the
// agent writes a request, then lets the agent notice.
func (f *managedFixture) rootSync(t *testing.T) {
	t.Helper()
	m, err := NewInstaller(f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(t.Context(), f.cat, f.cfg.ManagedRuntimes.RequestFile); err != nil {
		t.Fatal(err)
	}
	if f.a != nil {
		f.a.Managed().Check()
		f.a.Preflight()
	}
}

func (f *managedFixture) run(t *testing.T, id, rt string, requires ...string) api.TaskUpdate {
	t.Helper()
	f.a.goos = "test"
	return f.a.execute(t.Context(), &run{as: api.Assignment{TaskID: id, Attempt: 1, Runtime: rt, Requires: requires}})
}

// A static runtime whose program is missing falls back to the catalog
// package providing the same command; a working static runtime wins. The
// agent never installs: it asks root, and the attempt is a Mac fault until
// root has installed the package.
func TestManagedFallbackAndPreference(t *testing.T) {
	skipWindows(t)
	good := filepath.Join(t.TempDir(), "good")
	writeProg(t, good, 0o755)
	f := managedAgent(t, map[string][]string{
		"ffmpeg": {"/nonexistent/homebrew/bin/ffmpeg"}, // missing: managed takes over
		"tool":   {good},                               // works: static wins
	})
	a := f.a
	avail, unavail := a.RuntimeStatus()
	if strings.Join(avail, ",") != "tool" || len(unavail) != 0 {
		t.Fatalf("avail %v unavail %+v", avail, unavail)
	}
	a.mu.Lock()
	inst, pkgs := a.installable, a.installablePkgs
	a.mu.Unlock()
	if strings.Join(inst, ",") != "ffmpeg,lib" || strings.Join(pkgs, ",") != "lib,tool" {
		t.Fatalf("installable %v packages %v", inst, pkgs)
	}
	argv, install, why := a.runtimeFor("ffmpeg")
	if argv != nil || install != "tool" || why != "" {
		t.Fatalf("runtimeFor(ffmpeg) = %v %q %q", argv, install, why)
	}
	if argv, install, _ := a.runtimeFor("tool"); install != "" || argv[0] != good {
		t.Fatalf("static runtime must win: %v %q", argv, install)
	}

	// First task: not installed, so root is asked and the attempt is a Mac
	// fault. Meanwhile the package is "installing", not installable.
	upd := f.run(t, "t1", "ffmpeg", "lib")
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.RuntimeUnavailable, "install requested") {
		t.Fatalf("task: %+v", upd)
	}
	req, err := runtimes.ReadRequest(f.cfg.ManagedRuntimes.RequestFile)
	if err != nil || strings.Join(req.Install, ",") != "tool" {
		t.Fatalf("request: %+v %v", req, err)
	}
	a.Preflight()
	a.mu.Lock()
	inst = a.installable
	a.mu.Unlock()
	if slices.Contains(inst, "ffmpeg") {
		t.Fatalf("a requested package must not be offered meanwhile: %v", inst)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.ManagedRuntimes.Dir, "tool")); err == nil {
		t.Fatal("the agent must not install anything itself")
	}

	f.rootSync(t) // root installs tool
	if upd := f.run(t, "t2", "ffmpeg", "lib"); !strings.Contains(upd.RuntimeUnavailable, "lib") {
		t.Fatalf("lib still missing: %+v", upd)
	}
	f.rootSync(t) // and lib
	if upd := f.run(t, "t3", "ffmpeg", "lib"); upd.Outcome != api.OutcomeSucceeded {
		t.Fatalf("task: %+v", upd)
	}
	avail, _ = a.RuntimeStatus()
	if !slices.Contains(avail, "ffmpeg") || !slices.Contains(avail, "lib") {
		t.Fatalf("after install: %v", avail)
	}
	argv, install, _ = a.runtimeFor("ffmpeg")
	if install != "tool" || !strings.HasSuffix(filepath.ToSlash(argv[0]), "/bin/tool") || argv[1] != "-hide_banner" {
		t.Fatalf("managed argv %v %q", argv, install)
	}
	a.mu.Lock()
	packages := a.packages
	a.mu.Unlock()
	if len(packages) != 2 || packages[0].State != api.PackageInstalled {
		t.Fatalf("package status %+v", packages)
	}
	// Installed by root, read-only for everyone.
	prog := filepath.Join(strings.TrimSuffix(argv[0], "/bin/tool"), "bin", "tool")
	if fi, err := os.Stat(prog); err != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("installed program mode: %v %v", fi.Mode(), err)
	}
}

// A package changed after root installed it is refused before the task
// runs, and root reinstalls it on request.
func TestManagedTamperIsRefusedAndReinstalled(t *testing.T) {
	skipWindows(t)
	f := managedAgent(t, nil)
	f.run(t, "t0", "tool")
	f.rootSync(t)
	if upd := f.run(t, "t1", "tool"); upd.Outcome != api.OutcomeSucceeded {
		t.Fatalf("task: %+v", upd)
	}
	argv, _, _ := f.a.runtimeFor("tool")
	os.Chmod(argv[0], 0o755)
	if err := os.WriteFile(argv[0], []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(argv[0], 0o555)
	upd := f.run(t, "t2", "tool")
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.RuntimeUnavailable, "failed verification") {
		t.Fatalf("tampered package must be refused: %+v", upd)
	}
	req, _ := runtimes.ReadRequest(f.cfg.ManagedRuntimes.RequestFile)
	if strings.Join(req.Reinstall, ",") != "tool" {
		t.Fatalf("reinstall request: %+v", req)
	}
	f.rootSync(t)
	if upd := f.run(t, "t3", "tool"); upd.Outcome != api.OutcomeSucceeded {
		t.Fatalf("after reinstall: %+v", upd)
	}
	if b, _ := os.ReadFile(argv[0]); strings.Contains(string(b), "evil") {
		t.Fatal("root must have restored the package")
	}
}

// Requires that cannot be met here is a Mac fault, not a task failure.
func TestManagedRequiresUnknownPackage(t *testing.T) {
	skipWindows(t)
	f := managedAgent(t, nil)
	upd := f.run(t, "t1", "tool", "nope")
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.RuntimeUnavailable, "nope") {
		t.Fatalf("got %+v", upd)
	}
}

func TestPrependPath(t *testing.T) {
	sep := string(filepath.ListSeparator)
	got := prependPath([]string{"HOME=/h", "PATH=/usr/bin"}, []string{"/a", "/b"})
	if got[1] != "PATH=/a"+sep+"/b"+sep+"/usr/bin" {
		t.Fatalf("%v", got)
	}
	if got := prependPath([]string{"HOME=/h"}, []string{"/a"}); got[1] != "PATH=/a" {
		t.Fatalf("%v", got)
	}
}
