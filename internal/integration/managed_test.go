package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent"
	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// toolPackage builds a tar.gz with bin/<name>: a script that runs the test
// binary in helper mode, standing in for a tool such as ffmpeg.
func toolPackage(t *testing.T, name string) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nexec '" + exe + "' \"$@\"\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "bin/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

type signer struct {
	t   *testing.T
	sk  *selfupdate.SecretKey
	pub string
}

func newSigner(t *testing.T) *signer {
	pub, sk, err := selfupdate.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &signer{t: t, sk: sk, pub: pub.String()}
}

// publish uploads data as a blob and publishes a signed package for it.
// sha overrides the signed hash (to simulate a bad download).
func (s *signer) publish(e *env, name, version string, data []byte, sha string, prewarm bool) {
	s.t.Helper()
	key := "runtimes/" + name + "-" + version + ".tar.gz"
	if err := e.admin.UploadBlob(e.ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
		s.t.Fatal(err)
	}
	if sha == "" {
		h := sha256.Sum256(data)
		sha = hex.EncodeToString(h[:])
	}
	p := api.RuntimePackage{Name: name, Version: version, Commands: map[string][]string{name: {"bin/" + name}},
		Artifacts: []api.RuntimeArtifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, Blob: key, SHA256: sha, Format: api.FormatTarGz}}}
	m, _ := json.Marshal(p)
	sig, err := runtimes.Sign(s.sk, m)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := e.admin.PublishRuntime(e.ctx, api.RuntimePublish{Manifest: string(m), Signature: string(sig), Prewarm: prewarm}); err != nil {
		s.t.Fatal(err)
	}
}

// managedMac starts an agent with managed runtimes on, trusting s, laid
// out like a Mac (<base>/umm/work, root's <base>/umm-runtimes). Its root
// helper runs in-process whenever the agent writes a request, as launchd's
// WatchPaths runs `umm-agent runtimes-sync` on a Mac. extra are static
// runtimes.
func (e *env) managedMac(name string, s *signer, extra map[string][]string) (*fakeMac, string) {
	e.t.Helper()
	return e.managedMacCfg(name, s, extra, nil)
}

// managedMacCfg is managedMac with a hook to adjust the agent config.
func (e *env) managedMacCfg(name string, s *signer, extra map[string][]string, mut func(*agent.Config)) (*fakeMac, string) {
	e.t.Helper()
	base := e.t.TempDir()
	dir := filepath.Join(base, "umm-runtimes")
	os.MkdirAll(filepath.Join(base, "umm", "work"), 0o700)
	var mu sync.Mutex
	f := e.macCfg(name, 1, quietTel(), extra, func(c *agent.Config) {
		on := true
		c.WorkDir = filepath.Join(base, "umm", "work")
		c.CredentialFile = filepath.Join(base, "credential")
		c.PreflightInterval = 200 * time.Millisecond
		c.ManagedRuntimes = agent.ManagedRuntimesConfig{Enabled: &on, Dir: dir, PublicKeys: []string{s.pub},
			OnRequest: func() {
				go func() {
					mu.Lock()
					defer mu.Unlock()
					if err := agent.SyncRuntimes(e.ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil && e.ctx.Err() == nil {
						e.t.Logf("root helper: %v", err)
					}
				}()
			}}
		if mut != nil {
			mut(c)
		}
	})
	return f, dir
}

func macByID(e *env, id string) api.Mac {
	ms, err := e.admin.Macs(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m
		}
	}
	e.t.Fatalf("mac %s not found", id)
	return api.Mac{}
}

func skipNoShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
}

// A task needs a tool the Mac lacks (its allowlisted Homebrew path is
// missing): the agent asks the root helper, the first attempt is a Mac
// fault (no failure counted), and once root has installed the signed
// package the task runs there. A Mac with managed runtimes off is never
// chosen.
func TestManagedRuntimeInstallsOnDemand(t *testing.T) {
	skipNoShell(t)
	e := newEnv(t)
	s := newSigner(t)
	s.publish(e, "tool", "1.0", toolPackage(t, "tool"), "", false)

	missing := map[string][]string{"tool": {"/nonexistent/homebrew/bin/tool"}}
	cold, dir := e.managedMac("cold-mac", s, missing)
	plain := e.macWith("plain-mac", 1, quietTel(), missing) // managed runtimes off

	waitFor(t, "cold-mac to offer tool as installable", 5*time.Second, func() bool {
		return slices.Contains(macByID(e, cold.id).InstallableRuntimes, "tool")
	})
	if m := macByID(e, plain.id); slices.Contains(m.InstallableRuntimes, "tool") || len(m.UnavailableRuntimes) == 0 {
		t.Fatalf("plain-mac must not offer tool: %+v", m)
	}

	zero := 0
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "managed", Tasks: []api.TaskSpec{
		{Key: "a", Runtime: "tool", Args: []string{"work", "0.1", "{out:o}", "from-managed"},
			Outputs: []api.Output{{Name: "o"}}, MaxRetries: &zero},
	}})
	if err != nil {
		t.Fatal(err)
	}
	final, err := e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded {
		t.Fatalf("job: %v %+v", err, final)
	}
	task := final.Tasks[0]
	if task.AgentID != cold.id {
		t.Fatalf("ran on %s, want cold-mac", task.AgentID)
	}
	if d, _ := e.app.Task(e.ctx, task.ID); d == nil || len(d.Attempts) < 2 || d.Attempts[0].Outcome != api.OutcomeUnavailable ||
		!strings.Contains(d.Attempts[0].Error, "install requested") || d.Failures != 0 {
		t.Fatalf("first attempt must be an install request (Mac fault): %+v", d)
	}

	waitFor(t, "cold-mac to report tool installed", 5*time.Second, func() bool {
		m := macByID(e, cold.id)
		return slices.Contains(m.Runtimes, "tool") && len(m.Packages) == 1 && m.Packages[0].State == api.PackageInstalled
	})
	rc, err := e.admin.Runtimes(e.ctx)
	if err != nil || len(rc.Packages) != 1 {
		t.Fatalf("catalog: %v %+v", err, rc)
	}
	var states []string
	for _, m := range rc.Packages[0].Macs {
		states = append(states, m.Name+"="+m.State)
	}
	if strings.Join(states, ",") != "cold-mac=installed" {
		t.Fatalf("fleet state: %v", states)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "tool")); len(ents) < 2 {
		t.Fatalf("install dir: %v", ents)
	}

	// requires: the package's bin dir goes first on PATH for another runtime.
	job, err = e.app.SubmitJob(e.ctx, api.JobSpec{Name: "requires", Tasks: []api.TaskSpec{
		{Key: "w", Runtime: "helper", Requires: []string{"tool"}, Args: []string{"which", "tool", "{out:o}"},
			Outputs: []api.Output{{Name: "o"}}, MaxRetries: &zero},
	}})
	if err != nil {
		t.Fatal(err)
	}
	final, err = e.app.WaitJob(e.ctx, job.ID, 100*time.Millisecond, nil)
	if err != nil || final.State != api.StateSucceeded || final.Tasks[0].AgentID != cold.id {
		t.Fatalf("requires job: %v %+v", err, final)
	}
	var out bytes.Buffer
	if _, err := e.app.DownloadOutput(e.ctx, final.Tasks[0].ID, "o", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), dir) {
		t.Fatalf("PATH lookup found %q, want the managed copy under %s", out.String(), dir)
	}
}

