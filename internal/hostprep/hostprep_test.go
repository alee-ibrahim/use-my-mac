package hostprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const infoData = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>APFSContainerReference</key><string>disk3</string>
<key>Internal</key><true/>
<key>TotalSize</key><integer>994662584320</integer>
</dict></plist>`

// 1 TiB container.
const containerSize = 1 << 40

func apfsList(withVol bool) string {
	vol := ""
	if withVol {
		vol = `<dict><key>APFSVolumeUUID</key><string>0f6a3c1e-1111-2222-3333-444455556666</string>
<key>DeviceIdentifier</key><string>disk3s7</string><key>Name</key><string>umm-work</string>
<key>Roles</key><array/></dict>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Containers</key><array>
<dict><key>ContainerReference</key><string>disk1</string><key>CapacityCeiling</key><integer>524288000</integer>
<key>Volumes</key><array>
<dict><key>APFSVolumeUUID</key><string>AAAAAAAA-1111-2222-3333-444455556666</string>
<key>DeviceIdentifier</key><string>disk1s1</string><key>Name</key><string>umm-work</string></dict></array></dict>
<dict><key>ContainerReference</key><string>disk3</string><key>CapacityCeiling</key><integer>` +
		fmt.Sprint(containerSize) + `</integer><key>Volumes</key><array>
