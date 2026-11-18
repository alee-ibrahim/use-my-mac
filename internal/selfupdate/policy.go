package selfupdate

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// DefaultDrainTimeoutSeconds is how long the updater waits for running tasks.
const DefaultDrainTimeoutSeconds = 3600

// Target is the router's decision for one Mac.
type Target struct {
	Version  string // "" = leave the Mac alone
	Explicit bool   // pinned by an admin: downgrades to it are allowed
	Reason   string
}

// ValidateSettings checks an admin's update settings.
func ValidateSettings(s *api.UpdateSettings) error {
	s.TargetVersion = strings.TrimSpace(s.TargetVersion)
	s.CanaryVersion = strings.TrimSpace(s.CanaryVersion)
	for _, v := range []string{s.TargetVersion, s.CanaryVersion} {
		if v != "" && !IsRelease(v) {
			return fmt.Errorf("%q is not a release version (want vX.Y.Z)", v)
		}
	}
	if s.CanaryPercent < 0 || s.CanaryPercent > 100 {
		return fmt.Errorf("canary_percent must be between 0 and 100")
	}
	if s.DrainTimeoutSeconds < 0 || s.DrainTimeoutSeconds > 7*24*3600 {
		return fmt.Errorf("drain_timeout_seconds must be between 0 and 604800")
	}
	if s.CanaryMacs == nil {
		s.CanaryMacs = []string{}
	}
	return nil
}

// CanaryBucket maps a Mac to 0..99, stable across versions, so the same Macs
// are the canary share every time.
func CanaryBucket(macID string) int {
	h := sha256.Sum256([]byte("umm-canary:" + macID))
	return int(binary.BigEndian.Uint32(h[:4]) % 100)
}

// IsCanary reports whether a Mac is in the canary group.
func IsCanary(s api.UpdateSettings, macID string) bool {
	return slices.Contains(s.CanaryMacs, macID) || CanaryBucket(macID) < s.CanaryPercent
}

// ResolveTarget picks the version a Mac should run.
func ResolveTarget(s api.UpdateSettings, routerVersion, macID string) Target {
	if s.Paused {
		return Target{Reason: "updates paused"}
	}
	if s.CanaryVersion != "" && IsCanary(s, macID) {
		return Target{Version: s.CanaryVersion, Explicit: true, Reason: "canary"}
	}
	if s.TargetVersion != "" {
		return Target{Version: s.TargetVersion, Explicit: true, Reason: "fleet target"}
	}
	if !IsRelease(routerVersion) {
		return Target{Reason: fmt.Sprintf("router version %q is not a release; set a target version", routerVersion)}
	}
	return Target{Version: routerVersion, Reason: "router version"}
}

// Action is what the updater should do.
type Action int

const (
	// Nothing: up to date, paused or no target.
	Nothing Action = iota
	// Install the target version.
	Install
	// Refuse: the target would be a downgrade (or the current build is not
	// a release) and nobody pinned it.
	Refuse
)

// Decide compares the installed version with the router's answer.
func Decide(current string, c api.UpdateCheck) (Action, string) {
	if c.Paused {
		return Nothing, "updates paused"
	}
	if c.TargetVersion == "" {
		return Nothing, "no target version" + reasonSuffix(c.Reason)
	}
	want, err := ParseVersion(c.TargetVersion)
	if err != nil || want.Describe != 0 {
		return Refuse, fmt.Sprintf("router asked for %q, which is not a release version", c.TargetVersion)
	}
	have, err := ParseVersion(current)
	if err != nil {
		if c.AllowDowngrade {
			return Install, fmt.Sprintf("replacing non-release build %q with pinned %s", current, want)
		}
		return Refuse, fmt.Sprintf("installed build %q is not a release; pin a target version to replace it", current)
	}
	switch Compare(want, have) {
	case 0:
		return Nothing, "up to date"
	case 1:
		return Install, fmt.Sprintf("%s -> %s%s", have, want, reasonSuffix(c.Reason))
	}
	if c.AllowDowngrade {
		return Install, fmt.Sprintf("%s -> %s (downgrade pinned by admin)%s", have, want, reasonSuffix(c.Reason))
	}
	return Refuse, fmt.Sprintf("%s is older than installed %s; refusing to downgrade unless an admin pins it", want, have)
}

func reasonSuffix(r string) string {
	if r == "" {
		return ""
	}
	return " (" + r + ")"
}
