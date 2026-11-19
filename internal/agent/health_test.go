package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/hostprep"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func modeOnly(_ string, fi fs.FileInfo) string {
	if fi.Mode().Perm()&0o005 != 0 {
		return "mode " + fi.Mode().String()
	}
	return ""
}

func TestCheckHomes(t *testing.T) {
	skipWindows(t)
	root := t.TempDir()
	for name, mode := range map[string]os.FileMode{"alice": 0o750, "bob": 0o755, "carol": 0o701, "Shared": 0o777, ".hidden": 0o755} {
		p := filepath.Join(root, name)
		os.Mkdir(p, 0o700)
		os.Chmod(p, mode)
	}
	os.WriteFile(filepath.Join(root, ".localized"), nil, 0o644)
	bad, err := CheckHomes(root, []string{"Shared"}, modeOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 2 || !strings.Contains(bad[0], "bob (mode drwxr-xr-x)") || !strings.Contains(bad[1], "carol") {
		t.Fatalf("got %v", bad)
	}
	// The real probe flags world bits whoever runs it.
	fi, _ := os.Stat(filepath.Join(root, "bob"))
	if probeHome(filepath.Join(root, "bob"), fi) == "" {
		t.Fatal("probeHome must flag a 0755 home")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"50GiB": 50 << 30, "50g": 50 << 30, "512MiB": 512 << 20, "1048576": 1 << 20,
		"1.5T": 3 << 39, "20 GB": 20 << 30} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	if _, err := ParseSize("lots"); err == nil {
		t.Error("must reject junk")
	}
}

func TestConfigIsolationDefaults(t *testing.T) {
	c := &Config{}
	if err := c.normalizeIsolation("darwin"); err != nil {
		t.Fatal(err)
	}
	if c.MinFreeDisk.MinBytes != 50<<30 || *c.MinFreeDisk.Percent != 15 || c.HomeCheck.Root != "/Users" ||
		c.WorkVolume.Quota != "auto" || !c.WorkVolume.On() || c.TaskNetwork != "allow" {
		t.Fatalf("darwin defaults: %+v", c)
	}
	if r := c.Reserve().Bytes(2000 << 30); r != 300<<30 {
		t.Fatalf("reserve on 2000 GiB: %d", r)
	}
	yes := true
	c = &Config{Sandbox: &yes}
	if err := c.normalizeIsolation("linux"); err != nil || len(c.Warnings) != 1 || c.HomeCheck.Root != "" || c.MinFreeDisk.MinBytes != 0 {
		t.Fatalf("old sandbox config must warn, not fail: %v %+v", err, c)
	}
	c = &Config{TaskNetwork: "block"}
	if err := c.normalizeIsolation("darwin"); err == nil {
		t.Fatal("bad task_network must fail")
	}
}

// fakeDisk returns fixed statfs results per path.
func fakeDisk(work, sysFree, sysTotal uint64) func(string) (uint64, uint64, error) {
	return func(p string) (uint64, uint64, error) {
		if p == systemVolume() {
			return sysFree, sysTotal, nil
		}
		return work, sysTotal, nil
	}
}

// Exposed homes, a breached reserve, an unmounted volume and an unapplied
// network policy each make the Mac unhealthy (no work); a healthy Mac
// reports its work space for the scheduler.
func TestHealthStates(t *testing.T) {
	skipWindows(t)
	homes := t.TempDir()
	os.Mkdir(filepath.Join(homes, "alice"), 0o700)
	a := newTestAgent(t, map[string][]string{"sh": {"/bin/sh"}})
	a.cfg.HomeCheck.Root = homes
	a.cfg.MinFreeDisk.MinBytes = 50 << 30
	pct := 15.0
	a.cfg.MinFreeDisk.Percent = &pct
	a.cfg.HostStatusFile = filepath.Join(t.TempDir(), "host.json")
	a.homeProbe = modeOnly
	a.statfs = fakeDisk(800<<30, 600<<30, 1000<<30)

	if why := a.checkHealth(time.Now(), true); why != "" || a.healthReason() != "" {
		t.Fatalf("healthy Mac: %q / %q", why, a.healthReason())
	}
	iso := a.health.iso
	// 600 GiB free, 150 GiB reserved: 450 GiB for work.
	if iso.WorkSpaceBytes != 450<<30 || iso.ReserveBytes != 150<<30 || iso.HomeDirs != "ok" || iso.WorkVolume != "system" {
		t.Fatalf("isolation: %+v", iso)
	}

	// A home opened up.
	os.Chmod(filepath.Join(homes, "alice"), 0o755)
	a.checkHealth(time.Now(), true)
	if r := a.healthReason(); !strings.Contains(r, "alice") || a.health.iso.HomeDirs != "exposed" {
		t.Fatalf("exposed home: %q", r)
	}
	a.heartbeatOnce(t.Context()) // router unreachable; the state is local
	if a.state != api.AgentUnhealthy || a.canTakeWork() {
		t.Fatalf("state %s", a.state)
	}
	upd := a.execute(t.Context(), &run{as: api.Assignment{TaskID: "t1", Attempt: 1, Runtime: "sh"}})
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.MacFault, "alice") {
		t.Fatalf("a task placed before the check must be a Mac fault: %+v", upd)
	}
	os.Chmod(filepath.Join(homes, "alice"), 0o700)

	// Reserve breached: unhealthy, and running tasks are stopped.
	a.statfs = fakeDisk(800<<30, 100<<30, 1000<<30)
	a.running["x"] = &run{cancel: func(error) {}}
	if why := a.checkHealth(time.Now(), true); !strings.Contains(why, "below the 150.0 GiB kept") {
		t.Fatalf("reserve: %q", why)
	}
	delete(a.running, "x")

	// The root side made a dedicated volume that is not mounted.
	a.statfs = fakeDisk(800<<30, 600<<30, 1000<<30)
	hostprep.WriteStatus(a.cfg.HostStatusFile, hostprep.Status{WorkVolume: hostprep.VolumeStatus{
		State: hostprep.VolumeDedicated, Name: "umm-work"}})
	a.isMount = func(string) bool { return false }
	a.checkHealth(time.Now(), true)
	if r := a.healthReason(); !strings.Contains(r, "not mounted") {
		t.Fatalf("unmounted volume: %q", r)
	}
	a.isMount = func(string) bool { return true }
	a.checkHealth(time.Now(), true)
	if a.healthReason() != "" || a.health.iso.WorkVolume != "dedicated" {
		t.Fatalf("mounted volume: %q %+v", a.healthReason(), a.health.iso)
	}

	// task_network deny without rules in force.
	a.cfg.TaskNetwork = "deny"
	a.checkHealth(time.Now(), true)
	if r := a.healthReason(); !strings.Contains(r, "task_network deny is not in force") {
		t.Fatalf("network: %q", r)
	}
	hostprep.WriteStatus(a.cfg.HostStatusFile, hostprep.Status{
		WorkVolume: hostprep.VolumeStatus{State: hostprep.VolumeDedicated},
		Network:    hostprep.NetworkStatus{Mode: "deny", Applied: true, BootTime: hostprep.BootTime()}})
	a.checkHealth(time.Now(), true)
	if r := a.healthReason(); r != "" {
		t.Fatalf("applied network: %q", r)
	}
}

// A task that prints "No space left on device" and fails is a Mac fault.
func TestTaskOutOfSpaceIsMacFault(t *testing.T) {
	skipWindows(t)
	a := newTestAgent(t, map[string][]string{"sh": {"/bin/sh", "-c"}})
	a.goos = "test"
	upd := a.execute(t.Context(), &run{as: api.Assignment{TaskID: "t1", Attempt: 1, Runtime: "sh",
		Args: []string{"echo 'write error: No space left on device' >&2; exit 1"}}})
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.MacFault, "ran out of disk space") {
		t.Fatalf("got %+v", upd)
	}
	upd = a.execute(t.Context(), &run{as: api.Assignment{TaskID: "t2", Attempt: 1, Runtime: "sh",
		Args: []string{"exit 3"}}})
	if upd.MacFault != "" {
		t.Fatalf("an ordinary failure is the task's: %+v", upd)
	}
}
