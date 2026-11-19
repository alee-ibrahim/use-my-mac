// Package server exposes the engine over HTTP: the app API, the admin API,
// the agent protocol, OIDC/API-key login for the dashboard, signed blob URLs
// for the local storage backend, Prometheus metrics and the static dashboard.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/alee-ibrahim/use-my-mac/internal/access"
	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/metrics"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Server is the HTTP layer.
type Server struct {
	E          *engine.Engine
	Log        *slog.Logger
	Metrics    *metrics.M
	SessionKey []byte
	OIDC       *auth.OIDC // nil when SSO is not configured
	// Access decides SSO users' roles (config rules + dashboard rules).
	// Built from the OIDC config when nil.
	Access *access.Manager
	// APIKeyLogin allows exchanging an API key for a dashboard session
	// (POST /auth/apikey). Bearer API keys work regardless.
	APIKeyLogin bool
	// AutoSignIn sends signed-out browsers on dashboard routes straight to
	// the OIDC provider. Needs OIDC.
	AutoSignIn bool
	WebDir     string // built dashboard; "" disables
	// MaxUploadBytes bounds app blob uploads and agent output uploads.
	MaxUploadBytes int64
	// SecureCookies sets the Secure flag (use behind HTTPS).
	SecureCookies bool
	// PollWaitMax caps agent long-poll duration.
	PollWaitMax time.Duration
	Now         func() time.Time
	// Updates configures the agent update relay.
	Updates UpdatesConfig
	// ReleaseHTTP fetches release artifacts for the relay (default client).
	ReleaseHTTP *http.Client
	// PublicURL is the router's public URL (install commands embed it).
	PublicURL string
	// Install configures the one-line Mac install.
	Install InstallConfig
	// ReleaseKey overrides the release public key embedded in install
	// scripts (tests). Default: the key compiled into the agent.
	ReleaseKey string
	// InstallRoot prefixes every path an install script touches. Tests only;
	// never set in production.
	InstallRoot string

	relayMu sync.Mutex
	limiter rateLimiter
}

