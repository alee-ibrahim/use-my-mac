package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/installer"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// One-line install tokens: short-lived, single- or N-use enrolment secrets
// bound to an install profile. Fetching the install script does not use one
// up; a successful enrolment does.

const (
	installDefaultTTL  = time.Hour
	installMaxTTL      = 7 * 24 * time.Hour
	installMaxUses     = 1000
	installEventsKept  = 200
	installTokenPrefix = 12 // characters shown in lists (after the umm_ins_ prefix)
)

// Install audit event kinds.
const (
	InstallEventScript   = "script"
	InstallEventDownload = "download"
	InstallEventEnrol    = "enrol"
	InstallEventDenied   = "denied"
	InstallEventRevoke   = "revoke"
	InstallEventCreate   = "create"
)

// CreateInstallToken mints an install token for an already validated
// profile (see installer.NormalizeProfile). extra labels override the
// profile's.
func (e *Engine) CreateInstallToken(ctx context.Context, by string, prof api.InstallProfile, req api.InstallTokenRequest) (api.InstallToken, error) {
	if err := installer.ValidLabels(req.Labels); err != nil {
		return api.InstallToken{}, &ValidationError{err.Error()}
	}
	labels := map[string]string{}
	for k, v := range prof.Labels {
		labels[k] = v
	}
	for k, v := range req.Labels {
		labels[k] = v
	}
	prof.Labels = labels
	if err := installer.NormalizeProfile(&prof); err != nil {
		return api.InstallToken{}, &ValidationError{err.Error()}
	}
	ttl := time.Duration(req.TTLMinutes) * time.Minute
	switch {
	case req.TTLMinutes == 0:
		ttl = installDefaultTTL
	case req.TTLMinutes < 0 || ttl > installMaxTTL:
		return api.InstallToken{}, &ValidationError{"ttl_minutes must be between 1 and 10080 (7 days)"}
	}
	uses := req.MaxUses
	switch {
	case uses == 0:
		uses = 1
	case uses < 0 || uses > installMaxUses:
		return api.InstallToken{}, &ValidationError{"max_uses must be between 1 and 1000"}
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 || strings.ContainsAny(note, "\r\n") {
		return api.InstallToken{}, &ValidationError{"note must be one line of at most 200 characters"}
	}
	tok := auth.NewSecret(auth.PrefixInstall)
	now := e.now()
	row := &store.InstallToken{
		ID: "ins_" + auth.NewID(), Hash: auth.Hash(tok), Prefix: tok[:len(auth.PrefixInstall)+installTokenPrefix],
		Profile: prof.Name, Spec: jsonStr(prof), Note: note, CreatedBy: by, CreatedAt: ms(now),
		ExpiresAt: ms(now.Add(ttl)), MaxUses: uses,
	}
	if err := e.st.CreateInstallToken(ctx, row); err != nil {
		return api.InstallToken{}, err
	}
	e.RecordInstallEvent(ctx, row.ID, InstallEventCreate, "", "", "by "+by)
	e.log.Info("install token created", "id", row.ID, "profile", prof.Name, "uses", uses, "ttl", ttl.String(), "by", by)
	out := e.installTokenView(row, now)
	out.Token = tok
	return out, nil
}

func (e *Engine) installTokenView(t *store.InstallToken, now time.Time) api.InstallToken {
	var prof api.InstallProfile
	_ = json.Unmarshal([]byte(t.Spec), &prof)
	v := api.InstallToken{ID: t.ID, Prefix: t.Prefix, Profile: prof, Note: t.Note, CreatedBy: t.CreatedBy,
		CreatedAt: time.UnixMilli(t.CreatedAt).UTC(), ExpiresAt: time.UnixMilli(t.ExpiresAt).UTC(),
		MaxUses: t.MaxUses, Uses: t.Uses, RevokedAt: tptr(t.RevokedAt), LastUsedAt: tptr(t.LastUsedAt)}
	switch {
	case t.RevokedAt != 0:
		v.State = api.InstallRevoked
	case t.Uses >= t.MaxUses:
		v.State = api.InstallUsed
	case t.ExpiresAt <= ms(now):
		v.State = api.InstallExpired
	default:
		v.State = api.InstallActive
	}
	return v
}

// InstallTokens lists install tokens, newest first.
func (e *Engine) InstallTokens(ctx context.Context) ([]api.InstallToken, error) {
	rows, err := e.st.ListInstallTokens(ctx, 200)
	if err != nil {
		return nil, err
	}
	now := e.now()
	out := make([]api.InstallToken, 0, len(rows))
	for _, r := range rows {
		out = append(out, e.installTokenView(r, now))
	}
	return out, nil
}

// RevokeInstallToken stops a token from being used again.
func (e *Engine) RevokeInstallToken(ctx context.Context, id, by, ip string) error {
	if err := e.st.RevokeInstallToken(ctx, id, ms(e.now())); err != nil {
		return err
	}
	e.RecordInstallEvent(ctx, id, InstallEventRevoke, ip, "", "by "+by)
	e.log.Info("install token revoked", "id", id, "by", by)
	return nil
}

// InstallEvents returns a token's audit log, newest first.
func (e *Engine) InstallEvents(ctx context.Context, id string) ([]api.InstallEvent, error) {
	if _, err := e.st.GetInstallToken(ctx, id); err != nil {
		return nil, err
	}
	evs, err := e.st.InstallEvents(ctx, id, installEventsKept)
	if err != nil {
		return nil, err
	}
	out := make([]api.InstallEvent, 0, len(evs))
	for _, ev := range evs {
		out = append(out, api.InstallEvent{At: time.UnixMilli(ev.TS).UTC(), Kind: ev.Kind, IP: ev.IP,
			AgentID: ev.AgentID, Detail: ev.Detail})
	}
	return out, nil
}

// RecordInstallEvent appends to a token's audit log (best effort).
func (e *Engine) RecordInstallEvent(ctx context.Context, tokenID, kind, ip, agentID, detail string) {
	if tokenID == "" {
		return
	}
	err := e.st.AddInstallEvent(ctx, &store.InstallEvent{TokenID: tokenID, TS: ms(e.now()), Kind: kind,
		IP: truncate(ip, 64), AgentID: agentID, Detail: truncate(detail, 300)}, installEventsKept)
	if err != nil {
		e.log.Warn("install audit log", "token", tokenID, "err", err)
	}
}

// ErrInstallToken: unknown, expired, revoked or used-up install token.
var ErrInstallToken = errors.New("install link is invalid, expired, revoked or used up")

// LookupInstallToken returns a live (usable) install token without using it.
// A known but dead token returns its view and ErrInstallToken.
func (e *Engine) LookupInstallToken(ctx context.Context, token string) (api.InstallToken, error) {
	if !installer.ValidToken(token) {
		return api.InstallToken{}, ErrInstallToken
	}
	row, err := e.st.InstallTokenByHash(ctx, auth.Hash(token))
	if errors.Is(err, store.ErrNotFound) {
		return api.InstallToken{}, ErrInstallToken
	}
	if err != nil {
		return api.InstallToken{}, err
	}
	v := e.installTokenView(row, e.now())
	if v.State != api.InstallActive {
		return v, ErrInstallToken
	}
	return v, nil
}

// AgentSelf is the router's view of an agent for itself.
func (e *Engine) AgentSelf(ctx context.Context, agentID string) (api.AgentSelf, error) {
	a, err := e.st.GetAgent(ctx, agentID)
	if err != nil {
		return api.AgentSelf{}, err
	}
	now := e.now()
	s := api.AgentSelf{ID: a.ID, Name: a.Name, Version: a.Version, LastSeenMS: a.LastSeen, NowMS: ms(now)}
	e.mu.Lock()
	if l := e.live[agentID]; l != nil && !l.seen.IsZero() {
		s.LastSeenMS, s.Version = ms(l.seen), l.hb.Version
	}
	e.mu.Unlock()
	return s, nil
}
