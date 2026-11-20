package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// The activity / audit log: notable fleet events (enrolments, yields,
// quarantines, failed jobs, update rollbacks) and every admin cleanup, with
// who did it. Pruned by age.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		ts BIGINT NOT NULL,
		kind TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		target TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL DEFAULT ''
	)`,
		`CREATE INDEX IF NOT EXISTS events_ts ON events(ts)`,
	)
}

// Event is a row of events.
type Event struct {
	ID, Kind, Actor, Target, Message string
	TS                               int64
}

// AddEvent appends an event.
func (s *Store) AddEvent(ctx context.Context, e *Event) error {
	_, err := s.exec(ctx, `INSERT INTO events(id, ts, kind, actor, target, message) VALUES(?,?,?,?,?,?)`,
		e.ID, e.TS, e.Kind, e.Actor, e.Target, e.Message)
	return err
}

// Events returns the newest events first. kinds filters by kind prefix
// ("mac." matches every Mac event); target filters exactly.
func (s *Store) Events(ctx context.Context, kinds []string, target string, before int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, ts, kind, actor, target, message FROM events WHERE 1=1`
	var args []any
	if len(kinds) > 0 {
		var ors []string
		for _, k := range kinds {
			if strings.HasSuffix(k, ".") {
				ors = append(ors, `kind LIKE ?`)
				args = append(args, k+"%")
			} else {
				ors = append(ors, `kind = ?`)
				args = append(args, k)
			}
		}
		q += ` AND (` + strings.Join(ors, " OR ") + `)`
	}
	if target != "" {
		q += ` AND target = ?`
		args = append(args, target)
	}
	if before > 0 {
		q += ` AND ts < ?`
		args = append(args, before)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Actor, &e.Target, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than before (Unix ms).
func (s *Store) PruneEvents(ctx context.Context, before int64) (int64, error) {
	r, err := s.exec(ctx, `DELETE FROM events WHERE ts < ?`, before)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// ---------------------------------------------------------------------------
// Cleanup of leftovers
// ---------------------------------------------------------------------------

// ErrActive is returned when deleting a record that is still in use (an
// unrevoked key, a live install link, an unfinished job).
var ErrActive = errors.New("still active")

// DeleteAPIKey deletes a revoked key. ErrActive if it is not revoked.
func (s *Store) DeleteAPIKey(ctx context.Context, id string) (*APIKey, error) {
	k, err := scanKey(s.queryRow(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if k.RevokedAt == 0 {
		return k, ErrActive
	}
	_, err = s.exec(ctx, `DELETE FROM api_keys WHERE id=? AND revoked_at<>0`, id)
	return k, err
}

// DeleteRevokedAPIKeys deletes every revoked key and returns them.
func (s *Store) DeleteRevokedAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.query(ctx, `SELECT `+keyCols+` FROM api_keys WHERE revoked_at<>0`)
	if err != nil {
		return nil, err
	}
	var ks []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ks = append(ks, k)
	}
	rows.Close()
	for _, k := range ks {
		if _, err := s.exec(ctx, `DELETE FROM api_keys WHERE id=?`, k.ID); err != nil {
			return nil, err
		}
	}
	return ks, nil
}

// installDead is true for a link that can no longer enrol a Mac.
func installDead(t *InstallToken, now int64) bool {
	return t.RevokedAt != 0 || t.ExpiresAt <= now || t.Uses >= t.MaxUses
}

// DeleteInstallToken deletes a revoked, expired or used-up install link and
// its audit log. ErrActive if it still works.
func (s *Store) DeleteInstallToken(ctx context.Context, id string, now int64) (*InstallToken, error) {
	t, err := s.GetInstallToken(ctx, id)
	if err != nil {
		return nil, err
	}
	if !installDead(t, now) {
		return t, ErrActive
	}
	if _, err := s.exec(ctx, `DELETE FROM install_events WHERE token_id=?`, id); err != nil {
		return t, err
	}
	_, err = s.exec(ctx, `DELETE FROM install_tokens WHERE id=?`, id)
	return t, err
}

// DeleteDeadInstallTokens deletes every link that can no longer enrol a Mac.
func (s *Store) DeleteDeadInstallTokens(ctx context.Context, now int64) ([]*InstallToken, error) {
	rows, err := s.query(ctx, `SELECT `+installTokenCols+` FROM install_tokens
		WHERE revoked_at<>0 OR expires_at<=? OR uses>=max_uses`, now)
	if err != nil {
		return nil, err
	}
	var ts []*InstallToken
	for rows.Next() {
		t, err := scanInstallToken(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ts = append(ts, t)
	}
	rows.Close()
	for _, t := range ts {
		if _, err := s.exec(ctx, `DELETE FROM install_events WHERE token_id=?`, t.ID); err != nil {
			return nil, err
		}
		if _, err := s.exec(ctx, `DELETE FROM install_tokens WHERE id=?`, t.ID); err != nil {
			return nil, err
		}
	}
	return ts, nil
}

// PruneEnrolTokens deletes used or expired one-time enrolment tokens.
func (s *Store) PruneEnrolTokens(ctx context.Context, now int64) (int64, error) {
	r, err := s.exec(ctx, `DELETE FROM enrol_tokens WHERE used_at<>0 OR expires_at<=?`, now)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// FinishedJobs returns IDs of finished jobs that finished before `before`
// (Unix ms), optionally only in the given states, oldest first.
func (s *Store) FinishedJobs(ctx context.Context, before int64, states []string, limit int) ([]string, error) {
	return s.FinishedJobsOf(ctx, "", before, states, limit)
}

// FinishedJobsOf is FinishedJobs limited to one owner ("" = everyone's).
func (s *Store) FinishedJobsOf(ctx context.Context, owner string, before int64, states []string, limit int) ([]string, error) {
	if len(states) == 0 {
		states = []string{"succeeded", "failed", "cancelled"}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := []any{before}
	for _, st := range states {
		args = append(args, st)
	}
	q := `SELECT id FROM jobs WHERE finished_at<>0 AND finished_at<? AND state IN (` + ph + `)`
	if owner != "" {
		q += ` AND owner=?`
		args = append(args, owner)
	}
	rows, err := s.query(ctx, q+` ORDER BY finished_at LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteJob deletes a job with its tasks, attempts and logs. The caller
// checks that it is finished and removes stored outputs.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Explicit deletes: SQLite's foreign keys may be off on old databases.
	for _, q := range []string{
		`DELETE FROM task_logs WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`,
		`DELETE FROM attempts WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`,
		`DELETE FROM tasks WHERE job_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, s.rebind(q), id); err != nil {
			return err
		}
	}
	r, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM jobs WHERE id = ?`), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Timelines
// ---------------------------------------------------------------------------

// TimelineRow is an attempt with its task (and job) identity.
type TimelineRow struct {
	Attempt
	TaskKey, JobID, JobName string
}

// JobAttempts returns every attempt of a job's tasks, by start time.
func (s *Store) JobAttempts(ctx context.Context, jobID string) ([]TimelineRow, error) {
	return s.timeline(ctx, `t.job_id = ?`, `a.started_at, a.task_id, a.attempt`, 5000, jobID)
}

// AgentAttempts returns a Mac's attempts, newest first.
func (s *Store) AgentAttempts(ctx context.Context, agentID string, limit int) ([]TimelineRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return s.timeline(ctx, `a.agent_id = ?`, `a.started_at DESC`, limit, agentID)
}

func (s *Store) timeline(ctx context.Context, where, order string, limit int, args ...any) ([]TimelineRow, error) {
	rows, err := s.query(ctx, `SELECT a.task_id, a.attempt, a.agent_id, a.started_at, a.finished_at, a.outcome, a.error,
		a.score, t.key, t.job_id, j.name
		FROM attempts a JOIN tasks t ON t.id = a.task_id JOIN jobs j ON j.id = t.job_id
		WHERE `+where+` ORDER BY `+order+` LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineRow
	for rows.Next() {
		var r TimelineRow
		if err := rows.Scan(&r.TaskID, &r.Attempt.Attempt, &r.AgentID, &r.StartedAt, &r.Finished, &r.Outcome, &r.Error,
			&r.Score, &r.TaskKey, &r.JobID, &r.JobName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountJobsByState returns global per-state job counts.
func (s *Store) CountJobsByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT state, COUNT(*) FROM jobs WHERE state IN ('queued','running') GROUP BY state`)
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