const sessionCookie = "umm_session"
const loginCookie = "umm_login"

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.PollWaitMax == 0 {
		s.PollWaitMax = 30 * time.Second
	}
	if s.MaxUploadBytes == 0 {
		s.MaxUploadBytes = 20 << 30
	}
	if s.Access == nil && s.E != nil {
		var cfg access.Config
		if s.OIDC != nil {
			cfg = access.ConfigFromOIDC(s.OIDC.Cfg)
		}
		s.Access = access.New(s.E.Store(), cfg, s.Now, s.Log)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	if s.Metrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	}

	// Dashboard auth.
	mux.HandleFunc("GET /auth/config", s.authConfig)
	mux.HandleFunc("GET /auth/login", s.oidcLogin)
	mux.HandleFunc("GET /auth/callback", s.oidcCallback)
	mux.HandleFunc("POST /auth/apikey", s.apiKeyLogin)
	mux.HandleFunc("POST /auth/logout", s.logout)

	// Built-in documentation (public; rendered by the dashboard at /docs).
	mux.HandleFunc("GET /v1/docs", s.listDocs)
	mux.HandleFunc("GET /v1/docs/{slug}", s.getDoc)

	// App / viewer / admin API.
	mux.HandleFunc("GET /v1/me", s.need(roleAny, s.me))
	mux.HandleFunc("POST /v1/jobs", s.need(roleSubmit, s.submitJob))
	mux.HandleFunc("GET /v1/jobs", s.need(roleAny, s.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", s.need(roleAny, s.getJob))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.need(roleSubmit, s.cancelJob))
	mux.HandleFunc("GET /v1/tasks/{id}", s.need(roleAny, s.getTask))
	mux.HandleFunc("GET /v1/tasks/{id}/logs", s.need(roleAny, s.taskLogs))
	mux.HandleFunc("GET /v1/tasks/{id}/outputs/{name}", s.need(roleAny, s.taskOutput))
	mux.HandleFunc("PUT /v1/blobs/{key...}", s.need(roleSubmit, s.putBlob))
	mux.HandleFunc("GET /v1/blobs/{key...}", s.need(roleSubmit, s.getBlob))
	mux.HandleFunc("GET /v1/macs", s.need(roleAny, s.listMacs))
	mux.HandleFunc("PATCH /v1/macs/{id}", s.need(roleAdmin, s.patchMac))
	mux.HandleFunc("DELETE /v1/macs/{id}", s.need(roleAdmin, s.deleteMac))
	mux.HandleFunc("POST /v1/enrolment-tokens", s.need(roleAdmin, s.createEnrolToken))
	mux.HandleFunc("GET /v1/api-keys", s.need(roleAdmin, s.listKeys))
	mux.HandleFunc("POST /v1/api-keys", s.need(roleAdmin, s.createKey))
	mux.HandleFunc("DELETE /v1/api-keys/{id}", s.need(roleAdmin, s.revokeKey))
	mux.HandleFunc("GET /v1/settings", s.need(roleRead, s.getSettings))
	mux.HandleFunc("PUT /v1/settings", s.need(roleAdmin, s.putSettings))
	mux.HandleFunc("GET /v1/updates", s.need(roleRead, s.getUpdates))
	mux.HandleFunc("PUT /v1/updates", s.need(roleAdmin, s.putUpdates))
	mux.HandleFunc("PUT /v1/updates/artifacts/{version}/{file}", s.need(roleAdmin, s.putArtifact))
	mux.HandleFunc("GET /v1/macs/{id}/updates", s.need(roleRead, s.macUpdateEvents))
	mux.HandleFunc("GET /v1/runtimes", s.need(roleRead, s.listRuntimes))
	mux.HandleFunc("PUT /v1/runtimes", s.need(roleAdmin, s.putRuntime))
	mux.HandleFunc("DELETE /v1/runtimes/{name}", s.need(roleAdmin, s.deleteRuntime))
	mux.HandleFunc("PUT /v1/runtimes/{name}/prewarm", s.need(roleAdmin, s.putRuntimePrewarm))
	mux.HandleFunc("GET /v1/access", s.need(roleAdmin, s.getAccess))
	mux.HandleFunc("POST /v1/access/rules", s.need(roleAdmin, s.putAccessRule))
	mux.HandleFunc("DELETE /v1/access/rules/{id}", s.need(roleAdmin, s.deleteAccessRule))
	mux.HandleFunc("PUT /v1/access/default", s.need(roleAdmin, s.putAccessDefault))
	mux.HandleFunc("PUT /v1/access/groups/{id}", s.need(roleAdmin, s.putAccessGroupName))
	mux.HandleFunc("GET /v1/access/users", s.need(roleAdmin, s.accessUsers))
	mux.HandleFunc("PUT /v1/access/users/{sub}/role", s.need(roleAdmin, s.putAccessUserRole))
	mux.HandleFunc("GET /v1/access/audit", s.need(roleAdmin, s.accessAudit))
	mux.HandleFunc("GET /v1/install/profiles", s.need(roleAdmin, s.installProfiles))
	mux.HandleFunc("GET /v1/install-tokens", s.need(roleAdmin, s.listInstallTokens))
	mux.HandleFunc("POST /v1/install-tokens", s.need(roleAdmin, s.createInstallToken))
	mux.HandleFunc("DELETE /v1/install-tokens/{id}", s.need(roleAdmin, s.revokeInstallToken))
	mux.HandleFunc("GET /v1/install-tokens/{id}/events", s.need(roleAdmin, s.installTokenEvents))

	s.opsRoutes(mux)
	s.uploadRoutes(mux)

	// One-line install (no auth header: the token in the path is the secret).
	mux.HandleFunc("GET /i/uninstall", s.uninstallScript)
	mux.HandleFunc("GET /i/{token}", s.installScript)
	mux.HandleFunc("GET /i/{token}/releases/{version}/{file}", s.installArtifact)

	// Agent protocol (outbound from the Macs).
	mux.HandleFunc("POST /v1/agent/enrol", s.agentEnrol)
	mux.HandleFunc("POST /v1/agent/heartbeat", s.agent(s.agentHeartbeat))
	mux.HandleFunc("GET /v1/agent/poll", s.agent(s.agentPoll))
	mux.HandleFunc("POST /v1/agent/tasks/{id}/status", s.agent(s.agentStatus))
	mux.HandleFunc("POST /v1/agent/tasks/{id}/logs", s.agent(s.agentLogs))
	mux.HandleFunc("GET /v1/agent/update", s.agent(s.agentUpdateCheck))
	mux.HandleFunc("POST /v1/agent/update/report", s.agent(s.agentUpdateReport))
	mux.HandleFunc("GET /v1/agent/update/artifacts/{version}/{file}", s.agent(s.agentArtifact))
	mux.HandleFunc("GET /v1/agent/self", s.agent(s.agentSelf))
	mux.HandleFunc("GET /v1/agent/runtimes", s.agent(s.agentRuntimes))
	mux.HandleFunc("GET "+storage.SignedPath, s.signedBlob)
	mux.HandleFunc("PUT "+storage.SignedPath, s.signedBlob)

	if s.WebDir != "" {
		mux.Handle("GET /", s.static())
	}
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("X-Frame-Options", "DENY")
		}
		h.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ve *engine.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, engine.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, engine.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, engine.ErrActive):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrStale):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, storage.ErrBadKey):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		s.Log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Identity and roles
