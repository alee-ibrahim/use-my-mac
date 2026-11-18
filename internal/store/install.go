package store

import (
	"context"
	"database/sql"
	"errors"
)

// One-line install tokens. The secret is in the install URL; only its SHA-256
// is stored. A token is bound at creation to a resolved install profile
// (spec, JSON) and may enrol up to max_uses Macs before it expires.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS install_tokens (
			id TEXT PRIMARY KEY,
			hash TEXT NOT NULL UNIQUE,
			prefix TEXT NOT NULL,
			profile TEXT NOT NULL,
			spec TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_by TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL,
			max_uses INTEGER NOT NULL,
			uses INTEGER NOT NULL DEFAULT 0,
			revoked_at BIGINT NOT NULL DEFAULT 0,
			last_used_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS install_tokens_created ON install_tokens(created_at)`,
		`CREATE TABLE IF NOT EXISTS install_events (
			token_id TEXT NOT NULL,
			ts BIGINT NOT NULL,
			kind TEXT NOT NULL,
			ip TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS install_events_token ON install_events(token_id, ts)`,
	)
}

// InstallToken is a row of install_tokens.
type InstallToken struct {
	ID         string
	Hash       string
	Prefix     string
	Profile    string
	Spec       string
	Note       string
	CreatedBy  string
	CreatedAt  int64
	ExpiresAt  int64
	MaxUses    int
	Uses       int
	RevokedAt  int64
	LastUsedAt int64
}

const installTokenCols = `id, hash, prefix, profile, spec, note, created_by, created_at, expires_at, max_uses, uses,
	revoked_at, last_used_at`

func scanInstallToken(sc interface{ Scan(...any) error }) (*InstallToken, error) {
	var t InstallToken
	err := sc.Scan(&t.ID, &t.Hash, &t.Prefix, &t.Profile, &t.Spec, &t.Note, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt,
		&t.MaxUses, &t.Uses, &t.RevokedAt, &t.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// CreateInstallToken inserts a token.
func (s *Store) CreateInstallToken(ctx context.Context, t *InstallToken) error {
	_, err := s.exec(ctx, `INSERT INTO install_tokens(`+installTokenCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Hash, t.Prefix, t.Profile, t.Spec, t.Note, t.CreatedBy, t.CreatedAt, t.ExpiresAt, t.MaxUses, t.Uses,
		t.RevokedAt, t.LastUsedAt)
	return err
}

// InstallTokenByHash looks a token up by the hash of its secret, whatever
// its state.
func (s *Store) InstallTokenByHash(ctx context.Context, hash string) (*InstallToken, error) {
	return scanInstallToken(s.queryRow(ctx, `SELECT `+installTokenCols+` FROM install_tokens WHERE hash = ?`, hash))
}

// GetInstallToken by ID.
func (s *Store) GetInstallToken(ctx context.Context, id string) (*InstallToken, error) {
	return scanInstallToken(s.queryRow(ctx, `SELECT `+installTokenCols+` FROM install_tokens WHERE id = ?`, id))
}

// ListInstallTokens returns the newest tokens first.
func (s *Store) ListInstallTokens(ctx context.Context, limit int) ([]*InstallToken, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT `+installTokenCols+` FROM install_tokens ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*InstallToken
	for rows.Next() {
		t, err := scanInstallToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ConsumeInstallToken atomically takes one use of a live token and returns
// it. ErrNotFound if the token is unknown, expired, revoked or used up.
func (s *Store) ConsumeInstallToken(ctx context.Context, hash string, now int64) (*InstallToken, error) {
	res, err := s.exec(ctx, `UPDATE install_tokens SET uses = uses + 1, last_used_at = ?
		WHERE hash = ? AND revoked_at = 0 AND expires_at > ? AND uses < max_uses`, now, hash, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return s.InstallTokenByHash(ctx, hash)
}

// RevokeInstallToken marks a token revoked (idempotent).
func (s *Store) RevokeInstallToken(ctx context.Context, id string, now int64) error {
	res, err := s.exec(ctx, `UPDATE install_tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	_, err = s.GetInstallToken(ctx, id)
	return err
}

// InstallEvent is a row of the install audit log.
type InstallEvent struct {
	TokenID string
	TS      int64
	Kind    string
	IP      string
	AgentID string
	Detail  string
}

// AddInstallEvent appends to a token's audit log, keeping the newest keep rows.
func (s *Store) AddInstallEvent(ctx context.Context, e *InstallEvent, keep int) error {
	if _, err := s.exec(ctx, `INSERT INTO install_events(token_id, ts, kind, ip, agent_id, detail) VALUES(?,?,?,?,?,?)`,
		e.TokenID, e.TS, e.Kind, e.IP, e.AgentID, e.Detail); err != nil {
		return err
	}
	var cutoff int64
	err := s.queryRow(ctx, `SELECT ts FROM install_events WHERE token_id = ? ORDER BY ts DESC LIMIT 1 OFFSET ?`,
		e.TokenID, keep).Scan(&cutoff)
	if err == nil {
		_, err = s.exec(ctx, `DELETE FROM install_events WHERE token_id = ? AND ts <= ?`, e.TokenID, cutoff)
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// InstallEvents returns a token's audit log, newest first.
func (s *Store) InstallEvents(ctx context.Context, tokenID string, limit int) ([]InstallEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.query(ctx, `SELECT token_id, ts, kind, ip, agent_id, detail FROM install_events
		WHERE token_id = ? ORDER BY ts DESC LIMIT ?`, tokenID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstallEvent
	for rows.Next() {
		var e InstallEvent
		if err := rows.Scan(&e.TokenID, &e.TS, &e.Kind, &e.IP, &e.AgentID, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
