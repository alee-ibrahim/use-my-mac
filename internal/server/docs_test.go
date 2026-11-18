package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsArePublic(t *testing.T) {
	h := (&Server{}).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index: %d", rec.Code)
	}
	var idx []struct{ Slug, Title, Path string }
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil || len(idx) == 0 {
		t.Fatalf("index: %v %s", err, rec.Body)
	}
	if idx[0].Slug != "overview" || idx[0].Path != "README.md" {
		t.Errorf("first doc: %+v", idx[0])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/docs/usage", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("usage: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "# Using use-my-mac") {
		t.Error("usage page content missing")
	}

	for _, p := range []string{"/v1/docs/nope", "/v1/docs/..%2Fgo.mod"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