// ---------------------------------------------------------------------------

type identity struct {
	Subject string // owner string for jobs: "key:<id>" or "user:<sub>"
	Name    string
	Role    string
	Via     string
}

type ctxKey int

const identKey ctxKey = 1

type roleCheck func(role string) bool

func roleAny(string) bool      { return true }
func roleRead(r string) bool   { return r == api.RoleAdmin || r == api.RoleViewer }
func roleAdmin(r string) bool  { return r == api.RoleAdmin }
func roleSubmit(r string) bool { return r == api.RoleAdmin || r == api.RoleApp }

func (s *Server) identify(r *http.Request) (*identity, int) {
	if tok := bearer(r); tok != "" {
		if !strings.HasPrefix(tok, auth.PrefixAPIKey) {
			return nil, http.StatusUnauthorized
		}
		k, err := s.E.AuthAPIKey(r.Context(), tok)
		if err != nil {
			return nil, http.StatusUnauthorized
		}
		return &identity{Subject: "key:" + k.ID, Name: k.Name, Role: k.Role, Via: "apikey"}, 0
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, http.StatusUnauthorized
	}
	sess, err := auth.VerifySession(s.SessionKey, c.Value, s.Now())
	if err != nil || sess.Via == "login-state" {
		return nil, http.StatusUnauthorized
	}
	// Sessions made from an API key die when that login path is turned off.
	if sess.Via == "apikey" && !s.APIKeyLogin {
		return nil, http.StatusUnauthorized
	}
	// CSRF: cookie-authenticated writes must carry a custom header, which
	// browsers will not send cross-site without a CORS preflight we never allow.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-UMM-CSRF") != "1" {
		return nil, http.StatusForbidden
	}
	role := sess.Role
	if sess.Via == "oidc" && s.Access != nil {
		// Re-evaluate on every request so role changes apply at once. A
		// session from before roles were tracked has no user record: sign in
		// again (auto sign-in makes that seamless).
		sub, _ := strings.CutPrefix(sess.Subject, "user:")
		d, known, err := s.Access.Resolve(r.Context(), sub)
		if err != nil {
			s.Log.Error("access resolve", "err", err)
			return nil, http.StatusServiceUnavailable
		}
		if !known || d.Denied() {
			return nil, http.StatusUnauthorized
		}
		role = d.Role
	}
	return &identity{Subject: sess.Subject, Name: sess.Name, Role: role, Via: sess.Via}, 0
}

