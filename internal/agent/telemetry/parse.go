package telemetry

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

var (
	reHIDIdle      = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)
	reScreenLocked = regexp.MustCompile(`"CGSSessionScreenIsLocked"\s*=\s*Yes`)
	reConsoleLock  = regexp.MustCompile(`"IOConsoleLocked"\s*=\s*Yes`)
	reGPUUtil      = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)
	reGPUCores     = regexp.MustCompile(`"gpu-core-count"\s*=\s*(\d+)`)
	rePageSize     = regexp.MustCompile(`page size of (\d+) bytes`)
	reVMLine       = regexp.MustCompile(`^(.+?):\s+(\d+)\.?$`)
	reBattPct      = regexp.MustCompile(`(\d+)%;`)
	reSpeedLimit   = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*(\d+)`)
	reThermLevel   = regexp.MustCompile(`(?i)thermal warning level\D*(\d+)`)
)

// ParseHIDIdle parses `ioreg -c IOHIDSystem -d 4` and returns idle seconds.
func ParseHIDIdle(out string) (float64, bool) {
	m := reHIDIdle.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	ns, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return ns / 1e9, true
}

// ParseScreenLocked parses `ioreg -n Root -d1`.
func ParseScreenLocked(out string) bool {
	return reScreenLocked.MatchString(out) || reConsoleLock.MatchString(out)
}

// ParseConsoleUser parses `stat -f '%u %Su' /dev/console`. root (uid 0)
// means the login window is up: nobody is logged in at the console.
func ParseConsoleUser(out string) (uid int, name string) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 2 {
		return -1, ""
	}
	uid, err := strconv.Atoi(f[0])
	if err != nil || uid == 0 || f[1] == "root" || f[1] == "_mbsetupuser" {
		return -1, ""
	}
	return uid, f[1]
}

// ProcLoad is the aggregated CPU/memory split between the user and us.
type ProcLoad struct {
	UserCPU, OurCPU float64 // percent of the whole machine
	UserMem, OurMem uint64  // bytes (RSS)
}

// ParsePS parses `ps -axo pid=,pgid=,uid=,pcpu=,rss=`. A process is ours if
// it is in one of our task process groups, or if it runs as our dedicated uid
// (and that uid is not the console user's). A process is the user's if it
// runs as the console user (scope "console") or is simply not ours (scope
// "all").
func ParsePS(out string, ncpu, ourUID, consoleUID int, scope string, ownPgids map[int]bool) ProcLoad {
	var l ProcLoad
	if ncpu <= 0 {
		ncpu = 1
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		pgid, err1 := strconv.Atoi(f[1])
		uid, err2 := strconv.Atoi(f[2])
		cpu, err3 := strconv.ParseFloat(strings.ReplaceAll(f[3], ",", "."), 64)
		rss, err4 := strconv.ParseUint(f[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		ours := ownPgids[pgid] || (uid == ourUID && uid != consoleUID)
		switch {
		case ours:
			l.OurCPU += cpu
			l.OurMem += rss * 1024
		case scope == "all" || (consoleUID > 0 && uid == consoleUID):
			l.UserCPU += cpu
			l.UserMem += rss * 1024
		}
	}
	l.UserCPU /= float64(ncpu)
	l.OurCPU /= float64(ncpu)
	return l
}

// ParseVMStat parses `vm_stat` and estimates available memory as
// free + inactive + speculative + purgeable pages.
func ParseVMStat(out string) uint64 {
	page := uint64(4096)
	if m := rePageSize.FindStringSubmatch(out); m != nil {
		if v, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			page = v
		}
	}
	var pages uint64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		m := reVMLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		switch m[1] {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			v, _ := strconv.ParseUint(m[2], 10, 64)
			pages += v
		}
	}
	return pages * page
}

// ParsePressureLevel maps kern.memorystatus_vm_pressure_level (1/2/4).
func ParsePressureLevel(out string) string {
	switch strings.TrimSpace(out) {
	case "1":
		return "normal"
	case "2":
		return "warn"
	case "4":
		return "critical"
	}
	return ""
}

// ParseThermal maps `pmset -g therm` to nominal/fair/serious/critical. On
// Apple Silicon this output is sparse; "no warning recorded" means nominal.
func ParseThermal(out string) string {
	if m := reSpeedLimit.FindStringSubmatch(out); m != nil {
		v, _ := strconv.Atoi(m[1])
		switch {
		case v >= 100:
			return "nominal"
		case v >= 80:
			return "fair"
		case v >= 50:
			return "serious"
		default:
			return "critical"
		}
	}
	if m := reThermLevel.FindStringSubmatch(out); m != nil {
		switch m[1] {
		case "0":
			return "nominal"
		case "1":
			return "fair"
		case "2":
			return "serious"
		default:
			return "critical"
		}
	}
	if strings.Contains(out, "No thermal warning level has been recorded") {
		return "nominal"
	}
	return ""
}

// ParsePower parses `pmset -g batt`.
func ParsePower(out string) (onAC bool, batteryPct int) {
	onAC = strings.Contains(out, "'AC Power'")
	batteryPct = -1
	if strings.Contains(out, "InternalBattery") {
		if m := reBattPct.FindStringSubmatch(out); m != nil {
			batteryPct, _ = strconv.Atoi(m[1])
		}
	}
	return
}

// ParseGPU parses `ioreg -r -d 1 -c AGXAccelerator`.
func ParseGPU(out string) (utilPct float64, cores int) {
	utilPct = -1
	if m := reGPUUtil.FindStringSubmatch(out); m != nil {
		utilPct, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := reGPUCores.FindStringSubmatch(out); m != nil {
		cores, _ = strconv.Atoi(m[1])
	}
	return
}

// ParseDF parses `df -k PATH` and returns available bytes.
func ParseDF(out string) uint64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0
	}
	v, err := strconv.ParseUint(f[3], 10, 64)
	if err != nil {
		return 0
	}
	return v * 1024
}
