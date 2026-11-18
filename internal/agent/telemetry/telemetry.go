// Package telemetry collects the signals the scheduler and the yield policy
// need. The macOS collector shells out to stock tools (ioreg, ps, sysctl,
// vm_stat, pmset, df) so it needs no cgo and no admin rights; all parsing is
// done by pure functions that are unit-tested on every OS with recorded
// output. Only the wiring that picks the collector is build-tagged.
package telemetry

import (
	"context"
	"os/exec"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Collector produces a telemetry snapshot.
type Collector interface {
	Collect(ctx context.Context) (api.Telemetry, error)
}

// Func adapts a function to Collector (handy for tests and simulations).
type Func func(ctx context.Context) (api.Telemetry, error)

// Collect implements Collector.
func (f Func) Collect(ctx context.Context) (api.Telemetry, error) { return f(ctx) }

// Runner runs a command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs real commands with a short timeout.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// Options configure the collectors.
type Options struct {
	// WorkDir is where tasks run; its volume's free space is reported.
	WorkDir string
	// UserLoadScope: "console" counts only the console user's processes as
	// user load (default); "all" counts every process that is not ours.
	UserLoadScope string
	// OurUID is the uid the agent and its tasks run as.
	OurUID int
	// OwnProcessGroups returns the process-group IDs of running tasks so their
	// load is never mistaken for the user's.
	OwnProcessGroups func() map[int]bool
}