func (s *Server) need(check roleCheck, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, code := s.identify(r)
		if id == nil {
			writeErr(w, code, http.StatusText(code))
			return
		}
		if !check(id.Role) {
			writeErr(w, http.StatusForbidden, "your role ("+id.Role+") cannot do this")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), identKey, id)))
	}
}

func who(r *http.Request) *identity { return r.Context().Value(identKey).(*identity) }

// canSee: apps only see their own jobs; viewers and admins see everything.
func canSee(id *identity, owner string) bool {
	return id.Role == api.RoleAdmin || id.Role == api.RoleViewer || id.Subject == owner
}

// ---------------------------------------------------------------------------
// App API
// ---------------------------------------------------------------------------

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	writeJSON(w, http.StatusOK, api.Me{Subject: id.Subject, Name: id.Name, Role: id.Role, Via: id.Via})
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	var spec api.JobSpec
	if !readJSON(w, r, &spec, 32<<20) {
		return
	}
	j, err := s.E.SubmitJob(r.Context(), who(r).Subject, spec)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	owner := ""
	if !roleRead(id.Role) {
		owner = id.Subject
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	js, err := s.E.Jobs(r.Context(), r.URL.Query().Get("state"), owner, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, js)
}

func (s *Server) loadJob(w http.ResponseWriter, r *http.Request, id string, tasks bool) *api.Job {
	j, err := s.E.Job(r.Context(), id, tasks)
	if err != nil {
		s.fail(w, err)
		return nil
	}
	if !canSee(who(r), j.Owner) {
		writeErr(w, http.StatusNotFound, "not found")
		return nil
	}
	return j
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if j := s.loadJob(w, r, r.PathValue("id"), true); j != nil {
		writeJSON(w, http.StatusOK, j)
	}
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if s.loadJob(w, r, r.PathValue("id"), false) == nil {
		return
	}
	j, err := s.E.CancelJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) loadTask(w http.ResponseWriter, r *http.Request) *engine.TaskDetail {
	t, err := s.E.Task(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return nil
	}
	if s.loadJob(w, r, t.JobID, false) == nil {
		return nil
	}
	return t
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if t := s.loadTask(w, r); t != nil {
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) taskLogs(w http.ResponseWriter, r *http.Request) {
	t := s.loadTask(w, r)
	if t == nil {
		return
	}
	attempt, _ := strconv.Atoi(r.URL.Query().Get("attempt"))
	logs, err := s.E.Logs(r.Context(), t.ID, attempt)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, logs)
}

func (s *Server) taskOutput(w http.ResponseWriter, r *http.Request) {
	t := s.loadTask(w, r)
	if t == nil {
		return
	}
	rc, err := s.E.OpenOutput(r.Context(), t.ID, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("name")+`"`)
	serveBlob(w, r, rc)
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := storage.ValidKey(key); err != nil {
		s.fail(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUploadBytes)
	if err := s.E.Blobs().Put(r.Context(), engine.UploadKey(key), r.Body, r.ContentLength); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"blob": key})
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := storage.ValidKey(key); err != nil {
		s.fail(w, err)
		return
	}
	rc, err := s.E.Blobs().Get(r.Context(), engine.UploadKey(key))
	if err != nil {
		s.fail(w, err)
		return
	}
	serveBlob(w, r, rc)
}

func (s *Server) listMacs(w http.ResponseWriter, r *http.Request) {
	macs, err := s.E.Macs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, macs)
}

