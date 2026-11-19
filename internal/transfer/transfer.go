// Package transfer moves large files over HTTP quickly and visibly: uploads
// in parallel parts (multipart), downloads in parallel byte ranges, each
// part retried on its own, with a byte meter for progress and speed. It is
// shared by the agent, the Go SDK and the CLI.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for chunked transfers.
const (
	DefaultPartSize    = 8 << 20
	DefaultConcurrency = 4
	DefaultRetries     = 4
)

// ---------------------------------------------------------------------------
// Meter
// ---------------------------------------------------------------------------

// Stats is a snapshot of a transfer.
type Stats struct {
	Done  int64
	Total int64 // 0 or less when unknown
	// BytesPerSec is the recent rate (last few seconds).
	BytesPerSec float64
	Elapsed     time.Duration
}

// Percent is 0-100, or -1 when the total is unknown.
func (s Stats) Percent() float64 {
	if s.Total <= 0 {
		return -1
	}
	return min(100, 100*float64(s.Done)/float64(s.Total))
}

// String renders "45% 120.0/266.0 MiB 38.2 MiB/s".
func (s Stats) String() string {
	mib := func(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) }
	rate := strconv.FormatFloat(s.BytesPerSec/(1<<20), 'f', 1, 64) + " MiB/s"
	if s.Total > 0 {
		return fmt.Sprintf("%3.0f%% %s/%s MiB %s", s.Percent(), mib(s.Done), mib(s.Total), rate)
	}
	return fmt.Sprintf("%s MiB %s", mib(s.Done), rate)
}

type sample struct {
	at   time.Time
	done int64
}

// Meter counts bytes moved. Safe for concurrent use.
type Meter struct {
	done  atomic.Int64
	total atomic.Int64
	start time.Time

	mu      sync.Mutex
	samples []sample
}

// NewMeter starts a meter for total bytes (0 if unknown).
func NewMeter(total int64) *Meter {
	m := &Meter{start: time.Now()}
	m.total.Store(total)
	return m
}

// Add records n bytes (negative to undo bytes of a failed attempt).
func (m *Meter) Add(n int64) {
	if m != nil {
		m.done.Add(n)
	}
}

// SetTotal sets the total once it is known.
func (m *Meter) SetTotal(n int64) {
	if m != nil {
		m.total.Store(n)
	}
}

// AddTotal grows the total (several files under one meter).
func (m *Meter) AddTotal(n int64) {
	if m != nil {
		m.total.Add(n)
	}
}

const rateWindow = 5 * time.Second

// Stats returns a snapshot; the rate covers the last few seconds.
func (m *Meter) Stats() Stats {
	now := time.Now()
	s := Stats{Done: m.done.Load(), Total: m.total.Load(), Elapsed: now.Sub(m.start)}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, sample{now, s.Done})
	i := 0
	for i < len(m.samples)-1 && now.Sub(m.samples[i].at) > rateWindow {
		i++
	}
	m.samples = m.samples[i:]
	first := m.samples[0]
	if dt := now.Sub(first.at).Seconds(); dt >= 0.5 {
		s.BytesPerSec = float64(s.Done-first.done) / dt
	} else if el := s.Elapsed.Seconds(); el > 0 {
		s.BytesPerSec = float64(s.Done) / el
	}
	if s.BytesPerSec < 0 {
		s.BytesPerSec = 0
	}
	return s
}

// Report calls fn every `every` until stop is called; stop calls fn once
// more with the final numbers. fn may be nil.
func (m *Meter) Report(every time.Duration, fn func(Stats)) (stop func()) {
	if fn == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fn(m.Stats())
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			fn(m.Stats())
		})
	}
}

// countingReader adds every byte read to a meter and to *n.
type countingReader struct {
	r io.Reader
	m *Meter
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	if k > 0 {
		c.m.Add(int64(k))
		if c.n != nil {
			c.n.Add(int64(k))
		}
	}
	return k, err
}

