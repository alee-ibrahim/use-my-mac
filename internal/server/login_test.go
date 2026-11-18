package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/access"
	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

func TestSafeReturn(t *testing.T) {
	ok := []string{"/", "/jobs", "/jobs?x=1", "/#jobs", "/docs/usage#install", "/a/b?c=d&e=f#g", "/%0d%0a"}
	for _, in := range ok {
		if got := safeReturn(in); got != in {
			t.Errorf("safeReturn(%q) = %q, want unchanged", in, got)
		}
	}
	bad := []string{
		"", "jobs", " /jobs", "//evil.example.com", "///evil.example.com", `/\evil.example.com`, `\\evil.example.com`,
		"https://evil.example.com", "http:/evil.example.com", "javascript:alert(1)", "/\t/evil.example.com",
		"/\n/evil.example.com", "/%2F%2Fevil.example.com", "/auth/login", "/auth/callback?code=x", "/auth/../auth/login",
		"/signed-out", "/signin-error?reason=x", "/login", "/" + strings.Repeat("a", 3000),
	}
	for _, in := range bad {
		if got := safeReturn(in); got != "/" {
			t.Errorf("safeReturn(%q) = %q, want /", in, got)
		}
	}
}

func TestAuthConfigResolution(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		mode       string
		oidc, want bool
	}{
		{"", false, true}, {"auto", false, true}, {"", true, false}, {"auto", true, false},
		{"true", true, true}, {"false", false, false}, {"false", true, false}, {"TRUE", false, true},
	}
	for _, c := range cases {
		if got := (AuthConfig{DashboardAPIKeyLogin: c.mode}).APIKeyLoginAllowed(c.oidc); got != c.want {
			t.Errorf("mode %q oidc %v: got %v", c.mode, c.oidc, got)
		}
	}
	if !(AuthConfig{}).AutoSignInEnabled(true) || (AuthConfig{}).AutoSignInEnabled(false) {
		t.Error("auto sign-in defaults to on with OIDC, off without")
	}
	if (AuthConfig{AutoSignIn: &no}).AutoSignInEnabled(true) || !(AuthConfig{AutoSignIn: &yes}).AutoSignInEnabled(true) {
		t.Error("auto_sign_in override")
	}
}

func TestLoadConfigAuth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("auth:\n  dashboard_api_key_login: true\n  auto_sign_in: false\noidc:\n  domain_hint: example.com\n  prompt: select_account\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Auth.APIKeyLoginAllowed(true) || c.Auth.AutoSignInEnabled(true) || c.OIDC.DomainHint != "example.com" || c.OIDC.Prompt != "select_account" {
		t.Fatalf("parsed %+v %+v", c.Auth, c.OIDC)
	}
	os.WriteFile(p, []byte("auth:\n  dashboard_api_key_login: maybe\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("invalid dashboard_api_key_login must be rejected")
	}
}

type authEnv struct {
	srv   *Server
	h     http.Handler
	admin string // admin API key secret
	app   string
}

// fakeIssuer serves just enough discovery for NewOIDC; no tokens are minted.
func fakeIssuer(t *testing.T) string {
	var iss string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss, "authorization_endpoint": iss + "/authorize", "token_endpoint": iss + "/token",
			"jwks_uri": iss + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(ts.Close)
	iss = ts.URL
	return iss
}

