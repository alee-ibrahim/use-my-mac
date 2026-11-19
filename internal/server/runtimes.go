package server

import (
	"net/http"

	"github.com/alee-ibrahim/use-my-mac/internal/runtimes"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Managed runtime catalog: admins publish signed packages, everyone who
// can read the fleet sees the catalog and where each package is installed,
// and agents fetch it with resolved download URLs.

func (s *Server) listRuntimes(w http.ResponseWriter, r *http.Request) {
	c, err := s.E.RuntimeCatalog(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) putRuntime(w http.ResponseWriter, r *http.Request) {
	var req api.RuntimePublish
	if !readJSON(w, r, &req, 2*runtimes.MaxManifestBytes) {
		return
	}
	e, err := s.E.PublishRuntime(r.Context(), req, actor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) deleteRuntime(w http.ResponseWriter, r *http.Request) {
	if err := s.E.DeleteRuntime(r.Context(), r.PathValue("name"), actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putRuntimePrewarm(w http.ResponseWriter, r *http.Request) {
	var req api.RuntimePrewarm
	if !readJSON(w, r, &req, 1024) {
		return
	}
	if err := s.E.SetRuntimePrewarm(r.Context(), r.PathValue("name"), req.Prewarm, actor(r)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentRuntimes(w http.ResponseWriter, r *http.Request) {
	c, err := s.E.AgentRuntimeCatalog(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
