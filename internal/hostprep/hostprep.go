// Package hostprep is the root-side preparation of a Mac for running tasks:
// a dedicated, quota-limited APFS work volume and, optionally, a pf anchor
// that limits the network of the agent's user. It runs as root from the
// installers (`umm-agent prepare`) and at the start of every root updater
// cycle, so already-enrolled Macs pick changes up without SSH.
//
// It writes its outcome to a status file (root-owned, readable by the
// agent) that the unprivileged agent checks before taking work.
package hostprep

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config is what hostprep needs from agent.yaml.
type Config struct {
	// WorkDir is the agent's work dir; the volume is mounted there.
	WorkDir string
	// AgentUID / AgentGID own the work volume and are the uid the pf
	// anchor applies to (the agent's user, _umm).
	AgentUID, AgentGID int
	Volume             VolumeConfig
	Network            NetworkConfig
	// StatusFile is where the outcome is written (agent-readable).
	StatusFile string
	// StateDir is a root-only directory for generated files (pf rules).
	StateDir string
}

// VolumeConfig configures the dedicated work volume.
type VolumeConfig struct {
	Enabled bool
	// Name of the APFS volume (default "umm-work").
	Name string
	// Quota: "auto" (the container's size minus the reserve) or a fixed
	// size as diskutil takes it, e.g. "400g".
	Quota string
	// Reserve is kept free for the Mac's user (see Reserve).
	Reserve ReserveConfig
}

// ReserveConfig is the disk space always kept free for the Mac's user:
// the larger of MinBytes and Percent of the disk.
type ReserveConfig struct {
	MinBytes uint64
	Percent  float64
}

// Bytes returns the reserve for a disk of total bytes.
func (r ReserveConfig) Bytes(total uint64) uint64 {
	p := uint64(float64(total) * r.Percent / 100)
	return max(p, r.MinBytes)
}

// AutoQuota returns the work volume quota for a container of total bytes
// in whole GiB as diskutil takes it ("" if the disk is too small).
func AutoQuota(total uint64, r ReserveConfig) string {
	res := r.Bytes(total)
	if total <= res {
		return ""
	}
	g := (total - res) >> 30
	if g < 1 {
		return ""
	}
	return fmt.Sprintf("%dg", g)
}

// NetworkConfig configures the task network policy.
type NetworkConfig struct {
	// Mode: "allow" (no rules) or "deny" (the agent's user may only reach
	// the Allow hosts over TCP/UDP).
	Mode string
	// Allow lists host names, IP addresses or CIDRs.
	Allow []string
}

// Status is written to Config.StatusFile.
type Status struct {
	UpdatedAt  time.Time     `json:"updated_at"`
	WorkVolume VolumeStatus  `json:"work_volume"`
	Network    NetworkStatus `json:"network"`
}

// Work volume states.
const (
	VolumeDedicated = "dedicated" // mounted at the work dir
	VolumeSystem    = "system"    // fallback: a plain directory
	VolumeDisabled  = "disabled"  // work_volume.enabled: false
)

