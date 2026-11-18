package telemetry

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
)

// The fixtures below follow the documented output formats of the macOS tools.
// They are representative samples, not captures from a specific machine.

const ioregHID = `+-o IOHIDSystem  <class IOHIDSystem, id 0x100000abc, registered, matched, active, busy 0 (0 ms), retain 30>
  | {
  |   "HIDIdleTime" = 125000000000
  |   "IOClass" = "IOHIDSystem"
  | }
`

const ioregRootLocked = `+-o Root  <class IORegistryEntry, id 0x100000100, retain 30>
    {
      "IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=Yes,"kCGSSessionUserNameKey"="alice","CGSSessionScreenIsLocked"=Yes,"kCGSSessionUserIDKey"=501})
    }
`

const ioregRootUnlocked = `+-o Root  <class IORegistryEntry, id 0x100000100, retain 30>
    {
      "IOConsoleUsers" = ({"kCGSSessionOnConsoleKey"=Yes,"kCGSSessionUserNameKey"="alice","kCGSSessionUserIDKey"=501})
    }
`

const psOut = `    1     1     0   0.5  12000
  400   400   501  150.0 900000
  401   400   501   50.0 100000
  500   500   299  300.0 2000000
  600   600   299    1.0   5000
  700   700    88   20.0  50000
`

const vmStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10000.
Pages active:                            300000.
Pages inactive:                          200000.
Pages speculative:                         5000.
Pages throttled:                              0.
Pages wired down:                        150000.
Pages purgeable:                           1000.
"Translation faults":                 123456789.
`

const battLaptop = `Now drawing from 'Battery Power'
 -InternalBattery-0 (id=4653155)	87%; discharging; 6:12 remaining present: true
`

const battDesktop = `Now drawing from 'AC Power'
`

const thermAS = `Note: No thermal warning level has been recorded
Note: No performance warning level has been recorded
Note: No CPU power status has been recorded
`

const thermIntel = `CPU_Power_notify
	CPU_Scheduler_Limit 	= 100
	CPU_Available_CPUs 	= 8
	CPU_Speed_Limit 	= 70
`

const agx = `+-o AGXAcceleratorG14X  <class AGXAcceleratorG14X, id 0x1000004a1, registered, matched, active, busy 0 (0 ms), retain 89>
    {
      "gpu-core-count" = 19
      "PerformanceStatistics" = {"In use system memory"=1234567,"Device Utilization %"=37,"Renderer Utilization %"=35,"Tiler Utilization %"=8}
    }
