package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Output shaped like `launchctl print` on the Mac where the updater got
// stuck (labels and hashes made up).
const printStuck = `system/com.example.umm-agent.updater = {
	active count = 0
	path = /Library/LaunchDaemons/com.example.umm-agent.updater.plist
	type = LaunchDaemon
	state = not running

	program = /usr/local/bin/umm-agent
	arguments = {
		/usr/local/bin/umm-agent
		update
	}

	stdout path = /usr/local/var/log/umm-updater.log
	runs = 12
	last exit code = 78: EX_CONFIG
	job state = spawn failed

	spawn type = daemon (3)
	properties = needs LWCR update | managed LWCR | has LWCR
	lwcr = {
		reqs: {cdhash: {$in: [1111111111111111111111111111111111111111, 2222222222222222222222222222222222222222]}}
	}
}
`

const printRunning = `system/com.example.umm-agent = {
	active count = 1
	path = /Library/LaunchDaemons/com.example.umm-agent.plist
	state = running

	program = /usr/local/bin/umm-agent
	environment = {
		state = ignored-nested
	}
	pid = 4242
	properties = managed LWCR | has LWCR
	lwcr = {
		reqs: {cdhash: {$in: [3333333333333333333333333333333333333333]}}
	}
}
`

func TestParseLaunchdPrint(t *testing.T) {
	p := ParseLaunchdPrint(printStuck)
	if p.State != "spawn failed" || p.LastExit != "78: EX_CONFIG" || !p.has("needs LWCR update") || !p.has("has LWCR") {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.LWCRCDHashes, ",") != strings.Repeat("1", 40)+","+strings.Repeat("2", 40) {
		t.Fatalf("hashes %v", p.LWCRCDHashes)
	}
	if stale, why := p.staleFor(""); !stale || !strings.Contains(why, "needs LWCR update") {
		t.Fatal("stuck job not stale")
	}

	p = ParseLaunchdPrint(printRunning)
	if p.State != "running" || p.PID != 4242 || len(p.LWCRCDHashes) != 1 {
		t.Fatalf("%+v", p)
	}
	if stale, _ := p.staleFor(strings.Repeat("3", 40)); stale {
		t.Fatal("matching cdhash reported stale")
	}
	if stale, why := p.staleFor(strings.Repeat("4", 40)); !stale || !strings.Contains(why, "cdhash") {
		t.Fatal("other cdhash not stale")
	}
	// No constraint at all (most Macs): never stale.
	p = ParseLaunchdPrint("system/x = {\n\tstate = running\n\tpid = 1\n}\n")
	if stale, _ := p.staleFor(strings.Repeat("4", 40)); stale || len(p.LWCRCDHashes) != 0 {
		t.Fatalf("%+v", p)
	}
	// Unknown output parses as nothing.
	if p := ParseLaunchdPrint("garbage"); p.State != "" || len(p.Properties) != 0 {
		t.Fatalf("%+v", p)
	}
}

// fakeLaunchd simulates launchd's system domain for Launchctl: a job is
// registered with the binary's cdhash at bootstrap time (like BTM's managed
// LWCR), and refuses to spawn once the binary's cdhash changes.
type fakeLaunchd struct {
	bin    string
	jobs   map[string]*fakeJob // label -> job
	plists map[string]string   // plist path -> label
	calls  []string
	// bootstrapFails: number of bootstraps that fail with "5: Input/output error".
	bootstrapFails int
}

type fakeJob struct {
	running bool
	cdhash  string
}

func (f *fakeLaunchd) cdhash() string {
	b, _ := os.ReadFile(f.bin)
	return strings.Repeat(fmt.Sprintf("%02x", len(b)%256), 20)
}

