package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Stored files, job deletion for owners, and router-side Mac settings.

// FileFilter selects stored files for Files. Empty fields match all.
type FileFilter struct {
	// Kind: api.FileUpload or api.FileOutput.
	Kind  string
	JobID string
	// Prefix matches the storage key (e.g. "uploads/myapp/").
	Prefix string
	// Owner (admins): another owner's files, or "*" for everyone's.
	Owner string
	Limit int
}

// Files lists the stored files you own (uploads and your jobs' outputs).
func (c *Client) Files(ctx context.Context, f FileFilter) ([]api.StoredFile, error) {
	q := url.Values{}
	for k, v := range map[string]string{"kind": f.Kind, "job": f.JobID, "prefix": f.Prefix, "owner": f.Owner} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out []api.StoredFile
	return out, c.do(ctx, http.MethodGet, "/v1/files?"+q.Encode(), nil, &out)
}

// DeleteFile deletes one stored file you own. key is a storage key
// (uploads/..., jobs/...) or a blob key as used in Input.Blob. Files an
// unfinished job still needs are refused (HTTP 409).
func (c *Client) DeleteFile(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/v1/files/"+escapeKey(key), nil, nil)
}

// DeleteFiles deletes several stored files; files in use are skipped and
// listed in the result.
func (c *Client) DeleteFiles(ctx context.Context, req api.DeleteFilesRequest) (*api.DeleteFilesResult, error) {
	var r api.DeleteFilesResult
	return &r, c.do(ctx, http.MethodPost, "/v1/files/delete", req, &r)
}

// RemoveJob deletes one of your jobs (admins: any job) with its tasks, logs
// and stored outputs, cancelling it first if it has not finished. inputs
// also deletes the input blobs you uploaded for it, unless another
// unfinished job uses them.
func (c *Client) RemoveJob(ctx context.Context, id string, inputs bool) error {
	p := "/v1/jobs/" + url.PathEscape(id)
	if inputs {
		p += "?inputs=true"
	}
	return c.do(ctx, http.MethodDelete, p, nil, nil)
}

// SetMacLabels replaces a Mac's router-side labels (admin). They are merged
// over the labels from the Mac's agent.yaml for task matching.
func (c *Client) SetMacLabels(ctx context.Context, macID string, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, "/v1/macs/"+url.PathEscape(macID)+"/labels", api.MacLabels{Labels: labels}, nil)
}

// ReserveMac reserves a Mac for some owners (admin).
func (c *Client) ReserveMac(ctx context.Context, macID string, req api.MacReservationRequest) error {
	return c.do(ctx, http.MethodPut, "/v1/macs/"+url.PathEscape(macID)+"/reservation", req, nil)
}

// UnreserveMac removes a Mac's reservation (admin).
func (c *Client) UnreserveMac(ctx context.Context, macID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/macs/"+url.PathEscape(macID)+"/reservation", nil, nil)
}

// SetMacMaxTasks sets how many tasks a Mac runs at once (admin); 0 returns
// to the agent's own max_tasks. The agent caps it at its max_tasks_limit.
func (c *Client) SetMacMaxTasks(ctx context.Context, macID string, n int) error {
	return c.PatchMac(ctx, macID, api.MacPatch{MaxTasks: &n})
}

func escapeKey(k string) string {
	parts := strings.Split(k, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
