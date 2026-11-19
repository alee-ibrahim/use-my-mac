package runtimes

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

type tarEntry struct {
	name   string
	body   string
	mode   int64
	typ    byte
	target string
}

func makeTarGz(t testing.TB, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.target}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fixture is a signed package served over HTTP.
type fixture struct {
	t     *testing.T
	sk    *selfupdate.SecretKey
	pub   *selfupdate.PublicKey
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  atomic.Int64
	delay time.Duration
}

func newFixture(t *testing.T) *fixture {
	pub, sk, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, sk: sk, pub: pub, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		time.Sleep(f.delay)
		f.mu.Lock()
		b, ok := f.files[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const script = "#!/bin/sh\necho tool-ok \"$@\"\n"

// pkg publishes a tool package version and returns its catalog entry.
func (f *fixture) pkg(name, version string, data []byte, format string) (api.AgentRuntimePackage, api.RuntimePackage) {
	path := "/" + name + "-" + version + ".bin"
	f.mu.Lock()
	f.files[path] = data
	f.mu.Unlock()
	p := api.RuntimePackage{Name: name, Version: version,
		Commands: map[string][]string{name: {"bin/" + name}, name + "-x": {"bin/" + name, "-x"}},
		Artifacts: []api.RuntimeArtifact{
			{OS: "plan9", Arch: "mips", URL: "https://example.com/x", SHA256: strings.Repeat("0", 64), Format: api.FormatTarGz},
			{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: f.srv.URL + path, SHA256: sum(data), Size: int64(len(data)), Format: format},
		}}
	if format == api.FormatFile {
		p.Artifacts[1].File = name
		p.Commands = map[string][]string{name: {name}}
	}
	return f.sign(p), p
}

func (f *fixture) sign(p api.RuntimePackage) api.AgentRuntimePackage {
	m, _ := json.Marshal(p)
	sig, err := Sign(f.sk, m)
	if err != nil {
		f.t.Fatal(err)
	}
	return api.AgentRuntimePackage{Manifest: string(m), Signature: string(sig), URLs: make([]string, len(p.Artifacts))}
}

func (f *fixture) tool(name string) []byte {
	return makeTarGz(f.t, []tarEntry{{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "bin/" + name, body: script, mode: 0o755}, {name: "README", body: "hi"}})
}

func (f *fixture) manager(t *testing.T, dir string) *Manager {
	m, err := New(Options{Dir: dir, Keys: []*selfupdate.PublicKey{f.pub}, Allow: []string{"*"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManifestValidation(t *testing.T) {
	good := `{"name":"ffmpeg","version":"7.1","commands":{"ffmpeg":["bin/ffmpeg"]},
		"artifacts":[{"os":"darwin","arch":"arm64","url":"https://example.com/f.tgz","sha256":"` + strings.Repeat("ab", 32) + `","format":"tar.gz"}]}`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("good manifest: %v", err)
	}
	bad := map[string]string{
		"abs program":   strings.Replace(good, `"bin/ffmpeg"`, `"/opt/homebrew/bin/ffmpeg"`, 1),
		"escape":        strings.Replace(good, `"bin/ffmpeg"`, `"../ffmpeg"`, 1),
		"unclean":       strings.Replace(good, `"bin/ffmpeg"`, `"bin/../bin/ffmpeg"`, 1),
		"name":          strings.Replace(good, `"ffmpeg","version"`, `"FF MPEG","version"`, 1),
		"sha":           strings.Replace(good, strings.Repeat("ab", 32), "abc", 1),
		"format":        strings.Replace(good, `"tar.gz"`, `"rar"`, 1),
		"scheme":        strings.Replace(good, `https://example.com/f.tgz`, `file:///etc/passwd`, 1),
		"unknown field": strings.Replace(good, `"version"`, `"postinstall":"rm -rf /","version"`, 1),
		"no source":     strings.Replace(good, `"url":"https://example.com/f.tgz",`, ``, 1),
	}
	for name, m := range bad {
		if _, err := Parse([]byte(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSignatureTrust(t *testing.T) {
	f := newFixture(t)
	e, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	if _, err := Verify([]*selfupdate.PublicKey{f.pub}, []byte(e.Manifest), []byte(e.Signature)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	other, _, _ := selfupdate.GenerateKey()
	if _, err := Verify([]*selfupdate.PublicKey{other}, []byte(e.Manifest), []byte(e.Signature)); err == nil {
		t.Fatal("untrusted key accepted")
	}
	// Tampered manifest (e.g. a different download hash).
	tampered := strings.Replace(e.Manifest, `"1.0"`, `"1.1"`, 1)
	if _, err := Verify([]*selfupdate.PublicKey{f.pub}, []byte(tampered), []byte(e.Signature)); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	// A release signature (other trusted comment) is not a package signature.
	m := []byte(e.Manifest)
	rel := f.sk.Sign(m, selfupdate.TrustedComment("v1.0.0"))
	if _, err := Verify([]*selfupdate.PublicKey{f.pub}, m, rel); err == nil {
		t.Fatal("release signature accepted as a package signature")
	}
}

func TestInstallUseAndCache(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	e, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	m := f.manager(t, dir)
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 3, Packages: []api.AgentRuntimePackage{e}})
	if m.Revision() != 3 {
		t.Fatal("revision")
	}
	st := m.Status()
	if len(st.Ready) != 0 || strings.Join(st.InstallableCommands, ",") != "tool,tool-x" ||
		strings.Join(st.InstallablePackages, ",") != "tool" {
		t.Fatalf("status before install: %+v", st)
	}
	var lines []string
	inst, release, err := m.Acquire(context.Background(), "tool", func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	argv := inst.Argv("tool-x")
	if !strings.HasPrefix(argv[0], dir) || argv[1] != "-x" || !strings.HasSuffix(filepath.ToSlash(argv[0]), "tool/1.0-"+sum(f.tool("tool"))[:12]+"/bin/tool") {
		t.Fatalf("argv %v", argv)
	}
	if b, err := os.ReadFile(argv[0]); err != nil || string(b) != script {
		t.Fatalf("installed program: %q %v", b, err)
	}
	if len(inst.PathDirs()) != 1 || !strings.HasSuffix(filepath.ToSlash(inst.PathDirs()[0]), "/bin") {
		t.Fatalf("path dirs %v", inst.PathDirs())
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "sha256 verified") {
		t.Fatalf("progress lines: %v", lines)
	}
	release()
	st = m.Status()
	if len(st.Ready["tool"]) != 1 || len(st.InstallableCommands) != 0 || len(st.Packages) != 1 ||
		st.Packages[0].State != api.PackageInstalled || st.Packages[0].SizeBytes == 0 {
		t.Fatalf("status after install: %+v", st)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(argv[0]); fi.Mode().Perm()&0o222 != 0 {
			t.Fatalf("installed file is writable: %v", fi.Mode())
		}
	}
	// Cached: a second acquire and a fresh manager (agent restart) don't download.
	hits := f.hits.Load()
	if _, rel, err := m.Acquire(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	m2 := f.manager(t, dir)
	m2.SetCatalog(api.AgentRuntimeCatalog{Revision: 3, Packages: []api.AgentRuntimePackage{e}})
	if len(m2.Status().Ready) != 2 {
		t.Fatal("restart lost the installed package")
	}
	if _, rel, err := m2.Acquire(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	if f.hits.Load() != hits {
		t.Fatal("downloaded again despite the cache")
	}
}

func TestConcurrentInstallDownloadsOnce(t *testing.T) {
	f := newFixture(t)
	f.delay = 100 * time.Millisecond
	e, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	m := f.manager(t, t.TempDir())
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e}})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rel, err := m.Acquire(context.Background(), "tool", nil)
			if err == nil {
				rel()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("downloads: %d, want 1", f.hits.Load())
	}
}

func TestBadDownloadsFailAndBackOff(t *testing.T) {
	f := newFixture(t)
	good := f.tool("tool")
	e, p := f.pkg("tool", "1.0", good, api.FormatTarGz)
	// Serve different bytes than the signed hash.
	f.mu.Lock()
	for k := range f.files {
		f.files[k] = f.tool("evil")
	}
	f.mu.Unlock()
	now := time.Now()
	m := f.manager(t, t.TempDir())
	m.o.Now = func() time.Time { return now }
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e}})
	_, _, err := m.Acquire(context.Background(), "tool", nil)
	if err == nil || !strings.Contains(err.Error(), "download") {
		t.Fatalf("tampered download installed: %v", err)
	}
	if m2 := f.manager(t, t.TempDir()); true {
		// Same size, different bytes: the hash check catches it.
		flipped := append([]byte(nil), good...)
		flipped[len(flipped)-1] ^= 0xff
		f.mu.Lock()
		for k := range f.files {
			f.files[k] = flipped
		}
		f.mu.Unlock()
		m2.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e}})
		if _, _, err := m2.Acquire(context.Background(), "tool", nil); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("same-size tampered download: %v", err)
		}
	}
	st := m.Status()
	if len(st.InstallableCommands) != 0 || st.Unavailable["tool"] == "" || st.Packages[0].State != api.PackageFailed {
		t.Fatalf("status after failure: %+v", st)
	}
	hits := f.hits.Load()
	if _, _, err := m.Acquire(context.Background(), "tool", nil); err == nil || f.hits.Load() != hits {
		t.Fatal("retried inside the backoff")
	}
	// After the backoff it is installable again; with the right bytes it works.
	f.mu.Lock()
	for k := range f.files {
		f.files[k] = good
	}
	f.mu.Unlock()
	now = now.Add(11 * time.Minute)
	if len(m.Status().InstallableCommands) != 2 {
		t.Fatal("not installable after the backoff")
	}
	if _, rel, err := m.Acquire(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	// A package whose command is missing from the archive is refused.
	p.Version = "2.0"
	p.Commands = map[string][]string{"tool": {"bin/missing"}}
	e2 := f.sign(p)
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 2, Packages: []api.AgentRuntimePackage{e2}})
	if _, _, err := m.Acquire(context.Background(), "tool", nil); err == nil || !strings.Contains(err.Error(), "not in the package") {
		t.Fatalf("missing program: %v", err)
	}
}

