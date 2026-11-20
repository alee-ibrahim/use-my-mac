package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/auth"
	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Multipart uploads: a large blob arrives as parts, in parallel and in any
// order, then is assembled. Apps use it for inputs (POST /v1/uploads);
// agents for outputs that go to router storage. With S3 the parts can go
// straight to the bucket via presigned URLs ("direct"); otherwise they go
// through the router ("relay"), which writes them to the backend. Parts
// that failed are simply sent again; GET on the upload lists what is
// stored, so an interrupted client can resume.

// UploadTTL is how long an unfinished upload is kept before it is aborted.
const UploadTTL = 24 * time.Hour

// partURLTTL bounds presigned/signed part URLs. Clients ask for fresh ones
// (GET the upload) when they expire.
const partURLTTL = 6 * time.Hour

// RelayPartPath is the router path apps PUT relay parts to.
func RelayPartPath(uploadID string, n int) string {
	return fmt.Sprintf("/v1/uploads/%s/parts/%d", uploadID, n)
}

// ownerAgent is the owner string of uploads started by an agent.
func ownerAgent(agentID string) string { return "agent:" + agentID }

func (e *Engine) multipart() (storage.Multipart, error) {
	m, ok := storage.MultipartOf(e.blobs)
	if !ok {
		return nil, storage.ErrNoMultipart
	}
	return m, nil
}

// normalizePartSize applies defaults and limits for size bytes.
func normalizePartSize(size, partSize int64) (int64, error) {
	if size <= 0 {
		return 0, &ValidationError{"size must be positive"}
	}
	if partSize == 0 {
		partSize = api.DefaultPartSize
	}
	if partSize < api.MinPartSize || partSize > api.MaxPartSize {
		return 0, &ValidationError{fmt.Sprintf("part_size must be between %d and %d bytes", api.MinPartSize, api.MaxPartSize)}
	}
	// Grow parts (in whole MiB) until the file fits in MaxParts.
	if api.PartCount(size, partSize) > api.MaxParts {
		need := (size + api.MaxParts - 1) / api.MaxParts
		partSize = (need + (1 << 20) - 1) &^ ((1 << 20) - 1)
		if partSize > api.MaxPartSize {
			return 0, &ValidationError{"file is too large for a multipart upload"}
		}
	}
	return partSize, nil
}

func (e *Engine) createUpload(ctx context.Context, owner, key string, size, partSize int64, direct bool) (*api.Upload, error) {
	m, err := e.multipart()
	if err != nil {
		return nil, &ValidationError{err.Error()}
	}
	if partSize, err = normalizePartSize(size, partSize); err != nil {
		return nil, err
	}
	bid, err := m.CreateMultipart(ctx, key)
	if err != nil {
		return nil, err
	}
	now := e.now()
	u := &store.Upload{ID: "upl_" + auth.NewID(), Owner: owner, Key: key, BackendID: bid, Size: size,
		PartSize: partSize, Direct: direct, CreatedAt: ms(now), ExpiresAt: ms(now.Add(UploadTTL))}
	if err := e.st.CreateUpload(ctx, u); err != nil {
		_ = m.AbortMultipart(ctx, key, bid)
		return nil, err
	}
	e.log.Info("multipart upload started", "upload", u.ID, "key", key, "size", size, "part_size", partSize,
		"direct", direct, "owner", owner)
	return e.uploadView(ctx, u, nil)
}

// CreateUpload starts an app upload of blob `key` (Input.Blob). direct
// means presigned part URLs when the backend has them (S3).
func (e *Engine) CreateUpload(ctx context.Context, owner string, req api.UploadRequest) (*api.Upload, error) {
	return e.CreateUploadAs(ctx, Caller{Subject: owner}, req)
}

