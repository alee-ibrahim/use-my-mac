package server

import (
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/engine"
	"github.com/alee-ibrahim/use-my-mac/internal/installer"
	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// One-line Mac install. An admin creates an install token (bound to a
// profile, short-lived, single- or N-use); the Mac runs
//
//	curl -fsSL <public_url>/i/<token> | sudo sh
//
// GET /i/<token> renders the script, GET /i/<token>/releases/... serves the
// signed release from the relay cache, and the agent's enrolment uses the
// token up. Answers to /i/<token> are always a shell script (an error script
// for a bad token), so `| sudo sh` prints a clear message and fails.

// installRate is the per-IP budget for /i/ requests (script + downloads).
const installRate = 30

// rateLimiter is a fixed one-minute window per client IP.
type rateLimiter struct {
	mu    sync.Mutex
	start time.Time
	n     map[string]int
}

func (l *rateLimiter) allow(ip string, now time.Time, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil || now.Sub(l.start) >= time.Minute {
		l.n, l.start = map[string]int{}, now
	}
	l.n[ip]++
	return l.n[ip] <= limit
}

// clientIP is the address the request came from: the last X-Forwarded-For
// hop (added by the proxy in front of the router) or the TCP peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// installRefusal says why one-line install cannot be served at all ("" = ok).
func (s *Server) installRefusal(r *http.Request) string {
	if s.Install.AllowHTTP {
		return ""
	}
	if !strings.HasPrefix(s.PublicURL, "https://") {
		return "one-line install needs the router public_url to be https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" && p != "https" {
		return "use https for the install link"
	}
	return ""
}

func (s *Server) installCommand(token string) string { return installer.Command(s.PublicURL, token) }

func writeScript(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) releaseKeyLine() (string, error) {
	if s.ReleaseKey != "" {
		return s.ReleaseKey, nil
	}
	pk, err := selfupdate.ReleasePublicKey()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(pk.String()), "\n")
	return lines[len(lines)-1], nil
}

// installVersion is the agent release new Macs get: the fleet target, else
// the router's own version.
func (s *Server) installVersion() (string, error) {
	if t := s.E.UpdateSettings().TargetVersion; t != "" {
		return t, nil
	}
	if v := s.E.RouterVersion(); selfupdate.IsRelease(v) {
		return v, nil
	}
	return "", errors.New("this router is not a release build; set a target version in Settings > Agent updates")
}

func (s *Server) installScript(w http.ResponseWriter, r *http.Request) {
	token, ip := r.PathValue("token"), clientIP(r)
	if !s.limiter.allow(ip, s.Now(), installRate) {
		writeScript(w, installer.RenderError("too many requests from this address; wait a minute and try again"))
		return
	}
	if msg := s.installRefusal(r); msg != "" {
		writeScript(w, installer.RenderError(msg))
		return
	}
	tok, err := s.E.LookupInstallToken(r.Context(), token)
	if err != nil {
		if tok.ID != "" {
			s.E.RecordInstallEvent(r.Context(), tok.ID, engine.InstallEventDenied, ip, "", "script refused: link "+tok.State)
		}
		if !errors.Is(err, engine.ErrInstallToken) {
			s.Log.Error("install script", "err", err)
		}
		writeScript(w, installer.RenderError("this install link is invalid, expired, revoked or used up. Create a new one in the dashboard (Macs > Add a Mac)."))
		return
	}
	version, err := s.installVersion()
	if err != nil {
		writeScript(w, installer.RenderError(err.Error()))
		return
	}
	key, err := s.releaseKeyLine()
	if err != nil {
		s.Log.Error("install script: release key", "err", err)
		writeScript(w, installer.RenderError("the router has no valid release key"))
		return
	}
	base := s.Updates.ReleaseURL + "/" + version
	if s.Updates.RelayEnabled() {
		base = s.PublicURL + "/i/" + token + "/releases/" + version
	}
	script, err := installer.Render(installer.Params{Router: s.PublicURL, Token: token, Version: version, PublicKey: key,
		ArtifactBase: base, Profile: tok.Profile, AllowHTTP: s.Install.AllowHTTP, Root: s.InstallRoot})
	if err != nil {
		s.Log.Error("install script", "token", tok.ID, "err", err)
		writeScript(w, installer.RenderError("the install script could not be built; see the router log"))
		return
	}
	s.E.RecordInstallEvent(r.Context(), tok.ID, engine.InstallEventScript, ip, "", "agent "+version+", profile "+tok.Profile.Name)
	writeScript(w, script)
}

func (s *Server) uninstallScript(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r), s.Now(), installRate) {
		writeScript(w, installer.RenderError("too many requests from this address; wait a minute and try again"))
		return
	}
	b, err := installer.RenderUninstall(s.InstallRoot)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeScript(w, b)
}

// installArtifact serves a release file to an installing Mac from the relay
// cache. The token must be live; downloading does not use it up.
func (s *Server) installArtifact(w http.ResponseWriter, r *http.Request) {
	token, version, file, ip := r.PathValue("token"), r.PathValue("version"), r.PathValue("file"), clientIP(r)
	if !s.limiter.allow(ip, s.Now(), installRate) {
		writeErr(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	if msg := s.installRefusal(r); msg != "" {
		writeErr(w, http.StatusForbidden, msg)
		return
	}
	tok, err := s.E.LookupInstallToken(r.Context(), token)
	if err != nil {
		writeErr(w, http.StatusForbidden, "install link is invalid, expired, revoked or used up")
		return
	}
	if !s.Updates.RelayEnabled() {
		writeErr(w, http.StatusNotFound, "update relay disabled")
		return
	}
	if !validArtifact(version, file) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	key := artifactKey(version, file)
	data, err := s.cachedArtifact(r, key)
	if errors.Is(err, storage.ErrNotFound) {
		if s.Updates.Offline {
			writeErr(w, http.StatusNotFound, "artifact not in the router cache (offline mode)")
			return
		}
		data, err = s.fetchArtifact(r, version, file, key)
	}
	if err != nil {
		s.Log.Warn("install relay", "version", version, "file", file, "err", err)
		writeErr(w, http.StatusBadGateway, "could not get "+file+" for "+version)
		return
	}
	if file == selfupdate.BundleFile("darwin", "arm64") {
		s.E.RecordInstallEvent(r.Context(), tok.ID, engine.InstallEventDownload, ip, "", file+" "+version)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (s *Server) installProfiles(w http.ResponseWriter, r *http.Request) {
	out := make([]api.InstallProfile, 0, len(s.Install.Resolved))
	for _, p := range s.Install.Resolved {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b api.InstallProfile) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createInstallToken(w http.ResponseWriter, r *http.Request) {
	var req api.InstallTokenRequest
	if r.ContentLength != 0 && !readJSON(w, r, &req, 16<<10) {
		return
	}
	if req.Profile == "" {
		req.Profile = "default"
	}
	prof, ok := s.Install.Resolved[req.Profile]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown profile "+strconv.Quote(req.Profile))
		return
	}
	tok, err := s.E.CreateInstallToken(r.Context(), who(r).Subject, prof, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	tok.Command = s.installCommand(tok.Token)
	writeJSON(w, http.StatusCreated, tok)
}

func (s *Server) listInstallTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.E.InstallTokens(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) revokeInstallToken(w http.ResponseWriter, r *http.Request) {
	if err := s.E.RevokeInstallToken(r.Context(), r.PathValue("id"), who(r).Subject, clientIP(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) installTokenEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.E.InstallEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) agentSelf(w http.ResponseWriter, r *http.Request) {
	a, err := s.E.AgentSelf(r.Context(), agentID(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
