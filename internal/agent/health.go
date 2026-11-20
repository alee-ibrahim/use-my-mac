package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/hostprep"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Mac health: conditions under which the Mac must not take work. Each is a
// Mac fault, reported as the agent state "unhealthy" with a reason; running
// tasks are left alone except when the disk reserve is breached.
//
//   - a user home directory is readable or traversable by the agent's user;
//   - free disk space is below the reserve kept for the Mac's user;
//   - the root side set up a dedicated work volume but it is not mounted;
//   - task_network is "deny" but the pf rules are not in force this boot.

// diskFullBytes: a work volume with less free space than this while tasks
// run is full; the tasks are stopped as a Mac fault.
const diskFullBytes = 64 << 20

// errDiskFull stops tasks when the disk reserve or the work volume runs out.
var errDiskFull = errors.New("not enough disk space on this Mac")

// diskFullMarkers are what tools print when a write fails for lack of space.
var diskFullMarkers = []string{"No space left on device", "Disc quota exceeded", "Disk quota exceeded"}

// healthState is the result of the last checks.
type healthState struct {
	homes     string // problem with home dirs ("" = fine)
	homesSt   string // ok, exposed, unchecked
	host      string // problem from the root side's status
	disk      string // reserve breached
	iso       api.Isolation
	checkedAt time.Time
}

// reason joins the problems ("" = healthy).
func (h *healthState) reason() string {
	var p []string
	for _, s := range []string{h.homes, h.host, h.disk} {
		if s != "" {
			p = append(p, s)
		}
	}
	return strings.Join(p, "; ")
}

// HomeProbe reports why the agent's user can get into a home directory (""
// = it cannot).
type HomeProbe func(path string, fi fs.FileInfo) string

// CheckHomes lists the home directories under root that the probe flags,
// as "path (reason)". Entries in ignore and dot-entries are skipped.
func CheckHomes(root string, ignore []string, probe HomeProbe) ([]string, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, n := range ignore {
		skip[n] = true
	}
	var bad []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || skip[n] {
			continue
		}
		p := filepath.Join(root, n)
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		if why := probe(p, fi); why != "" {
			bad = append(bad, p+" ("+why+")")
		}
	}
	sort.Strings(bad)
	return bad, nil
}

// probeHome flags a home whose mode lets others read or traverse it, and
// (when not root) one the agent's user can read or traverse anyway, e.g.
// through an ACL or a shared group. access(2) on macOS evaluates ACLs.
func probeHome(p string, fi fs.FileInfo) string {
	if m := fi.Mode().Perm(); m&0o005 != 0 {
		return "mode " + fi.Mode().String()
	}
	if os.Geteuid() == 0 {
		return ""
	}
	if canAccess(p, 4) {
		return "readable by the agent's user (" + fi.Mode().String() + ", ACL or group)"
	}
	if canAccess(p, 1) {
		return "traversable by the agent's user (" + fi.Mode().String() + ", ACL or group)"
	}
	return ""
}

// diskSpace measures the work volume and the system volume against the
// reserve.
type diskSpace struct {
	workFree, sysFree, sysTotal, reserve uint64
}

func (a *Agent) measureDisk() (diskSpace, error) {
	var d diskSpace
	wf, _, err := a.statfs(a.cfg.WorkDir)
	if err != nil {
		return d, err
	}
	d.workFree = wf
	d.sysFree, d.sysTotal, err = a.statfs(systemVolume())
	if err != nil {
		return d, err
	}
	d.reserve = a.cfg.Reserve().Bytes(d.sysTotal)
	return d, nil
}

// space is what new work may use: the work volume's free space, capped by
// the system volume's free space above the reserve (they share the disk).
func (d diskSpace) space() uint64 {
	avail := uint64(0)
	if d.sysFree > d.reserve {
		avail = d.sysFree - d.reserve
	}
	return min(d.workFree, avail)
}

func systemVolume() string {
	if fi, err := os.Stat("/System/Volumes/Data"); err == nil && fi.IsDir() {
		return "/System/Volumes/Data"
	}
	return "/"
}

// workUsed sums the sizes of files under the running tasks' work dirs.
// Dot-directories in the work dir (the mise download cache) stay when tasks
// end, so they are not counted.
func (a *Agent) workUsed() uint64 {
	var n uint64
	_ = filepath.WalkDir(a.cfg.WorkDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && filepath.Dir(p) == filepath.Clean(a.cfg.WorkDir) && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += uint64(fi.Size())
			}
		}
		return nil
	})
	return n
}

