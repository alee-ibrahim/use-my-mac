package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/sched"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Router-side Mac settings managed by admins: labels merged over the
// agent's own, reservations for some owners, and concurrency.

// ForbiddenError is a request the caller may not make, with the reason
// (HTTP 403).
type ForbiddenError struct{ Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

func forbidden(format string, a ...any) error { return &ForbiddenError{fmt.Sprintf(format, a...)} }

// Caller is who is acting: the owner subject ("key:<id>" or "user:<sub>"),
// whether they are an admin, and how to name them in the audit log.
type Caller struct {
	Subject string
	Admin   bool
	Actor   string
}

func (c Caller) actor() string {
	if c.Actor != "" {
		return c.Actor
	}
	return c.Subject
}

var (
	labelKeyRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	labelValueRE = regexp.MustCompile(`^[A-Za-z0-9._:/ -]{0,128}$`)
)

// ValidateLabels checks label keys and values.
func ValidateLabels(m map[string]string) error {
	if len(m) > 64 {
		return vErr("at most 64 labels")
	}
	for k, v := range m {
		if !labelKeyRE.MatchString(k) {
			return vErr("label key %q must match %s", k, labelKeyRE)
		}
		if !labelValueRE.MatchString(v) {
			return vErr("label %s: value %q must match %s", k, v, labelValueRE)
		}
	}
	return nil
}

// mergeLabels returns the agent's labels overridden by the admin's.
func mergeLabels(agent, admin map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range agent {
		out[k] = v
	}
	for k, v := range admin {
		out[k] = v
	}
	return out
}

func parseLabels(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// reservedFor returns a Mac's active reservation owners (nil when it is not
// reserved, or the reservation ended).
func reservedFor(a *store.Agent, now time.Time) []string {
	if a.ReservedUntil > 0 && a.ReservedUntil <= ms(now) {
		return nil
	}
	var owners []string
	_ = json.Unmarshal([]byte(a.ReservedFor), &owners)
	return owners
}

// canUseMac: the owner may run tasks on the Mac (not reserved for others).
func canUseMac(a *store.Agent, owner string, now time.Time) bool {
	rs := reservedFor(a, now)
	return len(rs) == 0 || contains(rs, owner)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// effectiveMaxTasks: the admin's setting capped at the agent's hard limit
// (agents too old to report one keep their own max_tasks).
func effectiveMaxTasks(set, agentMax, limit int) int {
	if set > 0 && limit > 0 {
		return min(set, limit)
	}
	if agentMax <= 0 {
		return 1
	}
	return agentMax
}

// SetMacLabels replaces a Mac's router-side labels.
func (e *Engine) SetMacLabels(ctx context.Context, id string, labels map[string]string, actor string) error {
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	a, err := e.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if err := e.st.SetAgentAdminLabels(ctx, id, jsonStr(nonNilMap(labels))); err != nil {
		return err
	}
	e.Audit(ctx, api.EventMacLabels, actor, id, "set router labels on "+a.Name+": "+labelString(labels))
	e.Kick()
	return nil
}

func labelString(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// normalizeOwner turns "key_x" into "key:key_x" and checks the form.
func (e *Engine) normalizeOwner(ctx context.Context, o string) (string, error) {
	o = strings.TrimSpace(o)
	if strings.HasPrefix(o, "key_") {
		o = "key:" + o
	}
	switch {
	case strings.HasPrefix(o, "key:"):
		if _, err := e.st.GetAPIKey(ctx, strings.TrimPrefix(o, "key:")); err != nil {
			return "", vErr("unknown API key %s", strings.TrimPrefix(o, "key:"))
		}
	case strings.HasPrefix(o, "user:") && len(o) > 5 && len(o) <= 300:
	default:
		return "", vErr("owner %q must be an API key ID (key_...), key:<id> or user:<subject>", o)
	}
	return o, nil
}

// SetMacReservation reserves a Mac for some owners (an empty list removes
// the reservation).
func (e *Engine) SetMacReservation(ctx context.Context, id string, req api.MacReservationRequest, actor string) error {
	a, err := e.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if len(req.Owners) == 0 {
		return e.ClearMacReservation(ctx, id, actor)
	}
	if len(req.Owners) > 100 {
		return vErr("at most 100 owners")
	}
	var owners []string
	for _, o := range req.Owners {
		n, err := e.normalizeOwner(ctx, o)
		if err != nil {
			return err
		}
		if !contains(owners, n) {
			owners = append(owners, n)
		}
	}
	now := e.now()
	var until int64
	switch {
	case req.Until != nil && req.Hours != 0:
		return vErr("give until or hours, not both")
	case req.Until != nil:
		if !req.Until.After(now) {
			return vErr("until must be in the future")
		}
		until = ms(*req.Until)
	case req.Hours < 0 || req.Hours > 24*366:
		return vErr("hours must be between 0 and 8784")
	case req.Hours > 0:
		until = ms(now.Add(time.Duration(req.Hours) * time.Hour))
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		return vErr("note is too long (200 characters at most)")
	}
	if err := e.st.SetAgentReservation(ctx, id, jsonStr(owners), until, note, actor); err != nil {
		return err
	}
	msg := "reserved " + a.Name + " for " + strings.Join(owners, ", ")
	if until > 0 {
		msg += " until " + time.UnixMilli(until).UTC().Format(time.RFC3339)
	}
	e.Audit(ctx, api.EventMacReserved, actor, id, msg)
	e.Kick()
	return nil
}

// ClearMacReservation removes a Mac's reservation.
func (e *Engine) ClearMacReservation(ctx context.Context, id, actor string) error {
	a, err := e.st.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if err := e.st.SetAgentReservation(ctx, id, "[]", 0, "", ""); err != nil {
		return err
	}
	e.Audit(ctx, api.EventMacReserved, actor, id, "removed the reservation of "+a.Name)
	e.Kick()
	return nil
}

// setMaxTasks stores the router-set concurrency of a Mac.
func (e *Engine) setMaxTasks(ctx context.Context, a *store.Agent, n int, actor string) error {
	if n < 0 || n > 64 {
		return vErr("max_tasks must be between 1 and 64 (0 = the agent's own)")
	}
	if err := e.st.SetAgentMaxTasks(ctx, a.ID, n); err != nil {
		return err
	}
	msg := "set max_tasks of " + a.Name + " to " + strconv.Itoa(n)
	if n == 0 {
		msg = "max_tasks of " + a.Name + " back to the agent's own"
	}
	e.Audit(ctx, api.EventMacMaxTasks, actor, a.ID, msg)
	e.Kick()
	return nil
}

// checkMacSelectors validates a job's macs and prefer selectors against
// the fleet: every name or ID must match a Mac, and the owner must be
// allowed to use at least one Mac each entry matches.
func (e *Engine) checkMacSelectors(ctx context.Context, owner string, spec *api.JobSpec) error {
	need := false
	for _, t := range spec.Tasks {
		if len(t.Macs) > 0 || (t.Prefer != nil && len(t.Prefer.Macs) > 0) {
			need = true
		}
	}
	if !need {
		return nil
	}
	agents, err := e.st.ListAgents(ctx)
	if err != nil {
		return err
	}
	now := e.now()
	checked := map[string]bool{}
	check := func(task, sel string, hard bool) error {
		if checked[sel] {
			return nil
		}
		found, usable := false, false
		for _, a := range agents {
			if sched.MatchesMac(a.ID, a.Name, []string{sel}) {
				found = true
				usable = usable || canUseMac(a, owner, now)
			}
		}
		if !found {
			return vErr("task %s: no Mac %q", task, sel)
		}
		if !usable && hard {
			return forbidden("task %s: Mac %q is reserved for other owners", task, sel)
		}
		checked[sel] = true
		return nil
	}
	for _, t := range spec.Tasks {
		for _, m := range t.Macs {
			if err := check(t.Key, m, true); err != nil {
				return err
			}
		}
		if t.Prefer != nil {
			for _, m := range t.Prefer.Macs {
				if err := check(t.Key, m, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// waitingReason explains a task no Mac took, when it is more than a busy
// fleet: only Macs reserved for other owners would fit, or every Mac it is
// pinned to is unavailable.
func waitingReason(cs []sched.Candidate, n sched.Need, all []sched.Result, w sched.Weights, l sched.Limits) string {
	byID := map[string]sched.Candidate{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	var reserved, pinned []string
	for _, r := range all {
		c := byID[r.ID]
		name := c.Name
		if name == "" {
			name = c.ID
		}
		switch r.Reason {
		case sched.ReasonReserved:
			c.ReservedFor = nil
			if sched.Score(c, n, w, l).OK {
				reserved = append(reserved, name)
			}
		case sched.ReasonNotPinned:
		default:
			if len(n.Macs) > 0 {
				pinned = append(pinned, name+" ("+r.Reason+")")
			}
		}
	}
	switch {
	case len(reserved) > 0:
		return "only Macs reserved for other owners could run it now: " + strings.Join(reserved, ", ")
	case len(n.Macs) > 0 && len(pinned) == 0:
		return "none of its macs can be used: they are reserved for other owners or were deleted"
	case len(n.Macs) > 0:
		return "waiting for its macs: " + strings.Join(pinned, ", ")
	}
	return ""
}
