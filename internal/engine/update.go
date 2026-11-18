package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

const settingUpdates = "updates"

// updateDrainTTL is how long one "draining"/"applying" report keeps the Mac
// drained. The updater refreshes it while it waits, so a dead updater cannot
// leave a Mac drained for good.
const updateDrainTTL = 3 * time.Minute

// updateEventsKept is the history kept per Mac.
const updateEventsKept = 50

// UpdateSettings returns the agent update policy.
func (e *Engine) UpdateSettings() api.UpdateSettings {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.updates
	s.CanaryMacs = append([]string{}, s.CanaryMacs...)
	return s
}

// RouterVersion is the router's own version (the default agent target).
func (e *Engine) RouterVersion() string { return e.version }

// SetUpdateSettings validates and persists the agent update policy.
func (e *Engine) SetUpdateSettings(ctx context.Context, s api.UpdateSettings) error {
	if err := selfupdate.ValidateSettings(&s); err != nil {
		return &ValidationError{err.Error()}
	}
	if err := e.st.PutSetting(ctx, settingUpdates, jsonStr(s)); err != nil {
		return err
	}
	e.mu.Lock()
	e.updates = s
	e.mu.Unlock()
	return nil
}

func (e *Engine) loadUpdateSettings(ctx context.Context) {
	e.updates = api.UpdateSettings{CanaryMacs: []string{}}
	if v, err := e.st.GetSetting(ctx, settingUpdates); err == nil {
		var s api.UpdateSettings
		if json.Unmarshal([]byte(v), &s) == nil && selfupdate.ValidateSettings(&s) == nil {
			e.updates = s
		}
	}
}

// DesiredVersion is the version the router wants on a Mac ("" = none).
func (e *Engine) DesiredVersion(macID string) string {
	return selfupdate.ResolveTarget(e.UpdateSettings(), e.version, macID).Version
}

// UpdateCheck answers an updater: what to run, and the agent's live state.
func (e *Engine) UpdateCheck(ctx context.Context, agentID string, relay bool) (api.UpdateCheck, error) {
	a, err := e.st.GetAgent(ctx, agentID)
	if err != nil {
		return api.UpdateCheck{}, err
	}
	now := e.now()
	_ = e.st.TouchUpdateCheck(ctx, agentID, ms(now))
	s := e.UpdateSettings()
	t := selfupdate.ResolveTarget(s, e.version, agentID)
	drain := s.DrainTimeoutSeconds
	if drain <= 0 {
		drain = selfupdate.DefaultDrainTimeoutSeconds
	}
	c := api.UpdateCheck{TargetVersion: t.Version, AllowDowngrade: t.Explicit, Paused: s.Paused, Reason: t.Reason,
		DrainTimeoutSeconds: drain, Relay: relay, NowMS: ms(now)}
	e.mu.Lock()
	defer e.mu.Unlock()
	c.Agent = api.UpdateAgentView{Version: a.Version, Online: e.online(agentID, now), LastSeenMS: a.LastSeen}
	if l := e.live[agentID]; l != nil && c.Agent.Online {
		c.Agent.Running = len(l.hb.Running)
		if !l.seen.IsZero() {
			c.Agent.LastSeenMS = ms(l.seen)
		}
	}
	return c, nil
}

// UpdateReport records an updater report. "draining" and "applying" keep the
// Mac drained for a short while; any other state lifts that drain.
func (e *Engine) UpdateReport(ctx context.Context, agentID string, r api.UpdateReport) error {
	switch r.State {
	case api.UpdateUpToDate, api.UpdatePaused, api.UpdateDownloading, api.UpdateDraining, api.UpdateApplying,
		api.UpdateSucceeded, api.UpdateRolledBack, api.UpdateFailed, api.UpdateRefused:
	default:
		return &ValidationError{"unknown update state"}
	}
	r.Message = truncate(r.Message, 1000)
	r.FromVersion, r.ToVersion = truncate(r.FromVersion, 64), truncate(r.ToVersion, 64)
	now := e.now()
	prev, err := e.st.GetAgentUpdate(ctx, agentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// History gets every change; a repeated "draining" refresh does not.
	event := prev == nil || prev.State != r.State || prev.ToVersion != r.ToVersion || prev.FromVersion != r.FromVersion
	u := &store.AgentUpdate{AgentID: agentID, State: r.State, FromVersion: r.FromVersion, ToVersion: r.ToVersion,
		Message: r.Message, At: ms(now), CheckedAt: ms(now)}
	if err := e.st.RecordUpdate(ctx, u, event, updateEventsKept); err != nil {
		return err
	}
	e.mu.Lock()
	l := e.liveFor(agentID)
	if r.State == api.UpdateDraining || r.State == api.UpdateApplying {
		l.updDrainUntil = now.Add(updateDrainTTL)
	} else {
		l.updDrainUntil = time.Time{}
	}
	e.mu.Unlock()
	if event {
		e.log.Info("agent update", "agent", agentID, "state", r.State, "from", r.FromVersion, "to", r.ToVersion, "msg", r.Message)
	}
	if r.State != api.UpdateDraining && r.State != api.UpdateApplying {
		e.Kick()
	}
	return nil
}

// updateDraining reports whether the updater has the Mac drained. Needs e.mu.
func (e *Engine) updateDrainingLocked(agentID string, now time.Time) bool {
	l := e.live[agentID]
	return l != nil && now.Before(l.updDrainUntil)
}

// UpdateEvents returns a Mac's update history, newest first.
func (e *Engine) UpdateEvents(ctx context.Context, agentID string) ([]api.UpdateEvent, error) {
	if _, err := e.st.GetAgent(ctx, agentID); err != nil {
		return nil, err
	}
	evs, err := e.st.UpdateEvents(ctx, agentID, 100)
	if err != nil {
		return nil, err
	}
	out := make([]api.UpdateEvent, 0, len(evs))
	for _, ev := range evs {
		out = append(out, api.UpdateEvent{At: time.UnixMilli(ev.TS).UTC(), State: ev.State,
			FromVersion: ev.FromVersion, ToVersion: ev.ToVersion, Message: ev.Message})
	}
	return out, nil
}

func macUpdate(u *store.AgentUpdate, desired string) *api.MacUpdate {
	if u == nil {
		if desired == "" {
			return nil
		}
		return &api.MacUpdate{Desired: desired}
	}
	return &api.MacUpdate{State: u.State, FromVersion: u.FromVersion, ToVersion: u.ToVersion, Message: u.Message,
		At: tptr(u.At), CheckedAt: tptr(u.CheckedAt), Desired: desired}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
