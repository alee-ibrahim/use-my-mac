package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidKey(t *testing.T) {
	good := []string{"a", "jobs/j1/t1/out.mp4", "in-1_2.bin"}
	bad := []string{"", "/abs", "a/../b", "..", "a//b", "a/", "a b", `a\b`, "./a"}
	for _, k := range good {
		if ValidKey(k) != nil {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range bad {
		if ValidKey(k) == nil {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func TestLocalRoundTripAndSignedURLs(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	l, err := NewLocal(dir, "https://router.example/", []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	l.Now = func() time.Time { return now }
	ctx := context.Background()

	if err := l.Put(ctx, "jobs/j/t/out.txt", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	rc, err := l.Get(ctx, "jobs/j/t/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	if _, err := l.Get(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := l.Put(ctx, "../escape", strings.NewReader("x"), 1); err != ErrBadKey {
		t.Fatalf("want ErrBadKey, got %v", err)
	}

	raw, _ := l.DownloadURL(ctx, "jobs/j/t/out.txt", time.Minute)
	if !strings.HasPrefix(raw, "https://router.example"+SignedPath+"?") {
		t.Fatalf("bad url %s", raw)
	}
	u, _ := url.Parse(raw)
	if k, err := l.Verify("GET", u.Query()); err != nil || k != "jobs/j/t/out.txt" {
		t.Fatalf("verify: %v %v", k, err)
	}
	if _, err := l.Verify("PUT", u.Query()); err == nil {
		t.Fatal("method swap must fail")
	}
	q := u.Query()
	q.Set("k", "jobs/j/t/other.txt")
	if _, err := l.Verify("GET", q); err == nil {
		t.Fatal("key swap must fail")
	}
	now = now.Add(2 * time.Minute)
	if _, err := l.Verify("GET", u.Query()); err == nil {
		t.Fatal("expired url must fail")
	}
	if err := l.Delete(ctx, "jobs/j/t/out.txt"); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, "jobs/j/t/out.txt"); err != nil {
		t.Fatal("deleting a missing key is not an error")
	}
}

func TestNewLocalRejectsShortSecret(t *testing.T) {
	if _, err := NewLocal(t.TempDir(), "http://x", []byte("short")); err == nil {
		t.Fatal("want error")
	}
}

// AWS's documented SigV4 query-string example
// (https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html).
func TestS3PresignMatchesAWSVector(t *testing.T) {
	s := &S3{
		Endpoint:  "https://s3.amazonaws.com",
		Region:    "us-east-1",
		Bucket:    "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now:       func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	got, err := s.Presign("GET", "test.txt", 86400*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "X-Amz-Signature="+wantSig) {
		t.Fatalf("signature mismatch:\n%s", got)
	}
	if !strings.HasPrefix(got, "https://examplebucket.s3.amazonaws.com/test.txt?") {
		t.Fatalf("bad host/path: %s", got)
	}
}

// fakeS3 is a minimal path-style S3 that only accepts presigned requests.
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") == "" || q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[r.URL.Path] = b
	case http.MethodGet:
		b, ok := f.objs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case http.MethodDelete:
		delete(f.objs, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestS3RoundTripAgainstFake(t *testing.T) {
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk", PathStyle: true, Prefix: "umm/"}
	ctx := context.Background()
	if err := s.Put(ctx, "jobs/x/y", bytes.NewReader([]byte("data")), 4); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objs["/b/umm/jobs/x/y"]; !ok {
		t.Fatalf("object not stored at expected path: %v", f.objs)
	}
	rc, err := s.Get(ctx, "jobs/x/y")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "data" {
		t.Fatalf("got %q", b)
	}
	// Agents use presigned URLs directly.
	up, _ := s.UploadURL(ctx, "jobs/x/z", time.Minute)
	req, _ := http.NewRequest(http.MethodPut, up, strings.NewReader("agent"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("agent upload: %v %v", err, resp)
	}
	resp.Body.Close()
	if string(f.objs["/b/umm/jobs/x/z"]) != "agent" {
		t.Fatal("agent upload not stored")
	}
	if _, err := s.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound got %v", err)
	}
	if err := s.Delete(ctx, "jobs/x/y"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Presign("GET", "k", 8*24*time.Hour); err == nil {
		t.Fatal("ttl > 7d must fail")
	}
}
