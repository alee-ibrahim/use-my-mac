package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Server-sent event streams of job changes. The engine publishes the ID of
// every job that changed (task state or live progress); a stream reloads it
// (debounced) and sends it only when its JSON changed. Streams also resync
// on a timer, because notifications are dropped rather than ever blocking
// the engine. Clients that cannot stream fall back to polling the REST API.
//
//	GET /v1/jobs/{id}/stream   events: job (with tasks), removed, end
//	GET /v1/jobs/stream        events: jobs (initial list), job, removed

// Stream timings (variables so tests can shorten them).
var (
	streamDebounce  = 250 * time.Millisecond
	streamResync    = 5 * time.Second
	streamKeepAlive = 15 * time.Second
)

type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx-style proxies: do not buffer
	w.WriteHeader(http.StatusOK)
	s := &sse{w: w, rc: http.NewResponseController(w)}
	fmt.Fprint(w, "retry: 3000\n\n")
	s.flush()
	return s
}

func (s *sse) flush() bool { return s.rc.Flush() == nil }

func (s *sse) event(name string, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return s.raw(name, b)
}

func (s *sse) raw(name string, data []byte) bool {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return false
	}
	return s.flush()
}

func (s *sse) ping() bool {
	if _, err := fmt.Fprint(s.w, ": ping\n\n"); err != nil {
		return false
	}
	return s.flush()
}

func stoppedTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

func (s *Server) streamJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j := s.loadJob(w, r, id, true)
	if j == nil {
		return
	}
	sub, cancel := s.E.Subscribe()
	defer cancel()
	out := newSSE(w)
	var last []byte
	// send returns false when the stream should end.
	send := func(j *api.Job) bool {
		b, _ := json.Marshal(j)
		if string(b) != string(last) {
			last = b
			if !out.raw("job", b) {
				return false
			}
		}
		if api.IsTerminal(j.State) {
			out.raw("end", []byte(`{"id":`+strconv.Quote(j.ID)+`}`))
			return false
		}
		return true
	}
	if !send(j) {
		return
	}
	debounce := stoppedTimer()
	resync := time.NewTicker(streamResync)
	keep := time.NewTicker(streamKeepAlive)
	defer resync.Stop()
	defer keep.Stop()
	pending := false
	reload := func() bool {
		j, err := s.E.Job(r.Context(), id, true)
		if errors.Is(err, engine.ErrNotFound) {
			out.event("removed", map[string]string{"id": id})
			return false
		}
		if err != nil {
			return r.Context().Err() == nil
		}
		return send(j)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case jid := <-sub:
			if jid == id && !pending {
				pending = true
				debounce.Reset(streamDebounce)
			}
		case <-debounce.C:
			pending = false
			if !reload() {
				return
			}
		case <-resync.C:
			if !reload() {
				return
			}
		case <-keep.C:
			if !out.ping() {
				return
			}
		}
	}
}

func (s *Server) streamJobs(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	owner := ""
	if !roleRead(id.Role) {
		owner = id.Subject
	}
	state := r.URL.Query().Get("state")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx := r.Context()
	list := func() ([]*api.Job, error) { return s.E.Jobs(ctx, state, owner, limit) }
	js, err := list()
	if err != nil {
		s.fail(w, err)
		return
	}
	sub, cancel := s.E.Subscribe()
	defer cancel()
	out := newSSE(w)
	sent := map[string]string{} // job ID -> last JSON sent
	// snapshot sends the full list when it changed (always the first time).
	first := true
	snapshot := func(js []*api.Job) bool {
		b, _ := json.Marshal(js)
		next := map[string]string{}
		for _, j := range js {
			jb, _ := json.Marshal(j)
			next[j.ID] = string(jb)
		}
		same := len(next) == len(sent)
		for k, v := range next {
			if sent[k] != v {
				same = false
			}
		}
		sent = next
		if same && !first {
			return true
		}
		first = false
		return out.raw("jobs", b)
	}
	if !snapshot(js) {
		return
	}
	debounce := stoppedTimer()
	resync := time.NewTicker(6 * streamResync)
	keep := time.NewTicker(streamKeepAlive)
	defer resync.Stop()
	defer keep.Stop()
	changed := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case jid := <-sub:
			if len(changed) == 0 {
				debounce.Reset(2 * streamDebounce)
			}
			changed[jid] = true
		case <-debounce.C:
			for jid := range changed {
				j, err := s.E.Job(ctx, jid, false)
				switch {
				case errors.Is(err, engine.ErrNotFound) || (err == nil && (owner != "" && j.Owner != owner || state != "" && j.State != state)):
					if _, ok := sent[jid]; ok {
						delete(sent, jid)
						if !out.event("removed", map[string]string{"id": jid}) {
							return
						}
					}
				case err != nil:
					if ctx.Err() != nil {
						return
					}
				default:
					b, _ := json.Marshal(j)
					if sent[jid] != string(b) {
						sent[jid] = string(b)
						if !out.raw("job", b) {
							return
						}
					}
				}
			}
			clear(changed)
		case <-resync.C:
			js, err := list()
			if err == nil && !snapshot(js) {
				return
			}
		case <-keep.C:
			if !out.ping() {
				return
			}
		}
	}
}
