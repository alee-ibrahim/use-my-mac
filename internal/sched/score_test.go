package sched

import (
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func baseCand(id string) Candidate {
	return Candidate{
		ID: id, Online: true, AgentState: api.AgentReady,
		Runtimes: []string{"ffmpeg"}, MaxTasks: 2, FleetMaxCPU: 12,
		Telemetry: api.Telemetry{
			CPUCount: 12, MemTotalBytes: 32 << 30, MemAvailableBytes: 20 << 30,
			MemoryPressure: "normal", Thermal: "nominal", OnAC: true, BatteryPercent: -1,
			GPUUtilPercent: 0, DiskFreeBytes: 200 << 30,
		},
	}
}

var need = Need{Runtime: "ffmpeg", Resources: api.Resources{RAMMB: 2048}}

func TestPresenceOrdering(t *testing.T) {
	nobody := api.Telemetry{}
	locked := api.Telemetry{ConsoleUser: "u", ScreenLocked: true}
	away := api.Telemetry{ConsoleUser: "u", UserIdleSeconds: 3600}
	idle := api.Telemetry{ConsoleUser: "u", UserIdleSeconds: 300}
	active := api.Telemetry{ConsoleUser: "u", UserIdleSeconds: 2}
	f := func(t api.Telemetry) float64 { return PresenceFactor(t, 900) }
	if !(f(nobody) > f(locked) && f(locked) > f(away) && f(away) > f(idle) && f(idle) > f(active)) {
		t.Fatalf("presence not ordered: %v %v %v %v %v", f(nobody), f(locked), f(away), f(idle), f(active))
	}
}

func TestScorePrefersNoUserOverActiveUser(t *testing.T) {
	a := baseCand("a")
	b := baseCand("b")
	b.Telemetry.ConsoleUser = "staff"
	b.Telemetry.UserIdleSeconds = 1
	best, all := Pick([]Candidate{b, a}, need, DefaultWeights(), DefaultLimits())
	if best == nil || best.ID != "a" {
		t.Fatalf("want a, got %+v (all %+v)", best, all)
	}
}

func TestLoggedInUserAloneDoesNotFilter(t *testing.T) {
	c := baseCand("a")
	c.Telemetry.ConsoleUser = "staff"
	c.Telemetry.UserIdleSeconds = 0
	r := Score(c, need, DefaultWeights(), DefaultLimits())
	if !r.OK {
		t.Fatalf("logged-in user must not filter the Mac out: %s", r.Reason)
	}
}

func TestHardFilters(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*Candidate)
		reason string
	}{
		{"offline", func(c *Candidate) { c.Online = false }, "offline"},
		{"drain", func(c *Candidate) { c.Drain = true }, "draining"},
		{"optout", func(c *Candidate) { c.OptedOut = true }, "opted out"},
		{"quarantine", func(c *Candidate) { c.Quarantined = true }, "quarantined"},
		{"yielding", func(c *Candidate) { c.AgentState = api.AgentBusyUser }, "agent yielding"},
		{"excluded", func(c *Candidate) { c.Excluded = true }, "excluded"},
		{"runtime", func(c *Candidate) { c.Runtimes = []string{"python"} }, "runtime"},
		{"full", func(c *Candidate) { c.Active = 2 }, "full"},
		{"ram total", func(c *Candidate) { c.Telemetry.MemTotalBytes = 1 << 30 }, "needs"},
		{"ram free", func(c *Candidate) { c.Telemetry.MemAvailableBytes = 1 << 30 }, "free memory"},
		{"ram reserved", func(c *Candidate) { c.ReservedMB = 19 * 1024 }, "free memory"},
		{"pressure", func(c *Candidate) { c.Telemetry.MemoryPressure = "critical" }, "pressure"},
		{"thermal", func(c *Candidate) { c.Telemetry.Thermal = "critical" }, "thermal"},
		{"battery", func(c *Candidate) { c.Telemetry.OnAC = false; c.Telemetry.BatteryPercent = 90 }, "battery"},
		{"disk", func(c *Candidate) { c.Telemetry.DiskFreeBytes = 1 << 30 }, "disk"},
		{"label", func(c *Candidate) { c.Labels = map[string]string{"chip": "m1"} }, "label"},
	}
	n := need
	n.Resources.Labels = map[string]string{"chip": "m2"}
	for _, tc := range cases {
		c := baseCand("x")
		c.Labels = map[string]string{"chip": "m2"}
		tc.mut(&c)
		r := Score(c, n, DefaultWeights(), DefaultLimits())
		if r.OK || !strings.Contains(r.Reason, tc.reason) {
			t.Errorf("%s: want reject containing %q, got ok=%v reason=%q", tc.name, tc.reason, r.OK, r.Reason)
		}
	}
}

