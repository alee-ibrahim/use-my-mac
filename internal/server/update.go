package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/selfupdate"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Agent self-update: policy (admin), check/report (agents) and the artifact
// relay. The relay is a cache, not a trust anchor: agents verify the
// signature themselves, so the router never needs the signing key.

func (s *Server) updatesInfo() api.UpdatesInfo {
	info := api.UpdatesInfo{Settings: s.E.UpdateSettings(), RouterVersion: s.E.RouterVersion(), Relay: s.Updates.RelayEnabled()}
	if info.Relay && !s.Updates.Offline {
		info.ReleaseURL = s.Updates.ReleaseURL
	}
	return info
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.updatesInfo())
}

func (s *Server) putUpdates(w http.ResponseWriter, r *http.Request) {
	var st api.UpdateSettings
	if !readJSON(w, r, &st, 64<<10) {
		return
	}
	if err := s.E.SetUpdateSettings(r.Context(), st); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("agent update settings changed", "by", who(r).Subject, "target", st.TargetVersion,
		"canary", st.CanaryVersion, "paused", st.Paused)
	writeJSON(w, http.StatusOK, s.updatesInfo())
}

func (s *Server) macUpdateEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := s.E.UpdateEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) agentUpdateCheck(w http.ResponseWriter, r *http.Request) {
	c, err := s.E.UpdateCheck(r.Context(), agentID(r), s.Updates.RelayEnabled())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) agentUpdateReport(w http.ResponseWriter, r *http.Request) {
	var rep api.UpdateReport
	if !readJSON(w, r, &rep, 16<<10) {
		return
	}
	if err := s.E.UpdateReport(r.Context(), agentID(r), rep); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func artifactKey(version, file string) string { return "agent-releases/" + version + "/" + file }

func validArtifact(version, file string) bool {
	return selfupdate.IsRelease(version) && selfupdate.ArtifactAllowed(file)
}

// agentArtifact serves a release artifact from the cache, fetching it from
// the release host on a miss (unless offline).
func (s *Server) agentArtifact(w http.ResponseWriter, r *http.Request) {
	version, file := r.PathValue("version"), r.PathValue("file")
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
		s.Log.Warn("update relay", "version", version, "file", file, "err", err)
		writeErr(w, http.StatusBadGateway, "could not get "+file+" for "+version)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (s *Server) cachedArtifact(r *http.Request, key string) ([]byte, error) {
	rc, err := s.E.Blobs().Get(r.Context(), key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readLimited(rc)
}

// fetchArtifact downloads one artifact into the cache. One fetch at a time:
// a fleet asking at once should hit the release host once.
func (s *Server) fetchArtifact(r *http.Request, version, file, key string) ([]byte, error) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if b, err := s.cachedArtifact(r, key); err == nil {
		return b, nil
	}
	hc := s.ReleaseHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	b, err := selfupdate.FetchURL(r.Context(), hc, s.Updates.ReleaseURL+"/"+version+"/"+file)
	if err != nil {
		return nil, err
	}
	if err := s.E.Blobs().Put(r.Context(), key, bytes.NewReader(b), int64(len(b))); err != nil {
		s.Log.Warn("update relay: cache write failed", "key", key, "err", err)
	}
	return b, nil
}

// putArtifact lets an admin seed the cache (routers without internet).
func (s *Server) putArtifact(w http.ResponseWriter, r *http.Request) {
	version, file := r.PathValue("version"), r.PathValue("file")
	if !validArtifact(version, file) {
		writeErr(w, http.StatusBadRequest, "version must be vX.Y.Z and file one of SHA256SUMS, SHA256SUMS.minisig, "+
			selfupdate.BundleFile("darwin", "arm64"))
		return
	}
	data, err := readLimited(http.MaxBytesReader(w, r.Body, selfupdate.MaxArtifactBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.E.Blobs().Put(r.Context(), artifactKey(version, file), bytes.NewReader(data), int64(len(data))); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"version": version, "file": file, "bytes": len(data)})
}

func readLimited(rd io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(rd, selfupdate.MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > selfupdate.MaxArtifactBytes {
		return nil, errors.New("artifact too large")
	}
	return b, nil
}