// VolumeStatus is the work volume's state.
type VolumeStatus struct {
	State  string `json:"state"`
	Name   string `json:"name,omitempty"`
	Device string `json:"device,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Mount  string `json:"mount,omitempty"`
	Quota  string `json:"quota,omitempty"`
	// Reason explains a fallback.
	Reason string `json:"reason,omitempty"`
}

// NetworkStatus is the pf anchor's state.
type NetworkStatus struct {
	Mode    string `json:"mode"`
	Applied bool   `json:"applied"`
	// BootTime (Unix seconds, kern.boottime) when the rules were loaded:
	// pf rules do not survive a reboot, so the agent only trusts them when
	// this matches the current boot.
	BootTime int64    `json:"boot_time,omitempty"`
	Allowed  []string `json:"allowed,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs real commands with a timeout.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Host abstracts the system for tests.
type Host struct {
	Run Runner
	// Fstab is /etc/fstab. It is not written any more; only the line and
	// temporary file older versions left are cleaned up (see cleanupFstab).
	Fstab string
	// Lookup resolves a host name to addresses.
	Lookup func(ctx context.Context, host string) ([]string, error)
	// BootTime returns kern.boottime (Unix seconds).
	BootTime func() int64
	// Mounted reports whether dir is a mount point.
	Mounted func(dir string) bool
	Log     *slog.Logger
}

// Prepare brings the Mac to the configured state and writes the status
// file. Failures are reported in the status, not returned: the agent reads
// them and reports them to the router.
func Prepare(ctx context.Context, cfg Config, h Host) Status {
	if h.Log == nil {
		h.Log = slog.Default()
	}
	prev, _ := ReadStatus(cfg.StatusFile)
	st := Status{UpdatedAt: time.Now().UTC()}
	cleanupFstab(h.Fstab, h.Log)
	st.WorkVolume = ensureVolume(ctx, cfg, h, prev.WorkVolume)
	st.Network = applyNetwork(ctx, cfg, h)
	if err := WriteStatus(cfg.StatusFile, st); err != nil {
		h.Log.Warn("write host status", "file", cfg.StatusFile, "err", err)
	}
	return st
}

// ReadStatus reads a status file (a missing file is the zero Status).
func ReadStatus(path string) (Status, error) {
	var st Status
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// WriteStatus writes the status atomically, mode 0644.
func WriteStatus(path string, st Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o644)
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Work volume
// ---------------------------------------------------------------------------

var (
	volNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	quotaRe   = regexp.MustCompile(`^[0-9]+[kKmMgGtT]?$`)
	devRe     = regexp.MustCompile(`^disk[0-9]+(s[0-9]+)*$`)
	uuidRe    = regexp.MustCompile(`^[0-9A-Fa-f-]{36}$`)
)

// ValidVolume checks a volume config.
func ValidVolume(v VolumeConfig) error {
	if !volNameRe.MatchString(v.Name) {
		return fmt.Errorf("work_volume.name %q is invalid", v.Name)
	}
	if v.Quota != "auto" && !quotaRe.MatchString(v.Quota) {
		return fmt.Errorf("work_volume.quota %q is invalid (use auto or a size such as 400g)", v.Quota)
	}
	return nil
}

func ensureVolume(ctx context.Context, cfg Config, h Host, prev VolumeStatus) VolumeStatus {
	v := cfg.Volume
	if !v.Enabled {
		return VolumeStatus{State: VolumeDisabled}
	}
	fallback := func(format string, a ...any) VolumeStatus {
		why := fmt.Sprintf(format, a...)
		h.Log.Warn("work volume unavailable; tasks use the system volume", "reason", why)
		return VolumeStatus{State: VolumeSystem, Name: v.Name, Mount: cfg.WorkDir, Reason: why}
	}
	if err := ValidVolume(v); err != nil {
		return fallback("%v", err)
	}
	container, err := dataContainer(ctx, h)
	if err != nil {
		return fallback("find the APFS container: %v", err)
	}
	vol, err := findVolume(ctx, h, container, v.Name)
	if err != nil {
		return fallback("list APFS volumes: %v", err)
	}
	quota := v.Quota
	if quota == "auto" {
		if quota = AutoQuota(vol.containerSize, v.Reserve); quota == "" {
			return fallback("disk too small for a work volume after the %d GiB reserve", v.Reserve.Bytes(vol.containerSize)>>30)
		}
	}
	if vol.dev == "" {
		// Never create a second one if a previous run made one we can no
		// longer see (e.g. diskutil output changed): fall back instead.
		if prev.UUID != "" {
			return fallback("volume %s (%s) recorded earlier is missing", v.Name, prev.UUID)
		}
		h.Log.Info("creating work volume", "container", container, "name", v.Name, "quota", quota)
		// -nomount: diskutil would otherwise mount it under /Volumes, where
		// the Mac's user would see it. It only affects this one mount.
		if _, err := h.Run(ctx, "/usr/sbin/diskutil", "apfs", "addVolume", container, "APFS", v.Name,
			"-quota", quota, "-nomount"); err != nil {
			return fallback("create volume: %v", err)
		}
		if vol, err = findVolume(ctx, h, container, v.Name); err != nil || vol.dev == "" {
			return fallback("volume %s not found after creating it: %v", v.Name, err)
		}
	}
	if prev.Quota != "" && prev.UUID == vol.uuid {
		quota = prev.Quota // set when the volume was created
	}
	st := VolumeStatus{State: VolumeDedicated, Name: v.Name, Device: vol.dev, UUID: vol.uuid, Mount: cfg.WorkDir,
		Quota: quota}
	mp, err := mountPoint(ctx, h, vol.dev)
	if err != nil {
		return fallback("volume info: %v", err)
	}
	if mp != cfg.WorkDir {
		// Something else mounted at the work dir: leave it alone.
		if h.Mounted != nil && h.Mounted(cfg.WorkDir) {
			return fallback("another file system is mounted at %s", cfg.WorkDir)
		}
		// Mounting over a work dir that holds a running task's files would
		// hide them from it. The agent removes each task dir when the task
		// ends, so an empty dir means no task is running.
		busy, err := dirBusy(cfg.WorkDir)
		if err != nil {
			return fallback("work dir: %v", err)
		}
		if busy {
			return fallback("work dir %s is in use; the volume is mounted on a later run", cfg.WorkDir)
		}
		if mp != "" {
			// Typically auto-mounted under /Volumes at boot: move it.
			h.Log.Info("moving work volume to the work dir", "from", mp, "to", cfg.WorkDir)
			if _, err := h.Run(ctx, "/usr/sbin/diskutil", "unmount", vol.dev); err != nil {
				return fallback("unmount from %s: %v", mp, err)
			}
		}
		if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
			return fallback("work dir: %v", err)
		}
		// Through Disk Arbitration (so diskutil and Finder know the mount),
		// with nobrowse: hidden from Finder and the desktop.
		if _, err := h.Run(ctx, "/usr/sbin/diskutil", "mount", "nobrowse", "-mountPoint", cfg.WorkDir,
			vol.dev); err != nil {
			return fallback("mount: %v", err)
		}
		if mp, err = mountPoint(ctx, h, vol.dev); err != nil || mp != cfg.WorkDir {
			return fallback("volume is at %q, not %s, after mounting: %v", mp, cfg.WorkDir, err)
		}
		h.Log.Info("work volume mounted", "device", vol.dev, "mount", cfg.WorkDir)
	}
	if h.Mounted != nil && !h.Mounted(cfg.WorkDir) {
		return fallback("%s is not a mount point after mounting", cfg.WorkDir)
	}
	// Honour ownership on the volume, then give its root to the agent's user.
	if _, err := h.Run(ctx, "/usr/sbin/diskutil", "enableOwnership", vol.dev); err != nil {
		h.Log.Warn("enable ownership on work volume", "err", err)
	}
	if err := os.Chown(cfg.WorkDir, cfg.AgentUID, cfg.AgentGID); err != nil {
		return fallback("chown work volume: %v", err)
	}
	if err := os.Chmod(cfg.WorkDir, 0o700); err != nil {
		return fallback("chmod work volume: %v", err)
	}
	// The hidden flag lives on the volume's root directory, so it also
	// applies when macOS mounts the volume itself (under /Volumes at boot,
	// before the updater moves it). Best effort.
	if _, err := h.Run(ctx, "/usr/bin/chflags", "hidden", cfg.WorkDir); err != nil {
		h.Log.Warn("hide work volume root", "err", err)
	}
	// Keep Spotlight and Time Machine off it (best effort; nothing shows).
	_, _ = h.Run(ctx, "/usr/bin/mdutil", "-i", "off", cfg.WorkDir)
	_, _ = h.Run(ctx, "/usr/bin/tmutil", "addexclusion", "-v", cfg.WorkDir)
	return st
}

// dirBusy reports whether dir holds anything but Finder droppings.
func dirBusy(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range ents {
		switch e.Name() {
		case ".DS_Store", ".fseventsd", ".Spotlight-V100", ".Trashes", ".TemporaryItems":
			continue
		}
		return true, nil
	}
	return false, nil
}

// apfsVolume is the work volume (dev == "" if it does not exist) and the
// size of the container it lives in.
type apfsVolume struct {
	dev, uuid     string
	containerSize uint64
}

// dataContainer returns the APFS container of the data volume.
func dataContainer(ctx context.Context, h Host) (string, error) {
	var last error
	for _, p := range []string{"/System/Volumes/Data", "/"} {
		out, err := h.Run(ctx, "/usr/sbin/diskutil", "info", "-plist", p)
		if err != nil {
			last = err
			continue
		}
		v, err := DecodePlist(out)
		if err != nil {
			last = err
			continue
		}
		if c := str(v, "APFSContainerReference"); devRe.MatchString(c) {
			return c, nil
		}
		last = fmt.Errorf("%s: no APFS container", p)
	}
	return "", last
}

func findVolume(ctx context.Context, h Host, container, name string) (*apfsVolume, error) {
	out, err := h.Run(ctx, "/usr/sbin/diskutil", "apfs", "list", "-plist")
	if err != nil {
		return nil, err
	}
	v, err := DecodePlist(out)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	cs, _ := m["Containers"].([]any)
	for _, c := range cs {
		if str(c, "ContainerReference") != container {
			continue
		}
		cm, _ := c.(map[string]any)
		size, _ := cm["CapacityCeiling"].(int64)
		if size <= 0 {
			return nil, fmt.Errorf("container %s: no CapacityCeiling", container)
		}
		out := &apfsVolume{containerSize: uint64(size)}
		vs, _ := cm["Volumes"].([]any)
		for _, vol := range vs {
			if str(vol, "Name") != name {
				continue
			}
			dev, id := str(vol, "DeviceIdentifier"), str(vol, "APFSVolumeUUID")
			if !devRe.MatchString(dev) || !uuidRe.MatchString(id) {
				return nil, fmt.Errorf("volume %s: unexpected identifiers %q %q", name, dev, id)
			}
			out.dev, out.uuid = dev, strings.ToUpper(id)
		}
		return out, nil
	}
	return nil, fmt.Errorf("APFS container %s not listed", container)
}

func mountPoint(ctx context.Context, h Host, dev string) (string, error) {
	out, err := h.Run(ctx, "/usr/sbin/diskutil", "info", "-plist", dev)
	if err != nil {
		return "", err
	}
	v, err := DecodePlist(out)
	if err != nil {
		return "", err
	}
	return str(v, "MountPoint"), nil
}

// FstabMarker ends the /etc/fstab line versions up to v0.1.8 tried to add.
const FstabMarker = "# use-my-mac work volume"

// cleanupFstab undoes what versions up to v0.1.8 did to /etc/fstab: it
// removes their leftover temporary file and, if they managed to add one,
// their line. Since macOS 26.6 the sandbox protects /etc/fstab (writing it
// needs Full Disk Access), so the work volume no longer uses it. Nothing
// is touched unless our line is there, and the file is only replaced
// atomically (temporary file and rename): if that is not permitted the
// file stays as it is.
func cleanupFstab(path string, log *slog.Logger) {
	if path == "" {
		return
	}
	tmp := path + ".umm.tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		log.Debug("remove leftover fstab temp file", "file", tmp, "err", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), FstabMarker) {
		return
	}
	var keep []string
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if strings.HasSuffix(strings.TrimRight(l, "\n"), FstabMarker) {
			continue
		}
		keep = append(keep, l)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "")), fi.Mode().Perm()); err != nil {
		log.Debug("old fstab line left in place", "err", err)
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Debug("old fstab line left in place", "err", err)
		_ = os.Remove(tmp)
		return
	}
	log.Info("removed the old work volume line from fstab", "file", path)
}