<dict><key>APFSVolumeUUID</key><string>BBBBBBBB-1111-2222-3333-444455556666</string>
<key>DeviceIdentifier</key><string>disk3s5</string><key>Name</key><string>Data</string></dict>` + vol + `
</array></dict></array></dict></plist>`
}

// fakeMac simulates diskutil, mount and pfctl.
type fakeMac struct {
	calls     []string
	hasVol    bool
	mountedAt string
	failAdd   bool
	failMount string
	// other is a different file system mounted at this path.
	other     string
	pfMain    string
	pfEnabled bool
	anchor    string
}

func (f *fakeMac) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := filepath.Base(name) + " " + strings.Join(args, " ")
	f.calls = append(f.calls, c)
	switch {
	case strings.HasPrefix(c, "diskutil info -plist /System/Volumes/Data"):
		return []byte(infoData), nil
	case c == "diskutil apfs list -plist":
		return []byte(apfsList(f.hasVol)), nil
	case strings.HasPrefix(c, "diskutil apfs addVolume"):
		if f.failAdd {
			return []byte("Error: -69625"), errors.New("exit status 1")
		}
		f.hasVol = true
		return nil, nil
	case c == "diskutil info -plist disk3s7":
		return []byte(`<plist><dict><key>MountPoint</key><string>` + f.mountedAt + `</string></dict></plist>`), nil
	case strings.HasPrefix(c, "diskutil mount "):
		if f.failMount != "" {
			return []byte(f.failMount), errors.New("exit status 1")
		}
		for i, a := range args {
			if a == "-mountPoint" {
				f.mountedAt = args[i+1]
			}
		}
		return nil, nil
	case strings.HasPrefix(c, "diskutil unmount"):
		f.mountedAt = ""
		return nil, nil
	case c == "pfctl -s rules":
		return []byte(f.pfMain), nil
	case c == "pfctl -s info":
		if f.pfEnabled {
			return []byte("Status: Enabled for 0 days"), nil
		}
		return []byte("Status: Disabled"), nil
	case c == "pfctl -E":
		f.pfEnabled = true
		return []byte("pf enabled\nToken : 123"), nil
	case strings.HasPrefix(c, "pfctl -a "+Anchor+" -f "):
		b, _ := os.ReadFile(args[len(args)-1])
		f.anchor = string(b)
		return nil, nil
	case c == "pfctl -a "+Anchor+" -s rules":
		// pfctl prints the user match as "user = N".
		return []byte(strings.ReplaceAll(f.anchor, "user ", "user = ")), nil
	case c == "pfctl -a "+Anchor+" -F all":
		f.anchor = ""
		return nil, nil
	}
	return nil, nil
}

func (f *fakeMac) called(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const fstabOrig = "# existing\nLABEL=Other none hfs rw,noauto\n"

var testReserve = ReserveConfig{MinBytes: 50 << 30, Percent: 15}

func setup(t *testing.T) (Config, *fakeMac, Host) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o700)
	f := &fakeMac{pfMain: "scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all\n"}
	cfg := Config{WorkDir: work, AgentUID: os.Getuid(), AgentGID: os.Getgid(),
		Volume:     VolumeConfig{Enabled: true, Name: "umm-work", Quota: "auto", Reserve: testReserve},
		StatusFile: filepath.Join(dir, "host.json"), StateDir: filepath.Join(dir, "state")}
	if cfg.AgentUID <= 0 {
		cfg.AgentUID = 451 // tests running as root
	}
	h := Host{Run: f.run, Fstab: filepath.Join(dir, "fstab"), Mounted: func(d string) bool { return d == f.mountedAt || d == f.other },
		BootTime: func() int64 { return 1700000000 },
		Lookup: func(_ context.Context, host string) ([]string, error) {
			if host == "router.example.com" {
				return []string{"192.0.2.10", "2001:db8::10"}, nil
			}
			return nil, fmt.Errorf("no such host")
		}}
	os.WriteFile(h.Fstab, []byte("# existing\nLABEL=Other none hfs rw,noauto\n"), 0o644)
	return cfg, f, h
}

func TestReserveAndAutoQuota(t *testing.T) {
	if got := testReserve.Bytes(200 << 30); got != 50<<30 {
		t.Fatalf("small disk reserve: %d", got)
	}
	if got := testReserve.Bytes(1000 << 30); got != 150<<30 {
		t.Fatalf("15%% reserve: %d", got)
	}
	// 1 TiB: 15% = 153.6 GiB reserved, 870 GiB for work.
	if q := AutoQuota(containerSize, testReserve); q != "870g" {
		t.Fatalf("auto quota %s", q)
	}
	if q := AutoQuota(40<<30, testReserve); q != "" {
		t.Fatalf("tiny disk must get no volume: %s", q)
	}
}

func TestVolumeCreatedHiddenAndIdempotent(t *testing.T) {
	cfg, f, h := setup(t)
	st := Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeDedicated || st.WorkVolume.Device != "disk3s7" || st.WorkVolume.Quota != "870g" {
		t.Fatalf("status: %+v", st.WorkVolume)
	}
	if f.called("diskutil apfs addVolume disk3 APFS umm-work -quota 870g -nomount") != 1 {
		t.Fatalf("addVolume must target the data container with the auto quota and -nomount: %v", f.calls)
	}
	if f.called("diskutil mount nobrowse -mountPoint "+cfg.WorkDir+" disk3s7") != 1 {
		t.Fatalf("must mount nobrowse at the work dir: %v", f.calls)
	}
	if f.called("chflags hidden "+cfg.WorkDir) != 1 {
		t.Fatalf("volume root must be hidden: %v", f.calls)
	}
	fstab, _ := os.ReadFile(h.Fstab)
	if string(fstab) != fstabOrig {
		t.Fatalf("fstab must not be written:\n%s", fstab)
	}
	if fi, _ := os.Stat(cfg.WorkDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("work dir mode %v", fi.Mode())
	}
	got, err := ReadStatus(cfg.StatusFile)
	if err != nil || got.WorkVolume.State != VolumeDedicated {
		t.Fatalf("status file: %+v %v", got, err)
	}
	// Second run: nothing created or mounted again, fstab unchanged.
	f.calls = nil
	st = Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeDedicated || f.called("diskutil apfs addVolume") != 0 || f.called("mount ") != 0 {
		t.Fatalf("second run: %+v %v", st.WorkVolume, f.calls)
	}
	fstab2, _ := os.ReadFile(h.Fstab)
	if string(fstab2) != string(fstab) {
		t.Fatalf("fstab changed:\n%s", fstab2)
	}
}

func TestOldFstabCleanup(t *testing.T) {
	// A line an older version added is removed; the rest is kept as is.
	cfg, _, h := setup(t)
	line := "UUID=0F6A3C1E-1111-2222-3333-444455556666 " + cfg.WorkDir + " apfs rw,nobrowse " + FstabMarker + "\n"
	os.WriteFile(h.Fstab, []byte(fstabOrig+line+"# after\n"), 0o644)
	os.WriteFile(h.Fstab+".umm.tmp", []byte("partial"), 0o644)
	Prepare(context.Background(), cfg, h)
	if b, _ := os.ReadFile(h.Fstab); string(b) != fstabOrig+"# after\n" {
		t.Fatalf("fstab:\n%s", b)
	}
	if _, err := os.Stat(h.Fstab + ".umm.tmp"); !os.IsNotExist(err) {
		t.Fatalf("leftover temp file must be removed: %v", err)
	}
	// No line of ours: the file is not rewritten at all.
	cfg, _, h = setup(t)
	Prepare(context.Background(), cfg, h)
	if b, _ := os.ReadFile(h.Fstab); string(b) != fstabOrig {
		t.Fatalf("fstab changed:\n%s", b)
	}
	if _, err := os.Stat(h.Fstab + ".umm.tmp"); !os.IsNotExist(err) {
		t.Fatalf("no temp file may be created: %v", err)
	}
	// Missing fstab: nothing happens.
	cfg, _, h = setup(t)
	os.Remove(h.Fstab)
	Prepare(context.Background(), cfg, h)
	if _, err := os.Stat(h.Fstab); !os.IsNotExist(err) {
		t.Fatalf("fstab must not be created: %v", err)
	}
}

func TestFixedQuota(t *testing.T) {
	cfg, f, h := setup(t)
	cfg.Volume.Quota = "400g"
	if st := Prepare(context.Background(), cfg, h); st.WorkVolume.Quota != "400g" ||
		f.called("diskutil apfs addVolume disk3 APFS umm-work -quota 400g -nomount") != 1 {
		t.Fatalf("fixed quota: %+v %v", st.WorkVolume, f.calls)
	}
}

func TestVolumeFallbacks(t *testing.T) {
	cfg, f, h := setup(t)
	f.failAdd = true
	st := Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeSystem || !strings.Contains(st.WorkVolume.Reason, "create volume") {
		t.Fatalf("create failure must fall back: %+v", st.WorkVolume)
	}

	// Existing, unmounted volume with a task dir in the work dir: wait.
	cfg, f, h = setup(t)
	f.hasVol = true
	os.Mkdir(filepath.Join(cfg.WorkDir, "task-1"), 0o700)
	st = Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeSystem || !strings.Contains(st.WorkVolume.Reason, "in use") || f.called("diskutil mount") != 0 {
		t.Fatalf("busy work dir must not be mounted over: %+v %v", st.WorkVolume, f.calls)
	}
	// Any other entry, even a dot-directory a task left, blocks the mount.
	os.Remove(filepath.Join(cfg.WorkDir, "task-1"))
	os.Mkdir(filepath.Join(cfg.WorkDir, ".left-by-a-task"), 0o700)
	if st = Prepare(context.Background(), cfg, h); f.called("diskutil mount") != 0 {
		t.Fatalf("a dot-dir must block the mount: %+v", st.WorkVolume)
	}
	os.Remove(filepath.Join(cfg.WorkDir, ".left-by-a-task"))
	// Auto-mounted elsewhere: moved to the work dir once it is idle. The
	// agent's own entry there (its mise download cache, in Keep) and file
	// system litter do not block the mount.
	cfg.Keep = []string{".mise-cache"}
	os.MkdirAll(filepath.Join(cfg.WorkDir, ".mise-cache", "downloads-cas"), 0o700)
	os.WriteFile(filepath.Join(cfg.WorkDir, ".DS_Store"), nil, 0o600)
	f.mountedAt = "/Volumes/umm-work"
	st = Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeDedicated || f.called("diskutil unmount disk3s7") != 1 || f.mountedAt != cfg.WorkDir ||
		f.called("diskutil mount nobrowse -mountPoint "+cfg.WorkDir+" disk3s7") != 1 {
		t.Fatalf("remount: %+v %v", st.WorkVolume, f.calls)
	}

	// Mount fails: the reason is reported, tasks use the system volume.
	cfg, f, h = setup(t)
	f.hasVol = true
	f.failMount = "Volume on disk3s7 failed to mount"
	st = Prepare(context.Background(), cfg, h)
	if st.WorkVolume.State != VolumeSystem || !strings.Contains(st.WorkVolume.Reason, "mount: ") {
		t.Fatalf("mount failure: %+v", st.WorkVolume)
	}
	if got, _ := ReadStatus(cfg.StatusFile); got.WorkVolume.Reason != st.WorkVolume.Reason {
		t.Fatalf("reason must reach the status file: %+v", got.WorkVolume)
	}

	// Mount "succeeds" but the volume ends up elsewhere.
	cfg, f, h = setup(t)
	f.hasVol = true
	h.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if filepath.Base(name) == "diskutil" && len(args) > 0 && args[0] == "mount" {
			f.mountedAt = "/Volumes/umm-work"
			return nil, nil
		}
		return f.run(ctx, name, args...)
	}
	if st := Prepare(context.Background(), cfg, h); st.WorkVolume.State != VolumeSystem ||
		!strings.Contains(st.WorkVolume.Reason, "after mounting") {
		t.Fatalf("wrong mount point must fall back: %+v", st.WorkVolume)
	}

	// Another file system already at the work dir: left alone.
	cfg, f, h = setup(t)
	f.hasVol = true
	f.other = cfg.WorkDir
	if st := Prepare(context.Background(), cfg, h); st.WorkVolume.State != VolumeSystem ||
		!strings.Contains(st.WorkVolume.Reason, "another file system") || f.called("diskutil mount") != 0 {
		t.Fatalf("foreign mount: %+v %v", st.WorkVolume, f.calls)
	}

	// Disabled, and invalid settings.
	cfg, _, h = setup(t)
	cfg.Volume.Enabled = false
	if st := Prepare(context.Background(), cfg, h); st.WorkVolume.State != VolumeDisabled {
		t.Fatalf("disabled: %+v", st.WorkVolume)
	}
	cfg.Volume = VolumeConfig{Enabled: true, Name: "umm work; rm", Quota: "auto", Reserve: testReserve}
	if st := Prepare(context.Background(), cfg, h); st.WorkVolume.State != VolumeSystem {
		t.Fatalf("invalid name: %+v", st.WorkVolume)
	}
}

func TestNetworkDenyAnchor(t *testing.T) {
	cfg, f, h := setup(t)
	cfg.Volume.Enabled = false
	cfg.Network = NetworkConfig{Mode: "deny", Allow: []string{"router.example.com", "198.51.100.0/24"}}
	st := Prepare(context.Background(), cfg, h)
	if !st.Network.Applied || st.Network.BootTime != 1700000000 || len(st.Network.Allowed) != 3 {
		t.Fatalf("network: %+v", st.Network)
	}
	if !f.pfEnabled || !strings.Contains(f.anchor, fmt.Sprintf("block return out quick proto { tcp, udp } from any to any user %d", cfg.AgentUID)) ||
		!strings.Contains(f.anchor, "table <umm_allow> const { 192.0.2.10, 2001:db8::10, 198.51.100.0/24 }") {
		t.Fatalf("anchor:\n%s", f.anchor)
	}
	// Back to allow: the anchor is flushed.
	cfg.Network.Mode = "allow"
	st = Prepare(context.Background(), cfg, h)
	if st.Network.Applied || f.anchor != "" {
		t.Fatalf("allow must flush: %+v %q", st.Network, f.anchor)
	}
}

func TestNetworkDenyRefusesWhatItCannotVerify(t *testing.T) {
	cfg, f, h := setup(t)
	cfg.Volume.Enabled = false
	cfg.Network = NetworkConfig{Mode: "deny", Allow: []string{"router.example.com"}}
	f.pfMain = "" // pf.conf without the com.apple anchor
	if st := Prepare(context.Background(), cfg, h); st.Network.Applied || !strings.Contains(st.Network.Reason, "com.apple") {
		t.Fatalf("must not claim rules it cannot reach: %+v", st.Network)
	}
	cfg, _, h = setup(t)
	cfg.Volume.Enabled = false
	cfg.Network = NetworkConfig{Mode: "deny", Allow: []string{"unknown.example.com"}}
	if st := Prepare(context.Background(), cfg, h); st.Network.Applied || !strings.Contains(st.Network.Reason, "resolve") {
		t.Fatalf("unresolvable host: %+v", st.Network)
	}
	cfg.Network.Allow = []string{"bad host"}
	if st := Prepare(context.Background(), cfg, h); st.Network.Applied {
		t.Fatalf("invalid entry: %+v", st.Network)
	}
}

func TestDecodePlist(t *testing.T) {
	v, err := DecodePlist([]byte(apfsList(true)))
	if err != nil {
		t.Fatal(err)
	}
	cs := v.(map[string]any)["Containers"].([]any)
	if len(cs) != 2 || str(cs[1], "ContainerReference") != "disk3" {
		t.Fatalf("%+v", v)
	}
	v, _ = DecodePlist([]byte(infoData))
	if m := v.(map[string]any); m["Internal"] != true || m["TotalSize"] != int64(994662584320) {
		t.Fatalf("%+v", m)
	}
}
