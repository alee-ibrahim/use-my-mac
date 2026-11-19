package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testMultipart(t *testing.T, m Multipart, b Backend, put func(url string, data []byte) error) {
	t.Helper()
	ctx := context.Background()
	key := "uploads/big/file.bin"
	id, err := m.CreateMultipart(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	p1 := bytes.Repeat([]byte("a"), 10)
	p2 := bytes.Repeat([]byte("b"), 10)
	p3 := []byte("ccc")
	// Out of order: part 3 server-side, part 1 via its URL, part 2 later.
	if err := m.PutPart(ctx, key, id, 3, bytes.NewReader(p3), 3); err != nil {
		t.Fatal(err)
	}
	u1, err := m.PartURL(ctx, key, id, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := put(u1, p1); err != nil {
		t.Fatal(err)
	}
	parts, err := m.ListParts(ctx, key, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].N != 1 || parts[0].Size != 10 || parts[1].N != 3 || parts[1].Size != 3 {
		t.Fatalf("list parts: %+v", parts)
	}
	if err := m.PutPart(ctx, key, id, 2, bytes.NewReader(p2), 10); err != nil {
		t.Fatal(err)
	}
	parts, _ = m.ListParts(ctx, key, id)
	if err := m.CompleteMultipart(ctx, key, id, parts); err != nil {
		t.Fatal(err)
	}
	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if want := string(p1) + string(p2) + string(p3); string(got) != want {
		t.Fatalf("assembled %q, want %q", got, want)
	}
	if _, err := m.ListParts(ctx, key, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parts of a completed upload: %v", err)
	}

	// Abort drops everything.
	id2, err := m.CreateMultipart(ctx, "uploads/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutPart(ctx, "uploads/x", id2, 1, strings.NewReader("zz"), 2); err != nil {
		t.Fatal(err)
	}
	if err := m.AbortMultipart(ctx, "uploads/x", id2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListParts(ctx, "uploads/x", id2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parts after abort: %v", err)
	}
	if _, err := b.Get(ctx, "uploads/x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("aborted upload must not create the blob")
	}
}

func TestLocalMultipart(t *testing.T) {
	l, err := NewLocal(t.TempDir(), "https://router.example", []byte("0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	// Signed part URLs are served by the router: emulate its handler.
	put := func(raw string, data []byte) error {
		u, _ := url.Parse(raw)
		key, err := l.Verify("PUT", u.Query())
		if err != nil {
			return err
		}
		if part, live := l.IsPartKey(key); !part || !live {
			return fmt.Errorf("not a live part key: %s", key)
		}
		return l.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)))
	}
	testMultipart(t, l, l, put)

	if _, err := l.PartURL(context.Background(), "k", "../../etc", 1, time.Minute); err == nil {
		t.Fatal("bad upload id must fail")
	}
	if part, live := l.IsPartKey("_multipart/0123456789abcdef0123456789abcdef/00001"); !part || live {
		t.Fatalf("unknown upload: part=%v live=%v", part, live)
	}
	if part, _ := l.IsPartKey("uploads/a"); part {
		t.Fatal("normal key is not a part")
	}
}

// fakeS3MP adds multipart to a minimal path-style S3.
type fakeS3MP struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte // uploadId -> parts
	next    int
}

func (f *fakeS3MP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("X-Amz-Signature") == "" {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	_, isCreate := q["uploads"]
	uid := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && isCreate:
		f.next++
		id := "up" + strconv.Itoa(f.next)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && uid != "":
		parts, ok := f.uploads[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		b, _ := io.ReadAll(r.Body)
		parts[n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d-%d"`, n, len(b)))
	case r.Method == http.MethodGet && uid != "":
		parts, ok := f.uploads[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		marker, _ := strconv.Atoi(q.Get("part-number-marker"))
		var ns []int
		for n := range parts {
			if n > marker {
				ns = append(ns, n)
			}
		}
		sort.Ints(ns)
		trunc := len(ns) > 2 // page size 2 to exercise pagination
		if trunc {
			ns = ns[:2]
		}
		fmt.Fprint(w, `<ListPartsResult>`)
		for _, n := range ns {
			fmt.Fprintf(w, `<Part><PartNumber>%d</PartNumber><ETag>"etag-%d-%d"</ETag><Size>%d</Size></Part>`, n, n, len(parts[n]), len(parts[n]))
		}
		if trunc {
			fmt.Fprintf(w, `<IsTruncated>true</IsTruncated><NextPartNumberMarker>%d</NextPartNumberMarker>`, ns[len(ns)-1])
		}
		fmt.Fprint(w, `</ListPartsResult>`)
	case r.Method == http.MethodPost && uid != "":
		parts, ok := f.uploads[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var body s3Complete
		if err := xml.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var buf bytes.Buffer
		for _, p := range body.Parts {
			if want := fmt.Sprintf(`"etag-%d-%d"`, p.PartNumber, len(parts[p.PartNumber])); p.ETag != want {
				fmt.Fprintf(w, "<Error><Code>InvalidPart</Code></Error>")
				return
			}
			buf.Write(parts[p.PartNumber])
		}
		f.objs[path] = buf.Bytes()
		delete(f.uploads, uid)
		fmt.Fprint(w, `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && uid != "":
		delete(f.uploads, uid)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet:
		b, ok := f.objs[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func TestS3Multipart(t *testing.T) {
	f := &fakeS3MP{objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := &S3{Endpoint: srv.URL, Bucket: "b", AccessKey: "ak", SecretKey: "sk", PathStyle: true}
	put := func(u string, data []byte) error {
		if !strings.Contains(u, "partNumber=1") || !strings.Contains(u, "uploadId=") {
			return fmt.Errorf("part URL lacks part query: %s", u)
		}
		req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(data))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("put: %s", resp.Status)
		}
		return nil
	}
	testMultipart(t, s, s, put)
}

// The extra query parameters are part of the signature: changing one must
// change it.
func TestS3PresignSignsExtraQuery(t *testing.T) {
	s := &S3{Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "ak", SecretKey: "sk",
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	a, _ := s.PartURL(context.Background(), "k", "u1", 1, time.Hour)
	b, _ := s.PartURL(context.Background(), "k", "u1", 2, time.Hour)
	sig := func(u string) string { x, _ := url.Parse(u); return x.Query().Get("X-Amz-Signature") }
	if sig(a) == "" || sig(a) == sig(b) {
		t.Fatalf("signatures must differ per part: %s %s", a, b)
	}
	plain, _ := s.Presign("PUT", "k", time.Hour)
	if strings.Contains(plain, "partNumber") {
		t.Fatal("plain presign must not carry part params")
	}
}