// ---------------------------------------------------------------------------
// Network (pf)
// ---------------------------------------------------------------------------

// Anchor is the pf anchor the rules are loaded into. macOS's stock
// /etc/pf.conf evaluates `anchor "com.apple/*"`, so a sub-anchor there is
// evaluated without editing pf.conf.
const Anchor = "com.apple/250.use-my-mac"

func applyNetwork(ctx context.Context, cfg Config, h Host) NetworkStatus {
	mode := cfg.Network.Mode
	if mode == "" {
		mode = "allow"
	}
	st := NetworkStatus{Mode: mode}
	if mode != "deny" {
		// Remove rules a previous "deny" left (no-op if none).
		_, _ = h.Run(ctx, "/sbin/pfctl", "-a", Anchor, "-F", "all")
		return st
	}
	fail := func(format string, a ...any) NetworkStatus {
		st.Reason = fmt.Sprintf(format, a...)
		h.Log.Warn("task network policy not applied", "reason", st.Reason)
		return st
	}
	if cfg.AgentUID <= 0 {
		return fail("cannot tell the agent's uid (got %d)", cfg.AgentUID)
	}
	var addrs []string
	for _, a := range cfg.Network.Allow {
		got, err := resolve(ctx, h, a)
		if err != nil {
			return fail("resolve %s: %v", a, err)
		}
		addrs = append(addrs, got...)
	}
	rules := Rules(cfg.AgentUID, addrs)
	main, err := h.Run(ctx, "/sbin/pfctl", "-s", "rules")
	if err != nil {
		return fail("read pf rules: %v", err)
	}
	if !strings.Contains(string(main), `anchor "com.apple/*"`) {
		return fail(`the main pf ruleset does not evaluate anchor "com.apple/*" (was /etc/pf.conf changed?)`)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fail("%v", err)
	}
	file := filepath.Join(cfg.StateDir, "pf-task-network.conf")
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		return fail("%v", err)
	}
	if _, err := h.Run(ctx, "/sbin/pfctl", "-a", Anchor, "-f", file); err != nil {
		return fail("load pf anchor: %v", err)
	}
	info, err := h.Run(ctx, "/sbin/pfctl", "-s", "info")
	if err != nil {
		return fail("pf status: %v", err)
	}
	if !strings.Contains(string(info), "Status: Enabled") {
		// -E takes a reference: pf stays on and other users of pf (which
		// enable it the same way) are not affected.
		if _, err := h.Run(ctx, "/sbin/pfctl", "-E"); err != nil {
			return fail("enable pf: %v", err)
		}
	}
	loaded, err := h.Run(ctx, "/sbin/pfctl", "-a", Anchor, "-s", "rules")
	if err != nil || !strings.Contains(string(loaded), fmt.Sprintf("user = %d", cfg.AgentUID)) {
		return fail("pf anchor did not load as expected: %v %s", err, strings.TrimSpace(string(loaded)))
	}
	st.Applied, st.Allowed = true, addrs
	if h.BootTime != nil {
		st.BootTime = h.BootTime()
	}
	return st
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$`)

// resolve turns an allow entry into pf addresses.
func resolve(ctx context.Context, h Host, entry string) ([]string, error) {
	if isAddr(entry) {
		return []string{entry}, nil
	}
	if !hostRe.MatchString(entry) {
		return nil, fmt.Errorf("not a host name, address or CIDR")
	}
	if h.Lookup == nil {
		return nil, fmt.Errorf("no resolver")
	}
	got, err := h.Lookup(ctx, entry)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range got {
		if isAddr(a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no addresses")
	}
	return out, nil
}

// Rules renders the anchor: the agent's user may open TCP/UDP connections
// to the allowed addresses only. pf matches `user` against the credentials
// of the socket that sent the packet (xnu pf_socket_lookup), so this covers
// every process running as that user — the agent and its tasks alike, which
// is why the router is always allowed.
func Rules(uid int, allowed []string) string {
	var b strings.Builder
	b.WriteString("# use-my-mac task network policy (generated; do not edit)\n")
	if len(allowed) > 0 {
		fmt.Fprintf(&b, "table <umm_allow> const { %s }\n", strings.Join(allowed, ", "))
		fmt.Fprintf(&b, "pass out quick proto { tcp, udp } from any to <umm_allow> user %d keep state\n", uid)
	}
	fmt.Fprintf(&b, "block return out quick proto { tcp, udp } from any to any user %d\n", uid)
	return b.String()
}
