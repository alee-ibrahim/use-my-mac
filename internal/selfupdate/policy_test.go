package selfupdate

import (
	"fmt"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestCompare(t *testing.T) {
	order := []string{"v0.1.0", "v0.1.1-rc.1", "v0.1.1-rc.2", "v0.1.1", "v0.1.1-3-gabcdef1", "v0.1.1-10-gabcdef1-dirty", "v0.1.2", "v0.2.0", "v1.0.0"}
	for i := range order {
		for j := range order {
			a, err := ParseVersion(order[i])
			if err != nil {
				t.Fatal(err)
			}
			b, _ := ParseVersion(order[j])
			want := cmpInt(i, j)
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	for _, bad := range []string{"", "dev", "0.1.0", "v1.2", "v1.2.3.4", "v1.2.3-"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if IsRelease("v0.1.1-3-gabcdef1") || !IsRelease("v0.1.1") || !IsRelease("v0.2.0-rc.1") {
		t.Fatal("IsRelease")
	}
}

func TestDecide(t *testing.T) {
	chk := func(target string, explicit, paused bool) api.UpdateCheck {
		return api.UpdateCheck{TargetVersion: target, AllowDowngrade: explicit, Paused: paused}
	}
	cases := []struct {
		name    string
		current string
		check   api.UpdateCheck
		want    Action
	}{
		{"paused", "v0.1.0", chk("v0.2.0", false, true), Nothing},
		{"no target", "v0.1.0", chk("", false, false), Nothing},
		{"up to date", "v0.2.0", chk("v0.2.0", false, false), Nothing},
		{"upgrade", "v0.1.0", chk("v0.2.0", false, false), Install},
		{"downgrade refused", "v0.3.0", chk("v0.2.0", false, false), Refuse},
		{"downgrade pinned", "v0.3.0", chk("v0.2.0", true, false), Install},
		{"dev build refused", "dev", chk("v0.2.0", false, false), Refuse},
		{"dev build pinned", "dev", chk("v0.2.0", true, false), Install},
		{"describe build newer than target", "v0.2.0-4-gabc1234", chk("v0.2.0", false, false), Refuse},
		{"describe build older than target", "v0.1.9-4-gabc1234", chk("v0.2.0", false, false), Install},
		{"bad target", "v0.1.0", chk("latest", true, false), Refuse},
		{"describe target", "v0.1.0", chk("v0.2.0-1-gabc1234", true, false), Refuse},
	}
	for _, c := range cases {
		if got, why := Decide(c.current, c.check); got != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, got, why, c.want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	s := api.UpdateSettings{}
	if got := ResolveTarget(s, "v0.3.0", "mac_a"); got.Version != "v0.3.0" || got.Explicit {
		t.Fatalf("default = router version, not explicit: %+v", got)
	}
	if got := ResolveTarget(s, "dev", "mac_a"); got.Version != "" {
		t.Fatalf("dev router must not push a version: %+v", got)
	}
	s.TargetVersion = "v0.2.0"
	if got := ResolveTarget(s, "v0.3.0", "mac_a"); got.Version != "v0.2.0" || !got.Explicit {
		t.Fatalf("pinned target: %+v", got)
	}
	s.Paused = true
	if got := ResolveTarget(s, "v0.3.0", "mac_a"); got.Version != "" {
		t.Fatalf("paused: %+v", got)
	}
	s.Paused = false

	// Canary by list.
	s.CanaryVersion = "v0.4.0"
	s.CanaryMacs = []string{"mac_canary"}
	if got := ResolveTarget(s, "v0.3.0", "mac_canary"); got.Version != "v0.4.0" || got.Reason != "canary" {
		t.Fatalf("listed canary: %+v", got)
	}
	if got := ResolveTarget(s, "v0.3.0", "mac_other"); got.Version != "v0.2.0" {
		t.Fatalf("non-canary stays on target: %+v", got)
	}

	// Canary by percentage: roughly that share, and stable per Mac.
	s.CanaryMacs = nil
	s.CanaryPercent = 20
	n := 0
	for i := range 1000 {
		id := fmt.Sprintf("mac_%d", i)
		a := ResolveTarget(s, "v0.3.0", id)
		if a != ResolveTarget(s, "v0.3.0", id) {
			t.Fatal("not stable")
		}
		if a.Version == "v0.4.0" {
			n++
		}
	}
	if n < 150 || n > 250 {
		t.Fatalf("20%% canary picked %d of 1000", n)
	}
	s.CanaryPercent = 100
	if got := ResolveTarget(s, "v0.3.0", "mac_x"); got.Version != "v0.4.0" {
		t.Fatal("100% canary")
	}
}

func TestValidateSettings(t *testing.T) {
	ok := api.UpdateSettings{TargetVersion: " v1.0.0 ", CanaryVersion: "v1.1.0-rc.1", CanaryPercent: 10}
	if err := ValidateSettings(&ok); err != nil || ok.TargetVersion != "v1.0.0" || ok.CanaryMacs == nil {
		t.Fatalf("valid settings: %v %+v", err, ok)
	}
	for _, bad := range []api.UpdateSettings{
		{TargetVersion: "latest"}, {CanaryVersion: "1.0"}, {CanaryPercent: 101}, {CanaryPercent: -1},
		{DrainTimeoutSeconds: -1}, {TargetVersion: "v1.0.0-2-gabcdef0"},
	} {
		if ValidateSettings(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
