package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// InstallProfiles lists the install profiles configured on the router (admin).
func (c *Client) InstallProfiles(ctx context.Context) ([]api.InstallProfile, error) {
	var ps []api.InstallProfile
	return ps, c.do(ctx, http.MethodGet, "/v1/install/profiles", nil, &ps)
}

// CreateInstallToken mints a one-line install link (admin). Token and Command
// are only returned once.
func (c *Client) CreateInstallToken(ctx context.Context, req api.InstallTokenRequest) (*api.InstallToken, error) {
	var t api.InstallToken
	return &t, c.do(ctx, http.MethodPost, "/v1/install-tokens", req, &t)
}

// InstallTokens lists install links, newest first (admin).
func (c *Client) InstallTokens(ctx context.Context) ([]api.InstallToken, error) {
	var ts []api.InstallToken
	return ts, c.do(ctx, http.MethodGet, "/v1/install-tokens", nil, &ts)
}

// RevokeInstallToken stops an install link from being used (admin).
func (c *Client) RevokeInstallToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/install-tokens/"+url.PathEscape(id), nil, nil)
}

// InstallTokenEvents returns an install link's audit log (admin).
func (c *Client) InstallTokenEvents(ctx context.Context, id string) ([]api.InstallEvent, error) {
	var evs []api.InstallEvent
	return evs, c.do(ctx, http.MethodGet, "/v1/install-tokens/"+url.PathEscape(id)+"/events", nil, &evs)
}