// CreateUploadAs is CreateUpload for a caller: a key another owner uploaded
// cannot be overwritten (admins can).
func (e *Engine) CreateUploadAs(ctx context.Context, c Caller, req api.UploadRequest) (*api.Upload, error) {
	owner := c.Subject
	if err := storage.ValidKey(req.Key); err != nil {
		return nil, err
	}
	if _, ok, err := e.blobAccess(ctx, c, UploadKey(req.Key)); err != nil {
		return nil, err
	} else if !ok {
		return nil, forbidden("blob %s belongs to another owner", req.Key)
	}
	direct := req.Direct && e.blobs.Name() == "s3"
	return e.createUpload(ctx, owner, UploadKey(req.Key), req.Size, req.PartSize, direct)
}

// CreateOutputUpload starts an agent upload of a task output. The task must
// be running on that agent with that attempt, and the output must go to
// router storage. Part URLs are always direct (presigned or signed): agents
// never send bytes with their bearer credential.
func (e *Engine) CreateOutputUpload(ctx context.Context, agentID, taskID, name string, req api.OutputUploadRequest) (*api.Upload, error) {
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.AgentID != agentID || t.Attempt != req.Attempt || t.State != api.StateRunning {
		return nil, ErrStale
	}
	var spec api.TaskSpec
	if err := json.Unmarshal([]byte(t.Spec), &spec); err != nil {
		return nil, err
	}
	for _, o := range spec.Outputs {
		if o.Name == name {
			if o.URL != "" {
				return nil, vErr("output %s goes to an app-supplied URL", name)
			}
			return e.createUpload(ctx, ownerAgent(agentID), OutputKey(t.JobID, t.Key, name), req.Size, req.PartSize, true)
		}
	}
	return nil, ErrNotFound
}

// loadUpload returns an upload the owner may use (admins may use any app
// upload). Expired uploads are not found.
func (e *Engine) loadUpload(ctx context.Context, id, owner string, admin bool) (*store.Upload, error) {
	u, err := e.st.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.ExpiresAt < ms(e.now()) {
		return nil, ErrNotFound
	}
	if u.Owner != owner && !(admin && !isAgentOwner(u.Owner)) {
		return nil, ErrNotFound
	}
	return u, nil
}

func isAgentOwner(o string) bool { return len(o) > 6 && o[:6] == "agent:" }

// uploadView lists stored parts and URLs for the missing ones.
func (e *Engine) uploadView(ctx context.Context, u *store.Upload, parts []storage.Part) (*api.Upload, error) {
	m, err := e.multipart()
	if err != nil {
		return nil, err
	}
	if parts == nil {
		if parts, err = m.ListParts(ctx, u.Key, u.BackendID); err != nil {
			return nil, err
		}
	}
	n := api.PartCount(u.Size, u.PartSize)
	v := &api.Upload{ID: u.ID, Key: u.Key, Size: u.Size, PartSize: u.PartSize, Parts: n, Direct: u.Direct,
		Done: []int{}, URLs: map[int]string{}, ExpiresAt: time.UnixMilli(u.ExpiresAt).UTC()}
	if k, ok := cutUploadKey(u.Key); ok {
		v.Key = k
	}
	have := map[int]bool{}
	for _, p := range parts {
		if p.N >= 1 && p.N <= n && p.Size == api.PartSizeOf(u.Size, u.PartSize, p.N) {
			have[p.N] = true
			v.Done = append(v.Done, p.N)
		}
	}
	for i := 1; i <= n; i++ {
		if have[i] {
			continue
		}
		if !u.Direct && !isAgentOwner(u.Owner) {
			v.URLs[i] = RelayPartPath(u.ID, i)
			continue
		}
		url, err := m.PartURL(ctx, u.Key, u.BackendID, i, partURLTTL)
		if err != nil {
			return nil, err
		}
		v.URLs[i] = url
	}
	return v, nil
}

func cutUploadKey(k string) (string, bool) {
	const p = "uploads/"
	if len(k) > len(p) && k[:len(p)] == p {
		return k[len(p):], true
	}
	return "", false
}

// GetUpload returns an upload's state with fresh URLs for missing parts.
func (e *Engine) GetUpload(ctx context.Context, id, owner string, admin bool) (*api.Upload, error) {
	u, err := e.loadUpload(ctx, id, owner, admin)
	if err != nil {
		return nil, err
	}
	return e.uploadView(ctx, u, nil)
}

