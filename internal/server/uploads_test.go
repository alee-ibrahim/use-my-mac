package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestUploadRoutes(t *testing.T) {
	e := newAuthEnv(t, false, true, false)
	viewer, _ := e.srv.E.CreateAPIKey(context.Background(), "viewer", api.RoleViewer)
	body := `{"key":"app/a.bin","size":12000000,"part_size":5242880}`
	if w := e.do("POST", "/v1/uploads", body, bearerAs(viewer.Secret)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer upload: %d", w.Code)
	}
	if w := e.do("POST", "/v1/uploads", `{"key":"../x","size":10}`, bearerAs(e.app)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/v1/uploads", body, bearerAs(e.app))
	var up api.Upload
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &up) != nil || up.Parts != 3 || up.Direct {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", up.URLs[1], "short", bearerAs(e.app)); w.Code != http.StatusBadRequest {
		t.Fatalf("short part: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/uploads/"+up.ID, "", bearerAs(e.admin)); w.Code != http.StatusOK {
		t.Fatalf("admin get: %d", w.Code)
	}
	if w := e.do("POST", "/v1/uploads/"+up.ID+"/complete", "", bearerAs(e.app)); w.Code != http.StatusBadRequest {
		t.Fatalf("incomplete: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/v1/uploads/"+up.ID, "", bearerAs(e.app)); w.Code != http.StatusNoContent {
		t.Fatalf("abort: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/uploads/"+up.ID, "", bearerAs(e.app)); w.Code != http.StatusNotFound {
		t.Fatalf("aborted upload: %d", w.Code)
	}
	// Progress is an agent endpoint.
	if w := e.do("POST", "/v1/agent/tasks/tsk_x/progress", `{"phase":"running"}`, bearerAs(e.app)); w.Code != http.StatusUnauthorized {
		t.Fatalf("app key on progress: %d", w.Code)
	}
}

// The jobs stream sends the list first, then each job as it changes.
func TestJobsStream(t *testing.T) {
	old := streamDebounce
	streamDebounce = 10 * time.Millisecond
	t.Cleanup(func() { streamDebounce = old })
	e := newAuthEnv(t, false, true, false)
	w := e.do("POST", "/v1/jobs", `{"tasks":[{"key":"t","runtime":"sh"}]}`, bearerAs(e.app))
	var job api.Job
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	ts := httptest.NewServer(e.h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/jobs/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.app)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() (string, string) {
		ev := ""
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				ev = l[7:]
			case strings.HasPrefix(l, "data: ") && ev != "":
				return ev, l[6:]
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return "", ""
	}
	if ev, data := next(); ev != "jobs" || !strings.Contains(data, job.ID) {
		t.Fatalf("first event %s %s", ev, data)
	}
	if w := e.do("POST", "/v1/jobs/"+job.ID+"/cancel", "", bearerAs(e.app)); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	ev, data := next()
	var j api.Job
	if ev != "job" || json.Unmarshal([]byte(data), &j) != nil || j.State != api.StateCancelled || j.Percent != 100 {
		t.Fatalf("change event %s %s", ev, data)
	}
}

// Direct downloads need the s3 backend: the local backend answers 400 with
// a clear message; viewers and bad ttls are refused first.
func TestDirectDownloadLocalBackend(t *testing.T) {
	e := newAuthEnv(t, false, true, false)
	viewer, _ := e.srv.E.CreateAPIKey(context.Background(), "viewer", api.RoleViewer)
	if w := e.do("PUT", "/v1/blobs/app/a.txt", "hello", bearerAs(e.app)); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "/v1/blobs/app/a.txt?direct=1", "", bearerAs(e.app))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "s3 storage backend") {
		t.Fatalf("local direct blob: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/blobs/app/a.txt?direct=1&ttl=10", "", bearerAs(e.app)); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "ttl") {
		t.Fatalf("short ttl: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/v1/blobs/app/a.txt", "", bearerAs(e.app)); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("relay still works: %d %s", w.Code, w.Body)
	}
	var me api.Me
	if w := e.do("GET", "/v1/me", "", bearerAs(e.app)); json.Unmarshal(w.Body.Bytes(), &me) != nil || me.Storage != "local" {
		t.Fatalf("me: %s", w.Body)
	}
	w = e.do("POST", "/v1/jobs", `{"tasks":[{"key":"t","runtime":"sh","outputs":[{"name":"o"}]}]}`, bearerAs(e.app))
	var job api.Job
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	full, _ := e.srv.E.Job(context.Background(), job.ID, true)
	path := "/v1/tasks/" + full.Tasks[0].ID + "/outputs/o"
	if w := e.do("GET", path+"?direct=1", "", bearerAs(viewer.Secret)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer direct: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", path+"?direct=1", "", bearerAs(e.app)); w.Code != http.StatusBadRequest {
		t.Fatalf("local direct output: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", path+"/downloaded", `{}`, bearerAs(viewer.Secret)); w.Code != http.StatusForbidden {
		t.Fatalf("viewer confirm: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", path+"/downloaded", `{}`, bearerAs(e.app)); w.Code != http.StatusNotFound {
		t.Fatalf("confirm before the output exists: %d %s", w.Code, w.Body)
	}
}
