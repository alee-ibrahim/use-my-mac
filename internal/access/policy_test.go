package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestEvaluatePrecedence(t *testing.T) {
	p := Policy{
		Config: Config{AdminGroups: []string{"G-Admin"}, ViewerGroups: []string{"g-view"}},
		Rules: []Rule{
			{ID: "r1", Kind: api.AccessKindGroup, Value: "g-ops", Role: api.RoleViewer},
			{ID: "r2", Kind: api.AccessKindGroup, Value: "g-leads", Role: api.RoleAdmin},
			{ID: "r3", Kind: api.AccessKindUser, Value: "alice@example.com", Role: api.RoleViewer},
			{ID: "r4", Kind: api.AccessKindUser, Value: "sub-bob", Role: api.RoleAdmin},
			{ID: "r5", Kind: api.AccessKindUser, Value: "sub-carol", Role: api.RoleViewer},
		},
	}
	cases := []struct {
		name   string
		u      User
		role   string
		ruleID string
	}{
		{"nobody: default deny", User{Sub: "x"}, "", ""},
		{"config admin group (case-insensitive)", User{Sub: "x", Groups: []string{"g-admin"}}, api.RoleAdmin, "config:group:g-admin"},
		{"config viewer group", User{Sub: "x", Groups: []string{"G-VIEW"}}, api.RoleViewer, "config:group:g-view"},
		{"highest UI group wins", User{Sub: "x", Groups: []string{"g-ops", "g-leads"}}, api.RoleAdmin, "r2"},
		{"user rule beats group rule (by email)", User{Sub: "x", Email: "Alice@Example.com", Groups: []string{"g-leads"}}, api.RoleViewer, "r3"},
		{"user rule raises above config viewer", User{Sub: "sub-bob", Groups: []string{"g-view"}}, api.RoleAdmin, "r4"},
		{"config admin is a floor over a user rule", User{Sub: "sub-carol", Groups: []string{"g-admin"}}, api.RoleAdmin, "config:group:g-admin"},
		{"UI admin group beats config viewer group", User{Sub: "x", Groups: []string{"g-view", "g-leads"}}, api.RoleAdmin, "r2"},
	}
	for _, c := range cases {
		d := p.Evaluate(c.u)
		if d.Role != c.role || d.RuleID != c.ruleID {
			t.Errorf("%s: got %q via %q (%s), want %q via %q", c.name, d.Role, d.RuleID, d.Reason, c.role, c.ruleID)
		}
	}

	// Default: the higher of config and dashboard.
	if d := (Policy{UIDefault: "viewer"}).Evaluate(User{Sub: "x"}); d.Role != api.RoleViewer || d.Source != "default" {
		t.Errorf("UI default viewer: %+v", d)
	}
	if d := (Policy{Config: Config{DefaultRole: "viewer"}, UIDefault: "deny"}).Evaluate(User{Sub: "x"}); d.Role != api.RoleViewer {
		t.Errorf("config default viewer must win over UI deny: %+v", d)
	}
	// Groups overage: refused unless a user rule or config... decides.
	over := User{Sub: "x", Overage: true}
	if d := (Policy{UIDefault: "viewer"}).Evaluate(over); !d.Denied() {
		t.Errorf("overage must be refused: %+v", d)
	}
	pr := Policy{Rules: []Rule{{ID: "u", Kind: api.AccessKindUser, Value: "x", Role: api.RoleViewer}}}
	if d := pr.Evaluate(over); d.Role != api.RoleViewer {
		t.Errorf("user rule must work despite overage: %+v", d)
	}
}

func TestLocksOut(t *testing.T) {
	users := []User{{Sub: "a"}, {Sub: "b"}, {Sub: "c", Groups: []string{"cfg"}}}
	before := Policy{Rules: []Rule{{ID: "1", Kind: api.AccessKindUser, Value: "a", Role: api.RoleAdmin}}}
	after := Policy{}
	if !LocksOut(before, after, users[:2], "a") {
		t.Error("removing the only admin's own rule must be refused")
	}
	if LocksOut(before, after, users[:2], "") {
		t.Error("API-key actors are never locked out")
	}
	// Another admin remains.
	before.Rules = append(before.Rules, Rule{ID: "2", Kind: api.AccessKindUser, Value: "b", Role: api.RoleAdmin})
	after.Rules = before.Rules[1:]
	if LocksOut(before, after, users[:2], "a") {
		t.Error("another admin remains")
	}
	// A config admin always remains.
	before.Rules, after.Rules = before.Rules[:1], nil
	before.Config.AdminGroups, after.Config.AdminGroups = []string{"cfg"}, []string{"cfg"}
	if LocksOut(before, after, users, "a") {
		t.Error("config admin remains")
	}
}