func TestCatalogFilters(t *testing.T) {
	f := newFixture(t)
	e, p := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	other := newFixture(t)
	untrusted, _ := other.pkg("other", "1.0", other.tool("other"), api.FormatTarGz)
	p.Name, p.Version = "noplat", "1.0"
	p.Artifacts = p.Artifacts[:1]
	noPlat := f.sign(p)
	m, _ := New(Options{Dir: t.TempDir(), Keys: []*selfupdate.PublicKey{f.pub}, Allow: []string{"noplat", "other"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{e, untrusted, noPlat}})
	st := m.Status()
	if len(st.InstallablePackages) != 0 {
		t.Fatalf("installable: %v", st.InstallablePackages)
	}
	for _, n := range []string{"tool", "other", "noplat"} {
		if m.rejected[n] == "" {
			t.Errorf("%s not rejected", n)
		}
	}
	if !strings.Contains(m.rejected["tool"], "not allowed") || !strings.Contains(m.rejected["other"], "does not trust") ||
		!strings.Contains(m.rejected["noplat"], "no build") {
		t.Fatalf("reasons: %v", m.rejected)
	}
	if _, _, err := m.Acquire(context.Background(), "tool", nil); err == nil {
		t.Fatal("installed a package agent.yaml does not allow")
	}
}

func TestGarbageCollection(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	v1, _ := f.pkg("tool", "1.0", f.tool("tool"), api.FormatTarGz)
	m := f.manager(t, dir)
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{v1}})
	old, relOld, err := m.Acquire(context.Background(), "tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := f.pkg("tool", "2.0", makeTarGz(t, []tarEntry{{name: "bin/tool", body: script + "#2\n", mode: 0o755}}), api.FormatTarGz)
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 2, Packages: []api.AgentRuntimePackage{v2}})
	cur, relCur, err := m.Acquire(context.Background(), "tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	relCur()
	m.GC()
	if _, err := os.Stat(old.Root); err != nil {
		t.Fatal("GC removed a version a task still uses")
	}
	relOld()
	m.GC()
	if _, err := os.Stat(old.Root); err == nil {
		t.Fatal("old version not collected")
	}
	if _, err := os.Stat(cur.Root); err != nil {
		t.Fatal("current version collected")
	}
	// Leftovers of an interrupted install go too.
	os.MkdirAll(filepath.Join(dir, ".tmp", "tool-123"), 0o755)
	os.MkdirAll(filepath.Join(dir, "tool", "9.9-deadbeef0000"), 0o755)
	m.GC()
	if _, err := os.Stat(filepath.Join(dir, ".tmp")); err == nil {
		t.Fatal(".tmp not cleaned")
	}
	if _, err := os.Stat(filepath.Join(dir, "tool", "9.9-deadbeef0000")); err == nil {
		t.Fatal("incomplete install not cleaned")
	}
	// Removed from the catalog: collected.
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 3})
	m.GC()
	if _, err := os.Stat(filepath.Join(dir, "tool")); err == nil {
		t.Fatal("package dropped from the catalog not collected")
	}
	// Nothing is collected before a catalog is known.
	m3 := f.manager(t, dir)
	os.MkdirAll(filepath.Join(dir, "keep", "1-aaaaaaaaaaaa"), 0o755)
	m3.GC()
	if _, err := os.Stat(filepath.Join(dir, "keep", "1-aaaaaaaaaaaa")); err != nil {
		t.Fatal("GC ran without a catalog")
	}
}

