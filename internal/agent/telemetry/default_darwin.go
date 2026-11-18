//go:build darwin

package telemetry

// NewDefault returns the macOS collector.
func NewDefault(opts Options) Collector {
	return &MacOS{Run: ExecRunner, Opts: opts}
}
