package access

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// settingDefault holds the dashboard default role.
const settingDefault = "access.default_role"

// cacheTTL bounds how stale a replica's view of the rules can be. Changes
// made through this router apply at once (the cache is dropped).
const cacheTTL = 10 * time.Second

// Errors.
var (
	ErrLocked   = errors.New("this rule comes from the router config and cannot be changed here")
	ErrLockout  = errors.New("this change would remove your own admin role and leave no other admin; add another admin first")
	ErrNotFound = store.ErrNotFound
)

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Manager stores dashboard rules and decides roles.
type Manager struct {
	st  *store.Store
	cfg Config
	now func() time.Time
	log *slog.Logger

	mu       sync.Mutex
	snap     *snapshot
	loadedAt time.Time
}

type snapshot struct {
	policy     Policy
	rules      []store.AccessRule
	users      []User
	byID       map[string]store.AccessUser
	groupNames map[string]string
}

// New creates a manager. now and log may be nil.
func New(st *store.Store, cfg Config, now func() time.Time, log *slog.Logger) *Manager {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{st: st, cfg: cfg, now: now, log: log}
}

// ConfigFromOIDC extracts the authoritative rules from the OIDC config.
func ConfigFromOIDC(c auth.OIDCConfig) Config {
	return Config{AdminGroups: c.AdminGroups, ViewerGroups: c.ViewerGroups, DefaultRole: c.DefaultRole}
}

func (m *Manager) invalidate() {
	m.mu.Lock()
	m.snap = nil
	m.mu.Unlock()
}

func (m *Manager) load(ctx context.Context) (*snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap != nil && m.now().Sub(m.loadedAt) < cacheTTL {
		return m.snap, nil
	}
	rules, err := m.st.AccessRules(ctx)
	if err != nil {
		return nil, err
	}
	def, err := m.st.GetSetting(ctx, settingDefault)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	rows, err := m.st.AccessUsers(ctx)
	if err != nil {
		return nil, err
	}
	names, err := m.st.AccessGroupNames(ctx)
	if err != nil {
		return nil, err
	}
	s := &snapshot{rules: rules, byID: map[string]store.AccessUser{}, groupNames: names,
		policy: Policy{Config: m.cfg, UIDefault: NormDefault(def)}}
	for _, r := range rules {
		s.policy.Rules = append(s.policy.Rules, Rule{ID: r.ID, Kind: r.Kind, Value: r.Value, Role: r.Role})
	}
	for _, u := range rows {
		s.byID[u.Sub] = u
		s.users = append(s.users, toUser(u))
	}
	m.snap, m.loadedAt = s, m.now()
	return s, nil
}

func toUser(u store.AccessUser) User {
	return User{Sub: u.Sub, Name: u.Name, Email: u.Email, UPN: u.UPN, OID: u.OID, Groups: u.Groups, Overage: u.Overage}
}

// Decide evaluates a user from fresh claims without storing anything.
func (m *Manager) Decide(ctx context.Context, u User) (Decision, error) {
	s, err := m.load(ctx)
	if err != nil {
		return Decision{}, err
	}
	return s.policy.Evaluate(u), nil
}

// RecordLogin stores a sign-in (allowed or not, so admins can grant access
// to people who were turned away) and returns the decision.
func (m *Manager) RecordLogin(ctx context.Context, u User) (Decision, error) {
	now := m.now().UnixMilli()
	err := m.st.UpsertAccessUser(ctx, store.AccessUser{Sub: u.Sub, Name: u.Name, Email: u.Email, UPN: u.UPN, OID: u.OID,
		Groups: u.Groups, Overage: u.Overage, FirstLogin: now, LastLogin: now})
	if err != nil {
		return Decision{}, err
	}
	m.invalidate()
	return m.Decide(ctx, u)
}

// Resolve re-evaluates a signed-in user's role for a request. ok is false
// when the user is unknown (signed in before roles were tracked).
func (m *Manager) Resolve(ctx context.Context, sub string) (d Decision, ok bool, err error) {
	s, err := m.load(ctx)
	if err != nil {
		return Decision{}, false, err
	}
	u, ok := s.byID[sub]
	if !ok {
		return Decision{}, false, nil
	}
	return s.policy.Evaluate(toUser(u)), true, nil
}

func t(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	v := time.UnixMilli(ms).UTC()
	return &v
}

