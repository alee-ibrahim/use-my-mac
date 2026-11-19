package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func data(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestUploadPartsParallelWithRetries(t *testing.T) {
	src := data(10*1024 + 17)
	var mu sync.Mutex
	got := map[int][]byte{}
	var calls, inflight, peak atomic.Int32
	failed := map[int]bool{}
	m := NewMeter(int64(len(src)))
	err := UploadParts(context.Background(), bytes.NewReader(src), int64(len(src)), 1024, 3, 2, map[int]bool{5: true}, m,
		func(ctx context.Context, n, try int, r io.Reader, size int64) error {
			calls.Add(1)
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			b, _ := io.ReadAll(r)
			if int64(len(b)) != size {
				t.Errorf("part %d: read %d of %d", n, len(b), size)
			}
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if (n == 2 || n == 7) && !failed[n] {
				failed[n] = true
				return &StatusError{Method: "PUT", Status: "500", Code: 500}
			}
			got[n] = b
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 || got[5] != nil {
		t.Fatalf("parts sent: %d (part 5 must be skipped)", len(got))
	}
	var buf bytes.Buffer
	for n := 1; n <= 11; n++ {
		if n == 5 {
			buf.Write(src[4*1024 : 5*1024])
			continue
		}
		buf.Write(got[n])
	}
	if !bytes.Equal(buf.Bytes(), src) {
		t.Fatal("reassembled parts differ")
	}
	if s := m.Stats(); s.Done != int64(len(src)) || s.Percent() != 100 {
		t.Fatalf("meter: %+v (retried bytes must not count twice)", s)
	}
	if peak.Load() < 2 || peak.Load() > 3 {
		t.Fatalf("concurrency peak %d, want 2-3", peak.Load())
	}
	if calls.Load() != 12 {
		t.Fatalf("calls %d, want 10 parts + 2 retries", calls.Load())
	}
}

func TestUploadPartsGivesUpOnClientError(t *testing.T) {
	src := data(5000)
	err := UploadParts(context.Background(), bytes.NewReader(src), 5000, 1000, 2, 3, nil, NewMeter(5000),
		func(ctx context.Context, n, try int, r io.Reader, size int64) error {
			io.Copy(io.Discard, r)
			if n == 3 {
				return &StatusError{Method: "PUT", Status: "400 Bad Request", Code: 400}
			}
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "part 3") || !IsStatus(err, 400) {
		t.Fatalf("want part 3 400, got %v", err)
	}
}

// rangeServer serves content with Range support and can fail requests.
func rangeServer(t *testing.T, content []byte, ranges bool, fail func(n int32, r *http.Request) bool) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := n.Add(1)
		if fail != nil && fail(c, r) {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		if !ranges {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.Write(content)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func download(t *testing.T, url string, o DownloadOptions) ([]byte, int64, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	n, err := Download(context.Background(), http.DefaultClient, url, f, o)
	f.Close()
	b, _ := os.ReadFile(p)
	return b, n, err
}

func TestDownloadRangesInParallelWithRetry(t *testing.T) {
	content := data(100_000)
	var failedOnce atomic.Bool
	srv, calls := rangeServer(t, content, true, func(n int32, r *http.Request) bool {
		// Fail one middle range once.
		return strings.HasPrefix(r.Header.Get("Range"), "bytes=40000-") && failedOnce.CompareAndSwap(false, true)
	})
	m := NewMeter(0)
	got, n, err := download(t, srv.URL, DownloadOptions{Chunk: 10_000, Concurrency: 4, Meter: m})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(content)) || !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, equal=%v", n, bytes.Equal(got, content))
	}
	if calls.Load() != 11 {
		t.Fatalf("requests %d, want 10 ranges + 1 retry", calls.Load())
	}
	if s := m.Stats(); s.Done != int64(len(content)) || s.Total != int64(len(content)) {
		t.Fatalf("meter %+v", s)
	}
}

func TestDownloadWithoutRangeSupportStreams(t *testing.T) {
	content := data(50_000)
	srv, calls := rangeServer(t, content, false, func(n int32, r *http.Request) bool { return n == 1 })
	got, _, err := download(t, srv.URL, DownloadOptions{Chunk: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) || calls.Load() != 2 {
		t.Fatalf("equal=%v calls=%d", bytes.Equal(got, content), calls.Load())
	}
}

func TestDownloadEmptyAndMissing(t *testing.T) {
	srv, _ := rangeServer(t, nil, true, nil)
	got, n, err := download(t, srv.URL, DownloadOptions{})
	if err != nil || n != 0 || len(got) != 0 {
		t.Fatalf("empty: %v %d", err, n)
	}
	nf := httptest.NewServer(http.NotFoundHandler())
	defer nf.Close()
	if _, _, err := download(t, nf.URL, DownloadOptions{}); !IsStatus(err, 404) {
		t.Fatalf("missing: %v", err)
	}
	var se *StatusError
	if _, _, err := download(t, nf.URL, DownloadOptions{}); !errors.As(err, &se) {
		t.Fatal("want StatusError")
	}
}

func TestMeterRateAndReport(t *testing.T) {
	m := NewMeter(1000)
	var last Stats
	var calls atomic.Int32
	stop := m.Report(10*time.Millisecond, func(s Stats) { calls.Add(1); last = s })
	m.Add(500)
	time.Sleep(30 * time.Millisecond)
	m.Add(500)
	stop()
	stop() // idempotent
	if last.Done != 1000 || last.Percent() != 100 || last.BytesPerSec <= 0 {
		t.Fatalf("final stats %+v", last)
	}
	if calls.Load() < 2 {
		t.Fatalf("report calls %d", calls.Load())
	}
	if !strings.Contains(last.String(), "100%") {
		t.Fatalf("string %q", last.String())
	}
	if (Stats{Done: 5}).Percent() != -1 {
		t.Fatal("unknown total = -1")
	}
}