// A package whose download does not match its signed hash is never
// installed. The failed install is the Mac's fault: the task is not failed,
// and the Mac stops offering the runtime.
func TestManagedRuntimeBadDownloadIsMacFault(t *testing.T) {
	skipNoShell(t)
	e := newEnv(t)
	s := newSigner(t)
	s.publish(e, "tool", "1.0", toolPackage(t, "tool"), strings.Repeat("ab", 32), false)
	mac, dir := e.managedMac("mac", s, nil)
	waitFor(t, "tool installable", 5*time.Second, func() bool {
		return slices.Contains(macByID(e, mac.id).InstallableRuntimes, "tool")
	})
	zero := 0
	job, err := e.app.SubmitJob(e.ctx, api.JobSpec{Name: "bad", Tasks: []api.TaskSpec{
		{Key: "a", Runtime: "tool", Args: []string{"work", "0", "{out:o}", "x"}, Outputs: []api.Output{{Name: "o"}}, MaxRetries: &zero},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an unavailable attempt", 10*time.Second, func() bool {
		j, _ := e.app.Job(e.ctx, job.ID)
		if j == nil || len(j.Tasks) == 0 {
			return false
		}
		d, _ := e.app.Task(e.ctx, j.Tasks[0].ID)
		return d != nil && len(d.Attempts) > 0 && d.Attempts[0].Outcome == api.OutcomeUnavailable
	})
	j, _ := e.app.Job(e.ctx, job.ID)
	if j.State == api.StateFailed || j.Tasks[0].Failures != 0 {
		t.Fatalf("task must not fail: %+v", j.Tasks[0])
	}
	waitFor(t, "runtime unavailable after the failed install", 5*time.Second, func() bool {
		m := macByID(e, mac.id)
		for _, u := range m.UnavailableRuntimes {
			if u.Name == "tool" && strings.Contains(u.Reason, "sha256 mismatch") {
				return !slices.Contains(m.InstallableRuntimes, "tool")
			}
		}
		return false
	})
	if ents, _ := os.ReadDir(filepath.Join(dir, "tool")); len(ents) > 1 {
		t.Fatalf("something was installed: %v", ents)
	}
	e.app.CancelJob(e.ctx, job.ID)
}

// An admin prewarms a package fleet-wide; a new version replaces it and the
// old one is garbage-collected.
func TestManagedRuntimePrewarmAndUpgrade(t *testing.T) {
	skipNoShell(t)
	e := newEnv(t)
	s := newSigner(t)
	s.publish(e, "tool", "1.0", toolPackage(t, "tool"), "", false)
	m1, dir1 := e.managedMac("m1", s, nil)
	m2, _ := e.managedMac("m2", s, nil)
	installed := func(version string) bool {
		rc, err := e.admin.Runtimes(e.ctx)
		if err != nil || len(rc.Packages) != 1 || rc.Packages[0].Package.Version != version {
			return false
		}
		n := 0
		for _, m := range rc.Packages[0].Macs {
			if m.State == api.PackageInstalled && (m.ID == m1.id || m.ID == m2.id) {
				n++
			}
		}
		return n == 2
	}
	time.Sleep(500 * time.Millisecond)
	if installed("1.0") {
		t.Fatal("installed without prewarm or a task")
	}
	if err := e.admin.SetRuntimePrewarm(e.ctx, "tool", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both Macs prewarmed 1.0", 10*time.Second, func() bool { return installed("1.0") })

	s.publish(e, "tool", "2.0", toolPackage(t, "tool"), "", true)
	waitFor(t, "both Macs on 2.0", 10*time.Second, func() bool { return installed("2.0") })
	waitFor(t, "1.0 removed from disk", 5*time.Second, func() bool {
		ents, _ := os.ReadDir(filepath.Join(dir1, "tool"))
		for _, en := range ents {
			if strings.HasPrefix(en.Name(), "1.0-") {
				return false
			}
		}
		return true
	})
	if err := e.admin.DeleteRuntime(e.ctx, "tool"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "package removed from disk", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir1, "tool"))
		return os.IsNotExist(err)
	})
}