// Policy returns all rules: config rules first (locked), then dashboard rules.
func (m *Manager) Policy(ctx context.Context, oidc bool) (api.AccessPolicy, error) {
	s, err := m.load(ctx)
	if err != nil {
		return api.AccessPolicy{}, err
	}
	out := api.AccessPolicy{
		DefaultRole: orDeny(s.policy.DefaultRole()), ConfigDefaultRole: orDeny(NormDefault(m.cfg.DefaultRole)),
		UIDefaultRole: orDeny(s.policy.UIDefault), GroupNames: s.groupNames, OIDC: oidc, Rules: []api.AccessRule{},
	}
	add := func(role string, groups []string) {
		for _, g := range groups {
			if g = strings.TrimSpace(g); g != "" {
				out.Rules = append(out.Rules, api.AccessRule{ID: ConfigRuleID(api.AccessKindGroup, g), Kind: api.AccessKindGroup,
					Value: g, Role: role, Name: s.groupNames[Norm(g)], Source: api.AccessFromConfig, Locked: true})
			}
		}
	}
	add(api.RoleAdmin, m.cfg.AdminGroups)
	add(api.RoleViewer, m.cfg.ViewerGroups)
	for _, r := range s.rules {
		ar := api.AccessRule{ID: r.ID, Kind: r.Kind, Value: r.Value, Role: r.Role, Source: api.AccessFromUI,
			CreatedBy: r.CreatedBy, CreatedAt: t(r.CreatedAt), UpdatedAt: t(r.UpdatedAt)}
		if r.Kind == api.AccessKindGroup {
			ar.Name = s.groupNames[r.Value]
		} else {
			for _, u := range s.users {
				for _, id := range u.Identifiers() {
					if id == r.Value && ar.Name == "" {
						ar.Name = u.Name
					}
				}
			}
		}
		out.Rules = append(out.Rules, ar)
	}
	return out, nil
}

func orDeny(r string) string {
	if r == "" {
		return api.RoleDeny
	}
	return r
}

// Users lists everyone who signed in with their effective role and why.
func (m *Manager) Users(ctx context.Context) ([]api.AccessUser, error) {
	s, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.AccessUser, 0, len(s.users))
	for _, u := range s.users {
		row := s.byID[u.Sub]
		d := s.policy.Evaluate(u)
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		out = append(out, api.AccessUser{Subject: u.Sub, Name: u.Name, Email: u.Email, UPN: u.UPN, OID: u.OID,
			Groups: groups, FirstLogin: time.UnixMilli(row.FirstLogin).UTC(), LastLogin: time.UnixMilli(row.LastLogin).UTC(),
			Role: d.Role, Reason: d.Reason, RuleID: d.RuleID, Source: d.Source})
	}
	return out, nil
}

// actorSub is the OIDC subject of an actor ("user:<sub>"), or "".
func actorSub(actor string) string {
	sub, _ := strings.CutPrefix(actor, "user:")
	if sub == actor {
		return ""
	}
	return sub
}

// change applies a policy change after the lockout check, then audits it.
func (m *Manager) change(ctx context.Context, actor, actorName string, edit func(*Policy), apply func() error, action, detail string) error {
	s, err := m.load(ctx)
	if err != nil {
		return err
	}
	after := s.policy
	after.Rules = append([]Rule(nil), s.policy.Rules...)
	edit(&after)
	if LocksOut(s.policy, after, s.users, actorSub(actor)) {
		return ErrLockout
	}
	if err := apply(); err != nil {
		return err
	}
	m.invalidate()
	m.audit(ctx, actor, actorName, action, detail)
	return nil
}

func (m *Manager) audit(ctx context.Context, actor, actorName, action, detail string) {
	who := actor
	if actorName != "" {
		who = actorName + " (" + actor + ")"
	}
	if err := m.st.AddAccessEvent(ctx, store.AccessEvent{TS: m.now().UnixMilli(), Actor: who, Action: action, Detail: detail}); err != nil {
		m.log.Warn("access audit log", "err", err)
	}
	m.log.Info("access changed", "actor", actor, "action", action, "detail", detail)
}

func validRole(r string) bool { return r == api.RoleAdmin || r == api.RoleViewer }

// PutRule adds a dashboard rule or changes the role of an existing one.
func (m *Manager) PutRule(ctx context.Context, actor, actorName string, req api.AccessRuleRequest) (api.AccessRule, error) {
	kind, value, role := Norm(req.Kind), Norm(req.Value), Norm(req.Role)
	if kind != api.AccessKindGroup && kind != api.AccessKindUser {
		return api.AccessRule{}, &ValidationError{"kind must be group or user"}
	}
	if value == "" || len(value) > 256 {
		return api.AccessRule{}, &ValidationError{"value must be 1-256 characters"}
	}
	if !validRole(role) {
		return api.AccessRule{}, &ValidationError{"role must be admin or viewer"}
	}
	if kind == api.AccessKindGroup && len(req.Name) > 200 {
		return api.AccessRule{}, &ValidationError{"name is too long"}
	}
	s, err := m.load(ctx)
	if err != nil {
		return api.AccessRule{}, err
	}
	id := auth.NewID()
	prev := ""
	for _, r := range s.policy.Rules {
		if r.Kind == kind && r.Value == value {
			id, prev = r.ID, r.Role
		}
	}
	var stored store.AccessRule
	detail := fmt.Sprintf("%s %s -> %s", kind, value, role)
	if prev != "" {
		detail = fmt.Sprintf("%s %s: %s -> %s", kind, value, prev, role)
	}
	err = m.change(ctx, actor, actorName, func(p *Policy) {
		for i := range p.Rules {
			if p.Rules[i].ID == id {
				p.Rules[i].Role = role
				return
			}
		}
		p.Rules = append(p.Rules, Rule{ID: id, Kind: kind, Value: value, Role: role})
	}, func() error {
		var err error
		stored, err = m.st.PutAccessRule(ctx, store.AccessRule{ID: id, Kind: kind, Value: value, Role: role,
			CreatedBy: actor, CreatedAt: m.now().UnixMilli()})
		return err
	}, "rule.set", detail)
	if err != nil {
		return api.AccessRule{}, err
	}
	if kind == api.AccessKindGroup && strings.TrimSpace(req.Name) != "" {
		if err := m.SetGroupName(ctx, actor, actorName, value, req.Name); err != nil {
			return api.AccessRule{}, err
		}
	}
	return api.AccessRule{ID: stored.ID, Kind: stored.Kind, Value: stored.Value, Role: stored.Role, Source: api.AccessFromUI,
		CreatedBy: stored.CreatedBy, CreatedAt: t(stored.CreatedAt), UpdatedAt: t(stored.UpdatedAt)}, nil
}