// CountingReader wraps r so that reads advance m.
func CountingReader(r io.Reader, m *Meter) io.Reader { return &countingReader{r: r, m: m} }

// countingWriter adds every byte written to a meter and to *n.
type countingWriter struct {
	w io.Writer
	m *Meter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	if k > 0 {
		c.m.Add(int64(k))
		if c.n != nil {
			c.n.Add(int64(k))
		}
	}
	return k, err
}

// CountingWriter wraps w so that writes advance m.
func CountingWriter(w io.Writer, m *Meter) io.Writer { return &countingWriter{w: w, m: m} }

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// StatusError is a non-2xx answer.
type StatusError struct {
	Method, Status string
	Code           int
	Body           string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return e.Method + ": " + e.Status + ": " + e.Body
	}
	return e.Method + ": " + e.Status
}

// IsStatus reports whether err is a StatusError with one of the codes.
func IsStatus(err error, codes ...int) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	for _, c := range codes {
		if se.Code == c {
			return true
		}
	}
	return false
}

func statusErr(method string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &StatusError{Method: method, Status: resp.Status, Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
}

// Put sends r (size bytes) with a PUT. hdr may be nil.
func Put(ctx context.Context, hc *http.Client, url string, hdr http.Header, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr("PUT", resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func backoff(ctx context.Context, attempt int) error {
	d := time.Duration(250<<min(attempt, 5)) * time.Millisecond
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// retryable: client errors other than timeouts/throttling/expired URLs
// will not get better by retrying.
func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500 || se.Code == 408 || se.Code == 429 || se.Code == 403
	}
	return true
}

// ---------------------------------------------------------------------------
// Parallel uploads
// ---------------------------------------------------------------------------

// PartFunc sends part n (1-based) whose bytes are r (size bytes). try
// counts attempts from 0 so callers can refresh an expired URL on retries.
type PartFunc func(ctx context.Context, n, try int, r io.Reader, size int64) error

// UploadParts sends parts 1..ceil(size/partSize) of src, `concurrency` at a
// time. Each part is retried up to `retries` more times on its own; parts
// in skip are already stored (resume) and count as done. The first part
// that fails for good cancels the rest.
func UploadParts(ctx context.Context, src io.ReaderAt, size, partSize int64, concurrency, retries int,
	skip map[int]bool, m *Meter, put PartFunc) error {
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if retries < 0 {
		retries = 0
	}
	parts := int((size + partSize - 1) / partSize)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(concurrency, max(parts, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				off := int64(n-1) * partSize
				ps := min(partSize, size-off)
				var err error
				for try := 0; try <= retries; try++ {
					if try > 0 {
						if berr := backoff(ctx, try); berr != nil {
							err = berr
							break
						}
					}
					var sent atomic.Int64
					r := &countingReader{r: io.NewSectionReader(src, off, ps), m: m, n: &sent}
					err = put(ctx, n, try, r, ps)
					if err == nil {
						break
					}
					m.Add(-sent.Load())
					if ctx.Err() != nil || !retryable(err) {
						break
					}
				}
				if err != nil {
					cancel(fmt.Errorf("part %d: %w", n, err))
				}
			}
		}()
	}
feed:
	for n := 1; n <= parts; n++ {
		if skip[n] {
			off := int64(n-1) * partSize
			m.Add(min(partSize, size-off))
			continue
		}
		select {
		case work <- n:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return context.Cause(ctx)
}

// ---------------------------------------------------------------------------
// Parallel downloads
// ---------------------------------------------------------------------------

// DownloadOptions tune Download.
type DownloadOptions struct {
	// Chunk is the byte range per request (default 8 MiB).
	Chunk int64
	// Concurrency is the number of ranges in flight (default 4).
	Concurrency int
	// Retries per range (default 4).
	Retries int
	// Header is added to every request (e.g. Authorization).
	Header http.Header
	Meter  *Meter
}

func (o *DownloadOptions) norm() {
	if o.Chunk <= 0 {
		o.Chunk = DefaultPartSize
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Retries == 0 {
		o.Retries = DefaultRetries
	}
}

func get(ctx context.Context, hc *http.Client, url string, hdr http.Header, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, statusErr("GET", resp)
	}
	return resp, nil
}

// totalFromContentRange parses "bytes a-b/total"; -1 when unknown.
func totalFromContentRange(v string) int64 {
	_, after, ok := strings.Cut(v, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Download fetches url into dst and returns the size. When the server
// honours Range requests the file is fetched as parallel byte ranges, each
// retried on its own; otherwise it is streamed (and restarted on failure).
func Download(ctx context.Context, hc *http.Client, url string, dst io.WriterAt, o DownloadOptions) (int64, error) {
	o.norm()
	m := o.Meter
	if m == nil {
		m = NewMeter(0)
	}
	var lastErr error
	for try := 0; try <= o.Retries; try++ {
		if try > 0 {
			if err := backoff(ctx, try); err != nil {
				return 0, err
			}
		}
		resp, err := get(ctx, hc, url, o.Header, fmt.Sprintf("bytes=0-%d", o.Chunk-1))
		if err != nil {
			if IsStatus(err, http.StatusRequestedRangeNotSatisfiable) {
				return 0, nil // empty file
			}
			lastErr = err
			if !retryable(err) {
				return 0, err
			}
			continue
		}
		total := int64(-1)
		ranged := resp.StatusCode == http.StatusPartialContent
		if ranged {
			total = totalFromContentRange(resp.Header.Get("Content-Range"))
		} else {
			total = resp.ContentLength
		}
		if total >= 0 {
			m.AddTotal(total)
		}
		var cnt atomic.Int64
		_, err = io.Copy(&countingWriter{w: io.NewOffsetWriter(dst, 0), m: m, n: &cnt}, resp.Body)
		resp.Body.Close()
		got := cnt.Load()
		if err == nil && total >= 0 && !ranged && got != total {
			err = io.ErrUnexpectedEOF
		}
		if err != nil || !ranged || total < 0 {
			if err == nil {
				return got, nil
			}
			m.Add(-got)
			if total >= 0 {
				m.AddTotal(-total)
			}
			lastErr = err
			continue
		}
		if got >= total {
			return total, nil
		}
		if err := downloadRanges(ctx, hc, url, dst, got, total, o, m); err != nil {
			return 0, err
		}
		return total, nil
	}
	return 0, lastErr
}

func downloadRanges(ctx context.Context, hc *http.Client, url string, dst io.WriterAt, from, total int64,
	o DownloadOptions, m *Meter) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	work := make(chan int64)
	var wg sync.WaitGroup
	for range o.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for off := range work {
				end := min(off+o.Chunk, total) - 1
				var err error
				for try := 0; try <= o.Retries; try++ {
					if try > 0 {
						if berr := backoff(ctx, try); berr != nil {
							err = berr
							break
						}
					}
					var got atomic.Int64
					err = fetchRange(ctx, hc, url, o.Header, dst, off, end, &countingWriter{m: m, n: &got})
					if err == nil {
						break
					}
					m.Add(-got.Load())
					if ctx.Err() != nil || !retryable(err) {
						break
					}
				}
				if err != nil {
					cancel(fmt.Errorf("bytes %d-%d: %w", off, end, err))
				}
			}
		}()
	}
feed:
	for off := from; off < total; off += o.Chunk {
		select {
		case work <- off:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return context.Cause(ctx)
}

// fetchRange GETs bytes off..end into dst; cw counts (its w is set here).
func fetchRange(ctx context.Context, hc *http.Client, url string, hdr http.Header, dst io.WriterAt, off, end int64,
	cw *countingWriter) error {
	resp, err := get(ctx, hc, url, hdr, fmt.Sprintf("bytes=%d-%d", off, end))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server ignored the byte range (status %s)", resp.Status)
	}
	cw.w = io.NewOffsetWriter(dst, off)
	n, err := io.Copy(cw, io.LimitReader(resp.Body, end-off+1))
	if err != nil {
		return err
	}
	if n != end-off+1 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
