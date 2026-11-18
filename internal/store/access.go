package store

import (
	"context"
	"encoding/json"
)

// Dashboard access: role rules added in the dashboard, the people who signed
// in with single sign-on (their groups as of their last login, so roles can
// be re-evaluated on every request), group display names and an audit log of
// role changes. Rules from the router config are not stored here.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS access_rules (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			value TEXT NOT NULL,
			role TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL DEFAULT 0,
			UNIQUE (kind, value)
		)`,
		`CREATE TABLE IF NOT EXISTS access_users (
			sub TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			upn TEXT NOT NULL DEFAULT '',
			oid TEXT NOT NULL DEFAULT '',
			group_ids TEXT NOT NULL DEFAULT '[]',
			overage INTEGER NOT NULL DEFAULT 0,
			first_login BIGINT NOT NULL,
			last_login BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS access_group_names (
			group_id TEXT PRIMARY KEY,
			name TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS access_audit (
			ts BIGINT NOT NULL,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS access_audit_ts ON access_audit(ts)`,
	)
}

// AccessRule is a row of access_rules. Value is stored lower-cased.
type AccessRule struct {
	ID        string
	Kind      string
	Value     string
	Role      string
	CreatedBy string
	CreatedAt int64
	UpdatedAt int64
}

// AccessUser is a row of access_users.
type AccessUser struct {
	Sub        string
	Name       string
	Email      string
	UPN        string
	OID        string
	Groups     []string
	Overage    bool
	FirstLogin int64
	LastLogin  int64
}

// AccessRules returns all dashboard rules.
func (s *Store) AccessRules(ctx context.Context) ([]AccessRule, error) {
	rows, err := s.query(ctx, `SELECT id, kind, value, role, created_by, created_at, updated_at FROM access_rules
		ORDER BY kind, value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessRule
	for rows.Next() {
		var r AccessRule
		if err := rows.Scan(&r.ID, &r.Kind, &r.Value, &r.Role, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutAccessRule inserts a rule or, for an existing kind+value, updates its
// role. It returns the stored rule.
func (s *Store) PutAccessRule(ctx context.Context, r AccessRule) (AccessRule, error) {
	_, err := s.exec(ctx, `INSERT INTO access_rules(id, kind, value, role, created_by, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT (kind, value) DO UPDATE SET role = excluded.role, updated_at = excluded.created_at`,
		r.ID, r.Kind, r.Value, r.Role, r.CreatedBy, r.CreatedAt, 0)
	if err != nil {
		return r, err
	}
	var out AccessRule
	err = s.queryRow(ctx, `SELECT id, kind, value, role, created_by, created_at, updated_at FROM access_rules
		WHERE kind = ? AND value = ?`, r.Kind, r.Value).Scan(&out.ID, &out.Kind, &out.Value, &out.Role, &out.CreatedBy,
		&out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// DeleteAccessRule removes a rule; ErrNotFound if there is none.
func (s *Store) DeleteAccessRule(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM access_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertAccessUser records a sign-in.
func (s *Store) UpsertAccessUser(ctx context.Context, u AccessUser) error {
	g, _ := json.Marshal(u.Groups)
	if u.Groups == nil {
		g = []byte("[]")
	}
	_, err := s.exec(ctx, `INSERT INTO access_users(sub, name, email, upn, oid, group_ids, overage, first_login, last_login)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT (sub) DO UPDATE SET name = excluded.name, email = excluded.email, upn = excluded.upn,
			oid = excluded.oid, group_ids = excluded.group_ids, overage = excluded.overage, last_login = excluded.last_login`,
		u.Sub, u.Name, u.Email, u.UPN, u.OID, string(g), b2i(u.Overage), u.FirstLogin, u.LastLogin)
	return err
}

// AccessUsers returns everyone who signed in, most recent first.
func (s *Store) AccessUsers(ctx context.Context) ([]AccessUser, error) {
	rows, err := s.query(ctx, `SELECT sub, name, email, upn, oid, group_ids, overage, first_login, last_login
		FROM access_users ORDER BY last_login DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessUser
	for rows.Next() {
		var (
			u  AccessUser
			g  string
			ov int
		)
		if err := rows.Scan(&u.Sub, &u.Name, &u.Email, &u.UPN, &u.OID, &g, &ov, &u.FirstLogin, &u.LastLogin); err != nil {
			return nil, err
		}
		u.Overage = ov != 0
		_ = json.Unmarshal([]byte(g), &u.Groups)
		out = append(out, u)
	}
	return out, rows.Err()
}

// AccessGroupNames returns group ID -> display name.
func (s *Store) AccessGroupNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.query(ctx, `SELECT group_id, name FROM access_group_names`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// SetAccessGroupName names a group ID; an empty name removes it.
func (s *Store) SetAccessGroupName(ctx context.Context, id, name string) error {
	if name == "" {
		_, err := s.exec(ctx, `DELETE FROM access_group_names WHERE group_id = ?`, id)
		return err
	}
	_, err := s.exec(ctx, `INSERT INTO access_group_names(group_id, name) VALUES(?, ?)
		ON CONFLICT (group_id) DO UPDATE SET name = excluded.name`, id, name)
	return err
}

// AccessEvent is a row of the role-change audit log.
type AccessEvent struct {
	TS     int64
	Actor  string
	Action string
	Detail string
}

// AddAccessEvent appends to the audit log.
func (s *Store) AddAccessEvent(ctx context.Context, e AccessEvent) error {
	_, err := s.exec(ctx, `INSERT INTO access_audit(ts, actor, action, detail) VALUES(?,?,?,?)`,
		e.TS, e.Actor, e.Action, e.Detail)
	return err
}

// AccessEvents returns the newest audit entries first.
func (s *Store) AccessEvents(ctx context.Context, limit int) ([]AccessEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := s.query(ctx, `SELECT ts, actor, action, detail FROM access_audit ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessEvent
	for rows.Next() {
		var e AccessEvent
		if err := rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
