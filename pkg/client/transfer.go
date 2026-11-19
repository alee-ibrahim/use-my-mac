package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/transfer"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Progress is a snapshot of a running upload or download.
type Progress = transfer.Stats

// TransferOptions tune uploads and downloads. The zero value is fine.
type TransferOptions struct {
	// PartSize is the multipart part size (default 8 MiB, minimum 5 MiB).
	// Files no bigger than one part use a single PUT.
	PartSize int64
	// Concurrency is the number of parts in flight (default 4).
	Concurrency int
	// Retries per part (default 4).
	Retries int
	// Direct asks for presigned part URLs straight to object storage when
	// the router uses S3 (default: through the router).
	Direct bool
	// OnProgress is called about every ProgressEvery (default 250 ms) and
	// once at the end.
	OnProgress    func(Progress)
	ProgressEvery time.Duration
	// OnUpload is called with the multipart upload ID as soon as it exists,
	// so a caller can save it and ResumeUpload after a crash.
	OnUpload func(uploadID string)
}

func (o *TransferOptions) norm() TransferOptions {
	var v TransferOptions
	if o != nil {
		v = *o
	}
	if v.PartSize <= 0 {
		v.PartSize = api.DefaultPartSize
	}
	if v.PartSize < api.MinPartSize {
		v.PartSize = api.MinPartSize
	}
	if v.Concurrency <= 0 {
		v.Concurrency = transfer.DefaultConcurrency
	}
	if v.Retries <= 0 {
		v.Retries = transfer.DefaultRetries
	}
	if v.ProgressEvery <= 0 {
		v.ProgressEvery = 250 * time.Millisecond
	}
	return v
}

func (c *Client) authHeader() http.Header {
	return http.Header{"Authorization": {"Bearer " + c.APIKey}}
}

// UploadFile uploads a local file as blob `key` (for Input.Blob), in
// parallel parts when it is bigger than one part.
func (c *Client) UploadFile(ctx context.Context, key, path string, opt *TransferOptions) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return c.Upload(ctx, key, f, st.Size(), opt)
}

// Upload stores size bytes from src as blob `key`. Big blobs go as a
// multipart upload (parallel parts, each retried on its own); small ones,
// or routers without multipart support, use a single PUT.
func (c *Client) Upload(ctx context.Context, key string, src io.ReaderAt, size int64, opt *TransferOptions) error {
	o := opt.norm()
	m := transfer.NewMeter(size)
	stop := m.Report(o.ProgressEvery, o.OnProgress)
	defer stop()
	if size > o.PartSize {
		var up api.Upload
		err := c.do(ctx, http.MethodPost, "/v1/uploads", api.UploadRequest{Key: key, Size: size, PartSize: o.PartSize, Direct: o.Direct}, &up)
		switch {
		case err == nil:
			if o.OnUpload != nil {
				o.OnUpload(up.ID)
			}
			return c.sendParts(ctx, &up, src, size, o, m)
		case !isStatus(err, http.StatusNotFound, http.StatusMethodNotAllowed):
			return err
		}
		// Older router: fall through to a single PUT.
	}
	return transfer.Put(ctx, c.HTTP, c.BaseURL+"/v1/blobs/"+key, c.authHeader(),
		transfer.CountingReader(io.NewSectionReader(src, 0, size), m), size)
}

// ResumeUpload finishes an interrupted multipart upload: parts already
// stored are skipped. src must hold the same bytes as before.
func (c *Client) ResumeUpload(ctx context.Context, uploadID string, src io.ReaderAt, opt *TransferOptions) error {
	up, err := c.UploadStatus(ctx, uploadID)
	if err != nil {
		return err
	}
	o := opt.norm()
	m := transfer.NewMeter(up.Size)
	stop := m.Report(o.ProgressEvery, o.OnProgress)
	defer stop()
	return c.sendParts(ctx, up, src, up.Size, o, m)
}

// UploadStatus returns a multipart upload's stored parts and fresh URLs.
func (c *Client) UploadStatus(ctx context.Context, uploadID string) (*api.Upload, error) {
	var up api.Upload
	return &up, c.do(ctx, http.MethodGet, "/v1/uploads/"+url.PathEscape(uploadID), nil, &up)
}

// AbortUpload drops an unfinished multipart upload.
func (c *Client) AbortUpload(ctx context.Context, uploadID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/uploads/"+url.PathEscape(uploadID), nil, nil)
}

