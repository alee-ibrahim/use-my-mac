package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func bearerAs(key string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
}

// TestOpsAuthz: metrics and events are for viewers and admins; deletes and
// retention changes for admins only; apps get neither.
func TestOpsAuthz(t *testing.T) {
	e := newAuthEnv(t, false, true, false)
	ctx := context.Background()
	viewer, err := e.srv.E.CreateAPIKey(ctx, "viewer", api.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := e.srv.E.CreateAPIKey(ctx, "old", api.RoleApp)

	reads := []string{"/v1/metrics/summary", "/v1/metrics/fleet?range=6h", "/v1/metrics/fleet?range=30d&step=3600",
		"/v1/metrics/macs?window=1h", "/v1/events?kind=mac.,key.&limit=5", "/v1/retention"}
	for _, p := range reads {
		if w := e.do("GET", p, "", bearerAs(viewer.Secret)); w.Code != http.StatusOK {
			t.Fatalf("viewer GET %s: %d %s", p, w.Code, w.Body)
		}
		if w := e.do("GET", p, "", bearerAs(e.app)); w.Code != http.StatusForbidden {
			t.Fatalf("app GET %s: %d", p, w.Code)
		}
	}
	if w := e.do("GET", "/v1/metrics/fleet?range=bogus", "", bearerAs(viewer.Secret)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", w.Code)
	}
	if w := e.do("GET", "/v1/metrics/macs/mac_nope?range=1h", "", bearerAs(viewer.Secret)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown mac: %d", w.Code)
	}

	writes := []struct{ method, path, body string }{
		{"POST", "/v1/api-keys/" + old.ID + "/purge", ""},
		{"POST", "/v1/api-keys/purge", ""},
		{"POST", "/v1/install-tokens/purge", ""},
		{"DELETE", "/v1/macs/mac_x", ""},
		{"PUT", "/v1/retention", `{"jobs_days":30}`},
	}
	for _, wr := range writes {
		for _, key := range []string{viewer.Secret, e.app} {
			if w := e.do(wr.method, wr.path, wr.body, bearerAs(key)); w.Code != http.StatusForbidden {
				t.Fatalf("non-admin %s %s: %d", wr.method, wr.path, w.Code)
			}
		}
	}

	// Job deletes are for owners (apps) and admins, not viewers.
	if w := e.do("POST", "/v1/jobs/purge", `{"older_than_days":30}`, bearerAs(viewer.Secret)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer purge: %d", w.Code)
	}
	if w := e.do("DELETE", "/v1/jobs/job_x", "", bearerAs(viewer.Secret)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer delete job: %d", w.Code)
	}
	if w := e.do("POST", "/v1/jobs/purge", `{"older_than_days":30,"owner":"key:someone"}`, bearerAs(e.app)); w.Code != http.StatusForbidden {
		t.Fatalf("app purge of another owner: %d", w.Code)
	}
	if w := e.do("POST", "/v1/jobs/purge", `{"older_than_days":30}`, bearerAs(e.app)); w.Code != http.StatusOK {
		t.Fatalf("app purge of its own: %d", w.Code)
	}
	if w := e.do("DELETE", "/v1/jobs/job_x", "", bearerAs(e.app)); w.Code != http.StatusNotFound {
		t.Fatalf("app delete missing job: %d", w.Code)
	}

	// Admin: an active key is refused with 409, a revoked one deleted.
	if w := e.do("POST", "/v1/api-keys/"+old.ID+"/purge", "", bearerAs(e.admin)); w.Code != http.StatusConflict {
		t.Fatalf("active key purge: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/v1/api-keys/"+old.ID, "", bearerAs(e.admin)); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := e.do("POST", "/v1/api-keys/"+old.ID+"/purge", "", bearerAs(e.admin)); w.Code != http.StatusNoContent {
		t.Fatalf("purge revoked key: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/v1/jobs/purge", `{"older_than_days":30}`, bearerAs(e.admin)); w.Code != http.StatusOK {
		t.Fatalf("purge jobs: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/v1/jobs/job_missing", "", bearerAs(e.admin)); w.Code != http.StatusNotFound {
		t.Fatalf("missing job: %d", w.Code)
	}
	if w := e.do("PUT", "/v1/retention", `{"metrics_raw_hours":1}`, bearerAs(e.admin)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad retention: %d", w.Code)
	}
	w := e.do("PUT", "/v1/retention", `{"jobs_days":30}`, bearerAs(e.admin))
	var ret api.Retention
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ret) != nil || ret.JobsDays != 30 || ret.MetricsRawHours != 24 {
		t.Fatalf("retention: %d %s", w.Code, w.Body)
	}

	// The audit log names the admin.
	w = e.do("GET", "/v1/events?kind=key.deleted", "", bearerAs(viewer.Secret))
	var evs []api.Event
	if json.Unmarshal(w.Body.Bytes(), &evs) != nil || len(evs) != 1 || evs[0].Actor == "" || evs[0].Target != old.ID {
		t.Fatalf("audit: %s", w.Body)
	}

	// Summary shape.
	w = e.do("GET", "/v1/metrics/summary", "", bearerAs(viewer.Secret))
	var sum api.Summary
	if json.Unmarshal(w.Body.Bytes(), &sum) != nil || sum.Macs == nil || sum.Last24h.SuccessRate != -1 {
		t.Fatalf("summary: %s", w.Body)
	}
}
