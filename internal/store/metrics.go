package store

import (
	"context"
	"database/sql"
	"strings"
)

// Metrics history. The router samples the fleet once a minute into
// fleet_samples / mac_samples at resolution 60 s and rolls each sample's
// 15-minute bucket up into resolution 900 s. Gauges are averages and counters
// sums, so a rollup is AVG/SUM over the raw rows of its bucket. Old rows are
// pruned by resolution (see Engine.PruneHistory).

// Resolutions in seconds.
const (
	ResRaw    = 60
	ResRollup = 900
)

// FleetGauges are averaged when buckets are merged.
var FleetGauges = []string{"macs_online", "macs_busy", "macs_idle", "macs_yielded", "macs_draining",
	"macs_offline", "tasks_running", "tasks_queued"}

// FleetCounters are summed when buckets are merged.
var FleetCounters = []string{"assigned", "succeeded", "failed", "yielded", "lost", "timeout", "cancelled",
	"task_seconds"}

// MacGauges are averaged when buckets are merged.
var MacGauges = []string{"user_cpu", "our_cpu", "gpu", "mem_used", "running", "online", "yielded"}

// MacCounters are summed when buckets are merged.
var MacCounters = []string{"task_seconds"}

func cols(names []string, typ string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(",\n\t\t" + n + " " + typ + " NOT NULL DEFAULT 0")
	}
	return b.String()
}

func init() {
	schema = append(schema,
		`CREATE TABLE IF NOT EXISTS fleet_samples (
		res INTEGER NOT NULL,
		ts BIGINT NOT NULL,
		n INTEGER NOT NULL DEFAULT 1`+cols(FleetGauges, "DOUBLE PRECISION")+cols(FleetCounters, "DOUBLE PRECISION")+`,
		PRIMARY KEY (res, ts)
	)`,
		`CREATE TABLE IF NOT EXISTS mac_samples (
		agent_id TEXT NOT NULL,
		res INTEGER NOT NULL,
		ts BIGINT NOT NULL,
		n INTEGER NOT NULL DEFAULT 1`+cols(MacGauges, "DOUBLE PRECISION")+cols(MacCounters, "DOUBLE PRECISION")+`,
		PRIMARY KEY (agent_id, res, ts)
	)`,
		`CREATE INDEX IF NOT EXISTS mac_samples_res_ts ON mac_samples(res, ts)`,
		`CREATE INDEX IF NOT EXISTS attempts_agent ON attempts(agent_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS jobs_finished ON jobs(finished_at)`,
	)
}

// Sample is one row: values by field name.
type Sample map[string]float64

func upsertSQL(table string, keys []string, fields []string) string {
	all := append(append([]string{}, keys...), "n")
	all = append(all, fields...)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(all)), ",")
	var set []string
	for _, f := range append([]string{"n"}, fields...) {
		set = append(set, f+" = excluded."+f)
	}
	return `INSERT INTO ` + table + `(` + strings.Join(all, ", ") + `) VALUES(` + ph + `)
		ON CONFLICT (` + strings.Join(keys, ", ") + `) DO UPDATE SET ` + strings.Join(set, ", ")
}

func values(s Sample, fields []string) []any {
	out := make([]any, len(fields))
	for i, f := range fields {
		out[i] = s[f]
	}
	return out
}

var (
	fleetFields = append(append([]string{}, FleetGauges...), FleetCounters...)
	macFields   = append(append([]string{}, MacGauges...), MacCounters...)
)

