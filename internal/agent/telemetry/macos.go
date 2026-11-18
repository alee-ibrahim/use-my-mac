package telemetry

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// MacOS collects telemetry on macOS using stock command-line tools. It is not
// build-tagged so the whole flow can be tested anywhere with a fake Runner;
// NewDefault picks it only on darwin.
type MacOS struct {
	Run  Runner
	Opts Options

	once     sync.Once
	ncpu     int
	memTotal uint64
	chip     string
	model    string
	osVer    string
}

func (m *MacOS) out(ctx context.Context, name string, args ...string) string {
	b, err := m.Run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return string(b)
}

func (m *MacOS) static(ctx context.Context) {
	m.once.Do(func() {
		m.ncpu, _ = strconv.Atoi(strings.TrimSpace(m.out(ctx, "sysctl", "-n", "hw.ncpu")))
		m.memTotal, _ = strconv.ParseUint(strings.TrimSpace(m.out(ctx, "sysctl", "-n", "hw.memsize")), 10, 64)
		m.chip = strings.TrimSpace(m.out(ctx, "sysctl", "-n", "machdep.cpu.brand_string"))
		m.model = strings.TrimSpace(m.out(ctx, "sysctl", "-n", "hw.model"))
		m.osVer = strings.TrimSpace(m.out(ctx, "sw_vers", "-productVersion"))
	})
}

// Collect implements Collector. Individual probes failing only blank out
// their fields; the snapshot is still useful.
func (m *MacOS) Collect(ctx context.Context) (api.Telemetry, error) {
	m.static(ctx)
	t := api.Telemetry{
		CollectedAt:    time.Now().UTC(),
		CPUCount:       m.ncpu,
		MemTotalBytes:  m.memTotal,
		Chip:           m.chip,
		Model:          m.model,
		OS:             "macOS " + m.osVer,
		BatteryPercent: -1,
		GPUUtilPercent: -1,
	}

	root := m.out(ctx, "ioreg", "-n", "Root", "-d1")
	sess, ok := ParseConsoleSession(root)
	consoleUID, consoleName := sess.UID, sess.Name
	if !ok {
		consoleUID, consoleName = ParseConsoleUser(m.out(ctx, "stat", "-f", "%u %Su", "/dev/console"))
		sess.Locked = ParseScreenLocked(root)
	}
	t.ConsoleUser = consoleName
	if consoleName != "" {
		if idle, ok := ParseHIDIdle(m.out(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4")); ok {
			t.UserIdleSeconds = idle
		}
		t.ScreenLocked = sess.Locked
	}

	var own map[int]bool
	if m.Opts.OwnProcessGroups != nil {
		own = m.Opts.OwnProcessGroups()
	}
	load := ParsePS(m.out(ctx, "ps", "-axo", "pid=,pgid=,uid=,pcpu=,rss="), m.ncpu, m.Opts.OurUID, consoleUID, m.Opts.UserLoadScope, own)
	t.UserCPUPercent, t.OurCPUPercent = load.UserCPU, load.OurCPU
	t.UserMemBytes, t.OurMemBytes = load.UserMem, load.OurMem

	t.MemAvailableBytes = ParseVMStat(m.out(ctx, "vm_stat"))
	t.MemoryPressure = ParsePressureLevel(m.out(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level"))
	t.Thermal = WorseThermal(
		ParseThermal(m.out(ctx, "pmset", "-g", "therm")),
		ParseThermalPressure(m.out(ctx, "notifyutil", "-g", "com.apple.system.thermalpressurelevel")))
	t.OnAC, t.BatteryPercent = ParsePower(m.out(ctx, "pmset", "-g", "batt"))
	t.GPUUtilPercent, t.GPUCores = ParseGPU(m.out(ctx, "ioreg", "-r", "-d", "1", "-c", "AGXAccelerator"))
	if m.Opts.WorkDir != "" {
		t.DiskFreeBytes = ParseDF(m.out(ctx, "df", "-k", m.Opts.WorkDir))
	}
	return t, nil
}
