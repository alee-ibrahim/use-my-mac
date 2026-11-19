// Package store persists agents, jobs, tasks, keys and logs in SQL.
// SQLite (pure Go, no cgo) is used for development and single-node installs;
// Postgres for production. Queries are written with '?' placeholders and
// rebound for Postgres. Times are stored as Unix milliseconds (0 = unset).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	_ "modernc.org/sqlite"             // sqlite driver "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps a database handle.
type Store struct {
	db       *sql.DB
	postgres bool
}

// Open connects using a URL: "sqlite://path/to/file.db", "sqlite://:memory:"
// or "postgres://user:pass@host/db?sslmode=...". It runs migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	var (
		db  *sql.DB
		err error
		pg  bool
	)
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		db, err = sql.Open("pgx", url)
		pg = true
	case strings.HasPrefix(url, "sqlite://"):
		path := strings.TrimPrefix(url, "sqlite://")
		if path != ":memory:" {
			if dir := filepath.Dir(path); dir != "." {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					return nil, err
				}
			}
		}
		dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(memory)"
		if path != ":memory:" {
			dsn += "&_pragma=journal_mode(WAL)"
		}
		db, err = sql.Open("sqlite", dsn)
		if err == nil {
			// One writer keeps SQLite simple and avoids SQLITE_BUSY.
			db.SetMaxOpenConns(1)
		}
	default:
		return nil, fmt.Errorf("unsupported database url (want sqlite:// or postgres://)")
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: pg}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// rebind converts '?' placeholders to $n for Postgres.
func (s *Store) rebind(q string) string {
	if !s.postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS enrol_tokens (
		hash TEXT PRIMARY KEY,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		used_at BIGINT NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS agents (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		cred_hash TEXT NOT NULL UNIQUE,
		created_at BIGINT NOT NULL,
		last_seen BIGINT NOT NULL DEFAULT 0,
		drain INTEGER NOT NULL DEFAULT 0,
		opted_out INTEGER NOT NULL DEFAULT 0,
		quarantined_until BIGINT NOT NULL DEFAULT 0,
		consec_failures INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		telemetry TEXT NOT NULL DEFAULT '',
		runtimes TEXT NOT NULL DEFAULT '[]',
		labels TEXT NOT NULL DEFAULT '{}',
		max_tasks INTEGER NOT NULL DEFAULT 1,
		version TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		prefix TEXT NOT NULL,
		role TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		last_used_at BIGINT NOT NULL DEFAULT 0,
		revoked_at BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		owner TEXT NOT NULL,
		labels TEXT NOT NULL DEFAULT '{}',
		state TEXT NOT NULL,
		cancel_requested INTEGER NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		finished_at BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created_at)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		key TEXT NOT NULL,
		spec TEXT NOT NULL,
		state TEXT NOT NULL,
		attempt INTEGER NOT NULL DEFAULT 0,
		failures INTEGER NOT NULL DEFAULT 0,
		agent_id TEXT NOT NULL DEFAULT '',
		excluded TEXT NOT NULL DEFAULT '{}',
		not_before BIGINT NOT NULL DEFAULT 0,
		assigned_at BIGINT NOT NULL DEFAULT 0,
		started_at BIGINT NOT NULL DEFAULT 0,
		deadline_at BIGINT NOT NULL DEFAULT 0,
		finished_at BIGINT NOT NULL DEFAULT 0,
		exit_code INTEGER,
		error TEXT NOT NULL DEFAULT '',
		outputs TEXT NOT NULL DEFAULT '[]',
		score DOUBLE PRECISION NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		UNIQUE(job_id, key)
	)`,
	`CREATE INDEX IF NOT EXISTS tasks_state ON tasks(state)`,
	`CREATE INDEX IF NOT EXISTS tasks_job ON tasks(job_id)`,
	`CREATE TABLE IF NOT EXISTS attempts (
		task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		attempt INTEGER NOT NULL,
		agent_id TEXT NOT NULL,
		started_at BIGINT NOT NULL,
		finished_at BIGINT NOT NULL DEFAULT 0,
		outcome TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		score DOUBLE PRECISION NOT NULL DEFAULT 0,
		PRIMARY KEY (task_id, attempt)
	)`,
	`CREATE TABLE IF NOT EXISTS agent_updates (
		agent_id TEXT PRIMARY KEY,
		state TEXT NOT NULL DEFAULT '',
		from_version TEXT NOT NULL DEFAULT '',
		to_version TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL DEFAULT '',
		at BIGINT NOT NULL DEFAULT 0,
		checked_at BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS update_events (
		agent_id TEXT NOT NULL,
		ts BIGINT NOT NULL,
		state TEXT NOT NULL,
		from_version TEXT NOT NULL DEFAULT '',
		to_version TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS update_events_agent ON update_events(agent_id, ts)`,
	`CREATE TABLE IF NOT EXISTS task_logs (
		task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		attempt INTEGER NOT NULL,
		seq INTEGER NOT NULL,
		ts BIGINT NOT NULL,
		data TEXT NOT NULL,
		PRIMARY KEY (task_id, attempt, seq)
	)`,
}

func (s *Store) migrate(ctx context.Context) error {
	for _, q := range schema {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// GetSetting returns "" and ErrNotFound if unset.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// PutSetting upserts a setting.
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.exec(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// Enrolment tokens and agents
// ---------------------------------------------------------------------------

// CreateEnrolToken stores a hashed one-time token.
func (s *Store) CreateEnrolToken(ctx context.Context, hash string, now, expires int64, by string) error {
	_, err := s.exec(ctx, `INSERT INTO enrol_tokens(hash, created_at, expires_at, created_by) VALUES(?,?,?,?)`,
		hash, now, expires, by)
	return err
}

// ConsumeEnrolToken atomically marks a valid token as used.
func (s *Store) ConsumeEnrolToken(ctx context.Context, hash string, now int64) error {
	res, err := s.exec(ctx, `UPDATE enrol_tokens SET used_at = ? WHERE hash = ? AND used_at = 0 AND expires_at > ?`,
		now, hash, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// Agent is a row of the agents table.
type Agent struct {
	ID               string
	Name             string
	CredHash         string
	CreatedAt        int64
	LastSeen         int64
	Drain            bool
	OptedOut         bool
	QuarantinedUntil int64
	ConsecFailures   int
	State            string
	Reason           string
	Telemetry        string
	Runtimes         string
	Labels           string
	MaxTasks         int
	Version          string
}

const agentCols = `id, name, cred_hash, created_at, last_seen, drain, opted_out, quarantined_until,
	consec_failures, state, reason, telemetry, runtimes, labels, max_tasks, version`

func scanAgent(sc interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var drain, opt int
	err := sc.Scan(&a.ID, &a.Name, &a.CredHash, &a.CreatedAt, &a.LastSeen, &drain, &opt, &a.QuarantinedUntil,
		&a.ConsecFailures, &a.State, &a.Reason, &a.Telemetry, &a.Runtimes, &a.Labels, &a.MaxTasks, &a.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	a.Drain, a.OptedOut = drain != 0, opt != 0
	return &a, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateAgent inserts a new agent.
func (s *Store) CreateAgent(ctx context.Context, a *Agent) error {
	_, err := s.exec(ctx, `INSERT INTO agents(`+agentCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.CredHash, a.CreatedAt, a.LastSeen, b2i(a.Drain), b2i(a.OptedOut), a.QuarantinedUntil,
		a.ConsecFailures, a.State, a.Reason, a.Telemetry, a.Runtimes, a.Labels, a.MaxTasks, a.Version)
	return err
}

// GetAgent by ID.
func (s *Store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.queryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

// AgentByCredential looks an agent up by its credential hash.
func (s *Store) AgentByCredential(ctx context.Context, hash string) (*Agent, error) {
	return scanAgent(s.queryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE cred_hash = ?`, hash))
}

// ListAgents returns all agents ordered by name.
func (s *Store) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAgentHeartbeat stores what an agent reported.
func (s *Store) UpdateAgentHeartbeat(ctx context.Context, a *Agent) error {
	_, err := s.exec(ctx, `UPDATE agents SET last_seen=?, state=?, reason=?, telemetry=?, runtimes=?, labels=?,
		max_tasks=?, version=? WHERE id=?`,
		a.LastSeen, a.State, a.Reason, a.Telemetry, a.Runtimes, a.Labels, a.MaxTasks, a.Version, a.ID)
	return err
}

// SetAgentName renames an agent.
func (s *Store) SetAgentName(ctx context.Context, id, name string) error {
	res, err := s.exec(ctx, `UPDATE agents SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentFlags sets drain / opt-out.
func (s *Store) SetAgentFlags(ctx context.Context, id string, drain, optedOut bool) error {
	res, err := s.exec(ctx, `UPDATE agents SET drain=?, opted_out=? WHERE id=?`, b2i(drain), b2i(optedOut), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentHealth records consecutive failures and quarantine.
func (s *Store) SetAgentHealth(ctx context.Context, id string, consec int, quarantinedUntil int64) error {
	_, err := s.exec(ctx, `UPDATE agents SET consec_failures=?, quarantined_until=? WHERE id=?`, consec, quarantinedUntil, id)
	return err
}

// DeleteAgent removes an agent (its credential stops working).
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	_, _ = s.exec(ctx, `DELETE FROM agent_updates WHERE agent_id=?`, id)
	_, _ = s.exec(ctx, `DELETE FROM update_events WHERE agent_id=?`, id)
	_, _ = s.exec(ctx, `DELETE FROM agent_runtimes WHERE agent_id=?`, id)
	res, err := s.exec(ctx, `DELETE FROM agents WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Agent updates
// ---------------------------------------------------------------------------

// AgentUpdate is the latest self-update status of one agent.
type AgentUpdate struct {
	AgentID, State, FromVersion, ToVersion, Message string
	At, CheckedAt                                   int64
}

// UpdateEvent is one row of an agent's update history.
type UpdateEvent struct {
	TS                                     int64
	State, FromVersion, ToVersion, Message string
}

// TouchUpdateCheck records that an agent's updater checked in.
func (s *Store) TouchUpdateCheck(ctx context.Context, agentID string, now int64) error {
	_, err := s.exec(ctx, `INSERT INTO agent_updates(agent_id, checked_at) VALUES(?, ?)
		ON CONFLICT (agent_id) DO UPDATE SET checked_at = excluded.checked_at`, agentID, now)
	return err
}

// GetAgentUpdate returns an agent's update status (ErrNotFound if none).
func (s *Store) GetAgentUpdate(ctx context.Context, agentID string) (*AgentUpdate, error) {
	var u AgentUpdate
	err := s.queryRow(ctx, `SELECT agent_id, state, from_version, to_version, message, at, checked_at
		FROM agent_updates WHERE agent_id = ?`, agentID).
		Scan(&u.AgentID, &u.State, &u.FromVersion, &u.ToVersion, &u.Message, &u.At, &u.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// ListAgentUpdates returns every agent's update status, keyed by agent ID.
func (s *Store) ListAgentUpdates(ctx context.Context) (map[string]*AgentUpdate, error) {
	rows, err := s.query(ctx, `SELECT agent_id, state, from_version, to_version, message, at, checked_at FROM agent_updates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*AgentUpdate{}
	for rows.Next() {
		var u AgentUpdate
		if err := rows.Scan(&u.AgentID, &u.State, &u.FromVersion, &u.ToVersion, &u.Message, &u.At, &u.CheckedAt); err != nil {
			return nil, err
		}
		out[u.AgentID] = &u
	}
	return out, rows.Err()
}

// RecordUpdate stores an updater report as the agent's status and, when
// event is true, appends it to the history (keeping the newest keep rows).
func (s *Store) RecordUpdate(ctx context.Context, u *AgentUpdate, event bool, keep int) error {
	_, err := s.exec(ctx, `INSERT INTO agent_updates(agent_id, state, from_version, to_version, message, at, checked_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_id) DO UPDATE SET state = excluded.state, from_version = excluded.from_version,
		to_version = excluded.to_version, message = excluded.message, at = excluded.at, checked_at = excluded.checked_at`,
		u.AgentID, u.State, u.FromVersion, u.ToVersion, u.Message, u.At, u.CheckedAt)
	if err != nil || !event {
		return err
	}
	if _, err := s.exec(ctx, `INSERT INTO update_events(agent_id, ts, state, from_version, to_version, message)
		VALUES(?, ?, ?, ?, ?, ?)`, u.AgentID, u.At, u.State, u.FromVersion, u.ToVersion, u.Message); err != nil {
		return err
	}
	// Prune: keep the newest `keep` events of this agent.
	var cutoff int64
	err = s.queryRow(ctx, `SELECT ts FROM update_events WHERE agent_id = ? ORDER BY ts DESC LIMIT 1 OFFSET ?`,
		u.AgentID, keep).Scan(&cutoff)
	if err == nil {
		_, err = s.exec(ctx, `DELETE FROM update_events WHERE agent_id = ? AND ts <= ?`, u.AgentID, cutoff)
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// UpdateEvents returns an agent's update history, newest first.
func (s *Store) UpdateEvents(ctx context.Context, agentID string, limit int) ([]UpdateEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.query(ctx, `SELECT ts, state, from_version, to_version, message FROM update_events
		WHERE agent_id = ? ORDER BY ts DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpdateEvent
	for rows.Next() {
		var e UpdateEvent
		if err := rows.Scan(&e.TS, &e.State, &e.FromVersion, &e.ToVersion, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// APIKey is a row of api_keys.
type APIKey struct {
	ID, Name, Hash, Prefix, Role     string
	CreatedAt, LastUsedAt, RevokedAt int64
}

const keyCols = `id, name, hash, prefix, role, created_at, last_used_at, revoked_at`

func scanKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	err := sc.Scan(&k.ID, &k.Name, &k.Hash, &k.Prefix, &k.Role, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

// CreateAPIKey inserts a key.
func (s *Store) CreateAPIKey(ctx context.Context, k *APIKey) error {
	_, err := s.exec(ctx, `INSERT INTO api_keys(`+keyCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		k.ID, k.Name, k.Hash, k.Prefix, k.Role, k.CreatedAt, k.LastUsedAt, k.RevokedAt)
	return err
}

// APIKeyByHash returns an active (not revoked) key.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	return scanKey(s.queryRow(ctx, `SELECT `+keyCols+` FROM api_keys WHERE hash=? AND revoked_at=0`, hash))
}

// TouchAPIKey records usage.
func (s *Store) TouchAPIKey(ctx context.Context, id string, now int64) error {
	_, err := s.exec(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, now, id)
	return err
}

// ListAPIKeys returns all keys.
func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.query(ctx, `SELECT `+keyCols+` FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CountActiveAdminKeys is used for bootstrap.
func (s *Store) CountActiveAdminKeys(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM api_keys WHERE role='admin' AND revoked_at=0`).Scan(&n)
	return n, err
}

// RevokeAPIKey revokes a key.
func (s *Store) RevokeAPIKey(ctx context.Context, id string, now int64) error {
	res, err := s.exec(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at=0`, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Jobs, tasks, attempts, logs
// ---------------------------------------------------------------------------

// Job is a row of jobs.
type Job struct {
	ID, Name, Owner, Labels, State string
	CancelRequested                bool
	CreatedAt, FinishedAt          int64
}

// Task is a row of tasks.
type Task struct {
	ID, JobID, Key, Spec, State string
	Attempt, Failures           int
	AgentID                     string
	Excluded                    string // JSON map agentID -> until (unix ms)
	NotBefore                   int64
	AssignedAt                  int64
	StartedAt                   int64
	DeadlineAt                  int64
	FinishedAt                  int64
	ExitCode                    *int
	Error                       string
	Outputs                     string // JSON []string of uploaded outputs
	Score                       float64
	CreatedAt                   int64
}

const jobCols = `id, name, owner, labels, state, cancel_requested, created_at, finished_at`
const taskCols = `id, job_id, key, spec, state, attempt, failures, agent_id, excluded, not_before, assigned_at,
	started_at, deadline_at, finished_at, exit_code, error, outputs, score, created_at`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var c int
	err := sc.Scan(&j.ID, &j.Name, &j.Owner, &j.Labels, &j.State, &c, &j.CreatedAt, &j.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	j.CancelRequested = c != 0
	return &j, err
}

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var exit sql.NullInt64
	err := sc.Scan(&t.ID, &t.JobID, &t.Key, &t.Spec, &t.State, &t.Attempt, &t.Failures, &t.AgentID, &t.Excluded,
		&t.NotBefore, &t.AssignedAt, &t.StartedAt, &t.DeadlineAt, &t.FinishedAt, &exit, &t.Error, &t.Outputs,
		&t.Score, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if exit.Valid {
		v := int(exit.Int64)
		t.ExitCode = &v
	}
	return &t, err
}

// CreateJob inserts a job and its tasks in one transaction.
func (s *Store) CreateJob(ctx context.Context, j *Job, tasks []*Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO jobs(`+jobCols+`) VALUES(?,?,?,?,?,?,?,?)`),
		j.ID, j.Name, j.Owner, j.Labels, j.State, b2i(j.CancelRequested), j.CreatedAt, j.FinishedAt); err != nil {
		return err
	}
	for _, t := range tasks {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO tasks(`+taskCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			t.ID, t.JobID, t.Key, t.Spec, t.State, t.Attempt, t.Failures, t.AgentID, t.Excluded, t.NotBefore,
			t.AssignedAt, t.StartedAt, t.DeadlineAt, t.FinishedAt, t.ExitCode, t.Error, t.Outputs, t.Score, t.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetJob by ID.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	return scanJob(s.queryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

// ListJobs newest first, optionally filtered by state and owner.
func (s *Store) ListJobs(ctx context.Context, state, owner string, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT ` + jobCols + ` FROM jobs WHERE 1=1`
	var args []any
	if state != "" {
		q += ` AND state=?`
		args = append(args, state)
	}
	if owner != "" {
		q += ` AND owner=?`
		args = append(args, owner)
	}
	q += ` ORDER BY created_at DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateJob updates state / cancel / finish.
func (s *Store) UpdateJob(ctx context.Context, j *Job) error {
	_, err := s.exec(ctx, `UPDATE jobs SET state=?, cancel_requested=?, finished_at=? WHERE id=?`,
		j.State, b2i(j.CancelRequested), j.FinishedAt, j.ID)
	return err
}

// GetTask by ID.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.queryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

func (s *Store) listTasks(ctx context.Context, where string, args ...any) ([]*Task, error) {
	rows, err := s.query(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TasksByJob returns a job's tasks.
func (s *Store) TasksByJob(ctx context.Context, jobID string) ([]*Task, error) {
	return s.listTasks(ctx, `job_id=?`, jobID)
}

// ActiveTasks returns every non-terminal task.
func (s *Store) ActiveTasks(ctx context.Context) ([]*Task, error) {
	return s.listTasks(ctx, `state IN ('blocked','pending','assigned','running')`)
}

// RunningTasksForAgent returns tasks in state "running" on an agent.
func (s *Store) RunningTasksForAgent(ctx context.Context, agentID string) ([]*Task, error) {
	return s.listTasks(ctx, `state='running' AND agent_id=?`, agentID)
}

// UpdateTask writes all mutable task fields.
func (s *Store) UpdateTask(ctx context.Context, t *Task) error {
	_, err := s.exec(ctx, `UPDATE tasks SET state=?, attempt=?, failures=?, agent_id=?, excluded=?, not_before=?,
		assigned_at=?, started_at=?, deadline_at=?, finished_at=?, exit_code=?, error=?, outputs=?, score=? WHERE id=?`,
		t.State, t.Attempt, t.Failures, t.AgentID, t.Excluded, t.NotBefore, t.AssignedAt, t.StartedAt, t.DeadlineAt,
		t.FinishedAt, t.ExitCode, t.Error, t.Outputs, t.Score, t.ID)
	return err
}

// TaskCounts returns per-state counts for a job.
func (s *Store) TaskCounts(ctx context.Context, jobID string) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT state, COUNT(*) FROM tasks WHERE job_id=? GROUP BY state`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// CountTasksByState returns global per-state counts (for metrics).
func (s *Store) CountTasksByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT state, COUNT(*) FROM tasks GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Attempt is a row of attempts.
type Attempt struct {
	TaskID              string
	Attempt             int
	AgentID             string
	StartedAt, Finished int64
	Outcome, Error      string
	Score               float64
}

// InsertAttempt records a new attempt.
func (s *Store) InsertAttempt(ctx context.Context, a *Attempt) error {
	_, err := s.exec(ctx, `INSERT INTO attempts(task_id, attempt, agent_id, started_at, finished_at, outcome, error, score)
		VALUES(?,?,?,?,?,?,?,?)`, a.TaskID, a.Attempt, a.AgentID, a.StartedAt, a.Finished, a.Outcome, a.Error, a.Score)
	return err
}

// FinishAttempt sets an attempt's outcome.
func (s *Store) FinishAttempt(ctx context.Context, taskID string, attempt int, outcome, errMsg string, now int64) error {
	_, err := s.exec(ctx, `UPDATE attempts SET outcome=?, error=?, finished_at=? WHERE task_id=? AND attempt=?`,
		outcome, errMsg, now, taskID, attempt)
	return err
}

// Attempts lists a task's attempts.
func (s *Store) Attempts(ctx context.Context, taskID string) ([]*Attempt, error) {
	rows, err := s.query(ctx, `SELECT task_id, attempt, agent_id, started_at, finished_at, outcome, error, score
		FROM attempts WHERE task_id=? ORDER BY attempt`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attempt
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.TaskID, &a.Attempt, &a.AgentID, &a.StartedAt, &a.Finished, &a.Outcome, &a.Error, &a.Score); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// AppendLog stores a log chunk; duplicate (attempt, seq) chunks are ignored.
func (s *Store) AppendLog(ctx context.Context, taskID string, attempt, seq int, ts int64, data string) error {
	_, err := s.exec(ctx, `INSERT INTO task_logs(task_id, attempt, seq, ts, data) VALUES(?,?,?,?,?)
		ON CONFLICT (task_id, attempt, seq) DO NOTHING`, taskID, attempt, seq, ts, data)
	return err
}

// LogSize returns the stored log bytes for a task.
func (s *Store) LogSize(ctx context.Context, taskID string) (int, error) {
	var n sql.NullInt64
	err := s.queryRow(ctx, `SELECT SUM(LENGTH(data)) FROM task_logs WHERE task_id=?`, taskID).Scan(&n)
	return int(n.Int64), err
}

// Logs returns a task's log text; attempt 0 means all attempts.
func (s *Store) Logs(ctx context.Context, taskID string, attempt int) (string, error) {
	q := `SELECT attempt, data FROM task_logs WHERE task_id=?`
	args := []any{taskID}
	if attempt > 0 {
		q += ` AND attempt=?`
		args = append(args, attempt)
	}
	q += ` ORDER BY attempt, seq`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	last := -1
	for rows.Next() {
		var a int
		var d string
		if err := rows.Scan(&a, &d); err != nil {
			return "", err
		}
		if attempt == 0 && a != last {
			fmt.Fprintf(&b, "=== attempt %d ===\n", a)
			last = a
		}
		b.WriteString(d)
	}
	return b.String(), rows.Err()
}