// PutSamples writes one raw sample of the fleet and of each Mac in one
// transaction (replacing rows of the same bucket).
func (s *Store) PutSamples(ctx context.Context, ts int64, fleet Sample, macs map[string]Sample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args := append([]any{ResRaw, ts, 1}, values(fleet, fleetFields)...)
	if _, err := tx.ExecContext(ctx, s.rebind(upsertSQL("fleet_samples", []string{"res", "ts"}, fleetFields)), args...); err != nil {
		return err
	}
	q := s.rebind(upsertSQL("mac_samples", []string{"agent_id", "res", "ts"}, macFields))
	for id, m := range macs {
		args := append([]any{id, ResRaw, ts, 1}, values(m, macFields)...)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func aggExprs(gauges, counters []string) string {
	var parts []string
	for _, g := range gauges {
		parts = append(parts, "AVG("+g+")")
	}
	for _, c := range counters {
		parts = append(parts, "SUM("+c+")")
	}
	return strings.Join(parts, ", ")
}

// Rollup recomputes the rollup bucket [from, from+ResRollup s) from raw rows.
func (s *Store) Rollup(ctx context.Context, from int64) error {
	to := from + ResRollup*1000
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Fleet.
	row := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*), `+aggExprs(FleetGauges, FleetCounters)+
		` FROM fleet_samples WHERE res = ? AND ts >= ? AND ts < ?`), ResRaw, from, to)
	n, vals, err := scanAgg(row, len(fleetFields))
	if err != nil {
		return err
	}
	if n > 0 {
		args := append([]any{ResRollup, from, n}, vals...)
		if _, err := tx.ExecContext(ctx, s.rebind(upsertSQL("fleet_samples", []string{"res", "ts"}, fleetFields)), args...); err != nil {
			return err
		}
	}
	// Macs.
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT agent_id, COUNT(*), `+aggExprs(MacGauges, MacCounters)+
		` FROM mac_samples WHERE res = ? AND ts >= ? AND ts < ? GROUP BY agent_id`), ResRaw, from, to)
	if err != nil {
		return err
	}
	type macRow struct {
		id   string
		n    int
		vals []any
	}
	var mrows []macRow
	for rows.Next() {
		var id string
		var n int
		ns := make([]sql.NullFloat64, len(macFields))
		dst := []any{&id, &n}
		for i := range ns {
			dst = append(dst, &ns[i])
		}
		if err := rows.Scan(dst...); err != nil {
			rows.Close()
			return err
		}
		mrows = append(mrows, macRow{id, n, nullVals(ns)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	q := s.rebind(upsertSQL("mac_samples", []string{"agent_id", "res", "ts"}, macFields))
	for _, m := range mrows {
		args := append([]any{m.id, ResRollup, from, m.n}, m.vals...)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullVals(ns []sql.NullFloat64) []any {
	out := make([]any, len(ns))
	for i, v := range ns {
		out[i] = v.Float64
	}
	return out
}

func scanAgg(row *sql.Row, nfields int) (int, []any, error) {
	var n int
	ns := make([]sql.NullFloat64, nfields)
	dst := []any{&n}
	for i := range ns {
		dst = append(dst, &ns[i])
	}
	if err := row.Scan(dst...); err != nil {
		return 0, nil, err
	}
	return n, nullVals(ns), nil
}

// Point is one bucket of a queried series.
type Point struct {
	TS     int64
	Values map[string]float64
}

// FleetSeries returns fleet points of resolution res in [from, to), merged
// into buckets of step seconds (a multiple of res).
func (s *Store) FleetSeries(ctx context.Context, res int, from, to int64, step int) ([]Point, error) {
	return s.series(ctx, "fleet_samples", "", "", FleetGauges, FleetCounters, res, from, to, step)
}

// MacSeries is FleetSeries for one Mac.
func (s *Store) MacSeries(ctx context.Context, agentID string, res int, from, to int64, step int) ([]Point, error) {
	return s.series(ctx, "mac_samples", "agent_id", agentID, MacGauges, MacCounters, res, from, to, step)
}

func (s *Store) series(ctx context.Context, table, keyCol, key string, gauges, counters []string, res int,
	from, to int64, step int) ([]Point, error) {
	stepMS := int64(step) * 1000
	q := `SELECT (ts / ?) * ? AS b, ` + aggExprs(gauges, counters) + ` FROM ` + table + ` WHERE res = ? AND ts >= ? AND ts < ?`
	args := []any{stepMS, stepMS, res, from, to}
	if keyCol != "" {
		q += ` AND ` + keyCol + ` = ?`
		args = append(args, key)
	}
	q += ` GROUP BY b ORDER BY b`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fields := append(append([]string{}, gauges...), counters...)
	var out []Point
	for rows.Next() {
		var b int64
		ns := make([]sql.NullFloat64, len(fields))
		dst := []any{&b}
		for i := range ns {
			dst = append(dst, &ns[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		p := Point{TS: b, Values: make(map[string]float64, len(fields))}
		for i, f := range fields {
			p.Values[f] = ns[i].Float64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllMacSeries returns per-Mac points (for sparklines), keyed by agent ID.
func (s *Store) AllMacSeries(ctx context.Context, res int, from, to int64, step int) (map[string][]Point, error) {
	stepMS := int64(step) * 1000
	rows, err := s.query(ctx, `SELECT agent_id, (ts / ?) * ? AS b, `+aggExprs(MacGauges, MacCounters)+
		` FROM mac_samples WHERE res = ? AND ts >= ? AND ts < ? GROUP BY agent_id, b ORDER BY agent_id, b`,
		stepMS, stepMS, res, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Point{}
	for rows.Next() {
		var id string
		var b int64
		ns := make([]sql.NullFloat64, len(macFields))
		dst := []any{&id, &b}
		for i := range ns {
			dst = append(dst, &ns[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		p := Point{TS: b, Values: make(map[string]float64, len(macFields))}
		for i, f := range macFields {
			p.Values[f] = ns[i].Float64
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// FleetTotals sums the counters of resolution res in [from, to).
func (s *Store) FleetTotals(ctx context.Context, res int, from, to int64) (map[string]float64, error) {
	var parts []string
	for _, c := range FleetCounters {
		parts = append(parts, "SUM("+c+")")
	}
	ns := make([]sql.NullFloat64, len(FleetCounters))
	dst := make([]any, len(ns))
	for i := range ns {
		dst[i] = &ns[i]
	}
	if err := s.queryRow(ctx, `SELECT `+strings.Join(parts, ", ")+` FROM fleet_samples WHERE res = ? AND ts >= ? AND ts < ?`,
		res, from, to).Scan(dst...); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for i, c := range FleetCounters {
		out[c] = ns[i].Float64
	}
	return out, nil
}

// OldestSample returns the earliest sample time of a resolution (0 if none).
func (s *Store) OldestSample(ctx context.Context, res int) (int64, error) {
	var v sql.NullInt64
	err := s.queryRow(ctx, `SELECT MIN(ts) FROM fleet_samples WHERE res = ?`, res).Scan(&v)
	return v.Int64, err
}

// PruneSamples deletes rows of a resolution older than before (Unix ms).
func (s *Store) PruneSamples(ctx context.Context, res int, before int64) (int64, error) {
	var total int64
	for _, t := range []string{"fleet_samples", "mac_samples"} {
		r, err := s.exec(ctx, `DELETE FROM `+t+` WHERE res = ? AND ts < ?`, res, before)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
	}
	return total, nil
}

// DeleteMacSamples removes a Mac's history.
func (s *Store) DeleteMacSamples(ctx context.Context, agentID string) error {
	_, err := s.exec(ctx, `DELETE FROM mac_samples WHERE agent_id = ?`, agentID)
	return err
}
