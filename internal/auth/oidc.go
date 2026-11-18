package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig configures dashboard login with any OpenID Connect provider.
type OIDCConfig struct {
	// Preset fills sensible defaults: "entra" (Microsoft Entra ID) or "".
	Preset string `yaml:"preset" json:"preset"`
	// TenantID is used by the "entra" preset to build the issuer URL.
	TenantID string `yaml:"tenant_id" json:"tenant_id"`

	Issuer   string `yaml:"issuer" json:"issuer"`
	ClientID string `yaml:"client_id" json:"client_id"`
	// ClientSecret is read from the environment (UMM_OIDC_CLIENT_SECRET),
	// never from argv.
	ClientSecret string   `yaml:"-" json:"-"`
	RedirectURL  string   `yaml:"redirect_url" json:"redirect_url"`
	Scopes       []string `yaml:"scopes" json:"scopes"`
	// DomainHint is sent as domain_hint on the authorize request (Entra:
	// skips the home-realm/account picker for that domain).
	DomainHint string `yaml:"domain_hint" json:"domain_hint"`
	// Prompt is sent as prompt on the authorize request, e.g.
	// "select_account" or "login". Empty = provider default.
	Prompt string `yaml:"prompt" json:"prompt"`

	// GroupsClaim holds group IDs/names (Entra: "groups", object IDs).
	GroupsClaim string `yaml:"groups_claim" json:"groups_claim"`
	// RolesClaim holds app roles (Entra: "roles"); its values are matched
	// against the same lists as groups.
	RolesClaim string `yaml:"roles_claim" json:"roles_claim"`
	// NameClaim is shown in the UI.
	NameClaim string `yaml:"name_claim" json:"name_claim"`

	AdminGroups  []string `yaml:"admin_groups" json:"admin_groups"`
	ViewerGroups []string `yaml:"viewer_groups" json:"viewer_groups"`
	// DefaultRole for authenticated users in no listed group: "viewer" or
	// "" (deny).
	DefaultRole string `yaml:"default_role" json:"default_role"`
}

// Enabled reports whether OIDC is configured.
func (c OIDCConfig) Enabled() bool { return c.ClientID != "" && (c.Issuer != "" || c.TenantID != "") }

// ApplyPreset fills defaults for known providers.
func (c *OIDCConfig) ApplyPreset() {
	if strings.EqualFold(c.Preset, "entra") {
		if c.Issuer == "" && c.TenantID != "" {
			c.Issuer = "https://login.microsoftonline.com/" + c.TenantID + "/v2.0"
		}
		if c.GroupsClaim == "" {
			c.GroupsClaim = "groups"
		}
		if c.RolesClaim == "" {
			c.RolesClaim = "roles"
		}
		if c.NameClaim == "" {
			c.NameClaim = "name"
		}
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.NameClaim == "" {
		c.NameClaim = "name"
	}
}

// ErrNoRole means the user authenticated but is not allowed in.
var ErrNoRole = errors.New("user is not in an allowed group")

// ErrGroupOverage is returned when the provider omitted groups because the
// user is in too many (Entra's "groups overage"). Use app roles or
// "groups assigned to the application" instead.
var ErrGroupOverage = errors.New("groups claim overage: configure app roles or assigned-groups-only in the identity provider")

func claimValues(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.FieldsFunc(x, func(r rune) bool { return r == ' ' || r == ',' })
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}

// MapRole maps ID-token claims to "admin" or "viewer". Admin wins over viewer.
func (c OIDCConfig) MapRole(claims map[string]any) (string, error) {
	var vals []string
	vals = append(vals, claimValues(claims[c.GroupsClaim])...)
	if c.RolesClaim != "" {
		vals = append(vals, claimValues(claims[c.RolesClaim])...)
	}
	in := func(list []string) bool {
		for _, v := range vals {
			for _, g := range list {
				if strings.EqualFold(strings.TrimSpace(g), v) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case in(c.AdminGroups):
		return "admin", nil
	case in(c.ViewerGroups):
		return "viewer", nil
	}
	if names, ok := claims["_claim_names"].(map[string]any); ok {
		if _, over := names[c.GroupsClaim]; over && len(vals) == 0 {
			return "", ErrGroupOverage
		}
	}
	if c.DefaultRole == "viewer" {
		return "viewer", nil
	}
	return "", ErrNoRole
}

// GroupValues returns the group and app-role values in the claims, and
// whether the provider left the groups out because there are too many
// (Entra "groups overage").
func (c OIDCConfig) GroupValues(claims map[string]any) (vals []string, overage bool) {
	vals = append(vals, claimValues(claims[c.GroupsClaim])...)
	if c.RolesClaim != "" {
		vals = append(vals, claimValues(claims[c.RolesClaim])...)
	}
	if names, ok := claims["_claim_names"].(map[string]any); ok {
		if _, over := names[c.GroupsClaim]; over && len(claimValues(claims[c.GroupsClaim])) == 0 {
			overage = true
		}
	}
	return vals, overage
}

// ClaimString returns the first non-empty string claim of keys.
func ClaimString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := claims[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// DisplayName picks a human name from claims.
func (c OIDCConfig) DisplayName(claims map[string]any) string {
	for _, k := range []string{c.NameClaim, "name", "preferred_username", "email", "sub"} {
		if s, ok := claims[k].(string); ok && s != "" {
			return s
		}
	}
	return "user"
}

// OIDC performs the authorization-code flow with PKCE.
type OIDC struct {
	Cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// NewOIDC discovers the provider.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	cfg.ApplyPreset()
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	return &OIDC{
		Cfg:      cfg,
		provider: p,
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: cfg.Scopes,
		},
	}, nil
}

// LoginState is kept in a short-lived signed cookie between redirect and callback.
type LoginState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
	Expires  int64  `json:"exp"`
}

func randString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL starts a login.
func (o *OIDC) AuthURL(ret string, expires int64) (string, LoginState) {
	st := LoginState{State: randString(), Nonce: randString(), Verifier: oauth2.GenerateVerifier(), Return: ret, Expires: expires}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)}
	if o.Cfg.DomainHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("domain_hint", o.Cfg.DomainHint))
	}
	if o.Cfg.Prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", o.Cfg.Prompt))
	}
	u := o.oauth.AuthCodeURL(st.State, opts...)
	return u, st
}

// Exchange completes a login and returns the verified claims.
func (o *OIDC) Exchange(ctx context.Context, st LoginState, code string) (map[string]any, error) {
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, err
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in response")
	}
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if idt.Nonce != st.Nonce {
		return nil, errors.New("nonce mismatch")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// EncodeState / DecodeState use the session signer for the login cookie.
func EncodeState(key []byte, st LoginState) string {
	b, _ := json.Marshal(st)
	return SignSession(key, Session{Subject: base64.RawURLEncoding.EncodeToString(b), Expires: st.Expires, Via: "login-state"})
}

// DecodeState verifies and decodes the login cookie.
func DecodeState(key []byte, v string, now time.Time) (*LoginState, error) {
	s, err := VerifySession(key, v, now)
	if err != nil {
		return nil, err
	}
	if s.Via != "login-state" {
		return nil, errors.New("not a login state")
	}
	b, err := base64.RawURLEncoding.DecodeString(s.Subject)
	if err != nil {
		return nil, err
	}
	var st LoginState
	return &st, json.Unmarshal(b, &st)
}
