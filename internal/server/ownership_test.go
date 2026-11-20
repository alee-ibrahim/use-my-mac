package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// TestOwnershipAuthz: over HTTP, an app key manages its own blobs, files
// and jobs; another app key gets 404/403; admins can act on anything;
// viewers can delete nothing; Mac labels and reservations are admin-only
// and apps only see whether a reservation is theirs.
func TestOwnershipAuthz(t *testing.T) {
	e := newAuthEnv(t, false, true, false)
	ctx := context.Background()
	other, _ := e.srv.E.CreateAPIKey(ctx, "other", api.RoleApp)
	viewer, _ := e.srv.E.CreateAPIKey(ctx, "viewer", api.RoleViewer)
	app, admin, oth, view := bearerAs(e.app), bearerAs(e.admin), bearerAs(other.Secret), bearerAs(viewer.Secret)

	if w := e.do("PUT", "/v1/blobs/mine/in.mov", "movie", app); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "/v1/blobs/mine/in.mov", "x", oth); w.Code != http.StatusForbidden {
		t.Fatalf("other overwrite: %d", w.Code)
	}
	if w := e.do("POST", "/v1/uploads", `{"key":"mine/in.mov","size":10}`, oth); w.Code != http.StatusForbidden {
		t.Fatalf("other multipart overwrite: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/blobs/mine/in.mov", "", oth); w.Code != http.StatusNotFound {
		t.Fatalf("other read: %d", w.Code)
	}
	if w := e.do("GET", "/v1/blobs/mine/in.mov", "", app); w.Code != http.StatusOK || w.Body.String() != "movie" {
		t.Fatalf("own read: %d", w.Code)
	}
	spec := `{"name":"e","tasks":[{"key":"t","runtime":"sh","inputs":[{"name":"i","blob":"mine/in.mov"}],"outputs":[{"name":"o"}]}]}`
	if w := e.do("POST", "/v1/jobs", spec, oth); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "another owner") {
		t.Fatalf("other uses input: %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/v1/jobs", spec, app)
	var job api.Job
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}

	// Files.
	var files []api.StoredFile
	w = e.do("GET", "/v1/files", "", app)
	if json.Unmarshal(w.Body.Bytes(), &files) != nil || len(files) != 1 || files[0].Blob != "mine/in.mov" || !files[0].InUse {
		t.Fatalf("files: %s", w.Body)
	}
	if w := e.do("GET", "/v1/files", "", oth); w.Body.String() != "[]\n" {
		t.Fatalf("other's files: %s", w.Body)
	}
	if w := e.do("GET", "/v1/files?owner=*", "", oth); w.Code != http.StatusForbidden {
		t.Fatalf("app listing everyone: %d", w.Code)
	}
	if w := e.do("GET", "/v1/files?owner=*", "", admin); !strings.Contains(w.Body.String(), "mine/in.mov") {
		t.Fatalf("admin listing everyone: %s", w.Body)
	}
	if w := e.do("GET", "/v1/files", "", view); w.Code != http.StatusForbidden {
		t.Fatalf("viewer files: %d", w.Code)
	}
	if w := e.do("DELETE", "/v1/files/mine/in.mov", "", app); w.Code != http.StatusConflict {
		t.Fatalf("delete in use: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/v1/files/mine/in.mov", "", oth); w.Code != http.StatusNotFound {
		t.Fatalf("other delete: %d", w.Code)
	}

	// Jobs: another app cannot see or delete it; a viewer cannot delete it;
	// the owner deletes it while queued (cancelled first), with its input.
	for _, c := range []struct {
		mod  func(*http.Request)
		code int
	}{{oth, http.StatusNotFound}, {view, http.StatusForbidden}} {
		if w := e.do("DELETE", "/v1/jobs/"+job.ID, "", c.mod); w.Code != c.code {
			t.Fatalf("delete job: %d want %d", w.Code, c.code)
		}
	}
	if w := e.do("DELETE", "/v1/jobs/"+job.ID+"?inputs=true", "", app); w.Code != http.StatusNoContent {
		t.Fatalf("owner delete: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/files", "", app); w.Body.String() != "[]\n" {
		t.Fatalf("files after delete: %s", w.Body)
	}
	if w := e.do("GET", "/v1/jobs/"+job.ID, "", app); w.Code != http.StatusNotFound {
		t.Fatalf("job after delete: %d", w.Code)
	}

	// Admins delete anyone's job and files.
	e.do("PUT", "/v1/blobs/o/1", "1", oth)
	w = e.do("POST", "/v1/jobs", `{"tasks":[{"key":"t","runtime":"sh"}]}`, oth)
	json.Unmarshal(w.Body.Bytes(), &job)
	if w := e.do("DELETE", "/v1/jobs/"+job.ID, "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("admin delete: %d", w.Code)
	}
	var res api.DeleteFilesResult
	w = e.do("POST", "/v1/files/delete", `{"all":true,"owner":"key:`+other.ID+`"}`, admin)
	if json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Deleted != 1 {
		t.Fatalf("admin bulk delete: %s", w.Body)
	}
	if w := e.do("POST", "/v1/files/delete", `{"all":true,"owner":"*"}`, oth); w.Code != http.StatusForbidden {
		t.Fatalf("app bulk delete of everyone: %d", w.Code)
	}

	// Macs: labels, reservations and max_tasks are admin-only.
	tok, _ := e.srv.E.CreateEnrolToken(ctx, "t", time.Hour)
	enr, err := e.srv.E.Enrol(ctx, api.EnrolRequest{Token: tok.Token, Name: "studio", Runtimes: []string{"sh"}, MaxTasks: 1})
	if err != nil {
		t.Fatal(err)
	}
	mac := "/v1/macs/" + enr.AgentID
	keys, _ := e.srv.E.APIKeys(ctx)
	appID := ""
	for _, k := range keys {
		if k.Name == "app" {
			appID = k.ID
		}
	}
	writes := []struct{ method, path, body string }{
		{"PUT", mac + "/labels", `{"labels":{"class":"studio"}}`},
		{"PUT", mac + "/reservation", `{"owners":["` + appID + `"],"hours":2,"note":"bulk"}`},
		{"PATCH", mac, `{"max_tasks":2}`},
	}
	for _, wr := range writes {
		for _, k := range []func(*http.Request){app, view} {
			if w := e.do(wr.method, wr.path, wr.body, k); w.Code != http.StatusForbidden {
				t.Fatalf("non-admin %s %s: %d", wr.method, wr.path, w.Code)
			}
		}
		if w := e.do(wr.method, wr.path, wr.body, admin); w.Code != http.StatusNoContent {
			t.Fatalf("admin %s %s: %d %s", wr.method, wr.path, w.Code, w.Body)
		}
	}
	seen := func(mod func(*http.Request)) api.Mac {
		var ms []api.Mac
		json.Unmarshal(e.do("GET", "/v1/macs", "", mod).Body.Bytes(), &ms)
		return ms[0]
	}
	if m := seen(admin); m.Reservation == nil || len(m.Reservation.Owners) != 1 || m.Reservation.Mine || m.Labels["class"] != "studio" ||
		m.MaxTasksSet != 2 || m.Reservation.OwnerNames["key:"+appID] != "app" {
		t.Fatalf("admin view: %+v %+v", m, m.Reservation)
	}
	if m := seen(app); m.Reservation == nil || !m.Reservation.Mine || m.Reservation.Owners != nil {
		t.Fatalf("owner view: %+v", m.Reservation)
	}
	if m := seen(oth); m.Reservation == nil || m.Reservation.Mine || m.Reservation.Owners != nil || m.Reservation.By != "" {
		t.Fatalf("other view: %+v", m.Reservation)
	}
	pin := `{"macs":["studio"],"tasks":[{"key":"t","runtime":"sh"}]}`
	if w := e.do("POST", "/v1/jobs", pin, oth); w.Code != http.StatusForbidden {
		t.Fatalf("other pins reserved Mac: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/v1/jobs", pin, app); w.Code != http.StatusCreated {
		t.Fatalf("owner pins reserved Mac: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", mac+"/reservation", "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("unreserve: %d", w.Code)
	}
	if m := seen(admin); m.Reservation != nil {
		t.Fatal("reservation must be gone")
	}
	var evs []api.Event
	json.Unmarshal(e.do("GET", "/v1/events?kind=mac.,job.deleted,files.", "", view).Body.Bytes(), &evs)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	if kinds[api.EventMacLabels] != 1 || kinds[api.EventMacReserved] != 2 || kinds[api.EventMacMaxTasks] != 1 ||
		kinds[api.EventJobDeleted] != 2 || kinds[api.EventFilesDeleted] != 1 {
		t.Fatalf("audit kinds: %v", kinds)
	}
}
