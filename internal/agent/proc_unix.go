//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setupProc puts the task in its own process group so the whole tree can be
// stopped, and makes cancellation send SIGTERM to the group first.
func setupProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// killGroup SIGKILLs anything left in the task's process group.
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