func newManager(t *testing.T, cfg Config) (*Manager, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Unix(1_800_000_000, 0)
	return New(st, cfg, func() time.Time { now = now.Add(time.Second); return now }, nil), &now
}

func TestManager(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t, Config{AdminGroups: []string{"cfg-admins"}})

	// A denied login is still recorded, so an admin can grant access.
	d, err := m.RecordLogin(ctx, User{Sub: "s-dana", Name: "Dana", Email: "dana@example.com"})
	if err != nil || !d.Denied() {
		t.Fatalf("dana: %+v %v", d, err)
	}
	if _, err := m.RecordLogin(ctx, User{Sub: "s-root", Name: "Root", Groups: []string{"cfg-admins"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUserRole(ctx, "user:s-root", "Root", "s-dana", "viewer"); err != nil {
		t.Fatal(err)
	}
	if d, ok, _ := m.Resolve(ctx, "s-dana"); !ok || d.Role != api.RoleViewer {
		t.Fatalf("dana after grant: %+v %v", d, ok)
	}
	if _, ok, _ := m.Resolve(ctx, "unknown"); ok {
		t.Fatal("unknown subject must not resolve")
	}

	// Config rules are listed, locked and cannot be deleted.
	p, err := m.Policy(ctx, true)
	if err != nil || len(p.Rules) != 2 || !p.Rules[0].Locked || p.Rules[0].Source != api.AccessFromConfig || p.DefaultRole != api.RoleDeny {
		t.Fatalf("policy: %+v %v", p, err)
	}
	if err := m.DeleteRule(ctx, "user:s-root", "Root", p.Rules[0].ID); !errors.Is(err, ErrLocked) {
		t.Fatalf("delete config rule: %v", err)
	}
	if p.Rules[1].Name != "Dana" {
		t.Errorf("user rule name: %+v", p.Rules[1])
	}

	// Lockout: Dana made admin by a UI rule cannot demote herself while she
	// is the only signed-in admin... but Root (config admin) is signed in.
	if err := m.SetUserRole(ctx, "user:s-root", "Root", "s-dana", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUserRole(ctx, "user:s-dana", "Dana", "s-dana", "viewer"); err != nil {
		t.Fatalf("self-demotion with a config admin present: %v", err)
	}

	// Without config admins, the last admin cannot demote themselves.
	m2, _ := newManager(t, Config{})
	m2.RecordLogin(ctx, User{Sub: "e", Name: "Eve"})
	m2.RecordLogin(ctx, User{Sub: "f", Name: "Finn"})
	if _, err := m2.PutRule(ctx, "key:k", "", api.AccessRuleRequest{Kind: "group", Value: "G1", Role: "admin", Name: "Ops"}); err != nil {
		t.Fatal(err)
	}
	if err := m2.SetUserRole(ctx, "key:k", "", "e", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := m2.SetUserRole(ctx, "user:e", "Eve", "e", "viewer"); !errors.Is(err, ErrLockout) {
		t.Fatalf("last admin self-demotion: %v", err)
	}
	if err := m2.SetUserRole(ctx, "user:e", "Eve", "e", ""); !errors.Is(err, ErrLockout) {
		t.Fatalf("last admin removing own rule: %v", err)
	}
	// After Finn is made admin, Eve may step down.
	if err := m2.SetUserRole(ctx, "user:e", "Eve", "f", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := m2.SetUserRole(ctx, "user:e", "Eve", "e", ""); err != nil {
		t.Fatalf("step down with another admin: %v", err)
	}
	if err := m2.SetDefault(ctx, "user:f", "Finn", "viewer"); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := m2.Resolve(ctx, "e"); d.Role != api.RoleViewer || d.Source != "default" {
		t.Fatalf("eve on default: %+v", d)
	}

	// Validation.
	for _, req := range []api.AccessRuleRequest{{Kind: "team", Value: "x", Role: "admin"}, {Kind: "group", Value: "", Role: "admin"},
		{Kind: "group", Value: "x", Role: "app"}, {Kind: "user", Value: "x", Role: "deny"}} {
		var ve *ValidationError
		if _, err := m2.PutRule(ctx, "key:k", "", req); !errors.As(err, &ve) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if err := m2.SetDefault(ctx, "key:k", "", "admin"); err == nil {
		t.Error("default admin must be refused")
	}

	// Audit log: who, what.
	evs, err := m2.Events(ctx, 0)
	if err != nil || len(evs) < 5 {
		t.Fatalf("events: %d %v", len(evs), err)
	}
	if evs[0].Action != "default.set" || evs[0].Actor != "Finn (user:f)" || evs[0].Detail != "deny -> viewer" {
		t.Errorf("latest event: %+v", evs[0])
	}
	pol, _ := m2.Policy(ctx, true)
	if pol.GroupNames["g1"] != "Ops" {
		t.Errorf("group name: %+v", pol.GroupNames)
	}
}
