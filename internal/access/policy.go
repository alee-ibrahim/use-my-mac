// Package access decides dashboard roles for single-sign-on users.
//
// Rules come from two places. Config rules (oidc.admin_groups,
// oidc.viewer_groups, oidc.default_role) are authoritative: they cannot be
// changed or removed at runtime, and a config group's role is a floor that
// nothing lowers, so a misconfigured dashboard rule can never lock the
// configured admins out. Dashboard rules (stored in the database) add on top.
//
// Precedence: an explicit user rule beats group rules (highest role wins),
// which beat the default role. The default is the higher of the config and
// dashboard defaults.
package access

import (
	"strings"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Config is the authoritative part of the policy, from the router config.
type Config struct {
	AdminGroups  []string
	ViewerGroups []string
	DefaultRole  string // "viewer" or "" (deny)
}

// Rule is one dashboard (database) rule. Value is lower-case.
type Rule struct {
	ID    string
	Kind  string // api.AccessKindGroup or api.AccessKindUser
	Value string
	Role  string // admin or viewer
}

// Policy is everything needed to decide a role.
type Policy struct {
	Config    Config
	Rules     []Rule
	UIDefault string // "viewer" or "" / "deny"
}

// User is what we know about a signed-in user (from their last login).
type User struct {
	Sub, Name, Email, UPN, OID string
	Groups                     []string // groups and app-role claim values
	Overage                    bool     // the provider left groups out (too many)
}

// Decision is a role and why.
type Decision struct {
	Role   string // admin, viewer or "" (denied)
	Reason string
	RuleID string // deciding rule; config rules are "config:<kind>:<value>"
	Source string // config, ui or default
}

// Denied reports whether the user may not sign in.
func (d Decision) Denied() bool { return d.Role == "" }

func rank(role string) int {
	switch role {
	case api.RoleAdmin:
		return 2
	case api.RoleViewer:
		return 1
	}
	return 0
}

// Norm normalises a rule value or claim value for matching.
func Norm(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// NormDefault maps a default role to "viewer" or "" (deny).
func NormDefault(r string) string {
	if Norm(r) == api.RoleViewer {
		return api.RoleViewer
	}
	return ""
}

// ConfigRuleID names a config rule.
func ConfigRuleID(kind, value string) string { return "config:" + kind + ":" + Norm(value) }

// Identifiers are the values a user rule can match, lower-cased, in
// priority order: subject, object ID, email, UPN.
func (u User) Identifiers() []string {
	var out []string
	for _, v := range []string{u.Sub, u.OID, u.Email, u.UPN} {
		if v = Norm(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// DefaultRole is the effective default: the higher of config and dashboard.
func (p Policy) DefaultRole() string {
	if NormDefault(p.Config.DefaultRole) == api.RoleViewer || NormDefault(p.UIDefault) == api.RoleViewer {
		return api.RoleViewer
	}
	return ""
}

// Evaluate decides a user's role.
func (p Policy) Evaluate(u User) Decision {
	groups := map[string]bool{}
	for _, g := range u.Groups {
		if g = Norm(g); g != "" {
			groups[g] = true
		}
	}
	ids := map[string]int{} // identifier -> priority (1 = highest)
	for i, v := range u.Identifiers() {
		if ids[v] == 0 {
			ids[v] = i + 1
		}
	}

	// Config groups: a floor.
	var cfg Decision
	for _, g := range p.Config.ViewerGroups {
		if groups[Norm(g)] {
			cfg = Decision{Role: api.RoleViewer, Reason: "config group " + strings.TrimSpace(g), RuleID: ConfigRuleID(api.AccessKindGroup, g), Source: api.AccessFromConfig}
			break
		}
	}
	for _, g := range p.Config.AdminGroups {
		if groups[Norm(g)] {
			cfg = Decision{Role: api.RoleAdmin, Reason: "config group " + strings.TrimSpace(g), RuleID: ConfigRuleID(api.AccessKindGroup, g), Source: api.AccessFromConfig}
			break
		}
	}

	// Dashboard rules: user rule first, else the highest group rule.
	var user, group *Decision
	userPrio := 0
	for _, r := range p.Rules {
		switch {
		case r.Kind == api.AccessKindUser && ids[r.Value] > 0 && (user == nil || ids[r.Value] < userPrio):
			// Several user rules: subject beats object ID beats email beats UPN.
			user, userPrio = &Decision{Role: r.Role, Reason: "user rule " + r.Value, RuleID: r.ID, Source: api.AccessFromUI}, ids[r.Value]
		case r.Kind == api.AccessKindGroup && groups[r.Value] && (group == nil || rank(r.Role) > rank(group.Role)):
			group = &Decision{Role: r.Role, Reason: "group rule " + r.Value, RuleID: r.ID, Source: api.AccessFromUI}
		}
	}

	var base Decision
	switch {
	case user != nil:
		base = *user
	case group != nil:
		base = *group
	case u.Overage && cfg.Role == "":
		// The token left groups out, so group rules cannot be evaluated.
		return Decision{Reason: "groups overage: the identity provider sent no groups"}
	default:
		base = Decision{Role: p.DefaultRole(), Reason: "default role", Source: "default"}
		if base.Role == "" {
			base.Reason = "no rule matches and the default is deny"
		}
	}
	if cfg.Role != "" && rank(cfg.Role) >= rank(base.Role) {
		return cfg
	}
	return base
}

// AdminSubjects returns the subjects that would be admin under p.
func (p Policy) AdminSubjects(users []User) map[string]bool {
	out := map[string]bool{}
	for _, u := range users {
		if p.Evaluate(u).Role == api.RoleAdmin {
			out[u.Sub] = true
		}
	}
	return out
}

// LocksOut reports whether changing the policy from before to after would
// demote actorSub (a signed-in admin) while leaving no other signed-in user
// as admin. Config admins are never demoted, so with any of them signed in
// this cannot happen.
func LocksOut(before, after Policy, users []User, actorSub string) bool {
	if actorSub == "" {
		return false
	}
	var actor *User
	for i := range users {
		if users[i].Sub == actorSub {
			actor = &users[i]
		}
	}
	if actor == nil || before.Evaluate(*actor).Role != api.RoleAdmin || after.Evaluate(*actor).Role == api.RoleAdmin {
		return false
	}
	admins := after.AdminSubjects(users)
	delete(admins, actorSub)
	return len(admins) == 0
}
