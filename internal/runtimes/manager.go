package runtimes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Options configure a Manager.
type Options struct {
	// Role: RoleInstaller (the root helper: downloads, verifies, installs,
	// garbage-collects) or RoleAgent (the unprivileged agent: reads what
	// root installed and asks for installs through Request).
	Role Role
	// Request is called (agent role) with the packages to install and the
	// ones to reinstall whenever the set changes.
	Request func(install, reinstall []string)
	// RequestTimeout: a requested install not done by then counts as
	// failed (agent role; default 15m).
	RequestTimeout time.Duration
	// InUseFile lists the install roots running tasks use: the agent
	// writes it, the root helper's clean-up keeps those versions.
	InUseFile string
	// Dir is the install prefix, root-owned on a Mac:
	// <Dir>/<name>/<version>-<sha12>/.
	Dir string
	// Keys are the trusted signing keys.
	Keys []*selfupdate.PublicKey
	// Allow lists the package names this Mac may install ("*" = any).
	Allow        []string
	GOOS, GOARCH string
	HTTP         *http.Client
	Log          *slog.Logger
	// RetryAfter: a failed install is not retried before this (default 10m).
	RetryAfter time.Duration
	// MaxBytes caps one download when the manifest has no size.
	MaxBytes int64
	Now      func() time.Time
}

// usable is a verified catalog package this Mac can install.
type usable struct {
	p       *api.RuntimePackage
	art     api.RuntimeArtifact
	url     string
	prewarm bool
}

// dirName is the install directory name of a package version.
func (u *usable) dirName() string { return u.p.Version + "-" + u.art.SHA256[:12] }

// marker is written next to an installed package root once it is complete.
type marker struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	InstalledAt time.Time `json:"installed_at"`
	// Files maps every file in the package (slash paths relative to its
	// root) to its SHA-256, or "symlink:<target>" for links. Written by
	// root from the archive whose hash the signed manifest names; the
	// agent checks the tree against it before each task.
	Files map[string]string `json:"files,omitempty"`
}

type failure struct {
	version string
	err     string
	at      time.Time
	retry   time.Time
}

type call struct {
	done chan struct{}
	err  error
}

// Installed is a package ready to use.
type Installed struct {
	Package *api.RuntimePackage
	Root    string
}

// Argv returns a command's argv with the program made absolute.
func (i *Installed) Argv(cmd string) []string {
	argv := i.Package.Commands[cmd]
	if len(argv) == 0 {
		return nil
	}
	out := append([]string{filepath.Join(i.Root, filepath.FromSlash(argv[0]))}, argv[1:]...)
	return out
}

// PathDirs returns the absolute directories to put on PATH.
func (i *Installed) PathDirs() []string {
	var out []string
	for _, d := range PathDirs(i.Package) {
		out = append(out, filepath.Join(i.Root, filepath.FromSlash(d)))
	}
	return out
}

// Manager installs and tracks managed runtimes on one Mac. Safe for
// concurrent use.
type Manager struct {
	o Options

	mu       sync.Mutex
	rev      int64
	loaded   bool
	catalog  map[string]*usable // package name -> usable here
	commands map[string]string  // command -> package name
	rejected map[string]string  // package name -> why it's not usable here
	inflight map[string]*call   // package name -> running install
	failed   map[string]failure // package name -> last failure
	inUse    map[string]int     // install root -> tasks using it
	markers  map[string]*marker // install root -> marker (installed)
	logged   map[string]bool    // reject messages already logged

	// Agent role: installs asked of the root helper, and packages whose
	// files did not match their marker (to be reinstalled).
	pending map[string]time.Time
	damaged map[string]string
	hashes  *hashCache
	lastReq string
}

// New creates a manager and scans what is already installed.
func New(o Options) (*Manager, error) {
	if o.Dir == "" {
		return nil, errors.New("managed runtimes: dir is required")
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 10 * time.Minute
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 15 * time.Minute
	}
	if o.Role == RoleInstaller {
		if err := prepareDir(o.Dir); err != nil {
			return nil, fmt.Errorf("managed runtimes: %w", err)
		}
	}
	m := &Manager{o: o, catalog: map[string]*usable{}, commands: map[string]string{}, rejected: map[string]string{},
		inflight: map[string]*call{}, failed: map[string]failure{}, inUse: map[string]int{},
		markers: map[string]*marker{}, logged: map[string]bool{},
		pending: map[string]time.Time{}, damaged: map[string]string{}, hashes: newHashCache()}
	m.scan()
	m.readStatusLocked() // failures (and their backoff) survive helper runs
	return m, nil
}

