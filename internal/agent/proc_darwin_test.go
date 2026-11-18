//go:build darwin

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Runs on a Mac only: a child process group is moved into and out of darwin
// background state. Background shows up as scheduler priority 4 in ps.
func TestSetGroupBackgroundOnMac(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & sleep 30; wait")
	setupProc(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killGroup(cmd.Process.Pid)
	time.Sleep(200 * time.Millisecond)
	pgid := cmd.Process.Pid
	pris := func() []int {
		out, err := exec.Command("/bin/ps", "-axo", "pgid=,pri=").Output()
		if err != nil {
			t.Fatal(err)
		}
		var p []int
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && f[0] == strconv.Itoa(pgid) {
				v, _ := strconv.Atoi(f[1])
				p = append(p, v)
			}
		}
		return p
	}
	if err := setGroupBackground(pgid, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range pris() {
		if p != 4 {
			t.Fatalf("after demote: priorities %v, want all 4", pris())
		}
	}
	if err := setGroupBackground(pgid, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range pris() {
		if p == 4 {
			t.Fatalf("after promote: priorities %v, want none at 4", pris())
		}
	}
	if len(pris()) < 3 {
		t.Fatalf("expected sh + 2 sleeps in the group, got %v", pris())
	}
}