// DeleteRule removes a dashboard rule. Config rules are locked.
func (m *Manager) DeleteRule(ctx context.Context, actor, actorName, id string) error {
	if strings.HasPrefix(id, "config:") {
		return ErrLocked
	}
	s, err := m.load(ctx)
	if err != nil {
		return err
	}
	var target *Rule
	for i := range s.policy.Rules {
		if s.policy.Rules[i].ID == id {
			target = &s.policy.Rules[i]
		}
	}
	if target == nil {
		return ErrNotFound
	}
	detail := fmt.Sprintf("%s %s (%s)", target.Kind, target.Value, target.Role)
	return m.change(ctx, actor, actorName, func(p *Policy) {
		p.Rules = deleteRule(p.Rules, id)
	}, func() error { return m.st.DeleteAccessRule(ctx, id) }, "rule.delete", detail)
}

func deleteRule(rules []Rule, id string) []Rule {
	out := rules[:0]
	for _, r := range rules {
		if r.ID != id {
			out = append(out, r)
		}
	}
	return out
}

// SetDefault sets the dashboard default role (deny or viewer). The config
// default still applies if it is higher.
func (m *Manager) SetDefault(ctx context.Context, actor, actorName, role string) error {
	role = Norm(role)
	if role != api.RoleDeny && role != api.RoleViewer && role != "" {
		return &ValidationError{"default role must be deny or viewer"}
	}
	nd := NormDefault(role)
	s, err := m.load(ctx)
	if err != nil {
		return err
	}
	return m.change(ctx, actor, actorName, func(p *Policy) { p.UIDefault = nd },
		func() error { return m.st.PutSetting(ctx, settingDefault, orDeny(nd)) },
		"default.set", fmt.Sprintf("%s -> %s", orDeny(s.policy.UIDefault), orDeny(nd)))
}

// SetGroupName sets (or with "" removes) a group's display name.
func (m *Manager) SetGroupName(ctx context.Context, actor, actorName, groupID, name string) error {
	groupID, name = Norm(groupID), strings.TrimSpace(name)
	if groupID == "" || len(groupID) > 256 || len(name) > 200 {
		return &ValidationError{"group ID must be 1-256 characters and the name at most 200"}
	}
	if err := m.st.SetAccessGroupName(ctx, groupID, name); err != nil {
		return err
	}
	m.invalidate()
	m.audit(ctx, actor, actorName, "group.name", fmt.Sprintf("%s = %q", groupID, name))
	return nil
}

// SetUserRole sets a signed-in user's own rule (by subject). An empty role
// removes it.
func (m *Manager) SetUserRole(ctx context.Context, actor, actorName, sub, role string) error {
	s, err := m.load(ctx)
	if err != nil {
		return err
	}
	if _, ok := s.byID[sub]; !ok {
		return ErrNotFound
	}
	value := Norm(sub)
	if Norm(role) == "" || Norm(role) == "none" {
		for _, r := range s.policy.Rules {
			if r.Kind == api.AccessKindUser && r.Value == value {
				return m.DeleteRule(ctx, actor, actorName, r.ID)
			}
		}
		return nil
	}
	_, err = m.PutRule(ctx, actor, actorName, api.AccessRuleRequest{Kind: api.AccessKindUser, Value: sub, Role: role})
	return err
}

// Events returns the audit log, newest first.
func (m *Manager) Events(ctx context.Context, limit int) ([]api.AccessEvent, error) {
	evs, err := m.st.AccessEvents(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.AccessEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, api.AccessEvent{At: time.UnixMilli(e.TS).UTC(), Actor: e.Actor, Action: e.Action, Detail: e.Detail})
	}
	return out, nil
}
