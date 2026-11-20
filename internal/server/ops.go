package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Operations API: metrics history for the dashboard, the activity/audit log,
// retention settings and admin cleanup of leftovers.

func (s *Server) opsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/metrics/summary", s.need(roleRead, s.metricsSummary))
	mux.HandleFunc("GET /v1/metrics/fleet", s.need(roleRead, s.metricsFleet))
	mux.HandleFunc("GET /v1/metrics/macs", s.need(roleRead, s.metricsSparks))
	mux.HandleFunc("GET /v1/metrics/macs/{id}", s.need(roleRead, s.metricsMac))
	mux.HandleFunc("GET /v1/macs/{id}/attempts", s.need(roleRead, s.macAttempts))
	mux.HandleFunc("GET /v1/jobs/{id}/timeline", s.need(roleAny, s.jobTimeline))
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.need(roleSubmit, s.deleteJob))
	mux.HandleFunc("POST /v1/jobs/purge", s.need(roleSubmit, s.purgeJobs))
	mux.HandleFunc("POST /v1/api-keys/{id}/purge", s.need(roleAdmin, s.deleteKey))
	mux.HandleFunc("POST /v1/api-keys/purge", s.need(roleAdmin, s.purgeKeys))
	mux.HandleFunc("POST /v1/install-tokens/{id}/purge", s.need(roleAdmin, s.deleteInstallToken))
	mux.HandleFunc("POST /v1/install-tokens/purge", s.need(roleAdmin, s.purgeInstallTokens))
	mux.HandleFunc("GET /v1/events", s.need(roleRead, s.listEvents))
	mux.HandleFunc("GET /v1/retention", s.need(roleRead, s.getRetention))
	mux.HandleFunc("PUT /v1/retention", s.need(roleAdmin, s.putRetention))
}

// actor names the caller in the audit log.
func actor(r *http.Request) string {
	id := who(r)
	if id.Name == "" || id.Name == id.Subject {
		return id.Subject
	}
	return id.Name + " (" + id.Subject + ")"
}

// parseTime accepts RFC 3339 or Unix milliseconds.
func parseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(n), true
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

// parseDur accepts Go durations plus a "d" (days) suffix.
func parseDur(v string) (time.Duration, bool) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		d, err := strconv.Atoi(n)
		return time.Duration(d) * 24 * time.Hour, err == nil && d > 0
	}
	d, err := time.ParseDuration(v)
	return d, err == nil && d > 0
}

// rangeParams reads from/to (RFC 3339 or Unix ms), or range=1h|24h|7d|… back
// from now, and step (seconds).
func (s *Server) rangeParams(w http.ResponseWriter, r *http.Request) (from, to time.Time, step int, ok bool) {
	q := r.URL.Query()
	from, ok1 := parseTime(q.Get("from"))
	to, ok2 := parseTime(q.Get("to"))
	if !ok1 || !ok2 {
		writeErr(w, http.StatusBadRequest, "from/to must be RFC 3339 or Unix milliseconds")
		return
	}
	if v := q.Get("range"); v != "" {
		d, good := parseDur(v)
		if !good {
			writeErr(w, http.StatusBadRequest, "range must look like 1h, 24h or 7d")
			return
		}
		if to.IsZero() {
			to = s.Now()
		}
		from = to.Add(-d)
	}
	if v := q.Get("step"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "step must be seconds")
			return
		}
		step = n
	}
	return from, to, step, true
}

func (s *Server) metricsSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.E.Summary(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) metricsFleet(w http.ResponseWriter, r *http.Request) {
	from, to, step, ok := s.rangeParams(w, r)
	if !ok {
		return
	}
	ser, err := s.E.FleetSeries(r.Context(), from, to, step)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ser)
}

func (s *Server) metricsMac(w http.ResponseWriter, r *http.Request) {
	from, to, step, ok := s.rangeParams(w, r)
	if !ok {
		return
	}
	ser, err := s.E.MacSeries(r.Context(), r.PathValue("id"), from, to, step)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ser)
}

func (s *Server) metricsSparks(w http.ResponseWriter, r *http.Request) {
	var win time.Duration
	if v := r.URL.Query().Get("window"); v != "" {
		d, ok := parseDur(v)
		if !ok {
			writeErr(w, http.StatusBadRequest, "window must look like 1h")
			return
		}
		win = d
	}
	step, _ := strconv.Atoi(r.URL.Query().Get("step"))
	sp, err := s.E.MacSparks(r.Context(), win, step)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (s *Server) macAttempts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	as, err := s.E.MacAttempts(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, as)
}

func (s *Server) jobTimeline(w http.ResponseWriter, r *http.Request) {
	if s.loadJob(w, r, r.PathValue("id"), false) == nil {
		return
	}
	tl, err := s.E.JobTimeline(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

// deleteJob deletes a job for its owner or an admin (cancelling it first if
// it still runs). ?inputs=true also deletes the owner's input files.
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	inputs := r.URL.Query().Get("inputs") == "true" || r.URL.Query().Get("inputs") == "1"
	if err := s.E.RemoveJob(r.Context(), caller(r), r.PathValue("id"), inputs); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) purgeJobs(w http.ResponseWriter, r *http.Request) {
	var req api.PurgeJobsRequest
	if !readJSON(w, r, &req, 4096) {
		return
	}
	if id := who(r); id.Role != api.RoleAdmin {
		if req.Owner != "" && req.Owner != id.Subject {
			writeErr(w, http.StatusForbidden, "apps can only purge their own jobs")
			return
		}
		req.Owner = id.Subject
	}
	n, err := s.E.PurgeJobs(r.Context(), req, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.PurgeResult{Deleted: n})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.E.DeleteAPIKey(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) purgeKeys(w http.ResponseWriter, r *http.Request) {
	n, err := s.E.PurgeAPIKeys(r.Context(), actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.PurgeResult{Deleted: n})
}

func (s *Server) deleteInstallToken(w http.ResponseWriter, r *http.Request) {
	if err := s.E.DeleteInstallToken(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) purgeInstallTokens(w http.ResponseWriter, r *http.Request) {
	n, err := s.E.PurgeInstallTokens(r.Context(), actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.PurgeResult{Deleted: n})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, ok := parseTime(q.Get("before"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "before must be RFC 3339 or Unix milliseconds")
		return
	}
	var kinds []string
	for _, k := range strings.Split(q.Get("kind"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds = append(kinds, k)
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	evs, err := s.E.Events(r.Context(), kinds, q.Get("target"), before, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) getRetention(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.E.Retention())
}

func (s *Server) putRetention(w http.ResponseWriter, r *http.Request) {
	var req api.Retention
	if !readJSON(w, r, &req, 4096) {
		return
	}
	ret, err := s.E.SetRetention(r.Context(), req, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ret)
}