func (f *fakeLaunchd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Base(name) == "codesign" {
		return []byte("Executable=" + f.bin + "\nCDHash=" + f.cdhash() + "\n"), nil
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "print":
		label := strings.TrimPrefix(args[1], "system/")
		j := f.jobs[label]
		if j == nil {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		state, props := "not running", "managed LWCR | has LWCR"
		if j.running {
			state = "running"
		}
		if j.cdhash != f.cdhash() {
			props = "needs LWCR update | " + props
		}
		return []byte(fmt.Sprintf("%s = {\n\tstate = %s\n\tproperties = %s\n\tlwcr = {\n\t\treqs: {cdhash: {$in: [%s]}}\n\t}\n}\n",
			args[1], state, props, j.cdhash)), nil
	case "bootout":
		label := strings.TrimPrefix(args[1], "system/")
		if f.jobs[label] == nil {
			return []byte("Boot-out failed: 3: No such process"), errors.New("exit status 3")
		}
		delete(f.jobs, label)
		return nil, nil
	case "bootstrap":
		if f.bootstrapFails > 0 {
			f.bootstrapFails--
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		label := f.plists[args[2]]
		if f.jobs[label] != nil {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.jobs[label] = &fakeJob{running: true, cdhash: f.cdhash()}
		return nil, nil
	case "kickstart":
		label := strings.TrimPrefix(args[2], "system/")
		if j := f.jobs[label]; j != nil {
			// kickstart keeps the registration: a stale constraint stays.
			j.running = j.cdhash == f.cdhash()
			return nil, nil
		}
		return nil, errors.New("exit status 113")
	}
	return nil, fmt.Errorf("unexpected launchctl %v", args)
}

func newFakeLaunchd(t *testing.T) (*fakeLaunchd, Launchctl, *[]string) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "umm-agent")
	os.WriteFile(bin, []byte("old"), 0o755)
	pl := filepath.Join(dir, "LaunchDaemons")
	os.MkdirAll(pl, 0o755)
	f := &fakeLaunchd{bin: bin, jobs: map[string]*fakeJob{}, plists: map[string]string{}}
	for _, l := range []string{"com.example.umm-agent", "com.example.umm-agent.updater", "com.example.umm-agent.runtimes"} {
		p := filepath.Join(pl, l+".plist")
		os.WriteFile(p, []byte("<plist/>"), 0o644)
		f.plists[p] = l
		f.jobs[l] = &fakeJob{cdhash: f.cdhash()}
	}
	f.jobs["com.example.umm-agent"].running = true
	var detached []string
	l := Launchctl{Label: "com.example.umm-agent", Plist: filepath.Join(pl, "com.example.umm-agent.plist"), Bin: bin,
		Run: f.run, Sleep: func(context.Context, time.Duration) {},
		Detach: func(s string) error { detached = append(detached, s); return nil }}
	return f, l, &detached
}

func TestLaunchctlRestartReregisters(t *testing.T) {
	f, l, _ := newFakeLaunchd(t)
	ctx := context.Background()
	os.WriteFile(f.bin, []byte("new binary"), 0o755) // the swap

	// Jobs sees every job pinned to the old cdhash.
	jobs, err := l.Jobs(ctx)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("%v %v", jobs, err)
	}
	for _, j := range jobs {
		if !j.Stale || !j.Loaded {
			t.Fatalf("not stale after the swap: %+v", j)
		}
	}
	if !jobs[0].Running || jobs[0].Role != JobAgent || jobs[1].Role != JobUpdater || jobs[2].Role != JobRuntimes {
		t.Fatalf("%+v", jobs)
	}

	// Restart: bootout, wait until gone, bootstrap (retrying the transient
	// launchd refusal) -> the agent runs with a fresh constraint.
	f.bootstrapFails = 2
	f.calls = nil
	if err := l.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	want := "bootout system/com.example.umm-agent|print system/com.example.umm-agent|bootstrap system " + l.Plist +
		"|bootstrap system " + l.Plist + "|bootstrap system " + l.Plist
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls\n%s\nwant\n%s", got, want)
	}
	if j := f.jobs["com.example.umm-agent"]; j == nil || !j.running || j.cdhash != f.cdhash() {
		t.Fatalf("agent job %+v", j)
	}

	// The helper: reloaded in place.
	if err := l.Reregister(ctx, jobs[2]); err != nil {
		t.Fatal(err)
	}
	jobs, _ = l.Jobs(ctx)
	if jobs[0].Stale || jobs[2].Stale || !jobs[1].Stale {
		t.Fatalf("%+v", jobs)
	}
}

func TestLaunchctlReloadsItselfDetached(t *testing.T) {
	f, l, detached := newFakeLaunchd(t)
	ctx := context.Background()
	os.WriteFile(f.bin, []byte("new binary"), 0o755)
	jobs, _ := l.Jobs(ctx)
	f.calls = nil
	if err := l.Reregister(ctx, jobs[1]); err != nil {
		t.Fatal(err)
	}
	// Nothing done in-process: a bootout would kill this very process.
	if len(f.calls) != 0 || len(*detached) != 1 {
		t.Fatalf("calls %v detached %d", f.calls, len(*detached))
	}
	s := (*detached)[0]
	upd := filepath.Join(filepath.Dir(l.Plist), "com.example.umm-agent.updater.plist")
	for _, want := range []string{
		fmt.Sprintf("kill -0 %d", os.Getpid()),
		"'/bin/launchctl' bootout 'system/com.example.umm-agent.updater'",
		"'/bin/launchctl' bootstrap system '" + upd + "'",
		"trap '' HUP INT TERM",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "bootout") > strings.Index(s, "bootstrap") || strings.Index(s, "kill -0") > strings.Index(s, "bootout") {
		t.Fatalf("wrong order:\n%s", s)
	}
}

func TestReloadScriptQuotes(t *testing.T) {
	s := ReloadScript("/bin/launchctl", "a'b", "/x y/a'b.plist", 7)
	if !strings.Contains(s, `'system/a'\''b'`) || !strings.Contains(s, `'/x y/a'\''b.plist'`) {
		t.Fatal(s)
	}
}

func TestLaunchctlWithoutPlist(t *testing.T) {
	f, l, _ := newFakeLaunchd(t)
	l.Plist = ""
	f.calls = nil
	if err := l.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "|") != "kickstart -k system/com.example.umm-agent" {
		t.Fatalf("%v", f.calls)
	}
	if jobs, _ := l.Jobs(context.Background()); len(jobs) != 0 {
		t.Fatalf("%v", jobs)
	}
	// A job that is not loaded: the agent's must be, the helpers' need not.
	_, l, _ = newFakeLaunchd(t)
	f2 := l.Run
	l.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "print" {
			return nil, errors.New("exit status 113")
		}
		return f2(ctx, name, args...)
	}
	jobs, _ := l.Jobs(context.Background())
	if !jobs[0].Stale || jobs[0].Loaded || jobs[1].Stale || jobs[2].Stale {
		t.Fatalf("%+v", jobs)
	}
}
