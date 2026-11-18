package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/alee-ibrahim/use-my-mac/internal/access"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Dashboard access management (admin only): role rules for single-sign-on
// users, the people who signed in and an audit log of role changes. Config
// rules are listed as locked; dashboard rules add on top.

func (s *Server) accessFail(w http.ResponseWriter, err error) {
	var ve *access.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, access.ErrLocked):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, access.ErrLockout):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, access.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		s.fail(w, err)
	}
}

func (s *Server) getAccess(w http.ResponseWriter, r *http.Request) {
	p, err := s.Access.Policy(r.Context(), s.OIDC != nil)
	if err != nil {
		s.accessFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putAccessRule(w http.ResponseWriter, r *http.Request) {
	var req api.AccessRuleRequest
	if !readJSON(w, r, &req, 4096) {
		return
	}
	id := who(r)
	rule, err := s.Access.PutRule(r.Context(), id.Subject, id.Name, req)
	if err != nil {
		s.accessFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) deleteAccessRule(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	if err := s.Access.DeleteRule(r.Context(), id.Subject, id.Name, r.PathValue("id")); err != nil {
		s.accessFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putAccessDefault(w http.ResponseWriter, r *http.Request) {
	var req api.AccessDefaultRequest
	if !readJSON(w, r, &req, 1024) {
		return
	}
	id := who(r)
	if err := s.Access.SetDefault(r.Context(), id.Subject, id.Name, req.Role); err != nil {
		s.accessFail(w, err)
		return
	}
	s.getAccess(w, r)
}

func (s *Server) putAccessGroupName(w http.ResponseWriter, r *http.Request) {
	var req api.AccessGroupNameRequest
	if !readJSON(w, r, &req, 1024) {
		return
	}
	id := who(r)
	if err := s.Access.SetGroupName(r.Context(), id.Subject, id.Name, r.PathValue("id"), req.Name); err != nil {
		s.accessFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accessUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.Access.Users(r.Context())
	if err != nil {
		s.accessFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, us)
}

func (s *Server) putAccessUserRole(w http.ResponseWriter, r *http.Request) {
	var req api.AccessUserRoleRequest
	if !readJSON(w, r, &req, 1024) {
		return
	}
	id := who(r)
	if err := s.Access.SetUserRole(r.Context(), id.Subject, id.Name, r.PathValue("sub"), req.Role); err != nil {
		s.accessFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accessAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.Access.Events(r.Context(), limit)
	if err != nil {
		s.accessFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}
