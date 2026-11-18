package telemetry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Captured on a Mac Studio (M3 Ultra, 28 CPUs, 96 GB, macOS 27.0.1) with the
// console user logged in and the screen locked. The user name and session
// UUID are sanitised; nothing else is edited (ioreg-root.txt and ioreg-hid.txt
// are trimmed to the nodes the collector reads).
const realDir = "testdata/macos27-m3-ultra"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(realDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRealMacParsers(t *testing.T) {
	s, ok := ParseConsoleSession(fixture(t, "ioreg-root.txt"))
	if !ok || s.UID != 501 || s.Name != "alice" || !s.Locked {
		t.Errorf("console session = %+v ok=%v", s, ok)
	}
	if idle, ok := ParseHIDIdle(fixture(t, "ioreg-hid.txt")); !ok || int(idle) != 332871 {
		t.Errorf("idle = %v %v", idle, ok)
	}
	if u, c := ParseGPU(fixture(t, "ioreg-agx.txt")); u != 81 || c != 60 {
		t.Errorf("gpu = %v %d", u, c)
	}
	// free 7866 + inactive 1056286 + speculative 111 + purgeable 82 pages of 16 KiB.
	if got := ParseVMStat(fixture(t, "vm_stat.txt")); got != (7866+1056286+111+82)*16384 {
		t.Errorf("vm_stat = %d", got)
	}
	if got := ParsePressureLevel(fixture(t, "pressure-level.txt")); got != "normal" {
		t.Errorf("pressure = %q", got)
	}
	if got := ParseThermal(fixture(t, "pmset-therm.txt")); got != "nominal" {
		t.Errorf("thermal = %q", got)
	}
	if ac, pct := ParsePower(fixture(t, "pmset-batt.txt")); !ac || pct != -1 {
		t.Errorf("power = %v %d", ac, pct)
	}
	if got := ParseDF(fixture(t, "df.txt")); got != 665782128*1024 {
		t.Errorf("df = %d", got)
	}
	l := ParsePS(fixture(t, "ps.txt"), 28, 299, 501, "console", nil)
	if l.UserCPU <= 0 || l.UserMem == 0 {
		t.Errorf("ps: console user's processes not counted: %+v", l)
	}
}

func TestConsoleSession(t *testing.T) {
	// Nobody on the console (login window): IOConsoleUsers present, no
	// on-console session.
	s, ok := ParseConsoleSession(`"IOConsoleLocked" = Yes
"IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=No,"kCGSSessionUserNameKey"="bob","kCGSSessionUserIDKey"=502,"CGSSessionScreenIsLocked"=Yes})`)
	if !ok || s.UID != -1 || s.Name != "" {
		t.Errorf("login window = %+v %v", s, ok)
	}
	// Fast user switching: only the on-console session counts, and its own
	// lock flag decides.
	s, ok = ParseConsoleSession(`"IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=No,"kCGSSessionUserNameKey"="bob","kCGSSessionUserIDKey"=502,"CGSSessionScreenIsLocked"=Yes},{"kCGSSessionOnConsoleKey"=Yes,"kCGSSessionUserNameKey"="alice","kCGSSessionUserIDKey"=501})`)
	if !ok || s.UID != 501 || s.Name != "alice" || s.Locked {
		t.Errorf("fast user switching = %+v", s)
	}
	if _, ok := ParseConsoleSession(""); ok {
		t.Error("missing IOConsoleUsers should fall back")
	}
}

// On the real Mac /dev/console was owned by root while the user was logged in
// (screen locked). The collector must still find the user via ioreg.
func TestMacOSCollectorRealOutputs(t *testing.T) {
	outputs := map[string]string{
		"sysctl -n hw.ncpu":                             "28\n",
		"sysctl -n hw.memsize":                          "103079215104\n",
		"sysctl -n machdep.cpu.brand_string":            "Apple M3 Ultra\n",
		"sysctl -n hw.model":                            "Mac15,14\n",
		"sw_vers -productVersion":                       "27.0.1\n",
		"stat -f %u %Su /dev/console":                   "0 root\n",
		"ioreg -c IOHIDSystem -d 4":                     fixture(t, "ioreg-hid.txt"),
		"ioreg -n Root -d1":                             fixture(t, "ioreg-root.txt"),
		"ps -axo pid=,pgid=,uid=,pcpu=,rss=":            fixture(t, "ps.txt"),
		"vm_stat":                                       fixture(t, "vm_stat.txt"),
		"sysctl -n kern.memorystatus_vm_pressure_level": fixture(t, "pressure-level.txt"),
		"pmset -g therm":                                fixture(t, "pmset-therm.txt"),
		"pmset -g batt":                                 fixture(t, "pmset-batt.txt"),
		"ioreg -r -d 1 -c AGXAccelerator":               fixture(t, "ioreg-agx.txt"),
		"df -k /var/umm":                                fixture(t, "df.txt"),
	}
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		k := name + " " + strings.Join(args, " ")
		if o, ok := outputs[k]; ok {
			return []byte(o), nil
		}
		return nil, fmt.Errorf("unexpected command %q", k)
	}
	c := &MacOS{Run: run, Opts: Options{WorkDir: "/var/umm", OurUID: 299, UserLoadScope: "console"}}
	tl, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tl.ConsoleUser != "alice" || !tl.ScreenLocked || int(tl.UserIdleSeconds) != 332871 {
		t.Errorf("presence: %+v", tl)
	}
	if tl.UserCPUPercent <= 0 || tl.UserMemBytes == 0 || tl.OurMemBytes != 0 {
		t.Errorf("load split: user=%v/%d our=%d", tl.UserCPUPercent, tl.UserMemBytes, tl.OurMemBytes)
	}
	if tl.GPUCores != 60 || tl.GPUUtilPercent != 81 || !tl.OnAC || tl.BatteryPercent != -1 {
		t.Errorf("gpu/power: %+v", tl)
	}
}
