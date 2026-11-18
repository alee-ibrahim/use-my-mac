package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/access"
	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// accessEnv: OIDC on, config admin group "cfg-admins", default deny.
func accessEnv(t *testing.T) *authEnv {
	e := newAuthEnv(t, true, false, true)
	e.srv.Access = access.New(e.srv.E.Store(), access.Config{AdminGroups: []string{"cfg-admins"}}, nil, e.srv.Log)
	e.h = e.srv.Handler()
	return e
}

// oidcCookie records a login and returns a session cookie, as the callback would.
func oidcCookie(t *testing.T, e *authEnv, u access.User) *http.Cookie {
	t.Helper()
	d, err := e.srv.Access.RecordLogin(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	sess := auth.SignSession(e.srv.SessionKey, auth.Session{Subject: "user:" + u.Sub, Name: u.Name, Role: d.Role,
		Via: "oidc", Expires: time.Now().Add(time.Hour).Unix()})
	return &http.Cookie{Name: sessionCookie, Value: sess}
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set("X-UMM-CSRF", "1")
	}
}

func withKey(k string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+k) }
}

func roleOf(t *testing.T, e *authEnv, c *http.Cookie) (int, string) {
	w := e.do("GET", "/v1/me", "", withCookie(c))
	var me api.Me
	json.Unmarshal(w.Body.Bytes(), &me)
	return w.Code, me.Role
}

