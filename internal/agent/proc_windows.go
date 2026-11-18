//go:build windows

package agent

import "os/exec"

// setupProc: Windows is only supported for development; the default
// exec.CommandContext kill is used.
func setupProc(cmd *exec.Cmd) {}

func killGroup(pid int) {}
