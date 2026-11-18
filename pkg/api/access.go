package api

import "time"

// Dashboard access: who gets which role after single sign-on. Rules from the
// router config are authoritative and read-only; rules added in the
// dashboard (or with `umm access`) are stored in the database and add on
// top. Precedence: a user rule beats group rules (highest role wins), which
// beat the default role. A config group rule is a floor nothing can lower.

// Access rule kinds and sources.
const (
	AccessKindGroup = "group" // value: a group ID (or app-role value) from the token
	AccessKindUser  = "user"  // value: a user's subject/object ID, email or UPN

	AccessFromConfig = "config"
	AccessFromUI     = "ui"

	// RoleDeny is the default role that lets nobody in. Only for defaults.
	RoleDeny = "deny"
)

// AccessRule maps a group or a user to a role.
type AccessRule struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`  // group or user
	Value string `json:"value"` // group ID, or subject/email/UPN
	Role  string `json:"role"`  // admin or viewer
	// Name is a display name: the group's name, or the matched user's name.
	Name      string     `json:"name,omitempty"`
	Source    string     `json:"source"` // config or ui
	Locked    bool       `json:"locked"` // from config: cannot be changed here
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// AccessRuleRequest creates or updates a rule (POST /v1/access/rules). An
// existing dashboard rule for the same kind and value is updated.
type AccessRuleRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Role  string `json:"role"`
	// Name optionally sets the group's display name (group rules).
	Name string `json:"name,omitempty"`
}

// AccessPolicy is the full rule set (GET /v1/access).
type AccessPolicy struct {
	// DefaultRole applies to signed-in users no rule matches: deny or viewer.
	// It is the higher of ConfigDefaultRole and UIDefaultRole.
	DefaultRole       string            `json:"default_role"`
	ConfigDefaultRole string            `json:"config_default_role"`
	UIDefaultRole     string            `json:"ui_default_role"`
	Rules             []AccessRule      `json:"rules"`
	GroupNames        map[string]string `json:"group_names"`
	OIDC              bool              `json:"oidc"`
}

// AccessDefaultRequest sets the dashboard default role (PUT /v1/access/default).
type AccessDefaultRequest struct {
	Role string `json:"role"` // deny or viewer
}

// AccessGroupNameRequest names a group ID (PUT /v1/access/groups/{id}).
// An empty name removes it.
type AccessGroupNameRequest struct {
	Name string `json:"name"`
}

// AccessUserRoleRequest sets a user's own rule (PUT /v1/access/users/{subject}/role).
// An empty role removes the user rule, so groups and the default apply again.
type AccessUserRoleRequest struct {
	Role string `json:"role"`
}

// AccessUser is someone who signed in to the dashboard with single sign-on.
type AccessUser struct {
	Subject    string    `json:"subject"`
	Name       string    `json:"name"`
	Email      string    `json:"email,omitempty"`
	UPN        string    `json:"upn,omitempty"`
	OID        string    `json:"oid,omitempty"`
	Groups     []string  `json:"groups"`
	FirstLogin time.Time `json:"first_login"`
	LastLogin  time.Time `json:"last_login"`
	// Role is the effective role now ("" = denied); Reason says why and
	// RuleID names the deciding rule (empty for the default role).
	Role   string `json:"role"`
	Reason string `json:"reason"`
	RuleID string `json:"rule_id,omitempty"`
	Source string `json:"source,omitempty"` // config, ui or default
}

// AccessEvent is one entry of the role-change audit log.
type AccessEvent struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}
