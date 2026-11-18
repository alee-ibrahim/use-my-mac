// Package metrics defines the Prometheus metrics exported at /metrics.
// All methods are nil-safe so tests can run without metrics.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// M holds the router's metrics.
type M struct {
	Registry    *prometheus.Registry
	Assignments *prometheus.CounterVec // by agent
	Outcomes    *prometheus.CounterVec // by outcome
	Duration    prometheus.Histogram
	TickSeconds prometheus.Histogram
	Agents      *prometheus.GaugeVec // by state
	Tasks       *prometheus.GaugeVec // by state
	Telemetry   *prometheus.GaugeVec // by agent, signal
}

// New registers all metrics on a fresh registry.
func New() *M {
	r := prometheus.NewRegistry()
	m := &M{
		Registry: r,
		Assignments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "umm_task_assignments_total", Help: "Tasks assigned to Macs.",
		}, []string{"agent"}),
		Outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "umm_task_attempt_outcomes_total", Help: "Finished task attempts by outcome (succeeded, failed, yielded, lost, timeout, cancelled).",
		}, []string{"outcome"}),
		Duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "umm_task_attempt_duration_seconds", Help: "Duration of task attempts.",
			Buckets: prometheus.ExponentialBuckets(1, 2.5, 12),
		}),
		TickSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "umm_scheduler_tick_seconds", Help: "Scheduler tick latency.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
		}),
		Agents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "umm_agents", Help: "Macs by state (ready, yielding, cooldown, draining, offline).",
		}, []string{"state"}),
		Tasks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "umm_tasks", Help: "Tasks by state.",
		}, []string{"state"}),
		Telemetry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "umm_agent_telemetry", Help: "Latest telemetry per Mac (user_cpu, our_cpu, gpu, mem_available_bytes, idle_seconds).",
		}, []string{"agent", "signal"}),
	}
	r.MustRegister(m.Assignments, m.Outcomes, m.Duration, m.TickSeconds, m.Agents, m.Tasks, m.Telemetry,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

// Assigned counts an assignment.
func (m *M) Assigned(agent string) {
	if m != nil {
		m.Assignments.WithLabelValues(agent).Inc()
	}
}

// Outcome counts a finished attempt.
func (m *M) Outcome(outcome string, seconds float64) {
	if m != nil {
		m.Outcomes.WithLabelValues(outcome).Inc()
		if seconds > 0 {
			m.Duration.Observe(seconds)
		}
	}
}

// Tick records scheduler latency.
func (m *M) Tick(seconds float64) {
	if m != nil {
		m.TickSeconds.Observe(seconds)
	}
}

// SetAgents replaces the agent-state gauge.
func (m *M) SetAgents(counts map[string]int) {
	if m == nil {
		return
	}
	m.Agents.Reset()
	for k, v := range counts {
		m.Agents.WithLabelValues(k).Set(float64(v))
	}
}

// SetTasks replaces the task-state gauge.
func (m *M) SetTasks(counts map[string]int) {
	if m == nil {
		return
	}
	m.Tasks.Reset()
	for k, v := range counts {
		m.Tasks.WithLabelValues(k).Set(float64(v))
	}
}

// SetTelemetry records a Mac's latest signals.
func (m *M) SetTelemetry(agent string, signals map[string]float64) {
	if m == nil {
		return
	}
	for k, v := range signals {
		m.Telemetry.WithLabelValues(agent, k).Set(v)
	}
}
