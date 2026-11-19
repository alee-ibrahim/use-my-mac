package runtimes

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func skipNonUnix(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("unix only")
	}
}

// agentManager is the agent's read-only view of dir; requests land in reqs.
func (f *fixture) agentManager(t *testing.T, dir string, now *time.Time, reqs *[]Request) *Manager {
	m, err := New(Options{Role: RoleAgent, Dir: dir, Keys: []*selfupdate.PublicKey{f.pub}, Allow: []string{"*"},
		InUseFile: filepath.Join(filepath.Dir(dir), "in-use.json"),
		Now:       func() time.Time { return *now }, RequestTimeout: time.Minute, RetryAfter: time.Hour,
		Request: func(install, reinstall []string) {
			*reqs = append(*reqs, Request{Install: install, Reinstall: reinstall})
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *fixture) installer(t *testing.T, dir string) *Manager {
	m, err := New(Options{Role: RoleInstaller, Dir: dir, Keys: []*selfupdate.PublicKey{f.pub}, Allow: []string{"*"},
		InUseFile: filepath.Join(filepath.Dir(dir), "in-use.json"), RetryAfter: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The agent never installs: it asks, reports "installing" meanwhile, and
// picks up what root installed. Root's tree is sealed and listed.
func TestAgentRequestsAndRootInstalls(t *testing.T) {
	skipNonUnix(t)
	f := newFixture(t)
	e, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	cat := api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e}}
	base := t.TempDir()
	dir := filepath.Join(base, "rt")
	reqFile := filepath.Join(base, "req.json")
	root := f.installer(t, dir)

	now := time.Now()
	var reqs []Request
	ag := f.agentManager(t, dir, &now, &reqs)
	ag.SetCatalog(cat)
	if st := ag.Status(); len(st.InstallableCommands) != 2 {
		t.Fatalf("installable: %+v", st)
	}
	if _, _, err := ag.Acquire(t.Context(), "tool", nil); !errors.Is(err, ErrInstallRequested) {
		t.Fatalf("want a request, got %v", err)
	}
	if len(reqs) != 1 || strings.Join(reqs[0].Install, ",") != "tool" {
		t.Fatalf("requests: %+v", reqs)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("the agent wrote into the prefix: %v", ents)
	}
	st := ag.Status()
	if len(st.InstallableCommands) != 0 || len(st.Packages) != 1 || st.Packages[0].State != api.PackageInstalling ||
		!strings.Contains(st.Unavailable["tool"], "requested from the root helper") {
		t.Fatalf("pending status: %+v", st)
	}

	if err := WriteRequest(reqFile, Request{Install: reqs[0].Install, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(t.Context(), cat, reqFile); err != nil {
		t.Fatal(err)
	}
	ag.Check()
	inst, release, err := ag.Acquire(t.Context(), "tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	// In use: listed for root's clean-up.
	if b, _ := os.ReadFile(filepath.Join(base, "in-use.json")); !strings.Contains(string(b), inst.Root) {
		t.Fatalf("in-use file: %s", b)
	}
	release()
	fi, _ := os.Stat(filepath.Join(inst.Root, "bin", "tool"))
	if fi.Mode().Perm() != 0o555 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if os.Geteuid() == 0 {
		if uid, _ := owner(filepath.Join(inst.Root, "bin", "tool")); uid != 0 {
			t.Fatal("must be root-owned")
		}
	}
	mk := root.markers[inst.Root]
	if mk == nil || len(mk.Files) != 2 || mk.Files["README"] != sum([]byte("hi")) {
		t.Fatalf("file list: %+v", mk)
	}
}

// A request root never fulfils times out into a failure with a retry
// time; a failure root reports reaches the agent.
func TestAgentRequestTimeoutAndRootFailure(t *testing.T) {
	skipNonUnix(t)
	f := newFixture(t)
	e, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	cat := api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e}}
	base := t.TempDir()
	dir := filepath.Join(base, "rt")
	f.installer(t, dir)
	now := time.Now()
	var reqs []Request
	ag := f.agentManager(t, dir, &now, &reqs)
	ag.SetCatalog(cat)
	ag.Acquire(t.Context(), "tool", nil)
	now = now.Add(2 * time.Minute)
	ag.Check()
	st := ag.Status()
	if len(st.Packages) != 1 || st.Packages[0].State != api.PackageFailed || !strings.Contains(st.Packages[0].Error, "did not install") {
		t.Fatalf("timeout: %+v", st)
	}

	// Root's own failure (bad hash) is reported through the status file.
	bad := f.sign(func() api.RuntimePackage {
		_, p := f.pkg("bad", "1.0", f.tool("bad"), api.FormatTarGz)
		p.Artifacts[1].SHA256 = strings.Repeat("ab", 32)
		return p
	}())
	cat2 := api.AgentRuntimeCatalog{Revision: 2, Packages: []api.AgentRuntimePackage{bad}}
	reqFile := filepath.Join(base, "req.json")
	WriteRequest(reqFile, Request{Install: []string{"bad"}, At: now})
	if err := f.installer(t, dir).Sync(t.Context(), cat2, reqFile); err != nil {
		t.Fatal(err)
	}
	ag2 := f.agentManager(t, dir, &now, &reqs)
	ag2.SetCatalog(cat2)
	if _, _, err := ag2.Acquire(t.Context(), "bad", nil); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("root's failure must reach the agent: %v", err)
	}
}

// The request file is read without following links and only if small;
// names are validated.
func TestReadRequestHardening(t *testing.T) {
	skipNonUnix(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	os.WriteFile(secret, []byte(`{"install":["x"]}`), 0o600)
	link := filepath.Join(dir, "req.json")
	os.Symlink(secret, link)
	if _, err := ReadRequest(link); err == nil {
		t.Fatal("must not follow a symlink")
	}
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, []byte(`{"install":["`+strings.Repeat("a", maxRequestBytes)+`"]}`), 0o644)
	if _, err := ReadRequest(big); err == nil {
		t.Fatal("must refuse a large file")
	}
	ok := filepath.Join(dir, "ok.json")
	os.WriteFile(ok, []byte(`{"install":["tool","../etc","tool"],"reinstall":["lib"]}`), 0o644)
	r, err := ReadRequest(ok)
	if err != nil || strings.Join(r.Install, ",") != "tool" || strings.Join(r.Reinstall, ",") != "lib" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := ReadRequest(filepath.Join(dir, "missing")); err != nil || len(r.Install) != 0 {
		t.Fatalf("missing file: %+v %v", r, err)
	}
	// The in-use list only yields paths inside the prefix.
	os.WriteFile(filepath.Join(dir, "inuse.json"), []byte(`{"roots":["/etc","/rt/tool/1-abc","/rt/../etc"]}`), 0o644)
	if got := readInUse(filepath.Join(dir, "inuse.json"), "/rt"); len(got) != 1 || got[0] != filepath.Clean("/rt/tool/1-abc") {
		t.Fatalf("in-use: %v", got)
	}
}

// Hashes are cached by file identity; a changed file is caught even when
// its size and mtime are restored.
func TestHashCacheAndVerify(t *testing.T) {
	skipNonUnix(t)
	root := t.TempDir()
	p := filepath.Join(root, "f")
	os.WriteFile(p, []byte("aaaa"), 0o644)
	files := map[string]string{"f": sum([]byte("aaaa"))}
	c := newHashCache()
	if err := c.verify(root, files, false); err != nil {
		t.Fatal(err)
	}
	if len(c.m) != 1 {
		t.Fatal("must cache")
	}
	fi, _ := os.Stat(p)
	mt := fi.ModTime()
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(p, []byte("bbbb"), 0o644)
	os.Chtimes(p, mt, mt)
	if err := c.verify(root, files, false); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tamper with restored mtime: %v", err)
	}
	os.WriteFile(filepath.Join(root, "extra"), nil, 0o644)
	os.WriteFile(p, []byte("aaaa"), 0o644)
	if err := c.verify(root, files, false); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("extra file: %v", err)
	}
	os.Remove(filepath.Join(root, "extra"))
	os.Remove(p)
	if err := c.verify(root, files, false); err == nil {
		t.Fatal("missing file")
	}
}
