package sched

import (
	"strings"
	"testing"
)

// A Mac that can install a managed runtime on demand qualifies, but one
// that already has it is preferred.
func TestInstallableRuntimesQualifyButInstalledWins(t *testing.T) {
	warm := baseCand("warm")
	cold := baseCand("cold")
	cold.Runtimes, cold.Installable = nil, []string{"ffmpeg"}
	none := baseCand("none")
	none.Runtimes = nil
	best, all := Pick([]Candidate{cold, warm, none}, need, DefaultWeights(), DefaultLimits())
	if best == nil || best.ID != "warm" {
		t.Fatalf("best %+v", best)
	}
	byID := map[string]Result{}
	for _, r := range all {
		byID[r.ID] = r
	}
	if !byID["cold"].OK || strings.Join(byID["cold"].Install, ",") != "ffmpeg" || byID["cold"].Score >= byID["warm"].Score {
		t.Fatalf("cold %+v warm %+v", byID["cold"], byID["warm"])
	}
	if byID["none"].OK {
		t.Fatal("a Mac that can neither run nor install it must not qualify")
	}
	// A busy warm Mac can still lose to a quiet one that has to install.
	warm.Telemetry.ConsoleUser, warm.Telemetry.UserIdleSeconds, warm.Telemetry.UserCPUPercent = "u", 1, 60
	if best, _ := Pick([]Candidate{cold, warm}, need, DefaultWeights(), DefaultLimits()); best == nil || best.ID != "cold" {
		t.Fatalf("best %+v", best)
	}
	// Unavailable (e.g. a failed install) beats installable.
	cold.Unavailable = map[string]string{"ffmpeg": "install failed"}
	if r := Score(cold, need, DefaultWeights(), DefaultLimits()); r.OK {
		t.Fatal("unavailable runtime scheduled")
	}
}

func TestRequiredPackages(t *testing.T) {
	n := need
	n.Requires = []string{"models"}
	has := baseCand("has")
	has.Packages = map[string]bool{"models": true}
	can := baseCand("can")
	can.InstallablePackages = []string{"models"}
	cannot := baseCand("cannot")
	if r := Score(has, n, DefaultWeights(), DefaultLimits()); !r.OK || len(r.Install) != 0 {
		t.Fatalf("has: %+v", r)
	}
	if r := Score(can, n, DefaultWeights(), DefaultLimits()); !r.OK || strings.Join(r.Install, ",") != "models" {
		t.Fatalf("can: %+v", r)
	}
	if r := Score(cannot, n, DefaultWeights(), DefaultLimits()); r.OK || !strings.Contains(r.Reason, "models") {
		t.Fatalf("cannot: %+v", r)
	}
}
