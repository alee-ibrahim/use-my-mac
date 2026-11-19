package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func signedPkg(t *testing.T, sk *selfupdate.SecretKey, p api.RuntimePackage) api.RuntimePublish {
	t.Helper()
	m, _ := json.Marshal(p)
	sig, err := runtimes.Sign(sk, m)
	if err != nil {
		t.Fatal(err)
	}
	return api.RuntimePublish{Manifest: string(m), Signature: string(sig)}
}

func toolPkg(name, version string, cmds ...string) api.RuntimePackage {
	c := map[string][]string{}
	for _, x := range cmds {
		c[x] = []string{"bin/" + x}
	}
	return api.RuntimePackage{Name: name, Version: version, Commands: c, Artifacts: []api.RuntimeArtifact{
		{OS: "darwin", Arch: "arm64", URL: "https://example.com/" + name + ".tgz", SHA256: strings.Repeat("ab", 32), Format: api.FormatTarGz},
		{OS: "linux", Arch: "amd64", Blob: "rt/" + name + ".tgz", SHA256: strings.Repeat("cd", 32), Format: api.FormatTarGz},
	}}
}

func TestRuntimeCatalogPublishPrewarmDelete(t *testing.T) {
	h := newHarness(t)
	_, sk, _ := selfupdate.GenerateKey()
	a := h.enrol("a", 1)
	if r := h.hb(a, goodTel(), api.AgentReady); r.CatalogRevision != 0 {
		t.Fatalf("empty catalog revision %d", r.CatalogRevision)
	}
	if _, err := h.e.PublishRuntime(h.ctx, signedPkg(t, sk, toolPkg("ffmpeg", "7.1", "ffmpeg", "ffprobe")), "admin"); err != nil {
		t.Fatal(err)
	}
	r1 := h.hb(a, goodTel(), api.AgentReady).CatalogRevision
	if r1 == 0 {
		t.Fatal("heartbeat must carry the catalog revision")
	}
	// Bad signatures and manifests are refused.
	bad := signedPkg(t, sk, toolPkg("other", "1", "other"))
	bad.Manifest = strings.Replace(bad.Manifest, `"version":"1"`, `"version":"2"`, 1)
	if _, err := h.e.PublishRuntime(h.ctx, bad, "admin"); err == nil {
		t.Fatal("signature for another version accepted")
	}
	if _, err := h.e.PublishRuntime(h.ctx, api.RuntimePublish{Manifest: "{}", Signature: "x"}, "admin"); err == nil {
		t.Fatal("garbage accepted")
	}
	// One command, one package.
	if _, err := h.e.PublishRuntime(h.ctx, signedPkg(t, sk, toolPkg("ff2", "1", "ffprobe")), "admin"); err == nil ||
		!strings.Contains(err.Error(), "already provided") {
		t.Fatalf("duplicate command: %v", err)
	}
	// Agents get blob artifacts as signed URLs.
	ac, err := h.e.AgentRuntimeCatalog(h.ctx)
	if err != nil || ac.Revision != r1 || len(ac.Packages) != 1 {
		t.Fatalf("agent catalog %+v %v", ac, err)
	}
	if ac.Packages[0].URLs[0] != "https://example.com/ffmpeg.tgz" || !strings.Contains(ac.Packages[0].URLs[1], "uploads%2Frt%2Fffmpeg.tgz") {
		t.Fatalf("urls %v", ac.Packages[0].URLs)
	}
	if err := h.e.SetRuntimePrewarm(h.ctx, "ffmpeg", true, "admin"); err != nil {
		t.Fatal(err)
	}
	r2 := h.hb(a, goodTel(), api.AgentReady).CatalogRevision
	if r2 <= r1 {
		t.Fatal("prewarm must bump the revision")
	}
	if err := h.e.SetRuntimePrewarm(h.ctx, "nope", true, "admin"); err != ErrNotFound {
		t.Fatalf("unknown package: %v", err)
	}
	// Fleet view: an older version installed, the current one installable.
	if _, err := h.e.Heartbeat(h.ctx, a, api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), MaxTasks: 1,
		Packages:            []api.RuntimePackageStatus{{Name: "ffmpeg", Version: "7.0", State: api.PackageInstalled}},
		InstallablePackages: []string{"ffmpeg"}, InstallableRuntimes: []string{"ffmpeg", "ffprobe"}}); err != nil {
		t.Fatal(err)
	}
	c, err := h.e.RuntimeCatalog(h.ctx)
	if err != nil || len(c.Packages) != 1 || !c.Packages[0].Prewarm || len(c.Packages[0].Macs) != 1 ||
		c.Packages[0].Macs[0].State != api.PackageInstallable || c.Packages[0].Macs[0].Version != "7.0" {
		t.Fatalf("catalog %+v %v", c, err)
	}
	if m := h.mac(a); strings.Join(m.InstallableRuntimes, ",") != "ffmpeg,ffprobe" || len(m.Packages) != 1 {
		t.Fatalf("mac view %+v", m)
	}
	// Survives a router restart.
	e2, err := New(h.ctx, Options{Store: h.e.st, Blobs: h.e.blobs, Config: h.e.cfg, Now: h.e.now})
	if err != nil {
		t.Fatal(err)
	}
	if c2, _ := e2.RuntimeCatalog(h.ctx); c2.Revision != r2 || len(c2.Packages) != 1 {
		t.Fatalf("after restart %+v", c2)
	}
	if err := h.e.DeleteRuntime(h.ctx, "ffmpeg", "admin"); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.e.RuntimeCatalog(h.ctx); len(c.Packages) != 0 || c.Revision <= r2 {
		t.Fatalf("after delete %+v", c)
	}
}

