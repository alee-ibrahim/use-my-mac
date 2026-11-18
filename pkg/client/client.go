// Package client is the Go SDK for apps that submit work to a use-my-mac
// router. It is a thin wrapper around the REST API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Client talks to the router with an API key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New returns a client. The API key should come from the environment or a
// file, never from command-line arguments.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 0}}
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Message) }

func (c *Client) req(ctx context.Context, method, path string, body io.Reader, size int64, ctype string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		r.ContentLength = size
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	r.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &Error{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	size := int64(-1)
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, size = bytes.NewReader(b), int64(len(b))
	}
	resp, err := c.req(ctx, method, path, body, size, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SubmitJob submits a job.
func (c *Client) SubmitJob(ctx context.Context, spec api.JobSpec) (*api.Job, error) {
	var j api.Job
	return &j, c.do(ctx, http.MethodPost, "/v1/jobs", spec, &j)
}

// Job fetches a job with its tasks.
func (c *Client) Job(ctx context.Context, id string) (*api.Job, error) {
	var j api.Job
	return &j, c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &j)
}

// Jobs lists recent jobs (state may be "").
func (c *Client) Jobs(ctx context.Context, state string, limit int) ([]api.Job, error) {
	var js []api.Job
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	return js, c.do(ctx, http.MethodGet, "/v1/jobs?"+q.Encode(), nil, &js)
}

// CancelJob cancels a job.
func (c *Client) CancelJob(ctx context.Context, id string) (*api.Job, error) {
	var j api.Job
	return &j, c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, &j)
}

// TaskDetail is a task with attempts and the last placement decision.
type TaskDetail struct {
	api.Task
	Placement []struct {
		ID     string             `json:"id"`
		OK     bool               `json:"ok"`
		Reason string             `json:"reason"`
		Score  float64            `json:"score"`
		Parts  map[string]float64 `json:"parts"`
	} `json:"placement"`
}

// Task fetches a task.
func (c *Client) Task(ctx context.Context, id string) (*TaskDetail, error) {
	var t TaskDetail
	return &t, c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(id), nil, &t)
}

// Logs returns a task's logs (attempt 0 = all attempts).
func (c *Client) Logs(ctx context.Context, taskID string, attempt int) (string, error) {
	resp, err := c.req(ctx, http.MethodGet, fmt.Sprintf("/v1/tasks/%s/logs?attempt=%d", url.PathEscape(taskID), attempt), nil, -1, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// DownloadOutput writes a task output to w.
func (c *Client) DownloadOutput(ctx context.Context, taskID, name string, w io.Writer) (int64, error) {
	resp, err := c.req(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/outputs/"+url.PathEscape(name), nil, -1, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// UploadBlob stores an input blob that tasks can reference as Input.Blob.
func (c *Client) UploadBlob(ctx context.Context, key string, r io.Reader, size int64) error {
	resp, err := c.req(ctx, http.MethodPut, "/v1/blobs/"+key, r, size, "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Macs lists the fleet.
func (c *Client) Macs(ctx context.Context) ([]api.Mac, error) {
	var ms []api.Mac
	return ms, c.do(ctx, http.MethodGet, "/v1/macs", nil, &ms)
}

// PatchMac drains, opts out or renames a Mac (admin).
func (c *Client) PatchMac(ctx context.Context, id string, p api.MacPatch) error {
	return c.do(ctx, http.MethodPatch, "/v1/macs/"+url.PathEscape(id), p, nil)
}

// CreateEnrolmentToken mints a one-time token for a new Mac (admin).
func (c *Client) CreateEnrolmentToken(ctx context.Context, ttlHours int) (*api.EnrolmentToken, error) {
	var t api.EnrolmentToken
	return &t, c.do(ctx, http.MethodPost, "/v1/enrolment-tokens", map[string]int{"ttl_hours": ttlHours}, &t)
}

// CreateAPIKey mints an API key (admin). The secret is only returned once.
func (c *Client) CreateAPIKey(ctx context.Context, name, role string) (*api.APIKey, error) {
	var k api.APIKey
	return &k, c.do(ctx, http.MethodPost, "/v1/api-keys", map[string]string{"name": name, "role": role}, &k)
}

// APIKeys lists keys (admin).
func (c *Client) APIKeys(ctx context.Context) ([]api.APIKey, error) {
	var ks []api.APIKey
	return ks, c.do(ctx, http.MethodGet, "/v1/api-keys", nil, &ks)
}

// RevokeAPIKey revokes a key (admin).
func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/api-keys/"+url.PathEscape(id), nil, nil)
}

// WaitJob polls until the job reaches a terminal state or ctx ends.
func (c *Client) WaitJob(ctx context.Context, id string, every time.Duration, progress func(*api.Job)) (*api.Job, error) {
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		j, err := c.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		if progress != nil {
			progress(j)
		}
		if api.IsTerminal(j.State) {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-t.C:
		}
	}
}
