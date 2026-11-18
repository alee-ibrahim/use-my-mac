//go:build darwin

package agent

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// From <sys/resource.h>.
const (
	prioDarwinProcess = 4
	prioDarwinBG      = 0x1000
)

// setGroupBackground puts every process in the task's process group into
// (bg=true) or out of (bg=false) darwin background state, the same thing
// `taskpolicy -b|-B -p PID` does. Tasks run as our own uid, so no
// privileges are needed. Processes forked later inherit the state.
func setGroupBackground(pgid int, bg bool) error {
	if pgid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return err
	}
	prio := 0
	if bg {
		prio = prioDarwinBG
	}
	var errs []error
	for _, pid := range PIDsInGroup(string(out), pgid) {
		if err := syscall.Setpriority(prioDarwinProcess, pid, prio); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
