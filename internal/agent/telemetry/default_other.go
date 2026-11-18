//go:build !darwin

package telemetry

import (
	"context"
	"runtime"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// NewDefault returns a minimal collector on non-macOS systems so the agent can
// be developed and tested on Linux/Windows. It reports an idle, mains-powered
// machine with unknown memory/GPU.
func NewDefault(opts Options) Collector {
	return Func(func(context.Context) (api.Telemetry, error) {
		return api.Telemetry{
			CollectedAt:    time.Now().UTC(),
			CPUCount:       runtime.NumCPU(),
			OnAC:           true,
			BatteryPercent: -1,
			GPUUtilPercent: -1,
			MemoryPressure: "normal",
			Thermal:        "nominal",
			OS:             runtime.GOOS,
		}, nil
	})
}
