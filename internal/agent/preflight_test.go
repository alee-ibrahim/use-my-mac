package agent

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func writeProg(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
}

func TestCheckProgram(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	writeProg(t, exe, 0o755)
	noexec := filepath.Join(dir, "data")
	writeProg(t, noexec, 0o644)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, want string
	}{
		{exe, ""},
		{link, ""},
		{filepath.Join(dir, "missing"), "program not found"},
		{dangling, "program not found"},
		{dir, "not a regular file"},
		{noexec, "not executable"},
	}
	for _, c := range cases {
		got := CheckProgram(c.path)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("CheckProgram(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestCheckProgramParentNotTraversable(t *testing.T) {
	skipWindows(t)
	if os.Getuid() == 0 {
		t.Skip("root can traverse anything")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "tool")
	writeProg(t, exe, 0o755)
	if err := os.Chmod(dir, 0o600); err != nil { // no x: cannot traverse
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if got := CheckProgram(exe); !strings.Contains(got, "not accessible") {
		t.Fatalf("got %q", got)
	}
}

func newTestAgent(t *testing.T, rts map[string][]string) *Agent {
	t.Helper()
	cfg := &Config{Server: "http://127.0.0.1:1", WorkDir: t.TempDir(), Runtimes: rts, Priority: "none"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	return New(cfg, "cred", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestPreflightAdvertisesOnlyRunnableRuntimes(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writeProg(t, good, 0o755)
	later := filepath.Join(dir, "later") // installed after the agent starts
	a := newTestAgent(t, map[string][]string{"good": {good}, "later": {later, "-x"}})

	avail, unavail := a.RuntimeStatus()
	if strings.Join(avail, ",") != "good" || len(unavail) != 1 || unavail[0].Name != "later" ||
		unavail[0].Program != later || !strings.Contains(unavail[0].Reason, "program not found") ||
		unavail[0].Source != api.RuntimeSourcePreflight {
		t.Fatalf("avail %v unavail %+v", avail, unavail)
	}
	if _, _, why := a.runtimeFor("later"); !strings.Contains(why, "unavailable") {
		t.Fatalf("runtimeFor must refuse: %q", why)
	}
	id1 := a.preflightID

	writeProg(t, later, 0o755)
	a.Preflight()
	if avail, unavail = a.RuntimeStatus(); len(avail) != 2 || len(unavail) != 0 {
		t.Fatalf("after install: avail %v unavail %+v", avail, unavail)
	}
	if a.preflightID <= id1 {
		t.Fatal("preflight ID must increase")
	}

	// Reload drops a runtime and adds a broken one.
	a.ReloadRuntimes(map[string][]string{"good": {good}, "dir": {dir}})
	if avail, unavail = a.RuntimeStatus(); strings.Join(avail, ",") != "good" || len(unavail) != 1 ||
		!strings.Contains(unavail[0].Reason, "regular file") {
		t.Fatalf("after reload: avail %v unavail %+v", avail, unavail)
	}
}

// The heartbeat re-runs the preflight once PreflightInterval has passed, so
// a program installed later shows up without a restart.
func TestHeartbeatRerunsPreflight(t *testing.T) {
	skipWindows(t)
	prog := filepath.Join(t.TempDir(), "tool")
	a := newTestAgent(t, map[string][]string{"tool": {prog}})
	a.cfg.PreflightInterval = time.Hour
	writeProg(t, prog, 0o755)
	ctx := t.Context()
	a.heartbeatOnce(ctx) // router unreachable: only the local part matters
	if avail, _ := a.RuntimeStatus(); len(avail) != 0 {
		t.Fatal("must not re-check before the interval")
	}
	a.mu.Lock()
	a.lastPreflight = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	a.heartbeatOnce(ctx)
	if avail, _ := a.RuntimeStatus(); len(avail) != 1 {
		t.Fatalf("tool installed later must show up: %v", avail)
	}
}

func TestExecFaultHoldsUntilProgramChanges(t *testing.T) {
	skipWindows(t)
	prog := filepath.Join(t.TempDir(), "tool")
	writeProg(t, prog, 0o755)
	a := newTestAgent(t, map[string][]string{"tool": {prog}})
	a.markExecFault("tool", "exit 127: command not found")
	a.Preflight()
	_, unavail := a.RuntimeStatus()
	if len(unavail) != 1 || unavail[0].Source != api.RuntimeSourceExec {
		t.Fatalf("exec fault must survive a passing preflight of an unchanged program: %+v", unavail)
	}
	// Reinstalling the program clears it at the next preflight.
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n# v2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Preflight()
	if avail, unavail := a.RuntimeStatus(); len(avail) != 1 || len(unavail) != 0 {
		t.Fatalf("changed program must be retried: %v %+v", avail, unavail)
	}
	// So does ExecFaultRetry.
	a.markExecFault("tool", "exit 127")
	a.mu.Lock()
	f := a.faults["tool"]
	f.at = time.Now().Add(-ExecFaultRetry - time.Second)
	a.faults["tool"] = f
	a.preflightLocked(time.Now())
	a.mu.Unlock()
	if avail, _ := a.RuntimeStatus(); len(avail) != 1 {
		t.Fatal("exec fault must expire")
	}
}

func TestExecFailureReason(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	writeProg(t, exe, 0o755)
	missing := filepath.Join(dir, "missing")
	enoent := &fs.PathError{Op: "fork/exec", Path: missing, Err: fs.ErrNotExist}
	cases := []struct {
		prog  string
		err   error
		code  int
		match string // "" = not a Mac fault
	}{
		{missing, nil, 127, "program not found"},
		{exe, nil, 127, "exit 127"},
		{missing, nil, 126, "program not found"},
		{exe, nil, 126, ""},
		{exe, nil, 1, ""},
		{missing, nil, 1, ""},
		{missing, enoent, 0, "program not found"},
		{exe, errors.New("some other start error"), 0, ""},
	}
	for i, c := range cases {
		got := execFailureReason(c.prog, c.err, c.code)
		if (c.match == "") != (got == "") || !strings.Contains(got, c.match) {
			t.Errorf("case %d: got %q want %q", i, got, c.match)
		}
	}
}

// A real exec: a runtime whose program vanished before the task starts is
// reported as RuntimeUnavailable and marked unavailable; the next task is
// refused up front.
func TestExecuteMissingProgramIsMacFault(t *testing.T) {
	skipWindows(t)
	prog := filepath.Join(t.TempDir(), "tool")
	writeProg(t, prog, 0o755)
	a := newTestAgent(t, map[string][]string{"tool": {prog}})
	a.goos = "test" // no priority wrapper: exec the program directly
	if err := os.Remove(prog); err != nil {
		t.Fatal(err)
	}
	upd := a.execute(t.Context(), &run{as: api.Assignment{TaskID: "t1", Attempt: 1, Runtime: "tool"}})
	if upd.Outcome != api.OutcomeFailed || !strings.Contains(upd.RuntimeUnavailable, "program not found") {
		t.Fatalf("got %+v", upd)
	}
	if _, _, why := a.runtimeFor("tool"); why == "" {
		t.Fatal("runtime must be unavailable after the exec failure")
	}
	upd = a.execute(t.Context(), &run{as: api.Assignment{TaskID: "t2", Attempt: 1, Runtime: "tool"}})
	if upd.RuntimeUnavailable == "" {
		t.Fatalf("got %+v", upd)
	}
}
