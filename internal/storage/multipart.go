package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Part is one stored part of a multipart upload.
type Part struct {
	N    int    `json:"n"`
	Size int64  `json:"size"`
	ETag string `json:"etag,omitempty"`
}

// Multipart is implemented by backends that can take a blob in parts that
// arrive in any order and in parallel. Parts are numbered from 1.
type Multipart interface {
	// CreateMultipart starts an upload and returns the backend's upload ID.
	CreateMultipart(ctx context.Context, key string) (string, error)
	// PartURL returns a short-lived URL that accepts part n with a plain
	// PUT and no other credentials (presigned or HMAC-signed).
	PartURL(ctx context.Context, key, uploadID string, n int, ttl time.Duration) (string, error)
	// PutPart stores part n from the router itself (relay mode).
	PutPart(ctx context.Context, key, uploadID string, n int, r io.Reader, size int64) error
	// ListParts returns the parts stored so far, sorted by number.
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	// CompleteMultipart assembles the parts (in number order) into key.
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	// AbortMultipart drops the parts. Unknown uploads are not an error.
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// ErrNoMultipart is returned when the backend cannot do multipart uploads.
var ErrNoMultipart = errors.New("storage backend does not support multipart uploads")

// MultipartOf returns the backend's multipart support, if any.
func MultipartOf(b Backend) (Multipart, bool) {
	m, ok := b.(Multipart)
	return m, ok
}

// ---------------------------------------------------------------------------
// Local backend
// ---------------------------------------------------------------------------

// multipartDir holds the parts of unfinished uploads inside Local.Dir. Real
// blob keys always start with "uploads/" or "jobs/", so it never collides.
const multipartDir = "_multipart"

var localUploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (l *Local) partKey(uploadID string, n int) (string, error) {
	if !localUploadIDRe.MatchString(uploadID) {
		return "", ErrBadKey
	}
	if n < 1 || n > 99999 {
		return "", fmt.Errorf("part number %d out of range", n)
	}
	return fmt.Sprintf("%s/%s/%05d", multipartDir, uploadID, n), nil
}

func (l *Local) CreateMultipart(_ context.Context, key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Join(l.Dir, multipartDir, id), 0o750); err != nil {
		return "", err
	}
	return id, nil
}

// PartURL is an HMAC-signed router URL (served at SignedPath) that writes
// the part file.
func (l *Local) PartURL(_ context.Context, _ string, uploadID string, n int, ttl time.Duration) (string, error) {
	k, err := l.partKey(uploadID, n)
	if err != nil {
		return "", err
	}
	return l.signedURL("PUT", k, ttl)
}

func (l *Local) uploadDir(uploadID string) (string, error) {
	if !localUploadIDRe.MatchString(uploadID) {
		return "", ErrBadKey
	}
	d := filepath.Join(l.Dir, multipartDir, uploadID)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	return d, nil
}

func (l *Local) PutPart(ctx context.Context, _ string, uploadID string, n int, r io.Reader, size int64) error {
	if _, err := l.uploadDir(uploadID); err != nil {
		return err
	}
	k, err := l.partKey(uploadID, n)
	if err != nil {
		return err
	}
	return l.Put(ctx, k, r, size)
}

// IsPartKey reports whether a signed-URL key is a multipart part, and
// whether its upload still exists (parts of finished or aborted uploads
// must not be written).
func (l *Local) IsPartKey(key string) (part, live bool) {
	var id string
	var n int
	if _, err := fmt.Sscanf(key, multipartDir+"/%32s/%d", &id, &n); err != nil {
		return false, false
	}
	_, err := l.uploadDir(id)
	return true, err == nil
}

func (l *Local) ListParts(_ context.Context, _ string, uploadID string) ([]Part, error) {
	d, err := l.uploadDir(uploadID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []Part
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || e.IsDir() {
			continue // temp files of parts still being written
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Part{N: n, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

func (l *Local) CompleteMultipart(_ context.Context, key, uploadID string, parts []Part) error {
	d, err := l.uploadDir(uploadID)
	if err != nil {
		return err
	}
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	for _, pt := range parts {
		f, err := os.Open(filepath.Join(d, fmt.Sprintf("%05d", pt.N)))
		if err != nil {
			return fail(err)
		}
		_, err = io.Copy(tmp, f)
		f.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.RemoveAll(d)
}

func (l *Local) AbortMultipart(_ context.Context, _ string, uploadID string) error {
	if !localUploadIDRe.MatchString(uploadID) {
		return ErrBadKey
	}
	return os.RemoveAll(filepath.Join(l.Dir, multipartDir, uploadID))
}
