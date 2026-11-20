package server

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Multipart uploads (app API and agent outputs) and live task progress.

func (s *Server) uploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/uploads", s.need(roleSubmit, s.createUpload))
	mux.HandleFunc("GET /v1/uploads/{id}", s.need(roleSubmit, s.getUpload))
	mux.HandleFunc("PUT /v1/uploads/{id}/parts/{n}", s.need(roleSubmit, s.putUploadPart))
	mux.HandleFunc("POST /v1/uploads/{id}/complete", s.need(roleSubmit, s.completeUpload))
	mux.HandleFunc("DELETE /v1/uploads/{id}", s.need(roleSubmit, s.abortUpload))

	mux.HandleFunc("POST /v1/agent/tasks/{id}/progress", s.agent(s.agentProgress))
	mux.HandleFunc("POST /v1/agent/tasks/{id}/outputs/{name}/upload", s.agent(s.agentCreateUpload))
	mux.HandleFunc("POST /v1/agent/uploads/{id}/complete", s.agent(s.agentCompleteUpload))
	mux.HandleFunc("DELETE /v1/agent/uploads/{id}", s.agent(s.agentAbortUpload))

	mux.HandleFunc("GET /v1/jobs/stream", s.need(roleAny, s.streamJobs))
	mux.HandleFunc("GET /v1/jobs/{id}/stream", s.need(roleAny, s.streamJob))
}

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	var req api.UploadRequest
	if !readJSON(w, r, &req, 64<<10) {
		return
	}
	if req.Size > s.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "file is larger than the router accepts ("+strconv.FormatInt(s.MaxUploadBytes, 10)+" bytes)")
		return
	}
	u, err := s.E.CreateUploadAs(r.Context(), caller(r), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	u, err := s.E.GetUpload(r.Context(), r.PathValue("id"), id.Subject, id.Role == api.RoleAdmin)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) putUploadPart(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad part number")
		return
	}
	id := who(r)
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxPartSize)
	if err := s.E.PutUploadPart(r.Context(), r.PathValue("id"), id.Subject, id.Role == api.RoleAdmin, n, r.Body, r.ContentLength); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	u, err := s.E.CompleteUpload(r.Context(), r.PathValue("id"), id.Subject, id.Role == api.RoleAdmin)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blob": u.Key, "size": u.Size, "parts": u.Parts})
}

func (s *Server) abortUpload(w http.ResponseWriter, r *http.Request) {
	id := who(r)
	if err := s.E.AbortUpload(r.Context(), r.PathValue("id"), id.Subject, id.Role == api.RoleAdmin); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentProgress(w http.ResponseWriter, r *http.Request) {
	var p api.TaskProgress
	if !readJSON(w, r, &p, 16<<10) {
		return
	}
	if err := s.E.ReportProgress(r.Context(), agentID(r), r.PathValue("id"), p); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentCreateUpload(w http.ResponseWriter, r *http.Request) {
	var req api.OutputUploadRequest
	if !readJSON(w, r, &req, 16<<10) {
		return
	}
	if req.Size > s.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "output is larger than the router accepts")
		return
	}
	u, err := s.E.CreateOutputUpload(r.Context(), agentID(r), r.PathValue("id"), r.PathValue("name"), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) agentCompleteUpload(w http.ResponseWriter, r *http.Request) {
	if err := s.E.CompleteAgentUpload(r.Context(), agentID(r), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentAbortUpload(w http.ResponseWriter, r *http.Request) {
	if err := s.E.AbortAgentUpload(r.Context(), agentID(r), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveBlob streams a blob. Seekable blobs (local backend) are served with
// http.ServeContent, so clients can use Range requests (parallel chunked
// downloads, resume).
func serveBlob(w http.ResponseWriter, r *http.Request, rc io.ReadCloser) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	io.Copy(w, rc)
}

// signedPartAllowed: a signed PUT to a multipart part key only works while
// that upload exists (it must not recreate parts of finished uploads).
func signedPartAllowed(l *storage.Local, key string) bool {
	part, live := l.IsPartKey(key)
	return !part || live
}

// wantDirect: ?direct=1 (or true) asks for a presigned URL instead of the
// bytes.
func wantDirect(r *http.Request) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get("direct"))
	return v
}

// directTTL reads ?ttl=SECONDS (default: the router's configured lifetime).
func (s *Server) directTTL(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	ttl := s.DirectDownloadTTL
	if v := r.URL.Query().Get("ttl"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 60 || time.Duration(n)*time.Second > api.MaxDirectDownloadTTL {
			writeErr(w, http.StatusBadRequest, "ttl must be seconds between 60 and 604800 (7 days)")
			return 0, false
		}
		ttl = time.Duration(n) * time.Second
	}
	return ttl, true
}

// writeDirect answers a direct download: the URL as JSON, or with
// ?redirect=1 a 307 to it (for tools that just follow redirects).
func writeDirect(w http.ResponseWriter, r *http.Request, d *api.DirectDownload) {
	w.Header().Set("Cache-Control", "no-store")
	if v, _ := strconv.ParseBool(r.URL.Query().Get("redirect")); v {
		http.Redirect(w, r, d.URL, http.StatusTemporaryRedirect)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// confirmDownload: a client fetched an output in full outside the router
// (direct download) and its checksum passed; count it for after_download.
func (s *Server) confirmDownload(w http.ResponseWriter, r *http.Request) {
	var req api.DownloadConfirm
	if r.ContentLength != 0 && !readJSON(w, r, &req, 4096) {
		return
	}
	t := s.loadTask(w, r)
	if t == nil {
		return
	}
	if err := s.E.ConfirmDownload(r.Context(), t.ID, r.PathValue("name"), req.SHA256); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
