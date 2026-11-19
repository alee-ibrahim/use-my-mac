// Package sched contains the pure scheduling logic: hard filters, the
// weighted score that picks the "ideal Mac" for a task, and retry backoff.
// It has no I/O so it is trivially unit-testable.
package sched

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Weights control how much each factor matters. They are relative; the final
// score is normalised to 0..100.
type Weights struct {
	Presence float64 `json:"presence" yaml:"presence"` // nobody there / locked / idle beats an active user
	Load     float64 `json:"load" yaml:"load"`         // low CPU load (user + ours)
	Spread   float64 `json:"spread" yaml:"spread"`     // prefer Macs running fewer of our tasks
	Memory   float64 `json:"memory" yaml:"memory"`     // memory headroom after this task
	Power    float64 `json:"power" yaml:"power"`       // AC power beats battery
	Thermal  float64 `json:"thermal" yaml:"thermal"`   // nominal thermal state
	GPU      float64 `json:"gpu" yaml:"gpu"`           // idle GPU (only for tasks that need a GPU)
	Capacity float64 `json:"capacity" yaml:"capacity"` // bigger Macs (more cores) slightly preferred
}

// Limits are hard thresholds used by the filters.
type Limits struct {
	// IdleFullSeconds: a user idle for this long is treated as away.
	IdleFullSeconds float64 `json:"idle_full_seconds" yaml:"idle_full_seconds"`
	// MinDiskFreeMB is always kept free on the Mac.
	MinDiskFreeMB int `json:"min_disk_free_mb" yaml:"min_disk_free_mb"`
	// AllowBattery lets work run on Macs that are on battery.
	AllowBattery bool `json:"allow_battery" yaml:"allow_battery"`
	// MinBatteryPercent is the floor when AllowBattery is true.
	MinBatteryPercent int `json:"min_battery_percent" yaml:"min_battery_percent"`
}

// DefaultWeights returns sensible defaults.
func DefaultWeights() Weights {
	return Weights{Presence: 3, Load: 2, Spread: 1.5, Memory: 1, Power: 1, Thermal: 1, GPU: 1.5, Capacity: 0.5}
}

// DefaultLimits returns sensible defaults.
func DefaultLimits() Limits {
	return Limits{IdleFullSeconds: 900, MinDiskFreeMB: 2048, AllowBattery: false, MinBatteryPercent: 40}
}

// Candidate is everything the scorer knows about one Mac.
type Candidate struct {
	ID          string
	Online      bool
	AgentState  string
	Drain       bool
	OptedOut    bool
	Quarantined bool
	Excluded    bool // excluded for this specific task (it failed/yielded here)
	Runtimes    []string
	// Unavailable maps allowlisted runtimes the Mac cannot run to the
	// reason (preflight failure or exec fault). They never match.
	Unavailable map[string]string
	Labels      map[string]string
	MaxTasks    int
	Active      int // our tasks assigned or running on it
	ReservedMB  int // RAM reserved by those tasks
	Telemetry   api.Telemetry
	FleetMaxCPU int // largest CPU count in the fleet (for the capacity factor)
	// Work space reported by the agent (WorkKnown false for older agents):
	// available now, and with no task running.
	WorkKnown    bool
	WorkSpace    uint64
	WorkCapacity uint64
	// ReservedDisk is the disk need of our tasks assigned or running on it.
	ReservedDisk uint64
}

// Need is what the task requires.
type Need struct {
	Runtime   string
	Resources api.Resources
	// DiskBytes is the task's work-space need (see DiskNeed).
	DiskBytes uint64
}

// DiskNeed is a task's work-space need: DiskEstimateMB when set, else twice
// its known input bytes plus DiskMB.
func DiskNeed(r api.Resources, inputBytes uint64) uint64 {
	if r.DiskEstimateMB > 0 {
		return uint64(r.DiskEstimateMB) << 20
	}
	return 2*inputBytes + uint64(max(r.DiskMB, 0))<<20
}

// FitsAnywhere reports whether a task needing need bytes could ever run on
// one of the candidates that can run its runtime: false only when every
// such Mac reports its work capacity and all are too small. biggest is the
// largest capacity seen.
func FitsAnywhere(cs []Candidate, runtime string, need uint64) (ok bool, biggest uint64, known bool) {
	if need == 0 {
		return true, 0, false
	}
	for _, c := range cs {
		if !contains(c.Runtimes, runtime) {
			continue
		}
		if !c.WorkKnown {
			return true, 0, false
		}
		known = true
		biggest = max(biggest, c.WorkCapacity)
		if c.WorkCapacity >= need {
			return true, biggest, true
		}
	}
	return !known, biggest, known
}

