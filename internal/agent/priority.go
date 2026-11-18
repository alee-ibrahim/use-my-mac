package agent

import (
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Priority modes and the effective QoS a task runs at.
const (
	PriorityAdaptive = "adaptive"
	QoSUtility       = "utility"
	QoSBackground    = "background"
)

// AdaptiveQoS decides how hard a task may push the Mac right now. Nobody at
// the console, a locked screen, or a user idle for at least idle means
// nobody is waiting on the machine, so tasks run at utility QoS (all cores,
// still below the user's apps). Otherwise they run at background QoS, which
// on Apple Silicon keeps CPU work on the efficiency cores. Unknown idle time
// (negative) counts as active.
func AdaptiveQoS(t api.Telemetry, idle time.Duration) string {
	switch {
	case t.ConsoleUser == "", t.ScreenLocked:
		return QoSUtility
	case t.UserIdleSeconds >= 0 && t.UserIdleSeconds >= idle.Seconds():
		return QoSUtility
	default:
		return QoSBackground
	}
}

// launchPriority maps the configured mode and the current QoS to the
// priority BuildArgv understands. Adaptive tasks always get the utility
// clamp; background is layered on top (and can be lifted later) so a task
// can move between the two without restarting.
func launchPriority(mode, qos string) string {
	if mode != PriorityAdaptive {
		return mode
	}
	if qos == QoSBackground {
		return "adaptive-background"
	}
	return "adaptive-utility"
}

// PIDsInGroup returns the PIDs in process group pgid from
// `ps -axo pid=,pgid=` output.
func PIDsInGroup(psOut string, pgid int) []int {
	var out []int
	for _, line := range strings.Split(psOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && g == pgid && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}
