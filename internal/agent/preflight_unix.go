//go:build !windows

package agent

import "syscall"

// canExec asks the kernel whether this process (the agent's user, e.g.
// _umm) may execute path. access(2) checks the real uid, which is the
// agent's own: it never runs setuid.
func canExec(path string) error {
	const xOK = 1 // X_OK
	return syscall.Access(path, xOK)
}