func TestAccessEndpointsAuthz(t *testing.T) {
	e := accessEnv(t)
	viewerKey, err := e.srv.E.CreateAPIKey(context.Background(), "v", api.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := []struct{ method, path, body string }{
		{"GET", "/v1/access", ""},
		{"POST", "/v1/access/rules", `{"kind":"group","value":"g","role":"viewer"}`},
		{"DELETE", "/v1/access/rules/x", ""},
		{"PUT", "/v1/access/default", `{"role":"viewer"}`},
		{"PUT", "/v1/access/groups/g", `{"name":"G"}`},
		{"GET", "/v1/access/users", ""},
		{"PUT", "/v1/access/users/s/role", `{"role":"viewer"}`},
		{"GET", "/v1/access/audit", ""},
	}
	viewer := oidcCookie(t, e, access.User{Sub: "v1", Name: "Vic"})
	e.srv.Access.SetDefault(context.Background(), "key:test", "", "viewer")
	for _, ep := range endpoints {
		if w := e.do(ep.method, ep.path, ep.body, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous: %d", ep.method, ep.path, w.Code)
		}
		for name, k := range map[string]string{"app": e.app, "viewer": viewerKey.Secret} {
			if w := e.do(ep.method, ep.path, ep.body, withKey(k)); w.Code != http.StatusForbidden {
				t.Errorf("%s %s %s key: %d", ep.method, ep.path, name, w.Code)
			}
		}
		if w := e.do(ep.method, ep.path, ep.body, withCookie(viewer)); w.Code != http.StatusForbidden {
			t.Errorf("%s %s viewer session: %d", ep.method, ep.path, w.Code)
		}
	}
	// Admin key works; cookie writes need the CSRF header.
	if w := e.do("GET", "/v1/access", "", withKey(e.admin)); w.Code != http.StatusOK {
		t.Fatalf("admin GET /v1/access: %d", w.Code)
	}
	admin := oidcCookie(t, e, access.User{Sub: "a1", Name: "Ada", Groups: []string{"cfg-admins"}})
	w := e.do("PUT", "/v1/access/default", `{"role":"deny"}`, func(r *http.Request) { r.AddCookie(admin) })
	if w.Code != http.StatusForbidden {
		t.Fatalf("cookie write without CSRF header: %d", w.Code)
	}
	if w = e.do("PUT", "/v1/access/default", `{"role":"deny"}`, withCookie(admin)); w.Code != http.StatusOK {
		t.Fatalf("admin session write: %d %s", w.Code, w.Body)
	}
}

func TestAccessConfigRulesLocked(t *testing.T) {
	e := accessEnv(t)
	w := e.do("GET", "/v1/access", "", withKey(e.admin))
	var p api.AccessPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 1 || !p.Rules[0].Locked || p.Rules[0].Source != "config" || p.Rules[0].Role != "admin" {
		t.Fatalf("config rules: %+v", p.Rules)
	}
	if w = e.do("DELETE", "/v1/access/rules/"+p.Rules[0].ID, "", withKey(e.admin)); w.Code != http.StatusForbidden {
		t.Fatalf("delete config rule: %d", w.Code)
	}
	// A UI rule for the same group can't lower the config role.
	e.do("POST", "/v1/access/rules", `{"kind":"group","value":"cfg-admins","role":"viewer"}`, withKey(e.admin))
	c := oidcCookie(t, e, access.User{Sub: "a1", Groups: []string{"CFG-ADMINS"}})
	if _, role := roleOf(t, e, c); role != "admin" {
		t.Fatalf("config admin lowered to %q", role)
	}
}

func TestAccessSessionReevaluation(t *testing.T) {
	e := accessEnv(t)
	ctx := context.Background()
	c := oidcCookie(t, e, access.User{Sub: "s-bo", Name: "Bo", Email: "bo@example.com", Groups: []string{"g-ops"}})
	if code, _ := roleOf(t, e, c); code != http.StatusUnauthorized {
		t.Fatalf("denied user: %d", code)
	}
	// Group rule -> viewer, on the next request with the same cookie.
	w := e.do("POST", "/v1/access/rules", `{"kind":"group","value":"g-ops","role":"viewer","name":"Ops"}`, withKey(e.admin))
	if w.Code != http.StatusOK {
		t.Fatalf("add rule: %d %s", w.Code, w.Body)
	}
	var rule api.AccessRule
	json.Unmarshal(w.Body.Bytes(), &rule)
	if _, role := roleOf(t, e, c); role != "viewer" {
		t.Fatalf("after group rule: %q", role)
	}
	// User rule by email -> admin.
	e.do("POST", "/v1/access/rules", `{"kind":"user","value":"BO@example.com","role":"admin"}`, withKey(e.admin))
	if _, role := roleOf(t, e, c); role != "admin" {
		t.Fatalf("after user rule: %q", role)
	}
	// Change via the users endpoint, then remove the user rule.
	if w = e.do("PUT", "/v1/access/users/s-bo/role", `{"role":"viewer"}`, withKey(e.admin)); w.Code != http.StatusNoContent {
		t.Fatalf("set user role: %d %s", w.Code, w.Body)
	}
	if _, role := roleOf(t, e, c); role != "viewer" {
		t.Fatalf("after user role: %q", role)
	}
	e.do("PUT", "/v1/access/users/s-bo/role", `{"role":""}`, withKey(e.admin))
	// Removing the group rule denies the existing session.
	if w = e.do("DELETE", "/v1/access/rules/"+rule.ID, "", withKey(e.admin)); w.Code != http.StatusNoContent {
		t.Fatalf("delete rule: %d", w.Code)
	}
	p, _ := e.srv.Access.Policy(ctx, true)
	// The email rule still applies.
	if _, role := roleOf(t, e, c); role != "admin" {
		t.Fatalf("email rule: %q", role)
	}
	for _, r := range p.Rules {
		if r.Source == "ui" {
			e.do("DELETE", "/v1/access/rules/"+r.ID, "", withKey(e.admin))
		}
	}
	if code, _ := roleOf(t, e, c); code != http.StatusUnauthorized {
		t.Fatalf("session after all rules removed: %d", code)
	}

	// Users list says who and why; the audit log records the changes.
	w = e.do("GET", "/v1/access/users", "", withKey(e.admin))
	var us []api.AccessUser
	json.Unmarshal(w.Body.Bytes(), &us)
	if len(us) != 1 || us[0].Subject != "s-bo" || us[0].Role != "" || us[0].Reason == "" {
		t.Fatalf("users: %+v", us)
	}
	w = e.do("GET", "/v1/access/audit", "", withKey(e.admin))
	if !strings.Contains(w.Body.String(), "rule.delete") || !strings.Contains(w.Body.String(), "key:") {
		t.Fatalf("audit: %s", w.Body)
	}

	// A session from before roles were tracked must sign in again.
	old := auth.SignSession(e.srv.SessionKey, auth.Session{Subject: "user:legacy", Role: "admin", Via: "oidc",
		Expires: time.Now().Add(time.Hour).Unix()})
	if code, _ := roleOf(t, e, &http.Cookie{Name: sessionCookie, Value: old}); code != http.StatusUnauthorized {
		t.Fatalf("legacy session: %d", code)
	}
}

func TestAccessLockoutViaAPI(t *testing.T) {
	e := newAuthEnv(t, true, false, true) // no config admin groups
	ctx := context.Background()
	c := oidcCookie(t, e, access.User{Sub: "s-al", Name: "Al"})
	if err := e.srv.Access.SetUserRole(ctx, "key:test", "", "s-al", "admin"); err != nil {
		t.Fatal(err)
	}
	w := e.do("PUT", "/v1/access/users/s-al/role", `{"role":"viewer"}`, withCookie(c))
	if w.Code != http.StatusConflict {
		t.Fatalf("last admin self-demotion: %d %s", w.Code, w.Body)
	}
	if _, role := roleOf(t, e, c); role != "admin" {
		t.Fatalf("role after refused change: %q", role)
	}
	// An admin API key may still change it (break-glass is the key itself).
	if w = e.do("PUT", "/v1/access/users/s-al/role", `{"role":"viewer"}`, withKey(e.admin)); w.Code != http.StatusNoContent {
		t.Fatalf("admin key: %d", w.Code)
	}
}