// PutUploadPart stores part n through the router (relay mode).
func (e *Engine) PutUploadPart(ctx context.Context, id, owner string, admin bool, n int, r io.Reader, size int64) error {
	u, err := e.loadUpload(ctx, id, owner, admin)
	if err != nil {
		return err
	}
	parts := api.PartCount(u.Size, u.PartSize)
	if n < 1 || n > parts {
		return vErr("part number must be between 1 and %d", parts)
	}
	want := api.PartSizeOf(u.Size, u.PartSize, n)
	if size >= 0 && size != want {
		return vErr("part %d must be %d bytes, got %d", n, want, size)
	}
	m, err := e.multipart()
	if err != nil {
		return err
	}
	return m.PutPart(ctx, u.Key, u.BackendID, n, io.LimitReader(r, want), want)
}

// CompleteUpload assembles the blob once every part is stored.
func (e *Engine) CompleteUpload(ctx context.Context, id, owner string, admin bool) (*api.Upload, error) {
	u, err := e.loadUpload(ctx, id, owner, admin)
	if err != nil {
		return nil, err
	}
	m, err := e.multipart()
	if err != nil {
		return nil, err
	}
	parts, err := m.ListParts(ctx, u.Key, u.BackendID)
	if err != nil {
		return nil, err
	}
	v, err := e.uploadView(ctx, u, parts)
	if err != nil {
		return nil, err
	}
	if len(v.Done) != v.Parts {
		return v, &ValidationError{fmt.Sprintf("upload incomplete: %d of %d parts stored", len(v.Done), v.Parts)}
	}
	var use []storage.Part
	for _, p := range parts {
		if p.N >= 1 && p.N <= v.Parts {
			use = append(use, p)
		}
	}
	if err := m.CompleteMultipart(ctx, u.Key, u.BackendID, use); err != nil {
		return nil, err
	}
	_ = e.st.DeleteUpload(ctx, u.ID)
	if _, app := cutUploadKey(u.Key); app && !isAgentOwner(u.Owner) {
		blobOwner := u.Owner
		if b, err := e.st.GetBlob(ctx, u.Key); err == nil {
			blobOwner = b.Owner // an admin replaced someone's blob
		}
		if err := e.recordUpload(ctx, u.Key, blobOwner, u.Size); err != nil {
			e.log.Warn("record upload owner", "key", u.Key, "err", err)
		}
	}
	e.log.Info("multipart upload completed", "upload", u.ID, "key", u.Key, "size", u.Size, "parts", v.Parts)
	v.URLs = nil
	return v, nil
}

// AbortUpload drops an upload and its parts.
func (e *Engine) AbortUpload(ctx context.Context, id, owner string, admin bool) error {
	u, err := e.loadUpload(ctx, id, owner, admin)
	if err != nil {
		return err
	}
	return e.abort(ctx, u)
}

func (e *Engine) abort(ctx context.Context, u *store.Upload) error {
	if m, err := e.multipart(); err == nil {
		if err := m.AbortMultipart(ctx, u.Key, u.BackendID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return e.st.DeleteUpload(ctx, u.ID)
}

// ExpireUploads aborts uploads that were never completed.
func (e *Engine) ExpireUploads(ctx context.Context) {
	for range 10 {
		us, err := e.st.ExpiredUploads(ctx, ms(e.now()), 100)
		if err != nil || len(us) == 0 {
			return
		}
		for _, u := range us {
			if err := e.abort(ctx, u); err != nil {
				e.log.Warn("abort expired upload", "upload", u.ID, "err", err)
				_ = e.st.DeleteUpload(ctx, u.ID)
			}
		}
	}
}

// CompleteAgentUpload / AbortAgentUpload act on an agent's own upload.
func (e *Engine) CompleteAgentUpload(ctx context.Context, agentID, id string) error {
	_, err := e.CompleteUpload(ctx, id, ownerAgent(agentID), false)
	return err
}

func (e *Engine) AbortAgentUpload(ctx context.Context, agentID, id string) error {
	return e.AbortUpload(ctx, id, ownerAgent(agentID), false)
}