// checkHealth refreshes the health state. full also re-runs the slower
// checks (home dirs, host status, work dir usage); the disk floor is checked
// every heartbeat. It returns true when running tasks must stop because the
// disk ran out.
func (a *Agent) checkHealth(now time.Time, full bool) (stopForDisk string) {
	a.mu.Lock()
	h := a.health
	running := len(a.running)
	a.mu.Unlock()

	if full {
		h.homes, h.homesSt = "", "unchecked"
		if root := a.cfg.HomeCheck.Root; root != "" && !a.cfg.HomeCheck.Disabled {
			bad, err := CheckHomes(root, a.cfg.HomeCheck.Ignore, a.homeProbe)
			switch {
			case err != nil && os.IsNotExist(err):
			case err != nil:
				h.homes, h.homesSt = "cannot check home directories: "+err.Error(), "exposed"
			case len(bad) > 0:
				h.homes, h.homesSt = "home directories readable by the agent's user: "+strings.Join(bad, ", "), "exposed"
			default:
				h.homesSt = "ok"
			}
		}
		h.host, h.iso.WorkVolume, h.iso.WorkVolumeNote = a.checkHost()
		h.iso.Network = a.cfg.TaskNetwork
		h.iso.HomeDirs = h.homesSt
		h.checkedAt = now
	}

	h.disk = ""
	d, err := a.measureDisk()
	if errors.Is(err, errors.ErrUnsupported) {
		// no disk checks on this platform
	} else if err != nil {
		h.disk = "cannot measure disk space: " + err.Error()
	} else {
		sp := d.space()
		used := uint64(0)
		if running > 0 {
			if full || h.iso.WorkCapacityBytes == 0 {
				used = a.workUsed()
			} else if h.iso.WorkCapacityBytes > h.iso.WorkSpaceBytes {
				used = h.iso.WorkCapacityBytes - h.iso.WorkSpaceBytes
			}
		}
		h.iso.WorkSpaceBytes, h.iso.WorkCapacityBytes = sp, sp+used
		h.iso.SystemFreeBytes, h.iso.ReserveBytes = d.sysFree, d.reserve
		switch {
		case d.reserve > 0 && d.sysFree < d.reserve:
			h.disk = fmt.Sprintf("%s free on the system disk, below the %s kept for the Mac's user",
				gib(d.sysFree), gib(d.reserve))
		case d.workFree < diskFullBytes:
			h.disk = "the work volume is full (" + gib(d.workFree) + " free)"
		}
		if h.disk != "" && running > 0 {
			stopForDisk = h.disk
		}
	}
	a.mu.Lock()
	a.health = h
	a.mu.Unlock()
	return stopForDisk
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// checkHost reads the root side's status: the work volume and the network
// policy.
func (a *Agent) checkHost() (problem, volume, note string) {
	st, err := hostprep.ReadStatus(a.cfg.HostStatusFile)
	if err != nil && !os.IsNotExist(err) {
		return "", "", "host status unreadable: " + err.Error()
	}
	var probs []string
	switch st.WorkVolume.State {
	case hostprep.VolumeDedicated:
		if a.isMount(a.cfg.WorkDir) {
			volume = "dedicated"
		} else {
			volume, note = "system", "work volume "+st.WorkVolume.Name+" is not mounted"
			probs = append(probs, "the work volume is not mounted at "+a.cfg.WorkDir+" (the root updater remounts it)")
		}
	case hostprep.VolumeSystem:
		volume, note = "system", st.WorkVolume.Reason
	case hostprep.VolumeDisabled:
		volume, note = "system", "work volume turned off"
	default:
		volume, note = "system", "work volume not set up yet (the root updater creates it)"
	}
	if a.cfg.TaskNetwork == "deny" {
		n := st.Network
		bt := hostprep.BootTime()
		switch {
		case n.Mode != "deny" || !n.Applied:
			why := "not applied yet"
			if n.Reason != "" {
				why = n.Reason
			}
			probs = append(probs, "task_network deny is not in force: "+why)
		case bt != 0 && n.BootTime != bt:
			probs = append(probs, "task_network deny is not in force since the last restart (the root updater reapplies it)")
		}
	}
	return strings.Join(probs, "; "), volume, note
}

// healthReason is the current problem ("" = healthy).
func (a *Agent) healthReason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.health.reason()
}

// isDiskFull reports whether err is ENOSPC or EDQUOT.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, errEDQUOT)
}

// diskFullTap watches a task's output for "No space left on device".
type diskFullTap struct{ seen bool }

func (t *diskFullTap) Write(p []byte) (int, error) {
	if !t.seen {
		s := string(p)
		for _, m := range diskFullMarkers {
			if strings.Contains(s, m) {
				t.seen = true
				break
			}
		}
	}
	return len(p), nil
}
