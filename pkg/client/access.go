package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Access returns the dashboard role rules: config rules (locked) and
// dashboard rules (admin).
func (c *Client) Access(ctx context.Context) (*api.AccessPolicy, error) {
	var p api.AccessPolicy
	return &p, c.do(ctx, http.MethodGet, "/v1/access", nil, &p)
}

// PutAccessRule adds a rule or changes an existing one's role (admin).
func (c *Client) PutAccessRule(ctx context.Context, req api.AccessRuleRequest) (*api.AccessRule, error) {
	var r api.AccessRule
	return &r, c.do(ctx, http.MethodPost, "/v1/access/rules", req, &r)
}

// DeleteAccessRule removes a dashboard rule (admin). Config rules are locked.
func (c *Client) DeleteAccessRule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/access/rules/"+url.PathEscape(id), nil, nil)
}

// SetAccessDefault sets the dashboard default role: deny or viewer (admin).
func (c *Client) SetAccessDefault(ctx context.Context, role string) (*api.AccessPolicy, error) {
	var p api.AccessPolicy
	return &p, c.do(ctx, http.MethodPut, "/v1/access/default", api.AccessDefaultRequest{Role: role}, &p)
}

// SetAccessGroupName names a group ID; "" removes the name (admin).
func (c *Client) SetAccessGroupName(ctx context.Context, groupID, name string) error {
	return c.do(ctx, http.MethodPut, "/v1/access/groups/"+url.PathEscape(groupID), api.AccessGroupNameRequest{Name: name}, nil)
}

// AccessUsers lists people who signed in, with their role and why (admin).
func (c *Client) AccessUsers(ctx context.Context) ([]api.AccessUser, error) {
	var us []api.AccessUser
	return us, c.do(ctx, http.MethodGet, "/v1/access/users", nil, &us)
}

// SetAccessUserRole sets a signed-in user's own rule; "" removes it (admin).
func (c *Client) SetAccessUserRole(ctx context.Context, subject, role string) error {
	return c.do(ctx, http.MethodPut, "/v1/access/users/"+url.PathEscape(subject)+"/role", api.AccessUserRoleRequest{Role: role}, nil)
}

// AccessAudit returns the role-change audit log, newest first (admin).
func (c *Client) AccessAudit(ctx context.Context) ([]api.AccessEvent, error) {
	var evs []api.AccessEvent
	return evs, c.do(ctx, http.MethodGet, "/v1/access/audit", nil, &evs)
}
