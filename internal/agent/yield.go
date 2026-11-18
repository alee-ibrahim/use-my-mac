package agent

import (
	"fmt"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// YieldConfig decides when the Mac's own user needs the machine back.
// A logged-in user alone never triggers a yield; only the user's own load
// (our processes excluded) or machine-level distress does.
type YieldConfig struct {
	// UserCPUHigh: the user's own CPU load (percent of the whole machine)
	// that counts as "heavy".
	UserCPUHigh float64 `yaml:"user_cpu_high" json:"user_cpu_high"`
	// UserCPULow: below this the user counts as calm again (hysteresis).
	UserCPULow float64 `yaml:"user_cpu_low" json:"user_cpu_low"`
	// SustainSeconds: heavy load must last this long before we yield, so a
	// short spike (opening an app) doesn't kill work.
	SustainSeconds float64 `yaml:"sustain_seconds" json:"sustain_seconds"`
	// CooldownSeconds: after a yield, how long the user must stay calm before
	// the Mac accepts work again.
	CooldownSeconds float64 `yaml:"cooldown_seconds" json:"cooldown_seconds"`
	// MemoryPressure at or above this level yields ("warn" or "critical").
	// "critical" always yields immediately.
	MemoryPressure string `yaml:"memory_pressure" json:"memory_pressure"`
	// Thermal at or above this level yields ("fair", "serious", "critical").
	// "critical" always yields immediately.
	Thermal string `yaml:"thermal" json:"thermal"`
	// YieldOnBattery stops work as soon as the Mac goes on battery.
	YieldOnBattery bool `yaml:"yield_on_battery" json:"yield_on_battery"`
}

// DefaultYieldConfig is deliberately conservative toward the user.
func DefaultYieldConfig() YieldConfig {
	return YieldConfig{
		UserCPUHigh: 50, UserCPULow: 25, SustainSeconds: 10, CooldownSeconds: 120,
		MemoryPressure: "warn", Thermal: "serious", YieldOnBattery: true,
	}
}

var pressureRank = map[string]int{"": 0, "normal": 0, "warn": 1, "critical": 2}
var thermalRank = map[string]int{"": 0, "nominal": 0, "fair": 1, "serious": 2, "critical": 3}

// Yield is the agent's "don't disturb the user" state machine:
//
//	ready --(heavy for SustainSeconds, or critical)--> yielding
//	yielding --(calm)--> cooldown
//	cooldown --(heavy)--> yielding
//	cooldown --(calm for CooldownSeconds)--> ready
//
// It is pure: feed it telemetry and a clock.
type Yield struct {
	cfg        YieldConfig
	state      string
	reason     string
	heavySince time.Time
	calmSince  time.Time
}

// NewYield starts in the ready state.
func NewYield(cfg YieldConfig) *Yield {
	return &Yield{cfg: cfg, state: api.AgentReady}
}

// State returns the current state and the reason for it.
func (y *Yield) State() (string, string) { return y.state, y.reason }

// assess returns (heavy, immediate, calm, reason).
func (y *Yield) assess(t api.Telemetry) (bool, bool, bool, string) {
	var reasons []string
	immediate := false
	heavy := false
	if t.UserCPUPercent >= y.cfg.UserCPUHigh {
		heavy = true
		reasons = append(reasons, fmt.Sprintf("user CPU %.0f%%", t.UserCPUPercent))
	}
	pr := pressureRank[t.MemoryPressure]
	if lim, ok := pressureRank[y.cfg.MemoryPressure]; ok && lim > 0 && pr >= lim {
		heavy = true
		reasons = append(reasons, "memory pressure "+t.MemoryPressure)
	}
	if pr >= 2 {
		heavy, immediate = true, true
	}
	th := thermalRank[t.Thermal]
	if lim, ok := thermalRank[y.cfg.Thermal]; ok && lim > 0 && th >= lim {
		heavy = true
		reasons = append(reasons, "thermal "+t.Thermal)
	}
	if th >= 3 {
		heavy, immediate = true, true
	}
	if y.cfg.YieldOnBattery && !t.OnAC {
		heavy, immediate = true, true
		reasons = append(reasons, "on battery")
	}
	calm := !heavy && t.UserCPUPercent < y.cfg.UserCPULow
	reason := ""
	for i, r := range reasons {
		if i > 0 {
			reason += ", "
		}
		reason += r
	}
	return heavy, immediate, calm, reason
}

// Update advances the state machine. It returns the new state and whether it
// changed. The caller must stop running tasks when the state becomes
// "yielding" and must not take new work unless the state is "ready".
func (y *Yield) Update(now time.Time, t api.Telemetry) (string, bool) {
	heavy, immediate, calm, why := y.assess(t)
	prev := y.state
	switch y.state {
	case api.AgentReady:
		if !heavy {
			y.heavySince = time.Time{}
			y.reason = ""
			break
		}
		if y.heavySince.IsZero() {
			y.heavySince = now
		}
		if immediate || now.Sub(y.heavySince).Seconds() >= y.cfg.SustainSeconds {
			y.state, y.reason = api.AgentBusyUser, why
		}
	case api.AgentBusyUser:
		if heavy {
			y.reason = why
		}
		if calm {
			y.state, y.calmSince, y.reason = api.AgentCooldown, now, "waiting for user load to settle"
		}
	case api.AgentCooldown:
		switch {
		case heavy:
			y.state, y.reason = api.AgentBusyUser, why
		case !calm:
			y.calmSince = now
		case now.Sub(y.calmSince).Seconds() >= y.cfg.CooldownSeconds:
			y.state, y.reason, y.heavySince = api.AgentReady, "", time.Time{}
		}
	}
	return y.state, y.state != prev
}
