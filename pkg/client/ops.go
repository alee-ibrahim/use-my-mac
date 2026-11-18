package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Summary returns the fleet's headline numbers (viewer or admin).
func (c *Client) Summary(ctx context.Context) (*api.Summary, error) {
	var s api.Summary
	return &s, c.do(ctx, http.MethodGet, "/v1/metrics/summary", nil, &s)
}

// FleetSeries returns fleet metrics for the last rng (e.g. "24h", "7d");
// step 0 = automatic.
func (c *Client) FleetSeries(ctx context.Context, rng string, step int) (*api.Series, error) {
	var s api.Series
	q := url.Values{"range": {rng}}
	if step > 0 {
		q.Set("step", strconv.Itoa(step))
	}
	return &s, c.do(ctx, http.MethodGet, "/v1/metrics/fleet?"+q.Encode(), nil, &s)
}

// MacSeries returns one Mac's metrics for the last rng.
func (c *Client) MacSeries(ctx context.Context, macID, rng string, step int) (*api.Series, error) {
	var s api.Series
	q := url.Values{"range": {rng}}
	if step > 0 {
		q.Set("step", strconv.Itoa(step))
	}
	return &s, c.do(ctx, http.MethodGet, "/v1/metrics/macs/"+url.PathEscape(macID)+"?"+q.Encode(), nil, &s)
}

// Events lists the activity/audit log, newest first. kinds may end in "."
// to match a prefix ("mac.").
func (c *Client) Events(ctx context.Context, kinds []string, limit int) ([]api.Event, error) {
	var evs []api.Event
	q := url.Values{}
	if len(kinds) > 0 {
		q.Set("kind", strings.Join(kinds, ","))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return evs, c.do(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil, &evs)
}

// Retention returns the retention settings.
func (c *Client) Retention(ctx context.Context) (*api.Retention, error) {
	var r api.Retention
	return &r, c.do(ctx, http.MethodGet, "/v1/retention", nil, &r)
}

// SetRetention changes the retention settings (admin).
func (c *Client) SetRetention(ctx context.Context, r api.Retention) (*api.Retention, error) {
	var out api.Retention
	return &out, c.do(ctx, http.MethodPut, "/v1/retention", r, &out)
}

// DeleteAPIKey deletes a revoked key (admin).
func (c *Client) DeleteAPIKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/api-keys/"+url.PathEscape(id)+"/purge", nil, nil)
}

// PurgeAPIKeys deletes every revoked key (admin).
func (c *Client) PurgeAPIKeys(ctx context.Context) (int, error) {
	var r api.PurgeResult
	return r.Deleted, c.do(ctx, http.MethodPost, "/v1/api-keys/purge", nil, &r)
}

// DeleteInstallToken deletes a revoked, expired or used-up install link (admin).
func (c *Client) DeleteInstallToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/install-tokens/"+url.PathEscape(id)+"/purge", nil, nil)
}

// PurgeInstallTokens deletes every link that can no longer enrol (admin).
func (c *Client) PurgeInstallTokens(ctx context.Context) (int, error) {
	var r api.PurgeResult
	return r.Deleted, c.do(ctx, http.MethodPost, "/v1/install-tokens/purge", nil, &r)
}

// DeleteMac deletes a Mac; its credential stops working (admin).
func (c *Client) DeleteMac(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/macs/"+url.PathEscape(id), nil, nil)
}

// DeleteJob deletes a finished job with its tasks, logs and outputs (admin).
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(id), nil, nil)
}

// PurgeJobs deletes finished jobs older than req.OlderThanDays (admin).
func (c *Client) PurgeJobs(ctx context.Context, req api.PurgeJobsRequest) (int, error) {
	var r api.PurgeResult
	return r.Deleted, c.do(ctx, http.MethodPost, "/v1/jobs/purge", req, &r)
}

// JobTimeline returns every attempt of a job's tasks.
func (c *Client) JobTimeline(ctx context.Context, id string) ([]api.TimelineAttempt, error) {
	var tl []api.TimelineAttempt
	return tl, c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/timeline", nil, &tl)
}
