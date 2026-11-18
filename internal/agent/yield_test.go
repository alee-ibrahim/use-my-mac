package agent

import (
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func tel(userCPU float64) api.Telemetry {
	return api.Telemetry{ConsoleUser: "staff", UserCPUPercent: userCPU, OnAC: true, MemoryPressure: "normal", Thermal: "nominal"}
}

func TestYieldNeedsSustainedLoad(t *testing.T) {
	cfg := DefaultYieldConfig() // high 50, low 25, sustain 10s, cooldown 120s
	y := NewYield(cfg)
	t0 := time.Unix(0, 0)

	// A logged-in, active user with light load: keep working.
	if s, _ := y.Update(t0, tel(10)); s != api.AgentReady {
		t.Fatalf("light load must stay ready, got %s", s)
	}
	// Short spike: 5s of heavy load then calm -> no yield.
	y.Update(t0.Add(1*time.Second), tel(90))
	y.Update(t0.Add(6*time.Second), tel(90))
	if s, _ := y.Update(t0.Add(7*time.Second), tel(5)); s != api.AgentReady {
		t.Fatalf("short spike must not yield, got %s", s)
	}
	// Sustained heavy load for >= 10s -> yield.
	y.Update(t0.Add(10*time.Second), tel(80))
	if s, _ := y.Update(t0.Add(15*time.Second), tel(80)); s != api.AgentReady {
		t.Fatalf("5s heavy should not yet yield, got %s", s)
	}
	s, changed := y.Update(t0.Add(20*time.Second), tel(80))
	if s != api.AgentBusyUser || !changed {
		t.Fatalf("10s heavy must yield, got %s changed=%v", s, changed)
	}
	if _, why := y.State(); why == "" {
		t.Fatal("yield must carry a reason")
	}
	// Between low and high: still yielding (hysteresis).
	if s, _ := y.Update(t0.Add(25*time.Second), tel(40)); s != api.AgentBusyUser {
		t.Fatalf("want still yielding, got %s", s)
	}
	// Calm -> cooldown.
	if s, _ := y.Update(t0.Add(30*time.Second), tel(5)); s != api.AgentCooldown {
		t.Fatalf("want cooldown, got %s", s)
	}
	// Not calm long enough -> stays in cooldown; a bump resets the timer.
	y.Update(t0.Add(100*time.Second), tel(30))
	if s, _ := y.Update(t0.Add(200*time.Second), tel(5)); s != api.AgentCooldown {
		t.Fatalf("cooldown timer should have reset, got %s", s)
	}
	if s, _ := y.Update(t0.Add(221*time.Second), tel(5)); s != api.AgentReady {
		t.Fatalf("want ready after 120s calm, got %s", s)
	}
}

func TestYieldCooldownBackToYielding(t *testing.T) {
	y := NewYield(DefaultYieldConfig())
	t0 := time.Unix(0, 0)
	y.Update(t0, tel(90))
	y.Update(t0.Add(11*time.Second), tel(90))
	y.Update(t0.Add(12*time.Second), tel(1))
	if s, _ := y.Update(t0.Add(13*time.Second), tel(95)); s != api.AgentBusyUser {
		t.Fatalf("heavy during cooldown must yield again, got %s", s)
	}
}

func TestYieldImmediateConditions(t *testing.T) {
	cases := map[string]api.Telemetry{
		"memory critical":  {UserCPUPercent: 0, OnAC: true, MemoryPressure: "critical"},
		"thermal critical": {UserCPUPercent: 0, OnAC: true, Thermal: "critical"},
		"battery":          {UserCPUPercent: 0, OnAC: false},
	}
	for name, tl := range cases {
		y := NewYield(DefaultYieldConfig())
		if s, _ := y.Update(time.Unix(0, 0), tl); s != api.AgentBusyUser {
			t.Errorf("%s: want immediate yield, got %s", name, s)
		}
	}
}

func TestYieldMemoryWarnIsSustained(t *testing.T) {
	y := NewYield(DefaultYieldConfig())
	t0 := time.Unix(0, 0)
	warn := api.Telemetry{OnAC: true, MemoryPressure: "warn"}
	if s, _ := y.Update(t0, warn); s != api.AgentReady {
		t.Fatal("warn should not be immediate")
	}
	if s, _ := y.Update(t0.Add(10*time.Second), warn); s != api.AgentBusyUser {
		t.Fatal("sustained warn should yield")
	}
}

func TestYieldBatteryAllowed(t *testing.T) {
	cfg := DefaultYieldConfig()
	cfg.YieldOnBattery = false
	y := NewYield(cfg)
	if s, _ := y.Update(time.Unix(0, 0), api.Telemetry{OnAC: false, UserCPUPercent: 1}); s != api.AgentReady {
		t.Fatalf("battery allowed: want ready, got %s", s)
	}
}

func TestOurLoadIsIgnored(t *testing.T) {
	y := NewYield(DefaultYieldConfig())
	t0 := time.Unix(0, 0)
	busyWithOurWork := api.Telemetry{ConsoleUser: "staff", UserCPUPercent: 5, OurCPUPercent: 95, OnAC: true}
	for i := 0; i < 30; i++ {
		if s, _ := y.Update(t0.Add(time.Duration(i)*time.Second), busyWithOurWork); s != api.AgentReady {
			t.Fatalf("our own load must never cause a yield (t=%d): %s", i, s)
		}
	}
}
