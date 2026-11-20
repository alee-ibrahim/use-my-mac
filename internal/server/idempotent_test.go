package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// TestIdempotencyKeyHeader: the header sets the job's client_id; a repeat
// returns 200 with the same job instead of 201 with a new one.
func TestIdempotencyKeyHeader(t *testing.T) {
	e := newAuthEnv(t, false, true, false)
	withKey := func(k string) func(*http.Request) {
		return func(r *http.Request) {
			bearerAs(e.app)(r)
			r.Header.Set("Idempotency-Key", k)
		}
	}
	spec := `{"name":"n","tasks":[{"key":"t","runtime":"sh"}]}`
	var a, b api.Job
	w := e.do("POST", "/v1/jobs", spec, withKey("batch-7"))
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.ClientID != "batch-7" {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/v1/jobs", spec, withKey("batch-7"))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &b) != nil || b.ID != a.ID || !b.Existing {
		t.Fatalf("repeat: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/v1/jobs", `{"name":"other","tasks":[{"key":"t","runtime":"sh"}]}`, withKey("batch-7")); w.Code != http.StatusConflict {
		t.Fatalf("same key, other spec: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/v1/jobs", `{"client_id":"x","tasks":[{"key":"t","runtime":"sh"}]}`, withKey("y")); w.Code != http.StatusBadRequest {
		t.Fatalf("header and body differ: %d", w.Code)
	}
}
