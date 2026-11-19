package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Local stores blobs on the router's own disk (a PVC in Kubernetes) and hands
// agents HMAC-signed URLs that the router serves at SignedPath.
type Local struct {
	Dir     string
	BaseURL string // external URL agents use to reach the router, e.g. https://umm.example.com
	Secret  []byte // HMAC key for signed URLs
	Now     func() time.Time
}

// SignedPath is where the router serves signed blob URLs.
const SignedPath = "/v1/agent/blob"

// NewLocal creates the directory if needed.
func NewLocal(dir, baseURL string, secret []byte) (*Local, error) {
	if len(secret) < 16 {
		return nil, errors.New("local storage: signing secret must be at least 16 bytes")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Local{Dir: dir, BaseURL: strings.TrimRight(baseURL, "/"), Secret: secret, Now: time.Now}, nil
}

func (l *Local) Name() string { return "local" }

func (l *Local) path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.Dir, filepath.FromSlash(key)), nil
}

func (l *Local) sign(method, key string, exp int64) string {
	m := hmac.New(sha256.New, l.Secret)
	fmt.Fprintf(m, "%s\n%s\n%d", method, key, exp)
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Local) signedURL(method, key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	exp := l.Now().Add(ttl).Unix()
	q := url.Values{}
	q.Set("k", key)
	q.Set("m", method)
	q.Set("e", strconv.FormatInt(exp, 10))
	q.Set("s", l.sign(method, key, exp))
	return l.BaseURL + SignedPath + "?" + q.Encode(), nil
}

func (l *Local) DownloadURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	return l.signedURL("GET", key, ttl)
}

func (l *Local) UploadURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	return l.signedURL("PUT", key, ttl)
}

// Verify checks a signed URL's query against the request method. It returns
// the key on success.
func (l *Local) Verify(method string, q url.Values) (string, error) {
	key, m, e, s := q.Get("k"), q.Get("m"), q.Get("e"), q.Get("s")
	if m != method {
		return "", errors.New("method mismatch")
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil {
		return "", errors.New("bad expiry")
	}
	if l.Now().Unix() > exp {
		return "", errors.New("url expired")
	}
	if err := ValidKey(key); err != nil {
		return "", err
	}
	want := l.sign(method, key, exp)
	if !hmac.Equal([]byte(want), []byte(s)) {
		return "", errors.New("bad signature")
	}
	return key, nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64) error {
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
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Size returns a blob's size.
func (l *Local) Size(_ context.Context, key string) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
