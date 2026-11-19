package api

import "time"

// ---------------------------------------------------------------------------
// Metrics history, events, retention and cleanup (dashboard and admin API)
// ---------------------------------------------------------------------------

// Fleet series fields (gauges are averaged over a bucket, counters summed).
//
// Gauges: macs_online, macs_busy, macs_idle, macs_yielded, macs_draining,
// macs_offline, tasks_running, tasks_queued.
// Counters: assigned, succeeded, failed, yielded, lost, timeout, cancelled,
// task_seconds (time tasks spent running on Macs).
//
// Mac series fields (averages): user_cpu, our_cpu, gpu, mem_used (percent),
// running (tasks), online (0..1), yielded (0..1); counter: task_seconds.

// Series is a time series answer of GET /v1/metrics/fleet and
// /v1/metrics/macs/{id}. Points are bucket starts (Unix ms, field "t"); a
// bucket without data has only "t".
type Series struct {
	From   time.Time            `json:"from"`
	To     time.Time            `json:"to"`
	Step   int                  `json:"step_seconds"`
	Res    int                  `json:"resolution_seconds"`
	Points []map[string]float64 `json:"points"`
}

// MacSparks is GET /v1/metrics/macs: a short series per Mac for sparklines.
type MacSparks struct {
	From time.Time                       `json:"from"`
	To   time.Time                       `json:"to"`
	Step int                             `json:"step_seconds"`
	Macs map[string][]map[string]float64 `json:"macs"`
}

// Window sums counters over a period.
type Window struct {
	Succeeded    int64   `json:"succeeded"`
	Failed       int64   `json:"failed"`
	Timeout      int64   `json:"timeout"`
	Yielded      int64   `json:"yielded"`
	Lost         int64   `json:"lost"`
	Cancelled    int64   `json:"cancelled"`
	Assigned     int64   `json:"assigned"`
	ComputeHours float64 `json:"compute_hours"`
	// SuccessRate is succeeded / (succeeded + failed + timeout), -1 when
	// nothing finished.
	SuccessRate float64 `json:"success_rate"`
}

// Summary is GET /v1/metrics/summary: what the Overview's tiles show.
type Summary struct {
	At           time.Time      `json:"at"`
	Macs         map[string]int `json:"macs"`  // total, online, busy, idle, yielded, draining, offline
	Tasks        map[string]int `json:"tasks"` // running, queued (pending+assigned), blocked
	Jobs         map[string]int `json:"jobs"`  // queued, running
	Last24h      Window         `json:"last_24h"`
	Last7d       Window         `json:"last_7d"`
	RouterVer    string         `json:"router_version"`
	HistoryStart *time.Time     `json:"history_start,omitempty"`
}

// Event is one entry of the activity / audit log (GET /v1/events).
type Event struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor,omitempty"`
	Target  string    `json:"target,omitempty"`
	Message string    `json:"message"`
}

// Event kinds.
const (
	EventMacEnrolled    = "mac.enrolled"
	EventMacDeleted     = "mac.deleted"
	EventMacQuarantined = "mac.quarantined"
	EventMacDrain       = "mac.drain"
	EventTaskYielded    = "task.yielded"
	EventJobFinished    = "job.finished"
	EventJobDeleted     = "job.deleted"
	EventJobsPurged     = "jobs.purged"
	EventKeyCreated     = "key.created"
	EventKeyRevoked     = "key.revoked"
	EventKeyDeleted     = "key.deleted"
	EventInstallDeleted = "install_token.deleted"
	EventUpdate         = "mac.update"
	EventRetention      = "retention.changed"
	// EventRuntimeUnavailable: a task could not execute a runtime on a Mac.
	EventRuntimeUnavailable = "mac.runtime_unavailable"
	// EventMacFault: an attempt failed because of the Mac (disk full,
	// unhealthy), not the task.
	EventMacFault = "mac.fault"
	// Managed runtime catalog changes (publish, remove, prewarm).
	EventRuntimeCatalog = "runtime.catalog"
)

// Retention controls background pruning. Zero means "keep forever" for
// JobsDays; the others fall back to their defaults when zero.
type Retention struct {
	// JobsDays deletes finished jobs (with tasks, logs and stored outputs)
	// this many days after they finished. 0 keeps them.
	JobsDays int `json:"jobs_days" yaml:"jobs_days"`
	// EventsDays keeps the activity/audit log this long (default 90).
	EventsDays int `json:"events_days" yaml:"events_days"`
	// MetricsRawHours keeps 1-minute metrics this long (default 24, max 168).
	MetricsRawHours int `json:"metrics_raw_hours" yaml:"metrics_raw_hours"`
	// MetricsRollupDays keeps 15-minute metrics this long (default 30).
	MetricsRollupDays int `json:"metrics_rollup_days" yaml:"metrics_rollup_days"`
}

// PurgeJobsRequest is POST /v1/jobs/purge.
type PurgeJobsRequest struct {
	// OlderThanDays deletes finished jobs that finished at least this many
	// days ago (0 = every finished job).
	OlderThanDays int `json:"older_than_days"`
	// States limits the purge (succeeded, failed, cancelled); empty = all
	// finished states.
	States []string `json:"states,omitempty"`
}

// PurgeResult reports how many records a cleanup removed.
type PurgeResult struct {
	Deleted int `json:"deleted"`
}

// TimelineAttempt is one attempt in a job timeline.
type TimelineAttempt struct {
	TaskID     string     `json:"task_id"`
	TaskKey    string     `json:"task_key"`
	Attempt    int        `json:"attempt"`
	AgentID    string     `json:"agent_id"`
	AgentName  string     `json:"agent_name,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Outcome    string     `json:"outcome"`
	Error      string     `json:"error,omitempty"`
	Score      float64    `json:"score"`
}

// MacAttempt is one attempt in a Mac's task history.
type MacAttempt struct {
	TimelineAttempt
	JobID   string `json:"job_id"`
	JobName string `json:"job_name"`
}