func newAuthEnv(t *testing.T, withOIDC, apiKeyLogin, autoSignIn bool) *authEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	blobs, err := storage.NewLocal(t.TempDir(), "http://router.test", []byte("test-secret-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(ctx, engine.Options{Store: st, Blobs: blobs, Config: engine.DefaultConfig(), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>dashboard</html>"), 0o644)
	os.MkdirAll(filepath.Join(web, "assets"), 0o755)
	os.WriteFile(filepath.Join(web, "assets", "app.js"), []byte("js"), 0o644)
	srv := &Server{E: eng, Log: log, SessionKey: []byte("session-key-session-key-session!"), WebDir: web,
		APIKeyLogin: apiKeyLogin, AutoSignIn: autoSignIn}
	if withOIDC {
		o, err := auth.NewOIDC(ctx, auth.OIDCConfig{Issuer: fakeIssuer(t), ClientID: "cid",
			RedirectURL: "http://router.test/auth/callback", DomainHint: "example.com"})
		if err != nil {
			t.Fatal(err)
		}
		srv.OIDC = o
	}
	ak, err := eng.CreateAPIKey(ctx, "admin", api.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	app, err := eng.CreateAPIKey(ctx, "app", api.RoleApp)
	if err != nil {
		t.Fatal(err)
	}
	return &authEnv{srv: srv, h: srv.Handler(), admin: ak.Secret, app: app.Secret}
}

