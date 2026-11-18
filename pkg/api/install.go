package api

import "time"

// One-line install: an admin creates an install token bound to a profile and
// runs `curl -fsSL <router>/i/<token> | sudo sh` on the Mac.

// InstallProfile is what a one-line install configures on the Mac. Profiles
// are defined in the router config (`install.profiles`); a token stores the
// resolved profile at creation.
type InstallProfile struct {
	Name         string              `json:"name"`
	Labels       map[string]string   `json:"labels,omitempty"`
	Runtimes     map[string][]string `json:"runtimes"`
	MaxTasks     int                 `json:"max_tasks"`
	Priority     string              `json:"priority"`
	ServiceLabel string              `json:"service_label"`
}

// InstallTokenRequest creates an install token.
type InstallTokenRequest struct {
	// Profile name (default "default").
	Profile string `json:"profile,omitempty"`
	// Labels added to (and overriding) the profile's labels.
	Labels map[string]string `json:"labels,omitempty"`
	// TTLMinutes: lifetime (default 60, max 10080 = 7 days).
	TTLMinutes int `json:"ttl_minutes,omitempty"`
	// MaxUses: how many Macs may enrol with it (default 1, max 1000).
	MaxUses int    `json:"max_uses,omitempty"`
	Note    string `json:"note,omitempty"`
}

// InstallToken describes an install token. Token and Command are only
// returned once, when it is created.
type InstallToken struct {
	ID         string         `json:"id"`
	Prefix     string         `json:"prefix"`
	Profile    InstallProfile `json:"profile"`
	Note       string         `json:"note,omitempty"`
	CreatedBy  string         `json:"created_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	ExpiresAt  time.Time      `json:"expires_at"`
	MaxUses    int            `json:"max_uses"`
	Uses       int            `json:"uses"`
	RevokedAt  *time.Time     `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
	// State: active, expired, used, revoked.
	State   string `json:"state"`
	Token   string `json:"token,omitempty"`
	Command string `json:"command,omitempty"`
}

// Install token states.
const (
	InstallActive  = "active"
	InstallExpired = "expired"
	InstallUsed    = "used"
	InstallRevoked = "revoked"
)

// InstallEvent is one entry of an install token's audit log.
type InstallEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // script, download, enrol, denied, revoke
	IP      string    `json:"ip,omitempty"`
	AgentID string    `json:"agent_id,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// AgentSelf is the router's view of the calling agent (GET /v1/agent/self),
// used by the installer to wait for the first heartbeat.
type AgentSelf struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	LastSeenMS int64  `json:"last_seen_ms"`
	NowMS      int64  `json:"now_ms"`
}
