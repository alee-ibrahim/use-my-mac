package server

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

const sessionTTL = 12 * time.Hour

// autoCookie marks a recent automatic SSO redirect. While it is set, a
// signed-out browser gets the dashboard's sign-in page instead of another
// redirect, so a broken login can never loop.
const autoCookie = "umm_auto"
const autoGuard = 30 * time.Second

// Dashboard routes that render without a session, so they never trigger an
// automatic SSO redirect.
var publicPages = []string{"/docs", "/login", "/signed-out", "/signin-error"}

func isPublicPage(p string) bool {
	for _, pub := range publicPages {
		if p == pub || strings.HasPrefix(p, pub+"/") {
			return true
		}
	}
	return false
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl / time.Second),
	})
}

// safeReturn turns an untrusted "return to" value into a same-origin path.
// Anything that could leave the site (absolute or scheme-relative URLs,
// backslashes, control characters) or that would loop back into the login
// flow becomes "/".
func safeReturn(raw string) string {
	if raw == "" || len(raw) > 2048 || raw[0] != '/' || strings.HasPrefix(raw, "//") {
		return "/"
	}
	for _, c := range raw {
		if c < 0x20 || c == 0x7f || c == 0x5c { // 0x5c = backslash
			return "/"
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	p := path.Clean("/" + u.Path)
	if strings.HasPrefix(u.Path, "//") || p == "/auth" || strings.HasPrefix(p, "/auth/") ||
		p == "/signed-out" || p == "/signin-error" || p == "/login" {
		return "/"
	}
	return raw
}

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"oidc": s.OIDC != nil, "apikey": s.APIKeyLogin, "auto_sign_in": s.AutoSignIn && s.OIDC != nil,
	})
}

// startLogin redirects the browser to the identity provider.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request, ret string) {
	exp := s.Now().Add(10 * time.Minute)
	u, st := s.OIDC.AuthURL(safeReturn(ret), exp.Unix())
	s.setCookie(w, loginCookie, auth.EncodeState(s.SessionKey, st), 10*time.Minute)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

// autoSignIn handles a signed-out browser on a dashboard route: with OIDC
// auto sign-in on, it goes straight to the identity provider and comes back
// to the page it asked for. Reports whether it wrote a response.
func (s *Server) autoSignIn(w http.ResponseWriter, r *http.Request, p string) bool {
	if !s.AutoSignIn || s.OIDC == nil || isPublicPage(p) {
		return false
	}
	if id, _ := s.identify(r); id != nil {
		return false
	}
	if _, err := r.Cookie(autoCookie); err == nil {
		return false // just tried; let the page offer a button instead of looping
	}
	s.setCookie(w, autoCookie, "1", autoGuard)
	s.startLogin(w, r, r.URL.RequestURI())
	return true
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	s.startLogin(w, r, r.URL.Query().Get("return"))
}

// loginFailed sends the browser to the dashboard's sign-in error page, which
// offers "try again" rather than redirecting on its own.
func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, code int, reason, ret string) {
	if s.WebDir == "" {
		writeErr(w, code, "sign-in failed: "+reason)
		return
	}
	q := url.Values{"reason": {reason}, "return": {safeReturn(ret)}}
	http.Redirect(w, r, "/signin-error?"+q.Encode(), http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	c, err := r.Cookie(loginCookie)
	if err != nil {
		s.loginFailed(w, r, http.StatusBadRequest, "expired", "/")
		return
	}
	st, err := auth.DecodeState(s.SessionKey, c.Value, s.Now())
	if err != nil || st.State != r.URL.Query().Get("state") {
		s.loginFailed(w, r, http.StatusBadRequest, "state", "/")
		return
	}
	s.setCookie(w, loginCookie, "", -time.Second)
	if e := r.URL.Query().Get("error"); e != "" {
		s.Log.Warn("oidc provider error", "error", e, "description", r.URL.Query().Get("error_description"))
		s.loginFailed(w, r, http.StatusUnauthorized, "provider", st.Return)
		return
	}
	claims, err := s.OIDC.Exchange(r.Context(), *st, r.URL.Query().Get("code"))
	if err != nil {
		s.Log.Warn("oidc exchange failed", "err", err)
		s.loginFailed(w, r, http.StatusUnauthorized, "exchange", st.Return)
		return
	}
	role, err := s.OIDC.Cfg.MapRole(claims)
	if err != nil {
		reason := "forbidden"
		if errors.Is(err, auth.ErrGroupOverage) {
			s.Log.Warn("oidc groups overage", "sub", claims["sub"])
			reason = "overage"
		}
		s.loginFailed(w, r, http.StatusForbidden, reason, st.Return)
		return
	}
	sub, _ := claims["sub"].(string)
	sess := auth.Session{Subject: "user:" + sub, Name: s.OIDC.Cfg.DisplayName(claims), Role: role, Via: "oidc",
		Expires: s.Now().Add(sessionTTL).Unix()}
	s.setCookie(w, sessionCookie, auth.SignSession(s.SessionKey, sess), sessionTTL)
	s.setCookie(w, autoCookie, "", -time.Second)
	http.Redirect(w, r, safeReturn(st.Return), http.StatusFound)
}

// apiKeyLogin lets admins/viewers sign in to the dashboard with an API key.
// It is on by default only when SSO is not configured; with SSO it is a
// break-glass option (auth.dashboard_api_key_login: true). Bearer API keys
// for the REST API are unaffected either way.
func (s *Server) apiKeyLogin(w http.ResponseWriter, r *http.Request) {
	if !s.APIKeyLogin {
		writeErr(w, http.StatusForbidden, "API-key sign-in to the dashboard is disabled; use single sign-on")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if !readJSON(w, r, &req, 4096) {
		return
	}
	k, err := s.E.AuthAPIKey(r.Context(), strings.TrimSpace(req.Key))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid key")
		return
	}
	if k.Role != api.RoleAdmin && k.Role != api.RoleViewer {
		writeErr(w, http.StatusForbidden, "app keys cannot sign in to the dashboard")
		return
	}
	sess := auth.Session{Subject: "key:" + k.ID, Name: k.Name, Role: k.Role, Via: "apikey",
		Expires: s.Now().Add(sessionTTL).Unix()}
	s.setCookie(w, sessionCookie, auth.SignSession(s.SessionKey, sess), sessionTTL)
	writeJSON(w, http.StatusOK, api.Me{Subject: sess.Subject, Name: sess.Name, Role: sess.Role, Via: sess.Via})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, sessionCookie, "", -time.Second)
	w.WriteHeader(http.StatusNoContent)
}
