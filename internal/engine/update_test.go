package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestUpdateDrainAndHistory(t *testing.T) {
	h := newHarness(t)
	h.e.version = "v0.2.0"
	id := h.enrol("studio", 1)
	hb := api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), Running: []string{}, MaxTasks: 1, Version: "v0.1.0"}
	if _, err := h.e.Heartbeat(h.ctx, id, hb); err != nil {
		t.Fatal(err)
	}

	c, err := h.e.UpdateCheck(h.ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.TargetVersion != "v0.2.0" || c.AllowDowngrade || !c.Agent.Online || c.Agent.Version != "v0.1.0" || !c.Relay {
		t.Fatalf("check %+v", c)
	}

	// "draining" drains the Mac (with a reason) until it expires or another
	// state is reported; repeated refreshes add one history entry.
	for range 3 {
		if err := h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: api.UpdateDraining, FromVersion: "v0.1.0", ToVersion: "v0.2.0"}); err != nil {
			t.Fatal(err)
		}
	}
	resp, _ := h.e.Heartbeat(h.ctx, id, hb)
	if !resp.Drain || resp.DrainReason != "updating" {
		t.Fatalf("not drained for the update: %+v", resp)
	}
	h.advance(updateDrainTTL + time.Second)
	if resp, _ = h.e.Heartbeat(h.ctx, id, hb); resp.Drain {
		t.Fatal("update drain did not expire")
	}
	h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: api.UpdateApplying, FromVersion: "v0.1.0", ToVersion: "v0.2.0"})
	h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: api.UpdateSucceeded, FromVersion: "v0.1.0", ToVersion: "v0.2.0"})
	if resp, _ = h.e.Heartbeat(h.ctx, id, hb); resp.Drain {
		t.Fatal("drain not lifted after success")
	}
	evs, err := h.e.UpdateEvents(h.ctx, id)
	if err != nil || len(evs) != 3 || evs[0].State != api.UpdateSucceeded {
		t.Fatalf("history %+v %v", evs, err)
	}
	if err := h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: "bogus"}); err == nil {
		t.Fatal("unknown state accepted")
	}

	macs, _ := h.e.Macs(h.ctx)
	if macs[0].Update == nil || macs[0].Update.State != api.UpdateSucceeded || macs[0].Update.Desired != "v0.2.0" {
		t.Fatalf("mac update %+v", macs[0].Update)
	}

	// Settings persist and validate.
	if err := h.e.SetUpdateSettings(h.ctx, api.UpdateSettings{TargetVersion: "latest"}); err == nil {
		t.Fatal("bad version accepted")
	}
	if err := h.e.SetUpdateSettings(h.ctx, api.UpdateSettings{TargetVersion: "v0.1.5", Paused: true}); err != nil {
		t.Fatal(err)
	}
	h.e.loadUpdateSettings(h.ctx)
	if s := h.e.UpdateSettings(); s.TargetVersion != "v0.1.5" || !s.Paused {
		t.Fatalf("settings not persisted: %+v", s)
	}
	if c, _ = h.e.UpdateCheck(h.ctx, id, false); c.TargetVersion != "" || !c.Paused {
		t.Fatalf("paused check %+v", c)
	}

	// Deleting the Mac removes its update rows.
	if err := h.e.DeleteMac(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.e.st.UpdateEvents(h.ctx, id, 10); len(n) != 0 {
		t.Fatal("events kept after delete")
	}
}

func TestUpdaterNotRunningWarning(t *testing.T) {
	h := newHarness(t)
	h.e.version = "v0.2.0"
	id := h.enrol("studio", 1)
	hb := api.Heartbeat{State: api.AgentReady, Telemetry: goodTel(), Running: []string{}, MaxTasks: 1, Version: "v0.2.0"}
	beat := func() {
		t.Helper()
		if _, err := h.e.Heartbeat(h.ctx, id, hb); err != nil {
			t.Fatal(err)
		}
	}
	warning := func() string {
		t.Helper()
		macs, err := h.e.Macs(h.ctx)
		if err != nil || len(macs) != 1 || macs[0].Update == nil {
			t.Fatalf("%+v %v", macs, err)
		}
		return macs[0].Update.Warning
	}
	beat()
	// Never checked in (e.g. updates off in agent.yaml): no warning.
	if w := warning(); w != "" {
		t.Fatalf("warning before the first check-in: %q", w)
	}
	h.e.UpdateCheck(h.ctx, id, true)
	h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: api.UpdateUpToDate, FromVersion: "v0.2.0", ToVersion: "v0.2.0"})
	// Quiet for 3 intervals is still fine; longer is not, while the agent
	// heartbeats.
	for _, step := range []time.Duration{30 * time.Minute, 15 * time.Minute} {
		h.advance(step)
		beat()
		if w := warning(); w != "" {
			t.Fatalf("early warning after %s: %q", step, w)
		}
	}
	h.advance(2 * time.Minute)
	beat()
	if w := warning(); !strings.HasPrefix(w, "updater not running") || !strings.Contains(w, "47m") {
		t.Fatalf("warning %q", w)
	}
	// The updater checks in again: cleared.
	h.e.UpdateCheck(h.ctx, id, true)
	if w := warning(); w != "" {
		t.Fatalf("not cleared: %q", w)
	}
	// "repaired" is a valid report.
	if err := h.e.UpdateReport(h.ctx, id, api.UpdateReport{State: api.UpdateRepaired, Message: "re-registered"}); err != nil {
		t.Fatal(err)
	}
	// An offline Mac gets no updater warning (it is offline).
	h.advance(2 * time.Hour)
	if w := warning(); w != "" {
		t.Fatalf("offline Mac warned: %q", w)
	}
}