func TestPrewarmAndFormats(t *testing.T) {
	f := newFixture(t)
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	h := &zip.FileHeader{Name: "bin/zt", Method: zip.Deflate}
	h.SetMode(0o755)
	w, _ := zw.CreateHeader(h)
	w.Write([]byte(script))
	zw.Close()
	ez, _ := f.pkg("zt", "1", zbuf.Bytes(), api.FormatZip)
	ef, _ := f.pkg("ft", "1", []byte(script), api.FormatFile)
	ez.Prewarm, ef.Prewarm = true, true
	m := f.manager(t, t.TempDir())
	m.SetCatalog(api.AgentRuntimeCatalog{Revision: 1, Packages: []api.AgentRuntimePackage{ez, ef}})
	m.Prewarm(context.Background())
	st := m.Status()
	if len(st.Ready["zt"]) != 1 || len(st.Ready["ft"]) != 1 {
		t.Fatalf("prewarm: %+v", st)
	}
	if b, _ := os.ReadFile(st.Ready["ft"][0]); string(b) != script {
		t.Fatal("file format content")
	}
}

func TestExtractRefusesEscapes(t *testing.T) {
	cases := map[string][]tarEntry{
		"dotdot":         {{name: "../evil", body: "x"}},
		"absolute":       {{name: "/etc/evil", body: "x"}},
		"through link":   {{name: "l", typ: tar.TypeSymlink, target: "."}, {name: "l/evil", body: "x"}},
		"abs link":       {{name: "l", typ: tar.TypeSymlink, target: "/etc"}},
		"hard link":      {{name: "h", typ: tar.TypeLink, target: "x"}},
		"device":         {{name: "d", typ: tar.TypeChar}},
		"duplicate file": {{name: "a", body: "1"}, {name: "a", body: "2"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "through link" && runtime.GOOS == "windows" {
				t.Skip("symlinks need privileges on Windows")
			}
			d := t.TempDir()
			src := filepath.Join(d, "a.tgz")
			os.WriteFile(src, makeTarGz(t, entries), 0o600)
			root := filepath.Join(d, "root")
			os.Mkdir(root, 0o755)
			if err := Extract(src, api.RuntimeArtifact{Format: api.FormatTarGz}, root); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(filepath.Join(d, "evil")); err == nil {
				t.Fatal("wrote outside the root")
			}
		})
	}
}