func (e *authEnv) do(method, target, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func cookieFrom(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

func authCfg(t *testing.T, e *authEnv) map[string]bool {
	w := e.do("GET", "/auth/config", "", nil)
	var m map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAPIKeyDashboardLoginRules(t *testing.T) {
	// OIDC on, default (auto): no API-key sessions, but Bearer keys still work.
	e := newAuthEnv(t, true, false, true)
	if c := authCfg(t, e); !c["oidc"] || c["apikey"] || !c["auto_sign_in"] {
		t.Fatalf("auth config %v", c)
	}
	w := e.do("POST", "/auth/apikey", `{"key":"`+e.admin+`"}`, nil)
	if w.Code != http.StatusForbidden || cookieFrom(w, sessionCookie) != nil {
		t.Fatalf("api-key login with OIDC: %d", w.Code)
	}
	w = e.do("GET", "/v1/me", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+e.admin) })
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"admin"`) {
		t.Fatalf("bearer admin key: %d %s", w.Code, w.Body)
	}
	// A pre-existing API-key session stops working once the path is off.
	old := auth.SignSession(e.srv.SessionKey, auth.Session{Subject: "key:x", Role: api.RoleAdmin, Via: "apikey",
		Expires: time.Now().Add(time.Hour).Unix()})
	w = e.do("GET", "/v1/me", "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: old}) })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old api-key session: %d", w.Code)
	}

	// No OIDC (auto => on): admin/viewer keys can sign in, app keys cannot.
	e = newAuthEnv(t, false, true, false)
	if c := authCfg(t, e); c["oidc"] || !c["apikey"] || c["auto_sign_in"] {
		t.Fatalf("auth config %v", c)
	}
	w = e.do("POST", "/auth/apikey", `{"key":"`+e.admin+`"}`, nil)
	sc := cookieFrom(w, sessionCookie)
	if w.Code != http.StatusOK || sc == nil {
		t.Fatalf("api-key login without OIDC: %d", w.Code)
	}
	if w = e.do("GET", "/v1/me", "", func(r *http.Request) { r.AddCookie(sc) }); w.Code != http.StatusOK {
		t.Fatalf("api-key session: %d", w.Code)
	}
	if w = e.do("POST", "/auth/apikey", `{"key":"`+e.app+`"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("app key login: %d", w.Code)
	}
	if w = e.do("POST", "/auth/apikey", `{"key":"umm_key_nope"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key login: %d", w.Code)
	}

	// Break-glass: OIDC on and API-key login explicitly enabled.
	e = newAuthEnv(t, true, true, true)
	if w = e.do("POST", "/auth/apikey", `{"key":"`+e.admin+`"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("break-glass api-key login: %d", w.Code)
	}
}

func TestAutoSignIn(t *testing.T) {
	e := newAuthEnv(t, true, false, true)
	w := e.do("GET", "/jobs?x=1", "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("signed-out dashboard route: %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasSuffix(loc.Path, "/authorize") || loc.Query().Get("domain_hint") != "example.com" ||
		loc.Query().Get("code_challenge") == "" {
		t.Fatalf("redirect %s", loc)
	}
	lc := cookieFrom(w, loginCookie)
	if lc == nil || cookieFrom(w, autoCookie) == nil {
		t.Fatal("login and guard cookies must be set")
	}
	st, err := auth.DecodeState(e.srv.SessionKey, lc.Value, time.Now())
	if err != nil || st.Return != "/jobs?x=1" {
		t.Fatalf("return path %v %v", st, err)
	}

	// Root and /index.html redirect too; public pages, assets and the API do not.
	for _, p := range []string{"/", "/index.html"} {
		if w = e.do("GET", p, "", nil); w.Code != http.StatusFound {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/docs", "/docs/usage", "/signed-out", "/signin-error?reason=state", "/login", "/assets/app.js"} {
		if w = e.do("GET", p, "", nil); w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w = e.do("GET", "/v1/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/me: %d", w.Code)
	}
	// A recent attempt (guard cookie) serves the page instead of looping.
	guard := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: autoCookie, Value: "1"}) }
	if w = e.do("GET", "/", "", guard); w.Code != http.StatusOK {
		t.Fatalf("guarded /: %d", w.Code)
	}
	// Signed in: the dashboard is served.
	if _, err := e.srv.Access.PutRule(context.Background(), "key:test", "", api.AccessRuleRequest{Kind: "user", Value: "u", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.Access.RecordLogin(context.Background(), access.User{Sub: "u", Name: "U"}); err != nil {
		t.Fatal(err)
	}
	sess := auth.SignSession(e.srv.SessionKey, auth.Session{Subject: "user:u", Role: api.RoleViewer, Via: "oidc",
		Expires: time.Now().Add(time.Hour).Unix()})
	w = e.do("GET", "/", "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess}) })
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "dashboard") {
		t.Fatalf("signed-in /: %d", w.Code)
	}

	// Explicit login sanitises the return target.
	w = e.do("GET", "/auth/login?return="+url.QueryEscape("//evil.example.com/x"), "", nil)
	st, err = auth.DecodeState(e.srv.SessionKey, cookieFrom(w, loginCookie).Value, time.Now())
	if err != nil || st.Return != "/" {
		t.Fatalf("open redirect via return: %v %v", st, err)
	}

	// Auto sign-in off: the page is served and shows its own sign-in button.
	e = newAuthEnv(t, true, false, false)
	if w = e.do("GET", "/", "", nil); w.Code != http.StatusOK {
		t.Fatalf("auto sign-in off: %d", w.Code)
	}
	// No OIDC: never redirects.
	e = newAuthEnv(t, false, true, true)
	if w = e.do("GET", "/", "", nil); w.Code != http.StatusOK {
		t.Fatalf("no oidc: %d", w.Code)
	}
}

func TestCallbackFailureGoesToErrorPage(t *testing.T) {
	e := newAuthEnv(t, true, false, true)
	// No login cookie.
	w := e.do("GET", "/auth/callback?state=x&code=y", "", nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/signin-error?") {
		t.Fatalf("missing cookie: %d %s", w.Code, w.Header().Get("Location"))
	}
	// Provider error keeps the (sanitised) return path for "try again".
	w = e.do("GET", "/auth/login?return=/jobs", "", nil)
	lc := cookieFrom(w, loginCookie)
	st, _ := auth.DecodeState(e.srv.SessionKey, lc.Value, time.Now())
	w = e.do("GET", "/auth/callback?state="+st.State+"&error=access_denied", "", func(r *http.Request) { r.AddCookie(lc) })
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || loc.Path != "/signin-error" || loc.Query().Get("reason") != "provider" ||
		loc.Query().Get("return") != "/jobs" {
		t.Fatalf("provider error: %d %s", w.Code, loc)
	}
	// Wrong state.
	w = e.do("GET", "/auth/callback?state=wrong&code=y", "", func(r *http.Request) { r.AddCookie(lc) })
	if loc, _ = url.Parse(w.Header().Get("Location")); loc.Query().Get("reason") != "state" {
		t.Fatalf("bad state: %s", loc)
	}
}
