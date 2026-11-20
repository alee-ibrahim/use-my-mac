//go:build unix

package selfupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// checkRootOwned refuses, when running as root, a directory that root does
// not own: someone else could swap files in it under us.
func checkRootOwned(dir string, fi os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not root", dir, st.Uid)
	}
	return nil
}

// chownRoot makes a file root:wheel when running as root.
func chownRoot(path string) {
	if os.Geteuid() == 0 {
		_ = os.Chown(path, 0, 0)
	}
}

// Lock takes an exclusive, non-blocking lock so two updaters never overlap.
// The returned func releases it.
func Lock(dir string) (func(), error) {
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another updater is running")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// detachShell starts /bin/sh -c script in a new session, so it is neither
// in this process's process group (which launchd may kill when the job's
// main process exits) nor waited for. Its output goes to our stderr (the
// updater log).
func detachShell(script string) error {
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
