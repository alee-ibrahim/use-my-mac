package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

const sessionTTL = 12 * time.Hour

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.SecureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl / time.Second),
	})
}

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"oidc": s.OIDC != nil, "apikey": true})
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	ret := r.URL.Query().Get("return")
	if !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		ret = "/"
	}
	exp := s.Now().Add(10 * time.Minute)
	u, st := s.OIDC.AuthURL(ret, exp.Unix())
	s.setCookie(w, loginCookie, auth.EncodeState(s.SessionKey, st), 10*time.Minute)
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	c, err := r.Cookie(loginCookie)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "login expired, try again")
		return
	}
	st, err := auth.DecodeState(s.SessionKey, c.Value, s.Now())
	if err != nil || st.State != r.URL.Query().Get("state") {
		writeErr(w, http.StatusBadRequest, "invalid login state")
		return
	}
	s.setCookie(w, loginCookie, "", -time.Second)
	if e := r.URL.Query().Get("error"); e != "" {
		writeErr(w, http.StatusUnauthorized, "identity provider: "+e)
		return
	}
	claims, err := s.OIDC.Exchange(r.Context(), *st, r.URL.Query().Get("code"))
	if err != nil {
		s.Log.Warn("oidc exchange failed", "err", err)
		writeErr(w, http.StatusUnauthorized, "login failed")
		return
	}
	role, err := s.OIDC.Cfg.MapRole(claims)
	if err != nil {
		if errors.Is(err, auth.ErrGroupOverage) {
			s.Log.Warn("oidc groups overage", "sub", claims["sub"])
		}
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	sub, _ := claims["sub"].(string)
	sess := auth.Session{Subject: "user:" + sub, Name: s.OIDC.Cfg.DisplayName(claims), Role: role, Via: "oidc",
		Expires: s.Now().Add(sessionTTL).Unix()}
	s.setCookie(w, sessionCookie, auth.SignSession(s.SessionKey, sess), sessionTTL)
	http.Redirect(w, r, st.Return, http.StatusFound)
}

// apiKeyLogin lets admins/viewers sign in to the dashboard with an API key
// when SSO is not configured (or as break-glass access).
func (s *Server) apiKeyLogin(w http.ResponseWriter, r *http.Request) {
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
