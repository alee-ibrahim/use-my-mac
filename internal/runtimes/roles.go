package runtimes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Role is who a Manager acts for.
type Role int

const (
	// RoleInstaller is the root helper (`umm-agent runtimes-sync`): it
	// downloads, verifies and installs packages into a root-owned prefix
	// and removes unused ones.
	RoleInstaller Role = iota
	// RoleAgent is the unprivileged agent: it never writes into the
	// prefix. It asks the root helper for installs (Options.Request) and
	// verifies a package's files before every task that uses it.
	RoleAgent
)

// ErrInstallRequested: the package is not installed yet and the root
// helper has been asked to install it. The attempt is a Mac fault.
var ErrInstallRequested = errors.New("not installed yet; install requested from the root helper")

// StatusFileName is the root helper's report in the install prefix.
const StatusFileName = ".status.json"

// helperStatus is written by the root helper and read by the agent.
type helperStatus struct {
	UpdatedAt time.Time                `json:"updated_at"`
	Failures  map[string]helperFailure `json:"failures,omitempty"`
}

type helperFailure struct {
	Version string    `json:"version"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
	RetryAt time.Time `json:"retry_at"`
}

// prepareDir creates the install prefix. As root it must end up a real
// directory owned by root, mode 0755.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, 0, 0); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
		if uid, _ := owner(dir); uid != 0 {
			return fmt.Errorf("%s is not owned by root", dir)
		}
	}
	return nil
}

// chownRoot gives path (not following links) to root:wheel when running
// as root.
func chownRoot(path string) {
	if os.Geteuid() == 0 {
		_ = os.Lchown(path, 0, 0)
	}
}

// rootOwned reports whether dir is owned by root (then ownership and
// modes of installed files are checked too).
func rootOwned(dir string) bool {
	uid, ok := owner(dir)
	return ok && uid == 0
}

// sealTree makes an unpacked package root's: directories 0755, files 0555
// (read and execute for everyone, writable by nobody but root through a
// chmod), owner root:wheel when running as root. It returns the total size
// and every file's SHA-256 (symlinks as "symlink:<target>").
func sealTree(root string) (int64, map[string]string, error) {
	var size int64
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			files[rel] = "symlink:" + t
			chownRoot(p)
			return nil
		case d.IsDir():
			chownRoot(p)
			return os.Chmod(p, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		sum, err := hashFile(p)
		if err != nil {
			return err
		}
		size += fi.Size()
		files[rel] = sum
		chownRoot(p)
		return os.Chmod(p, 0o555)
	})
	delete(files, ".")
	return size, files, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashCache remembers file hashes by the file's identity (device, inode,
// size, mtime and ctime), so an unchanged package is not re-read before
// every task. ctime cannot be set by users, so a file rewritten in place
// and given back its old mtime still misses the cache.
type hashCache struct {
	mu sync.Mutex
	m  map[string]cachedHash
}

type cachedHash struct {
	id  fileID
	sum string
}

func newHashCache() *hashCache { return &hashCache{m: map[string]cachedHash{}} }

func (c *hashCache) sum(p string, fi fs.FileInfo) (string, error) {
	id := identity(fi)
	c.mu.Lock()
	e, ok := c.m[p]
	c.mu.Unlock()
	if ok && e.id == id {
		return e.sum, nil
	}
	s, err := hashFile(p)
	if err != nil {
		return "", err
	}
	// Only cache if the file did not change while it was read.
	if fi2, err := os.Lstat(p); err == nil && identity(fi2) == id {
		c.mu.Lock()
		c.m[p] = cachedHash{id: id, sum: s}
		c.mu.Unlock()
	}
	return s, nil
}

// verify checks an installed package root against its file list: the same
// set of entries, every file's SHA-256 and every link's target, and (when
// the prefix is root-owned) that nothing is owned by or writable for
// anyone but root.
func (c *hashCache) verify(root string, files map[string]string, wantRoot bool) error {
	if len(files) == 0 {
		return errors.New("no file list recorded for this install")
	}
	seen := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if wantRoot {
			if uid, _ := owner(p); uid != 0 {
				return fmt.Errorf("%s is not owned by root", rel)
			}
			if fi.Mode()&fs.ModeSymlink == 0 && fi.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("%s is writable by group or others (%s)", rel, fi.Mode())
			}
		}
		if rel == "." || fi.IsDir() {
			if rel != "." {
				if _, listed := files[rel]; listed {
					return fmt.Errorf("%s should be a file", rel)
				}
			}
			return nil
		}
		want, ok := files[rel]
		if !ok {
			return fmt.Errorf("unexpected file %s", rel)
		}
		seen++
		if fi.Mode()&fs.ModeSymlink != 0 {
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if want != "symlink:"+t {
				return fmt.Errorf("link %s changed", rel)
			}
			return nil
		}
		if !fi.Mode().IsRegular() || strings.HasPrefix(want, "symlink:") {
			return fmt.Errorf("%s is not the file that was installed", rel)
		}
		got, err := c.sum(p, fi)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s does not match its SHA-256", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(files) {
		return fmt.Errorf("%d of %d files are missing", len(files)-seen, len(files))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Requests (agent -> root helper)
// ---------------------------------------------------------------------------

// Request is what the agent asks of the root helper.
type Request struct {
	Install   []string  `json:"install,omitempty"`
	Reinstall []string  `json:"reinstall,omitempty"`
	At        time.Time `json:"at"`
}

// maxRequestBytes caps the request file root reads.
const maxRequestBytes = 64 << 10

// WriteRequest writes the request file atomically (agent).
func WriteRequest(path string, r Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + randHex()
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ReadRequest reads the agent's request file (root). The file lives in a
// directory the agent controls, so it is opened without following links,
// must be a small regular file, and only valid package names are taken
// from it. A missing file is an empty request.
func ReadRequest(path string) (Request, error) {
	var r Request
	b, err := readSmallFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("request file: %w", err)
	}
	var raw Request
	if json.Unmarshal(b, &raw) != nil {
		return r, errors.New("request file is not valid JSON")
	}
	clean := func(in []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, n := range in {
			if PackageNameRe.MatchString(n) && !seen[n] && len(out) < 256 {
				seen[n] = true
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	r.Install, r.Reinstall, r.At = clean(raw.Install), clean(raw.Reinstall), raw.At
	return r, nil
}

// requestLocked hands the current request (pending installs and damaged
// packages) to Options.Request when it changed. Caller holds m.mu.
func (m *Manager) requestLocked() {
	if m.o.Request == nil {
		return
	}
	var inst, re []string
	for n := range m.pending {
		inst = append(inst, n)
	}
	for n := range m.damaged {
		re = append(re, n)
	}
	sort.Strings(inst)
	sort.Strings(re)
	key := strings.Join(inst, ",") + "|" + strings.Join(re, ",")
	if key == m.lastReq {
		return
	}
	m.lastReq = key
	m.o.Request(inst, re)
}

// RequestSync wakes the root helper even if nothing new is requested
// (agent role, after the catalog changed): it then also prewarms and
// removes versions the catalog no longer has.
func (m *Manager) RequestSync() {
	m.mu.Lock()
	m.lastReq = "\x00"
	m.mu.Unlock()
	m.RequestPrewarm()
}

// RequestPrewarm asks the root helper for the catalog's prewarm packages
// that are not installed yet (agent role).
func (m *Manager) RequestPrewarm() {
	if m.o.Role != RoleAgent {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.o.Now()
	for n, u := range m.catalog {
		if !u.prewarm || m.installedLocked(u) || !m.pending[n].IsZero() {
			continue
		}
		if f, ok := m.failed[n]; ok && f.version == u.p.Version && now.Before(f.retry) {
			continue
		}
		m.pending[n] = now
	}
	m.requestLocked()
}

// readStatusLocked loads the root helper's failures (agent role).
func (m *Manager) readStatusLocked() {
	b, err := os.ReadFile(filepath.Join(m.o.Dir, StatusFileName))
	if err != nil {
		return
	}
	var st helperStatus
	if json.Unmarshal(b, &st) != nil {
		return
	}
	for n, f := range st.Failures {
		m.failed[n] = failure{version: f.Version, err: f.Error, at: f.At, retry: f.RetryAt}
	}
}

// refreshLocked re-reads what root installed and settles pending and
// damaged packages (agent role, every preflight interval).
func (m *Manager) refreshLocked() {
	m.scan()
	m.readStatusLocked()
	now := m.o.Now()
	for n, since := range m.pending {
		u := m.catalog[n]
		switch {
		case u == nil:
			delete(m.pending, n)
		case m.markers[m.rootLocked(u)] != nil && m.markers[m.rootLocked(u)].SHA256 == u.art.SHA256:
			delete(m.pending, n)
		case func() bool { f, ok := m.failed[n]; return ok && f.version == u.p.Version && f.at.After(since) }():
			delete(m.pending, n)
		case now.Sub(since) > m.o.RequestTimeout:
			delete(m.pending, n)
			m.failed[n] = failure{version: u.p.Version, at: now, retry: now.Add(m.o.RetryAfter),
				err: "the root helper did not install it within " + m.o.RequestTimeout.String()}
		}
	}
	for n := range m.damaged {
		u := m.catalog[n]
		if u == nil {
			delete(m.damaged, n)
			continue
		}
		root := m.rootLocked(u)
		mk := m.markers[root]
		if mk == nil {
			delete(m.damaged, n) // removed: installable (and requested) again
			m.pending[n] = now
			continue
		}
		if m.hashes.verify(root, mk.Files, rootOwned(m.o.Dir)) == nil {
			delete(m.damaged, n) // reinstalled
		}
	}
	m.requestLocked()
}

// ---------------------------------------------------------------------------
// Root helper
// ---------------------------------------------------------------------------

// Sync is one run of the root helper: load the catalog (verified as
// always), reinstall packages the agent reported damaged if root's own
// check agrees, install the requested and prewarm packages, remove unused
// versions and write the status the agent reads.
func (m *Manager) Sync(ctx context.Context, cat api.AgentRuntimeCatalog, requestFile string) error {
	if m.o.Role != RoleInstaller {
		return errors.New("runtimes: Sync needs the installer role")
	}
	m.SetCatalog(cat)
	for i := 0; i < 3; i++ {
		req, rerr := ReadRequest(requestFile)
		if rerr != nil {
			m.o.Log.Warn("ignoring the agent's runtime request", "err", rerr)
		}
		m.handleRequest(ctx, req)
		// A request written while this pass ran may have found the helper
		// busy: handle it now rather than at the next trigger.
		if again, err := ReadRequest(requestFile); err != nil || again.At.Equal(req.At) {
			break
		}
	}
	m.Prewarm(ctx)
	m.GC()
	return m.writeStatus()
}

// handleRequest reinstalls damaged packages and installs requested ones.
func (m *Manager) handleRequest(ctx context.Context, req Request) {
	m.mu.Lock()
	var todo []string
	for _, n := range req.Reinstall {
		u := m.catalog[n]
		if u == nil {
			continue
		}
		root := m.rootLocked(u)
		mk := m.markers[root]
		if mk == nil || m.inUse[root] > 0 {
			continue
		}
		if err := newHashCache().verify(root, mk.Files, rootOwned(m.o.Dir)); err != nil {
			m.o.Log.Warn("managed runtime damaged; reinstalling", "package", n, "version", u.p.Version, "err", err)
			delete(m.markers, root)
			delete(m.failed, n)
			_ = os.Remove(root + ".json")
			_ = removeTree(root)
			todo = append(todo, n)
		}
	}
	for _, n := range req.Install {
		if m.catalog[n] != nil {
			todo = append(todo, n)
		}
	}
	m.mu.Unlock()
	seen := map[string]bool{}
	for _, n := range todo {
		if seen[n] || ctx.Err() != nil {
			continue
		}
		seen[n] = true
		if _, release, err := m.Acquire(ctx, n, nil); err == nil {
			release()
		}
	}
}

// inUseList is the agent's in-use file.
type inUseList struct {
	Roots []string `json:"roots"`
}

// writeInUseLocked records the roots running tasks use (agent role).
func (m *Manager) writeInUseLocked() {
	if m.o.Role != RoleAgent || m.o.InUseFile == "" {
		return
	}
	l := inUseList{Roots: []string{}}
	for r := range m.inUse {
		l.Roots = append(l.Roots, r)
	}
	sort.Strings(l.Roots)
	b, _ := json.Marshal(l)
	tmp := m.o.InUseFile + ".tmp-" + randHex()
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		if err := os.Rename(tmp, m.o.InUseFile); err != nil {
			os.Remove(tmp)
		}
	}
}

// readInUse reads the agent's in-use list (root), hardened like
// ReadRequest. Only roots inside dir are returned; the list can only keep
// versions on disk longer, never remove or install anything.
func readInUse(path, dir string) []string {
	if path == "" {
		return nil
	}
	b, err := readSmallFile(path)
	if err != nil {
		return nil
	}
	var l inUseList
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	var out []string
	for _, r := range l.Roots {
		if c := filepath.Clean(r); strings.HasPrefix(c, prefix) && len(out) < 1024 {
			out = append(out, c)
		}
	}
	return out
}

// readSmallFile opens a file in an agent-controlled place without following
// links and reads it if it is a small regular file.
func readSmallFile(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > maxRequestBytes {
		return nil, errors.New("too large")
	}
	return io.ReadAll(io.LimitReader(f, maxRequestBytes+1))
}

// writeStatus records failures for the agent (installer role).
func (m *Manager) writeStatus() error {
	m.mu.Lock()
	st := helperStatus{UpdatedAt: m.o.Now().UTC(), Failures: map[string]helperFailure{}}
	for n, f := range m.failed {
		st.Failures[n] = helperFailure{Version: f.version, Error: f.err, At: f.at, RetryAt: f.retry}
	}
	m.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(m.o.Dir, StatusFileName), b)
}