`

const dfOut = `Filesystem     1024-blocks      Used Available Capacity iused      ifree %iused  Mounted on
/dev/disk3s5    482797652 300000000 150000000    67%  1234 1500000000    0%   /System/Volumes/Data
`

func TestParsers(t *testing.T) {
	if s, ok := ParseHIDIdle(ioregHID); !ok || s != 125 {
		t.Errorf("idle = %v %v", s, ok)
	}
	if _, ok := ParseHIDIdle("nothing"); ok {
		t.Error("idle should be absent")
	}
	if !ParseScreenLocked(ioregRootLocked) || ParseScreenLocked(ioregRootUnlocked) {
		t.Error("screen lock parse")
	}
	if !ParseScreenLocked(`"IOConsoleLocked" = Yes`) {
		t.Error("IOConsoleLocked form")
	}
	if uid, name := ParseConsoleUser("501 alice\n"); uid != 501 || name != "alice" {
		t.Errorf("console = %d %q", uid, name)
	}
	if uid, name := ParseConsoleUser("0 root\n"); uid != -1 || name != "" {
		t.Errorf("login window should be nobody, got %d %q", uid, name)
	}

	// 10 CPUs, user=501 (console), we run as uid 299.
	l := ParsePS(psOut, 10, 299, 501, "console", nil)
	if math.Abs(l.UserCPU-20) > 0.001 || math.Abs(l.OurCPU-30.1) > 0.001 {
		t.Errorf("ps split user=%v our=%v", l.UserCPU, l.OurCPU)
	}
	if l.UserMem != (900000+100000)*1024 || l.OurMem != (2000000+5000)*1024 {
		t.Errorf("ps mem user=%d our=%d", l.UserMem, l.OurMem)
	}
	// Scope "all": root + _daemon (88) also count as user load.
	la := ParsePS(psOut, 10, 299, 501, "all", nil)
	if math.Abs(la.UserCPU-22.05) > 0.001 {
		t.Errorf("scope all user=%v", la.UserCPU)
	}
	// Agent running as the console user (dev setup): only task process
	// groups count as ours.
	ld := ParsePS(psOut, 10, 501, 501, "console", map[int]bool{400: true})
	if math.Abs(ld.OurCPU-20) > 0.001 || ld.UserCPU != 0 {
		t.Errorf("pgid split user=%v our=%v", ld.UserCPU, ld.OurCPU)
	}

	if got := ParseVMStat(vmStat); got != (10000+200000+5000+1000)*16384 {
		t.Errorf("vm_stat = %d", got)
	}
	for in, want := range map[string]string{"1\n": "normal", "2": "warn", "4": "critical", "x": ""} {
		if got := ParsePressureLevel(in); got != want {
			t.Errorf("pressure(%q)=%q", in, got)
		}
	}
	if ParseThermal(thermAS) != "nominal" || ParseThermal(thermIntel) != "serious" || ParseThermal("") != "" {
		t.Error("thermal parse")
	}
	if ac, pct := ParsePower(battLaptop); ac || pct != 87 {
		t.Errorf("laptop power %v %d", ac, pct)
	}
	if ac, pct := ParsePower(battDesktop); !ac || pct != -1 {
		t.Errorf("desktop power %v %d", ac, pct)
	}
	if u, c := ParseGPU(agx); u != 37 || c != 19 {
		t.Errorf("gpu %v %d", u, c)
	}
	if u, _ := ParseGPU(""); u != -1 {
		t.Error("gpu unknown should be -1")
	}
	if ParseDF(dfOut) != 150000000*1024 {
		t.Error("df parse")
	}
}

func TestThermalPressure(t *testing.T) {
	cases := map[string]string{
		"com.apple.system.thermalpressurelevel 0\n": "nominal",
		"com.apple.system.thermalpressurelevel 1\n": "fair",
		"com.apple.system.thermalpressurelevel 2\n": "serious",
		"com.apple.system.thermalpressurelevel 3\n": "critical",
		"com.apple.system.thermalpressurelevel 4\n": "critical",
		"":                    "",
		"notifyutil: unknown": "",
	}
	for in, want := range cases {
		if got := ParseThermalPressure(in); got != want {
			t.Errorf("ParseThermalPressure(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range [][3]string{
		{"nominal", "serious", "serious"}, {"critical", "fair", "critical"},
		{"", "fair", "fair"}, {"nominal", "", "nominal"}, {"", "", ""},
	} {
		if got := WorseThermal(c[0], c[1]); got != c[2] {
			t.Errorf("WorseThermal(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestMacOSCollectorWithFakeRunner(t *testing.T) {
	outputs := map[string]string{
		"sysctl -n hw.ncpu":                                   "10\n",
		"sysctl -n hw.memsize":                                "34359738368\n",
		"sysctl -n machdep.cpu.brand_string":                  "Apple M2 Pro\n",
		"sysctl -n hw.model":                                  "Mac14,12\n",
		"sw_vers -productVersion":                             "15.1\n",
		"stat -f %u %Su /dev/console":                         "501 alice\n",
		"ioreg -c IOHIDSystem -d 4":                           ioregHID,
		"ioreg -n Root -d1":                                   ioregRootLocked,
		"ps -axo pid=,pgid=,uid=,pcpu=,rss=":                  psOut,
		"vm_stat":                                             vmStat,
		"sysctl -n kern.memorystatus_vm_pressure_level":       "1\n",
		"pmset -g therm":                                      thermAS,
		"notifyutil -g com.apple.system.thermalpressurelevel": "com.apple.system.thermalpressurelevel 2\n",
		"pmset -g batt":                                       battDesktop,
		"ioreg -r -d 1 -c AGXAccelerator":                     agx,
		"df -k /var/umm":                                      dfOut,
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
	if tl.ConsoleUser != "alice" || !tl.ScreenLocked || tl.UserIdleSeconds != 125 {
		t.Errorf("presence: %+v", tl)
	}
	if tl.CPUCount != 10 || tl.MemTotalBytes != 32<<30 || tl.Chip != "Apple M2 Pro" || tl.OS != "macOS 15.1" {
		t.Errorf("static: %+v", tl)
	}
	if !tl.OnAC || tl.BatteryPercent != -1 || tl.GPUUtilPercent != 37 || tl.GPUCores != 19 {
		t.Errorf("power/gpu: %+v", tl)
	}
	if tl.MemoryPressure != "normal" || tl.Thermal != "serious" || tl.DiskFreeBytes == 0 {
		t.Errorf("mem/thermal/disk: %+v", tl)
	}
	if math.Abs(tl.UserCPUPercent-20) > 0.001 {
		t.Errorf("user cpu %v", tl.UserCPUPercent)
	}
}