// Dir returns the install prefix.
func (m *Manager) Dir() string { return m.o.Dir }

// scan loads the markers of installed packages.
func (m *Manager) scan() {
	m.markers = map[string]*marker{}
	names, _ := os.ReadDir(m.o.Dir)
	for _, n := range names {
		if !n.IsDir() || !PackageNameRe.MatchString(n.Name()) {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(m.o.Dir, n.Name()))
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			root := filepath.Join(m.o.Dir, n.Name(), strings.TrimSuffix(f.Name(), ".json"))
			b, err := os.ReadFile(root + ".json")
			if err != nil {
				continue
			}
			var mk marker
			if json.Unmarshal(b, &mk) != nil || mk.Name != n.Name() {
				continue
			}
			if st, err := os.Lstat(root); err != nil || !st.IsDir() {
				continue
			}
			m.markers[root] = &mk
		}
	}
}

// Revision is the catalog revision last loaded (0 = none yet).
func (m *Manager) Revision() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rev
}

// SetCatalog loads a catalog from the router. Packages that fail signature
// verification, are not allowed by agent.yaml or have no build for this
// Mac are left out (and logged once).
func (m *Manager) SetCatalog(c api.AgentRuntimeCatalog) {
	cat := map[string]*usable{}
	cmds := map[string]string{}
	rejected := map[string]string{}
	for _, e := range c.Packages {
		p, err := Verify(m.o.Keys, []byte(e.Manifest), []byte(e.Signature))
		if err != nil {
			name := "?"
			if q, perr := Parse([]byte(e.Manifest)); perr == nil {
				name = q.Name
			}
			rejected[name] = err.Error()
			continue
		}
		if !Allowed(m.o.Allow, p.Name) {
			rejected[p.Name] = "not allowed by managed_runtimes.allow"
			continue
		}
		i := ArtifactFor(p, m.o.GOOS, m.o.GOARCH)
		if i < 0 {
			rejected[p.Name] = "no build for " + m.o.GOOS + "/" + m.o.GOARCH
			continue
		}
		u := p.Artifacts[i].URL
		if i < len(e.URLs) && e.URLs[i] != "" {
			u = e.URLs[i]
		}
		if u == "" {
			rejected[p.Name] = "no download URL"
			continue
		}
		if cat[p.Name] != nil {
			continue
		}
		cat[p.Name] = &usable{p: p, art: p.Artifacts[i], url: u, prewarm: e.Prewarm}
	}
	// A command provided by two packages goes to the first by name.
	names := make([]string, 0, len(cat))
	for n := range cat {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for c := range cat[n].p.Commands {
			if _, dup := cmds[c]; !dup {
				cmds[c] = n
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, why := range rejected {
		key := n + "\x00" + why
		if !m.logged[key] {
			m.logged[key] = true
			m.o.Log.Warn("managed runtime not usable on this Mac", "package", n, "reason", why)
		}
	}
	for n, f := range m.failed {
		if u := cat[n]; u == nil || u.p.Version != f.version {
			delete(m.failed, n) // a new version gets a fresh chance
		}
	}
	m.catalog, m.commands, m.rejected = cat, cmds, rejected
	m.rev, m.loaded = c.Revision, true
}

// Provides returns the package that provides a command ("" = none).
func (m *Manager) Provides(cmd string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commands[cmd]
}

func (m *Manager) rootLocked(u *usable) string {
	return filepath.Join(m.o.Dir, u.p.Name, u.dirName())
}

func (m *Manager) installedLocked(u *usable) bool {
	mk := m.markers[m.rootLocked(u)]
	return mk != nil && mk.SHA256 == u.art.SHA256 && m.damaged[u.p.Name] == ""
}

// Status is the manager's view for the heartbeat.
type Status struct {
	// Ready maps commands of installed packages to their absolute argv.
	Ready map[string][]string
	// Installable commands and packages (allowed, signed, built for this
	// Mac, not installed, not in a failure backoff).
	InstallableCommands []string
	InstallablePackages []string
	// Unavailable maps commands to why they can't be installed right now.
	Unavailable map[string]string
	Packages    []api.RuntimePackageStatus
}

// Status reports installed, installable and failed packages.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.o.Now()
	st := Status{Ready: map[string][]string{}, Unavailable: map[string]string{}}
	reported := map[string]bool{}
	names := make([]string, 0, len(m.catalog))
	for n := range m.catalog {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := m.catalog[n]
		root := m.rootLocked(u)
		ps := api.RuntimePackageStatus{Name: n, Version: u.p.Version}
		f, failed := m.failed[n]
		switch {
		case m.installedLocked(u):
			mk := m.markers[root]
			at := mk.InstalledAt
			ps.State, ps.SizeBytes, ps.InstalledAt = api.PackageInstalled, mk.SizeBytes, &at
			inst := &Installed{Package: u.p, Root: root}
			for c := range u.p.Commands {
				if m.commands[c] == n {
					st.Ready[c] = inst.Argv(c)
				}
			}
		case m.damaged[n] != "" || !m.pending[n].IsZero():
			// Asked of the root helper: not offered until it is done, so
			// the router does not place tasks here meanwhile.
			ps.State = api.PackageInstalling
			for c := range u.p.Commands {
				if m.commands[c] == n {
					why := "installing " + n + " " + u.p.Version + " (requested from the root helper)"
					if d := m.damaged[n]; d != "" {
						why = "reinstalling " + n + ": " + d
					}
					st.Unavailable[c] = why
				}
			}
		case m.inflight[n] != nil:
			ps.State = api.PackageInstalling
			st.InstallablePackages = append(st.InstallablePackages, n)
			for c := range u.p.Commands {
				if m.commands[c] == n {
					st.InstallableCommands = append(st.InstallableCommands, c)
				}
			}
		case failed && now.Before(f.retry):
			retry := f.retry
			ps.State, ps.Error, ps.RetryAt = api.PackageFailed, f.err, &retry
			for c := range u.p.Commands {
				if m.commands[c] == n {
					st.Unavailable[c] = "install of " + n + " " + u.p.Version + " failed: " + f.err
				}
			}
		default:
			st.InstallablePackages = append(st.InstallablePackages, n)
			for c := range u.p.Commands {
				if m.commands[c] == n {
					st.InstallableCommands = append(st.InstallableCommands, c)
				}
			}
			ps.State = ""
		}
		if ps.State != "" {
			st.Packages = append(st.Packages, ps)
			reported[n] = true
		}
	}
	// Other installed versions (outdated, or no longer in the catalog).
	var roots []string
	for r := range m.markers {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	for _, r := range roots {
		mk := m.markers[r]
		if reported[mk.Name] {
			continue
		}
		reported[mk.Name] = true
		at := mk.InstalledAt
		st.Packages = append(st.Packages, api.RuntimePackageStatus{Name: mk.Name, Version: mk.Version,
			State: api.PackageInstalled, SizeBytes: mk.SizeBytes, InstalledAt: &at})
	}
	sort.Strings(st.InstallableCommands)
	return st
}

// Acquire makes sure a package is installed (installing it if needed) and
// pins it against garbage collection until release is called. logf, if
// set, gets progress lines (for the task's log).
func (m *Manager) Acquire(ctx context.Context, name string, logf func(string)) (*Installed, func(), error) {
	if logf == nil {
		logf = func(string) {}
	}
	for {
		m.mu.Lock()
		u := m.catalog[name]
		if u == nil {
			why := m.rejected[name]
			m.mu.Unlock()
			if why == "" {
				why = "not in the runtime catalog"
			}
			return nil, nil, fmt.Errorf("managed runtime %s: %s", name, why)
		}
		root := m.rootLocked(u)
		if m.o.Role == RoleAgent {
			if !m.installedLocked(u) {
				if f, ok := m.failed[name]; ok && f.version == u.p.Version && m.o.Now().Before(f.retry) {
					m.mu.Unlock()
					return nil, nil, fmt.Errorf("install of %s %s failed %s ago: %s", name, u.p.Version,
						m.o.Now().Sub(f.at).Round(time.Second), f.err)
				}
				if m.pending[name].IsZero() {
					m.pending[name] = m.o.Now()
					m.requestLocked()
				}
				m.mu.Unlock()
				logf(fmt.Sprintf("%s %s is not installed yet; asked the root helper to install it", name, u.p.Version))
				return nil, nil, fmt.Errorf("%w: %s %s", ErrInstallRequested, name, u.p.Version)
			}
			mk := m.markers[root]
			m.mu.Unlock()
			// Check the tree against root's file list before every use.
			if err := m.hashes.verify(root, mk.Files, rootOwned(m.o.Dir)); err != nil {
				m.mu.Lock()
				m.damaged[name] = err.Error()
				m.requestLocked()
				m.mu.Unlock()
				m.o.Log.Warn("managed runtime does not match what root installed; asking for a reinstall",
					"package", name, "version", u.p.Version, "err", err)
				return nil, nil, fmt.Errorf("managed runtime %s %s failed verification (reinstall requested): %w",
					name, u.p.Version, err)
			}
			m.mu.Lock()
			if !m.installedLocked(u) {
				m.mu.Unlock()
				continue
			}
		}
		if m.installedLocked(u) {
			m.inUse[root]++
			m.writeInUseLocked()
			m.mu.Unlock()
			var once sync.Once
			return &Installed{Package: u.p, Root: root}, func() {
				once.Do(func() {
					m.mu.Lock()
					if m.inUse[root]--; m.inUse[root] <= 0 {
						delete(m.inUse, root)
					}
					m.writeInUseLocked()
					m.mu.Unlock()
				})
			}, nil
		}
		if c := m.inflight[name]; c != nil {
			m.mu.Unlock()
			logf(fmt.Sprintf("waiting for %s %s to finish installing", name, u.p.Version))
			select {
			case <-c.done:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			if c.err != nil {
				return nil, nil, c.err
			}
			continue
		}
		if f, ok := m.failed[name]; ok && f.version == u.p.Version && m.o.Now().Before(f.retry) {
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("install of %s %s failed %s ago: %s", name, u.p.Version,
				m.o.Now().Sub(f.at).Round(time.Second), f.err)
		}
		c := &call{done: make(chan struct{})}
		m.inflight[name] = c
		m.mu.Unlock()

		mk, err := m.install(ctx, u, root, logf)
		m.mu.Lock()
		delete(m.inflight, name)
		if err != nil {
			now := m.o.Now()
			if ctx.Err() == nil {
				m.failed[name] = failure{version: u.p.Version, err: err.Error(), at: now, retry: now.Add(m.o.RetryAfter)}
			}
			c.err = fmt.Errorf("install %s %s: %w", name, u.p.Version, err)
			m.mu.Unlock()
			close(c.done)
			m.o.Log.Warn("managed runtime install failed", "package", name, "version", u.p.Version, "err", err)
			return nil, nil, c.err
		}
		delete(m.failed, name)
		m.markers[root] = mk
		m.mu.Unlock()
		close(c.done)
		m.o.Log.Info("managed runtime installed", "package", name, "version", u.p.Version, "bytes", mk.SizeBytes)
		go m.GC()
	}
}

// install downloads, verifies and unpacks a package. The install directory
// only appears (by rename) once everything checked out; the marker is
// written last.
func (m *Manager) install(ctx context.Context, u *usable, root string, logf func(string)) (*marker, error) {
	pkgDir := filepath.Dir(root)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return nil, err
	}
	chownRoot(pkgDir)
	unlock, err := lockFile(ctx, filepath.Join(pkgDir, ".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Another process may have finished it while we waited for the lock.
	if b, err := os.ReadFile(root + ".json"); err == nil {
		var mk marker
		if json.Unmarshal(b, &mk) == nil && mk.SHA256 == u.art.SHA256 {
			if st, err := os.Lstat(root); err == nil && st.IsDir() {
				return &mk, nil
			}
		}
	}
	_ = os.Remove(root + ".json")
	if err := removeTree(root); err != nil {
		return nil, err
	}

	tmpBase := filepath.Join(m.o.Dir, ".tmp")
	if err := os.MkdirAll(tmpBase, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(tmpBase, u.p.Name+"-")
	if err != nil {
		return nil, err
	}
	defer removeTree(tmp)

	logf(fmt.Sprintf("installing managed runtime %s %s (%s/%s)", u.p.Name, u.p.Version, u.art.OS, u.art.Arch))
	dl := filepath.Join(tmp, "download")
	n, err := m.download(ctx, u, dl)
	if err != nil {
		return nil, err
	}
	logf(fmt.Sprintf("downloaded %d bytes, sha256 verified", n))
	stage := filepath.Join(tmp, "root")
	if err := os.Mkdir(stage, 0o755); err != nil {
		return nil, err
	}
	if err := Extract(dl, u.art, stage); err != nil {
		return nil, fmt.Errorf("unpack: %w", err)
	}
	_ = os.Remove(dl)
	for c, argv := range u.p.Commands {
		if err := checkProgram(stage, argv[0]); err != nil {
			return nil, fmt.Errorf("command %s: %w", c, err)
		}
	}
	size, files, err := sealTree(stage)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(stage, root); err != nil {
		return nil, err
	}
	mk := &marker{Name: u.p.Name, Version: u.p.Version, SHA256: u.art.SHA256, SizeBytes: size, InstalledAt: m.o.Now().UTC(),
		Files: files}
	b, _ := json.Marshal(mk)
	if err := writeAtomic(root+".json", b); err != nil {
		_ = removeTree(root)
		return nil, err
	}
	logf(fmt.Sprintf("installed %s %s", u.p.Name, u.p.Version))
	return mk, nil
}

func (m *Manager) download(ctx context.Context, u *usable, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := m.o.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download: %s", resp.Status)
	}
	limit := m.o.MaxBytes
	if u.art.Size > 0 {
		limit = u.art.Size
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("download: %w", err)
	}
	if n > limit {
		return n, fmt.Errorf("download is larger than %d bytes", limit)
	}
	if u.art.Size > 0 && n != u.art.Size {
		return n, fmt.Errorf("download is %d bytes, manifest says %d", n, u.art.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != u.art.SHA256 {
		return n, fmt.Errorf("sha256 mismatch: got %s, manifest says %s", got, u.art.SHA256)
	}
	return n, nil
}

// checkProgram makes sure a command's program is in the package and is an
// executable regular file there.
func checkProgram(root, rel string) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("program %s is not in the package", rel)
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if r, err := filepath.Rel(rr, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("program %s points outside the package", rel)
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("program %s is not a regular file", rel)
	}
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		return fmt.Errorf("program %s is not executable", rel)
	}
	return nil
}

// removeTree deletes a (possibly read-only) tree. Missing is fine.
func removeTree(root string) error {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(root)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp-" + randHex()
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o644)
	chownRoot(tmp)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// GC removes package versions that are not the catalog's current one and
// not in use by a running task, plus leftovers of interrupted installs. It
// does nothing until a catalog has been loaded.
//
// It holds the manager's lock throughout: installs register themselves
// (under the lock) before they create anything, so GC never removes the
// work of an install that is starting or running.
func (m *Manager) GC() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded || m.o.Role == RoleAgent {
		return
	}
	keep := map[string]bool{}
	for _, u := range m.catalog {
		keep[m.rootLocked(u)] = true
	}
	for r := range m.inUse {
		keep[r] = true
	}
	for _, r := range readInUse(m.o.InUseFile, m.o.Dir) {
		keep[r] = true // used by a running task (the agent's list)
	}

	names, _ := os.ReadDir(m.o.Dir)
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		if n.Name() == ".tmp" {
			if len(m.inflight) == 0 {
				_ = removeTree(filepath.Join(m.o.Dir, ".tmp"))
			}
			continue
		}
		if !PackageNameRe.MatchString(n.Name()) || m.inflight[n.Name()] != nil {
			continue
		}
		pkgDir := filepath.Join(m.o.Dir, n.Name())
		entries, _ := os.ReadDir(pkgDir)
		left := 0
		for _, e := range entries {
			if e.Name() == ".lock" {
				continue
			}
			root := filepath.Join(pkgDir, strings.TrimSuffix(e.Name(), ".json"))
			if keep[root] {
				left++
				continue
			}
			mk := m.markers[root]
			delete(m.markers, root)
			if e.IsDir() && mk != nil {
				m.o.Log.Info("removing unused managed runtime", "package", mk.Name, "version", mk.Version)
			}
			_ = os.Remove(root + ".json")
			_ = removeTree(root)
		}
		if left == 0 {
			_ = os.Remove(filepath.Join(pkgDir, ".lock"))
			_ = os.Remove(pkgDir)
		}
	}
}

// Prewarm installs, one after another, every package the catalog marks for
// prewarming that is not installed yet (and not in a failure backoff).
func (m *Manager) Prewarm(ctx context.Context) {
	if m.o.Role == RoleAgent {
		return // the root helper prewarms
	}
	m.mu.Lock()
	var todo []string
	for n, u := range m.catalog {
		if !u.prewarm || m.installedLocked(u) || m.inflight[n] != nil {
			continue
		}
		if f, ok := m.failed[n]; ok && m.o.Now().Before(f.retry) {
			continue
		}
		todo = append(todo, n)
	}
	m.mu.Unlock()
	sort.Strings(todo)
	for _, n := range todo {
		if ctx.Err() != nil {
			return
		}
		m.o.Log.Info("prewarming managed runtime", "package", n)
		if _, release, err := m.Acquire(ctx, n, nil); err == nil {
			release()
		}
	}
}

// Check re-verifies that the installed packages' programs are still there
// (someone may have deleted them) and forgets the ones that are not, so
// they become installable again.
func (m *Manager) Check() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.Role == RoleAgent {
		m.refreshLocked()
		return
	}
	for _, u := range m.catalog {
		root := m.rootLocked(u)
		if m.markers[root] == nil || m.inUse[root] > 0 {
			continue
		}
		for _, argv := range u.p.Commands {
			if checkProgram(root, argv[0]) != nil {
				m.o.Log.Warn("managed runtime damaged; it will be reinstalled", "package", u.p.Name, "version", u.p.Version)
				delete(m.markers, root)
				_ = os.Remove(root + ".json")
				break
			}
		}
	}
}
