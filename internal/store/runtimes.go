package store

import (
	"context"
	"database/sql"
	"errors"
)

// Runtime availability per agent: the allowlisted runtimes an agent cannot
// run (from its preflight) and the ones the router marked unavailable after
// an exec failure. Both are JSON so the shape can grow without migrations.

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS agent_runtimes (
			agent_id TEXT PRIMARY KEY,
			unavailable TEXT NOT NULL DEFAULT '[]',
			faults TEXT NOT NULL DEFAULT '{}',
			updated_at BIGINT NOT NULL DEFAULT 0
		)`,
	)
}

// AgentRuntimes is a row of agent_runtimes.
type AgentRuntimes struct {
	AgentID string
	// Unavailable is a JSON []api.UnavailableRuntime (what the dashboard
	// shows: the agent's preflight failures plus router faults).
	Unavailable string
	// Faults is the router's own JSON map runtime -> fault.
	Faults    string
	UpdatedAt int64
}

// PutAgentRuntimes upserts an agent's runtime availability.
func (s *Store) PutAgentRuntimes(ctx context.Context, r *AgentRuntimes) error {
	_, err := s.exec(ctx, `INSERT INTO agent_runtimes(agent_id, unavailable, faults, updated_at) VALUES(?,?,?,?)
		ON CONFLICT (agent_id) DO UPDATE SET unavailable = excluded.unavailable, faults = excluded.faults,
		updated_at = excluded.updated_at`, r.AgentID, r.Unavailable, r.Faults, r.UpdatedAt)
	return err
}

// GetAgentRuntimes returns ErrNotFound when nothing was stored yet.
func (s *Store) GetAgentRuntimes(ctx context.Context, agentID string) (*AgentRuntimes, error) {
	var r AgentRuntimes
	err := s.queryRow(ctx, `SELECT agent_id, unavailable, faults, updated_at FROM agent_runtimes WHERE agent_id = ?`,
		agentID).Scan(&r.AgentID, &r.Unavailable, &r.Faults, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// ListAgentRuntimes returns all rows by agent ID.
func (s *Store) ListAgentRuntimes(ctx context.Context) (map[string]*AgentRuntimes, error) {
	rows, err := s.query(ctx, `SELECT agent_id, unavailable, faults, updated_at FROM agent_runtimes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*AgentRuntimes{}
	for rows.Next() {
		var r AgentRuntimes
		if err := rows.Scan(&r.AgentID, &r.Unavailable, &r.Faults, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out[r.AgentID] = &r
	}
	return out, rows.Err()
}

// SetAgentRuntimeList replaces the agents.runtimes column (the runtimes the
// Mac can run now) between heartbeats.
func (s *Store) SetAgentRuntimeList(ctx context.Context, agentID, runtimes string) error {
	_, err := s.exec(ctx, `UPDATE agents SET runtimes=? WHERE id=?`, runtimes, agentID)
	return err
}
