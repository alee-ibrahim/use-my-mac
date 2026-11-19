// Package storage provides pluggable blob storage for task inputs and outputs.
//
// Agents never hold long-lived storage credentials: they only ever receive
// short-lived URLs. With the local backend those are HMAC-signed URLs served
// by the router itself; with S3 they are SigV4 presigned URLs.
package storage

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

// Backend stores blobs by key.
type Backend interface {
	// Name identifies the backend ("local", "s3").
	Name() string
	// DownloadURL returns a short-lived URL an agent can GET.
	DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// UploadURL returns a short-lived URL an agent can PUT to.
	UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Put stores a blob (used by the app-facing API).
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens a blob (used by the app-facing API).
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes a blob; missing keys are not an error.
	Delete(ctx context.Context, key string) error
}

// ErrNotFound is returned by Get for missing keys.
var ErrNotFound = errors.New("blob not found")

// ErrBadKey is returned for keys that could escape the store.
var ErrBadKey = errors.New("invalid blob key")

var keyRe = regexp.MustCompile(`^[A-Za-z0-9._\-/]+$`)

// ValidKey rejects absolute paths, traversal and odd characters.
func ValidKey(key string) error {
	if key == "" || len(key) > 512 || !keyRe.MatchString(key) ||
		strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return ErrBadKey
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrBadKey
		}
	}
	return nil
}

// Sizer is implemented by backends that can report a blob's size without
// reading it. The scheduler uses it to estimate a task's disk need.
type Sizer interface {
	Size(ctx context.Context, key string) (int64, error)
}
