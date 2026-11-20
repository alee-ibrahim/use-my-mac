package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/storage"
	"github.com/alee-ibrahim/use-my-mac/internal/store"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Direct downloads: with the s3 backend an app can fetch an output or blob
// it may read straight from the bucket with a presigned GET, instead of
// streaming it through the router. The router never sees those bytes, so
// for after_download cleanup the client confirms a complete, verified
// download with ConfirmDownload.

// DefaultDirectDownloadTTL is how long a presigned download URL lives when
// neither the request nor the router's config says otherwise.
const DefaultDirectDownloadTTL = time.Hour

// ErrNoDirect: the backend cannot hand out presigned URLs.
var ErrNoDirect = errors.New("direct downloads need the s3 storage backend; download through the router instead (without direct=1)")

// ErrChecksumMismatch: a confirmed download's SHA-256 is not the one the
// Mac recorded.
var ErrChecksumMismatch = errors.New("sha256 does not match the output's recorded checksum")

func (e *Engine) presignGet(ctx context.Context, key string, ttl time.Duration, b *store.Blob) (*api.DirectDownload, error) {
	if e.blobs.Name() != "s3" {
		return nil, ErrNoDirect
	}
	if ttl <= 0 {
		ttl = DefaultDirectDownloadTTL
	}
	if ttl > api.MaxDirectDownloadTTL {
		return nil, vErr("ttl must be at most %d seconds (7 days)", int(api.MaxDirectDownloadTTL/time.Second))
	}
	d := &api.DirectDownload{}
	if b != nil {
		d.Size, d.SHA256 = b.Size, b.SHA256
	}
	// Files without an ownership row (stored before tracking) or with an
	// unknown size: ask the store, which also turns a missing file into 404.
	if b == nil || b.Size == 0 {
		sz, ok := e.blobs.(storage.Sizer)
		if ok {
			n, err := sz.Size(ctx, key)
			if err != nil {
				return nil, err
			}
			d.Size = n
		}
	}
	u, err := e.blobs.DownloadURL(ctx, key, ttl)
	if err != nil {
		return nil, err
	}
	d.URL, d.ExpiresAt = u, e.now().Add(ttl).UTC().Truncate(time.Second)
	return d, nil
}

// outputBlobKey resolves a task output stored in router storage.
func (e *Engine) outputBlobKey(ctx context.Context, taskID, name string) (*store.Task, string, error) {
	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	var spec api.TaskSpec
	_ = json.Unmarshal([]byte(t.Spec), &spec)
	for _, o := range spec.Outputs {
		if o.Name == name {
			if o.URL != "" {
				return nil, "", vErr("output %s was sent to an app-supplied URL", name)
			}
			return t, OutputKey(t.JobID, t.Key, name), nil
		}
	}
	return nil, "", ErrNotFound
}

// DirectOutputURL presigns a GET for a task output. The caller has
// already been allowed to read the task's job.
func (e *Engine) DirectOutputURL(ctx context.Context, taskID, name string, ttl time.Duration) (*api.DirectDownload, error) {
	t, key, err := e.outputBlobKey(ctx, taskID, name)
	if err != nil {
		return nil, err
	}
	b, err := e.st.GetBlob(ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b = nil
	case err != nil:
		return nil, err
	}
	d, err := e.presignGet(ctx, key, ttl, b)
	if err != nil {
		return nil, err
	}
	if d.SHA256 == "" {
		digests := map[string]string{}
		_ = json.Unmarshal([]byte(t.OutputSHA256), &digests)
		d.SHA256 = digests[name]
	}
	return d, nil
}

// DirectBlobURL presigns a GET for an app upload the caller may read.
func (e *Engine) DirectBlobURL(ctx context.Context, c Caller, key string, ttl time.Duration) (*api.DirectDownload, error) {
	if err := storage.ValidKey(key); err != nil {
		return nil, err
	}
	b, ok, err := e.blobAccess(ctx, c, UploadKey(key))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return e.presignGet(ctx, UploadKey(key), ttl, b)
}

// ConfirmDownload records that a client fetched a task output in full
// outside the router (a direct download). It counts for after_download
// cleanup exactly like a complete download through the router. A given
// sha256 must match the Mac's checksum, when one was recorded.
func (e *Engine) ConfirmDownload(ctx context.Context, taskID, name, sha string) error {
	t, key, err := e.outputBlobKey(ctx, taskID, name)
	if err != nil {
		return err
	}
	b, err := e.st.GetBlob(ctx, key)
	if err != nil {
		return err // no stored output (yet): nothing to count
	}
	if sha = strings.ToLower(strings.TrimSpace(sha)); sha != "" {
		want := b.SHA256
		if want == "" {
			digests := map[string]string{}
			_ = json.Unmarshal([]byte(t.OutputSHA256), &digests)
			want = digests[name]
		}
		if want != "" && want != sha {
			return ErrChecksumMismatch
		}
	}
	e.RecordDownload(ctx, t.ID, name, http.StatusOK, b.Size)
	return nil
}
