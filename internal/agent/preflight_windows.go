//go:build windows

package agent

// canExec: Windows is only supported for development; existence and
// regular-file checks are enough there.
func canExec(path string) error { return nil }
