package agent

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/agent/telemetry"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestAdaptiveQoS(t *testing.T) {
	idle := 5 * time.Minute
	cases := []struct {
		name string
		tel  api.Telemetry
		want string
	}{
		{"nobody logged in", api.Telemetry{}, QoSUtility},
		{"screen locked", api.Telemetry{ConsoleUser: "alice", ScreenLocked: true}, QoSUtility},
		{"idle past threshold", api.Telemetry{ConsoleUser: "alice", UserIdleSeconds: 300}, QoSUtility},
		{"recently active", api.Telemetry{ConsoleUser: "alice", UserIdleSeconds: 299}, QoSBackground},
		{"unknown idle", api.Telemetry{ConsoleUser: "alice", UserIdleSeconds: -1}, QoSBackground},
	}
	for _, c := range cases {
		if got := AdaptiveQoS(c.tel, idle); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestLaunchArgvAdaptive(t *testing.T) {
	rt := []string{"/bin/x"}
	bg := BuildArgv("darwin", launchPriority(PriorityAdaptive, QoSBackground), false, "/w", rt, nil)
	if want := []string{"/usr/sbin/taskpolicy", "-c", "utility", "-b", "/usr/bin/nice", "-n", "10", "/bin/x"}; !reflect.DeepEqual(bg, want) {
		t.Errorf("adaptive/background argv = %v", bg)
	}
	ut := BuildArgv("darwin", launchPriority(PriorityAdaptive, QoSUtility), false, "/w", rt, nil)
	if want := []string{"/usr/sbin/taskpolicy", "-c", "utility", "/usr/bin/nice", "-n", "10", "/bin/x"}; !reflect.DeepEqual(ut, want) {
		t.Errorf("adaptive/utility argv = %v", ut)
	}
	if got := launchPriority("background", QoSUtility); got != "background" {
		t.Errorf("fixed mode must not change: %s", got)
	}
}

func TestPIDsInGroup(t *testing.T) {
	ps := "  1     1\n 500   500\n 501   500\n 502   777\ngarbage\n"
	if got := PIDsInGroup(ps, 500); !reflect.DeepEqual(got, []int{500, 501}) {
		t.Errorf("got %v", got)
	}
}

type qosCall struct {
	pgid int
	bg   bool
}

// The running task is demoted when the user comes back and promoted when they
// leave, without being stopped.
func TestAdjustQoSDemotesAndPromotes(t *testing.T) {
	cfg := &Config{Server: "http://127.0.0.1:1", WorkDir: t.TempDir(), Runtimes: map[string][]string{"x": {"/bin/x"}}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	var tel api.Telemetry
	var mu sync.Mutex
	collector := func(context.Context) (api.Telemetry, error) { mu.Lock(); defer mu.Unlock(); return tel, nil }
	setTel := func(v api.Telemetry) { mu.Lock(); tel = v; mu.Unlock() }
	a := New(cfg, "cred", telemetry.Func(collector), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls []qosCall
	a.applyQoS = func(pgid int, bg bool) error { calls = append(calls, qosCall{pgid, bg}); return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := false
	r := &run{as: api.Assignment{TaskID: "t1"}, pgid: 4242, qos: QoSUtility,
		cancel: func(error) { stopped = true }}
	a.running["t1"] = r

	// Nobody at the Mac: already utility, nothing to do.
	setTel(api.Telemetry{OnAC: true})
	a.heartbeatOnce(ctx)
	if len(calls) != 0 {
		t.Fatalf("unexpected calls %v", calls)
	}
	// User active (light load): demote, keep running.
	setTel(api.Telemetry{OnAC: true, ConsoleUser: "alice", UserIdleSeconds: 3, UserCPUPercent: 5})
	a.heartbeatOnce(ctx)
	a.heartbeatOnce(ctx) // re-applied while background (covers new children)
	if want := []qosCall{{4242, true}, {4242, true}}; !reflect.DeepEqual(calls, want) || r.qos != QoSBackground {
		t.Fatalf("demote: calls %v qos %s", calls, r.qos)
	}
	// User locks the screen: promote back to utility.
	setTel(api.Telemetry{OnAC: true, ConsoleUser: "alice", ScreenLocked: true})
	a.heartbeatOnce(ctx)
	if last := calls[len(calls)-1]; last != (qosCall{4242, false}) || r.qos != QoSUtility {
		t.Fatalf("promote: calls %v qos %s", calls, r.qos)
	}
	if stopped {
		t.Fatal("task must not be stopped by a priority change")
	}
	// New tasks start at the current QoS.
	if q := a.currentQoS(); q != QoSUtility {
		t.Errorf("currentQoS = %s", q)
	}
}