// GiB formats bytes for reasons.
func GiB(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// Result is the outcome of scoring one candidate.
type Result struct {
	ID     string             `json:"id" yaml:"id"`
	OK     bool               `json:"ok" yaml:"ok"`
	Reason string             `json:"reason,omitempty"`   // why it was filtered out
	Score  float64            `json:"score" yaml:"score"` // 0..100
	Parts  map[string]float64 `json:"parts,omitempty"`    // factor values 0..1
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// PresenceFactor scores how "away" the user is: 1 = nobody logged in.
func PresenceFactor(t api.Telemetry, idleFull float64) float64 {
	switch {
	case t.ConsoleUser == "":
		return 1
	case t.ScreenLocked:
		return 0.95
	}
	if idleFull <= 0 {
		idleFull = 900
	}
	if t.UserIdleSeconds >= idleFull {
		return 0.85
	}
	idle := math.Max(0, t.UserIdleSeconds)
	return 0.2 + 0.6*(idle/idleFull)
}

func thermalFactor(s string) (float64, bool) {
	switch s {
	case "", "nominal":
		return 1, true
	case "fair":
		return 0.6, true
	case "serious":
		return 0.15, true
	case "critical":
		return 0, false
	}
	return 0.5, true
}

// Score applies the hard filters and computes the weighted score.
func Score(c Candidate, n Need, w Weights, l Limits) Result {
	r := Result{ID: c.ID}
	reject := func(format string, a ...any) Result {
		r.Reason = fmt.Sprintf(format, a...)
		return r
	}
	t := c.Telemetry
	switch {
	case !c.Online:
		return reject("offline")
	case c.Drain:
		return reject("draining")
	case c.OptedOut:
		return reject("opted out")
	case c.Quarantined:
		return reject("quarantined after failures")
	case c.AgentState != "" && c.AgentState != api.AgentReady:
		return reject("agent %s", c.AgentState)
	case c.Excluded:
		return reject("excluded for this task")
	}
	if why, bad := c.Unavailable[n.Runtime]; bad {
		return reject("runtime %q unavailable: %s", n.Runtime, why)
	}
	if !contains(c.Runtimes, n.Runtime) {
		return reject("runtime %q not available", n.Runtime)
	}
	for k, v := range n.Resources.Labels {
		if c.Labels[k] != v {
			return reject("label %s=%s not matched", k, v)
		}
	}
	maxTasks := c.MaxTasks
	if maxTasks <= 0 {
		maxTasks = 1
	}
	if c.Active >= maxTasks {
		return reject("full (%d/%d)", c.Active, maxTasks)
	}

	// Memory.
	needB := uint64(n.Resources.RAMMB) << 20
	reservedB := uint64(c.ReservedMB) << 20
	memF := 0.5
	if t.MemTotalBytes > 0 {
		if needB > t.MemTotalBytes {
			return reject("needs %d MB RAM, Mac has %d MB", n.Resources.RAMMB, t.MemTotalBytes>>20)
		}
		avail := t.MemAvailableBytes
		if avail < reservedB+needB {
			return reject("not enough free memory")
		}
		after := float64(avail-reservedB-needB) / float64(t.MemTotalBytes)
		memF = clamp01(after * 2) // 50% free after placement = perfect
	}
	switch t.MemoryPressure {
	case "critical":
		return reject("memory pressure critical")
	case "warn":
		memF *= 0.4
	}

	// Disk.
	if t.DiskFreeBytes > 0 {
		needDisk := uint64(l.MinDiskFreeMB+n.Resources.DiskMB) << 20
		if t.DiskFreeBytes < needDisk {
			return reject("not enough free disk")
		}
	}

	// Work space (agents that report it).
	if c.WorkKnown && n.DiskBytes > 0 {
		if n.DiskBytes > c.WorkCapacity {
			return reject("needs %s work space, Mac holds at most %s", GiB(n.DiskBytes), GiB(c.WorkCapacity))
		}
		if c.WorkSpace < c.ReservedDisk+n.DiskBytes {
			return reject("not enough work space now (needs %s, %s free)", GiB(n.DiskBytes),
				GiB(c.WorkSpace-min(c.WorkSpace, c.ReservedDisk)))
		}
	}

	// Thermal.
	thermF, ok := thermalFactor(t.Thermal)
	if !ok {
		return reject("thermal critical")
	}

	// Power.
	powF := 1.0
	if !t.OnAC {
		if !l.AllowBattery {
			return reject("on battery")
		}
		if t.BatteryPercent >= 0 && t.BatteryPercent < l.MinBatteryPercent {
			return reject("battery below %d%%", l.MinBatteryPercent)
		}
		powF = 0.3
	}

	parts := map[string]float64{}
	weights := map[string]float64{}
	add := func(name string, weight, value float64) {
		if weight <= 0 {
			return
		}
		parts[name] = clamp01(value)
		weights[name] = weight
	}

	presW := w.Presence
	if n.Resources.ExpectedSeconds > 0 {
		// Long work cares more about landing on a quiet Mac.
		presW *= 1 + math.Min(float64(n.Resources.ExpectedSeconds)/3600, 1)
	}
	add("presence", presW, PresenceFactor(t, l.IdleFullSeconds))
	add("load", w.Load, 1-(math.Max(0, t.UserCPUPercent)+math.Max(0, t.OurCPUPercent))/100)
	add("spread", w.Spread, 1-float64(c.Active)/float64(maxTasks))
	add("memory", w.Memory, memF)
	add("power", w.Power, powF)
	add("thermal", w.Thermal, thermF)
	if n.Resources.GPU {
		g := 0.5
		if t.GPUUtilPercent >= 0 {
			g = 1 - t.GPUUtilPercent/100
		}
		add("gpu", w.GPU, g)
	}
	if c.FleetMaxCPU > 0 && t.CPUCount > 0 {
		add("capacity", w.Capacity, float64(t.CPUCount)/float64(c.FleetMaxCPU))
	}

	var sum, wsum float64
	for k, v := range parts {
		sum += v * weights[k]
		wsum += weights[k]
	}
	if wsum > 0 {
		r.Score = math.Round(sum/wsum*10000) / 100
	}
	r.OK = true
	r.Parts = parts
	return r
}

// Pick scores all candidates and returns the best OK one (ties broken by ID
// for determinism) plus every result for explainability.
func Pick(cands []Candidate, n Need, w Weights, l Limits) (best *Result, all []Result) {
	for _, c := range cands {
		all = append(all, Score(c, n, w, l))
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].OK != all[j].OK {
			return all[i].OK
		}
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].ID < all[j].ID
	})
	if len(all) > 0 && all[0].OK {
		b := all[0]
		return &b, all
	}
	return nil, all
}

// Backoff returns the delay before the next attempt after `failures` failed
// attempts: base * 2^(failures-1), capped at max.
func Backoff(failures int, base, max time.Duration) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
