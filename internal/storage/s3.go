package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is any S3-compatible store (AWS S3, Ceph RGW, MinIO, R2, ...). The router
// holds the credentials; agents only see SigV4 presigned URLs that expire
// quickly. Implemented without an SDK to keep the dependency tree small.
type S3 struct {
	Endpoint  string // e.g. https://s3.us-east-1.amazonaws.com or http://minio:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PathStyle uses endpoint/bucket/key instead of bucket.endpoint/key.
	PathStyle bool
	// Prefix is prepended to every key (optional).
	Prefix string
	Client *http.Client
	Now    func() time.Time
}

func (s *S3) Name() string { return "s3" }

func (s *S3) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *S3) objectURL(key string) (*url.URL, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	full := s.Prefix + key
	if s.PathStyle {
		u.Path = "/" + s.Bucket + "/" + full
	} else {
		u.Host = s.Bucket + "." + u.Host
		u.Path = "/" + full
	}
	return u, nil
}

// uriEncode implements AWS SigV4 URI encoding.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Presign returns a SigV4 query-string presigned URL.
func (s *S3) Presign(method, key string, ttl time.Duration) (string, error) {
	return s.presign(method, key, ttl, nil)
}

// presign signs extra query parameters (uploadId, partNumber, ...) too.
func (s *S3) presign(method, key string, ttl time.Duration, extra map[string]string) (string, error) {
	if s.AccessKey == "" || s.SecretKey == "" {
		return "", errors.New("s3: missing credentials")
	}
	u, err := s.objectURL(key)
	if err != nil {
		return "", err
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return "", errors.New("s3: presign ttl must be within 7 days")
	}
	t := s.now()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	region := s.Region
	if region == "" {
		region = "us-east-1"
	}
	scope := day + "/" + region + "/s3/aws4_request"

	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    s.AccessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(ttl / time.Second)),
		"X-Amz-SignedHeaders": "host",
	}
	for k, v := range extra {
		q[k] = v
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cq []string
	for _, k := range keys {
		cq = append(cq, uriEncode(k, false)+"="+uriEncode(q[k], false))
	}
	canonicalQuery := strings.Join(cq, "&")
	canonicalPath := uriEncode(u.Path, true)

	canonicalReq := strings.Join([]string{
		method,
		canonicalPath,
		canonicalQuery,
		"host:" + u.Host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	h := sha256.Sum256([]byte(canonicalReq))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])

	kDate := hmacSHA256([]byte("AWS4"+s.SecretKey), day)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return u.Scheme + "://" + u.Host + canonicalPath + "?" + canonicalQuery + "&X-Amz-Signature=" + sig, nil
}

func (s *S3) DownloadURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	return s.Presign(http.MethodGet, key, ttl)
}

func (s *S3) UploadURL(_ context.Context, key string, ttl time.Duration) (string, error) {
	return s.Presign(http.MethodPut, key, ttl)
}

func (s *S3) do(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Response, error) {
	return s.doQ(ctx, method, key, nil, body, size)
}

func (s *S3) doQ(ctx context.Context, method, key string, extra map[string]string, body io.Reader, size int64) (*http.Response, error) {
	u, err := s.presign(method, key, 15*time.Minute, extra)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil && size >= 0 {
		req.ContentLength = size
	}
	return s.client().Do(req)
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	resp, err := s.do(ctx, http.MethodPut, key, r, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 put: %s: %s", resp.Status, b)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("s3 get: %s", resp.Status)
	}
	return resp.Body, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, -1)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 delete: %s", resp.Status)
	}
	return nil
}