func (s *Server) patchMac(w http.ResponseWriter, r *http.Request) {
	var p api.MacPatch
	if !readJSON(w, r, &p, 4096) {
		return
	}
	if err := s.E.PatchMac(r.Context(), r.PathValue("id"), p); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteMac(w http.ResponseWriter, r *http.Request) {
	if err := s.E.DeleteMacBy(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createEnrolToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TTLHours int `json:"ttl_hours"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req, 4096) {
		return
	}
	tok, err := s.E.CreateEnrolToken(r.Context(), who(r).Subject, time.Duration(req.TTLHours)*time.Hour)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.E.APIKeys(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !readJSON(w, r, &req, 4096) {
		return
	}
	k, err := s.E.CreateAPIKey(r.Context(), req.Name, req.Role)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.E.Audit(r.Context(), api.EventKeyCreated, actor(r), k.ID, "created "+k.Role+" API key "+k.Name)
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.E.RevokeAPIKey(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.E.Audit(r.Context(), api.EventKeyRevoked, actor(r), r.PathValue("id"), "revoked API key "+r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.E.Settings())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var st engine.SchedulerSettings
	if !readJSON(w, r, &st, 64<<10) {
		return
	}
	if err := s.E.SetSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.E.Settings())
}

// ---------------------------------------------------------------------------
// Agent protocol
// ---------------------------------------------------------------------------

type agentKey int

const agentIDKey agentKey = 1

func (s *Server) agent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !strings.HasPrefix(tok, auth.PrefixAgent) {
			writeErr(w, http.StatusUnauthorized, "agent credential required")
			return
		}
		id, err := s.E.AuthAgent(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown agent credential")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), agentIDKey, id)))
	}
}

func agentID(r *http.Request) string { return r.Context().Value(agentIDKey).(string) }

func (s *Server) agentEnrol(w http.ResponseWriter, r *http.Request) {
	var req api.EnrolRequest
	if !readJSON(w, r, &req, 64<<10) {
		return
	}
	resp, err := s.E.EnrolFrom(r.Context(), req, clientIP(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb api.Heartbeat
	if !readJSON(w, r, &hb, 1<<20) {
		return
	}
	resp, err := s.E.Heartbeat(r.Context(), agentID(r), hb)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) agentPoll(w http.ResponseWriter, r *http.Request) {
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > s.PollWaitMax {
		wait = s.PollWaitMax
	}
	as, err := s.E.Poll(r.Context(), agentID(r), wait)
	if err != nil {
		s.fail(w, err)
		return
	}
	if as == nil {
		as = []api.Assignment{}
	}
	writeJSON(w, http.StatusOK, api.PollResponse{Assignments: as})
}

func (s *Server) agentStatus(w http.ResponseWriter, r *http.Request) {
	var u api.TaskUpdate
	if !readJSON(w, r, &u, 64<<10) {
		return
	}
	if err := s.E.TaskUpdate(r.Context(), agentID(r), r.PathValue("id"), u); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentLogs(w http.ResponseWriter, r *http.Request) {
	var c api.LogChunk
	if !readJSON(w, r, &c, 2<<20) {
		return
	}
	if err := s.E.AppendLog(r.Context(), agentID(r), r.PathValue("id"), c); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// signedBlob serves HMAC-signed URLs for the local storage backend. No
// bearer token: the signature is the credential and it expires.
func (s *Server) signedBlob(w http.ResponseWriter, r *http.Request) {
	l, ok := s.E.Blobs().(*storage.Local)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	key, err := l.Verify(r.Method, r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !signedPartAllowed(l, key) {
			writeErr(w, http.StatusNotFound, "upload not found (finished, aborted or expired)")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxUploadBytes)
		if err := l.Put(r.Context(), key, r.Body, r.ContentLength); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		rc, err := l.Get(r.Context(), key)
		if err != nil {
			s.fail(w, err)
			return
		}
		serveBlob(w, r, rc)
	}
}

// ---------------------------------------------------------------------------
// Static dashboard (SPA)
// ---------------------------------------------------------------------------

func (s *Server) static() http.Handler {
	fs := http.FileServer(http.Dir(s.WebDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		full := filepath.Join(s.WebDir, filepath.FromSlash(p))
		if st, err := os.Stat(full); err != nil || st.IsDir() || p == "/index.html" {
			// SPA fallback. Signed-out browsers go straight to SSO.
			if s.autoSignIn(w, r, p) {
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(s.WebDir, "index.html"))
			return
		}
		if strings.HasPrefix(p, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fs.ServeHTTP(w, r)
	})
}