// Tasks go to a Mac that can install the runtime when none has it, prefer
// one that has it, and need the packages they require.
func TestSchedulingWithInstallableRuntimes(t *testing.T) {
	h := newHarness(t)
	cold := h.enrol("cold", 1)
	warm := h.enrol("warm", 1)
	hbM := func(id string, rts, inst []string, pk []api.RuntimePackageStatus, ipk []string) {
		t.Helper()
		if _, err := h.e.Heartbeat(h.ctx, id, api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), MaxTasks: 1,
			Runtimes: rts, InstallableRuntimes: inst, Packages: pk, InstallablePackages: ipk, PreflightID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	_, sk, _ := selfupdate.GenerateKey()
	if _, err := h.e.PublishRuntime(h.ctx, signedPkg(t, sk, toolPkg("sh", "1", "sh")), "admin"); err != nil {
		t.Fatal(err)
	}
	hbM(cold, []string{}, []string{"sh"}, nil, []string{"sh"})
	hbM(warm, []string{}, nil, nil, nil)
	j := h.submit(api.JobSpec{Tasks: []api.TaskSpec{simpleTask("t1")}})
	h.tick()
	if d := h.task(j.Tasks[0].ID); d.AgentID != cold {
		t.Fatalf("placed on %q, want the Mac that can install", d.AgentID)
	}
	if _, err := h.e.CancelJob(h.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	h.tick()

	// warm now has it installed: preferred over cold (same telemetry).
	hbM(warm, []string{"sh"}, nil, []api.RuntimePackageStatus{{Name: "sh", Version: "1", State: api.PackageInstalled}}, nil)
	hbM(cold, []string{}, []string{"sh"}, nil, []string{"sh"})
	spec := simpleTask("t2")
	spec.Requires = []string{"sh"}
	j = h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	if d := h.task(j.Tasks[0].ID); d.AgentID != warm {
		t.Fatalf("placed on %q, want warm", d.AgentID)
	}
	as := h.poll(warm)
	if len(as) != 1 || strings.Join(as[0].Requires, ",") != "sh" {
		t.Fatalf("assignment %+v", as)
	}

	// A package nobody has or can install: unplaced, and the placement says why.
	spec = simpleTask("t3")
	spec.Requires = []string{"missing"}
	j = h.submit(api.JobSpec{Tasks: []api.TaskSpec{spec}})
	h.tick()
	d := h.task(j.Tasks[0].ID)
	if d.AgentID != "" || len(d.Placement) == 0 || !strings.Contains(d.Placement[len(d.Placement)-1].Reason, "missing") {
		t.Fatalf("t3 %+v", d)
	}
}

func TestValidateRequires(t *testing.T) {
	for _, req := range [][]string{{"Bad Name"}, {"a", "a"}} {
		spec := api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh", Requires: req}}}
		if err := ValidateJob(&spec); err == nil {
			t.Errorf("requires %v accepted", req)
		}
	}
	spec := api.JobSpec{Tasks: []api.TaskSpec{{Key: "t", Runtime: "sh", Requires: []string{"ffmpeg"}}}}
	if err := ValidateJob(&spec); err != nil {
		t.Fatal(err)
	}
}