func (c *Client) sendParts(ctx context.Context, up *api.Upload, src io.ReaderAt, size int64, o TransferOptions, m *transfer.Meter) error {
	if size != up.Size {
		return errors.New("upload: size does not match the upload")
	}
	skip := map[int]bool{}
	for _, n := range up.Done {
		skip[n] = true
	}
	var mu sync.Mutex
	urls := up.URLs
	refreshed := time.Time{}
	err := transfer.UploadParts(ctx, src, size, up.PartSize, o.Concurrency, o.Retries, skip, m,
		func(ctx context.Context, n, try int, r io.Reader, sz int64) error {
			mu.Lock()
			if try > 0 && time.Since(refreshed) > 10*time.Second {
				// Presigned URLs may have expired: ask for fresh ones.
				if fresh, err := c.UploadStatus(ctx, up.ID); err == nil {
					urls, refreshed = fresh.URLs, time.Now()
				}
			}
			u := urls[n]
			mu.Unlock()
			if u == "" {
				return nil // stored meanwhile (seen on refresh)
			}
			var hdr http.Header
			if strings.HasPrefix(u, "/") {
				u, hdr = c.BaseURL+u, c.authHeader()
			}
			return transfer.Put(ctx, c.HTTP, u, hdr, r, sz)
		})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(up.ID)+"/complete", nil, nil)
}

func isStatus(err error, codes ...int) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range codes {
		if e.Status == c {
			return true
		}
	}
	return false
}

// DownloadOutputFile saves a task output to path, in parallel byte ranges
// when the router can serve them, with progress.
func (c *Client) DownloadOutputFile(ctx context.Context, taskID, name, path string, opt *TransferOptions) (int64, error) {
	o := opt.norm()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	m := transfer.NewMeter(0)
	stop := m.Report(o.ProgressEvery, o.OnProgress)
	n, err := transfer.Download(ctx, c.HTTP,
		c.BaseURL+"/v1/tasks/"+url.PathEscape(taskID)+"/outputs/"+url.PathEscape(name), f,
		transfer.DownloadOptions{Chunk: o.PartSize, Concurrency: o.Concurrency, Retries: o.Retries,
			Header: c.authHeader(), Meter: m})
	stop()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		var se *transfer.StatusError
		if errors.As(err, &se) {
			return 0, &Error{Status: se.Code, Message: se.Body}
		}
	}
	return n, err
}

// ---------------------------------------------------------------------------
// Live job updates
// ---------------------------------------------------------------------------

// WatchJob streams a job's changes (server-sent events) and calls fn with
// every new state until the job finishes or ctx ends. When the router
// cannot stream, it polls every `every` instead.
func (c *Client) WatchJob(ctx context.Context, id string, every time.Duration, fn func(*api.Job)) (*api.Job, error) {
	j, err := c.streamJob(ctx, id, fn)
	if j != nil && api.IsTerminal(j.State) {
		return j, nil
	}
	if ctx.Err() != nil {
		return j, ctx.Err()
	}
	if err != nil && isStatus(err, http.StatusNotFound) {
		// Old router (no stream route) or a missing job: polling tells.
		if _, gerr := c.Job(ctx, id); gerr != nil {
			return nil, gerr
		}
	}
	return c.pollJob(ctx, id, every, fn)
}

func (c *Client) streamJob(ctx context.Context, id string, fn func(*api.Job)) (*api.Job, error) {
	var last *api.Job
	for attempt := 0; attempt < 30; attempt++ {
		resp, err := c.req(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/stream", nil, -1, "")
		if err != nil {
			return last, err
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body.Close()
			return last, errors.New("router did not stream")
		}
		ended, start := false, time.Now()
		err = readSSE(resp.Body, func(event string, data []byte) bool {
			switch event {
			case "job":
				var j api.Job
				if json.Unmarshal(data, &j) == nil {
					last = &j
					if fn != nil {
						fn(&j)
					}
				}
			case "end", "removed":
				ended = true
				return false
			}
			return true
		})
		resp.Body.Close()
		if ended || ctx.Err() != nil {
			return last, ctx.Err()
		}
		_ = err // connection dropped (proxy timeout, restart): reconnect
		if time.Since(start) > 30*time.Second {
			attempt = 0 // it was working: only quick repeated drops give up on streaming
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return last, errors.New("stream kept dropping")
}

// readSSE parses a text/event-stream; fn returns false to stop.
func readSSE(r io.Reader, fn func(event string, data []byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	event, data := "", []byte(nil)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != nil {
				if !fn(nz(event, "message"), data) {
					return nil
				}
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line[5:], " ")
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, d...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