func TestBatteryAllowedButPenalised(t *testing.T) {
	l := DefaultLimits()
	l.AllowBattery = true
	ac := baseCand("ac")
	bat := baseCand("bat")
	bat.Telemetry.OnAC = false
	bat.Telemetry.BatteryPercent = 80
	best, _ := Pick([]Candidate{bat, ac}, need, DefaultWeights(), l)
	if best == nil || best.ID != "ac" {
		t.Fatalf("want ac, got %+v", best)
	}
	bat.Telemetry.BatteryPercent = 10
	if r := Score(bat, need, DefaultWeights(), l); r.OK {
		t.Fatal("low battery should be rejected")
	}
}

func TestSpreadAcrossMacs(t *testing.T) {
	a := baseCand("a")
	a.Active = 1
	a.ReservedMB = 2048
	b := baseCand("b")
	best, _ := Pick([]Candidate{a, b}, need, DefaultWeights(), DefaultLimits())
	if best.ID != "b" {
		t.Fatalf("want b (less loaded), got %s", best.ID)
	}
}

func TestGPUFactorOnlyForGPUTasks(t *testing.T) {
	a := baseCand("a")
	a.Telemetry.GPUUtilPercent = 90
	b := baseCand("b")
	b.Telemetry.GPUUtilPercent = 5
	gpuNeed := need
	gpuNeed.Resources.GPU = true
	best, _ := Pick([]Candidate{a, b}, gpuNeed, DefaultWeights(), DefaultLimits())
	if best.ID != "b" {
		t.Fatalf("gpu task: want b, got %s", best.ID)
	}
	ra := Score(a, need, DefaultWeights(), DefaultLimits())
	if _, ok := ra.Parts["gpu"]; ok {
		t.Fatal("gpu factor should not apply to non-gpu tasks")
	}
}

func TestThermalAndWeights(t *testing.T) {
	a := baseCand("a")
	a.Telemetry.Thermal = "serious"
	b := baseCand("b")
	b.Telemetry.UserCPUPercent = 30
	w := DefaultWeights()
	best, _ := Pick([]Candidate{a, b}, need, w, DefaultLimits())
	if best.ID != "b" {
		t.Fatalf("hot Mac should lose: got %s", best.ID)
	}
	// Ignore thermal entirely: then the cooler-loaded "a" wins.
	w.Thermal = 0
	best, _ = Pick([]Candidate{a, b}, need, w, DefaultLimits())
	if best.ID != "a" {
		t.Fatalf("with thermal weight 0 want a, got %s", best.ID)
	}
}

func TestScoreRange(t *testing.T) {
	r := Score(baseCand("a"), need, DefaultWeights(), DefaultLimits())
	if r.Score <= 0 || r.Score > 100 {
		t.Fatalf("score out of range: %v", r.Score)
	}
}

func TestPickNoneOK(t *testing.T) {
	c := baseCand("a")
	c.Online = false
	best, all := Pick([]Candidate{c}, need, DefaultWeights(), DefaultLimits())
	if best != nil || len(all) != 1 {
		t.Fatal("expected no pick")
	}
}

func TestBackoff(t *testing.T) {
	base, max := 2*time.Second, 30*time.Second
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := Backoff(i, base, max); got != w {
			t.Errorf("Backoff(%d)=%v want %v", i, got, w)
		}
	}
}

func TestUnavailableRuntimeFilters(t *testing.T) {
	c := baseCand("a")
	// Even if a stale runtime list still names it, an unavailable runtime
	// never matches, and the reason is surfaced.
	c.Unavailable = map[string]string{"ffmpeg": "program not found: /opt/homebrew/bin/ffmpeg"}
	r := Score(c, need, DefaultWeights(), DefaultLimits())
	if r.OK || !strings.Contains(r.Reason, "program not found") {
		t.Fatalf("want filtered with reason, got %+v", r)
	}
	c.Unavailable = map[string]string{"other": "missing"}
	if r := Score(c, need, DefaultWeights(), DefaultLimits()); !r.OK {
		t.Fatalf("other runtimes must not matter: %s", r.Reason)
	}
}
