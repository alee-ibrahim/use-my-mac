package server

import (
	"net/http"

	usemymac "github.com/alee-ibrahim/use-my-mac"
)

// Docs are public: they are the project's own documentation, embedded in the
// binary, and hold nothing specific to this deployment.

func (s *Server) listDocs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, usemymac.Docs())
}

func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	b, ok := usemymac.DocMarkdown(r.PathValue("slug"))
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
