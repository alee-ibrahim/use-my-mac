package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/internal/transfer"
	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Direct downloads: with the router's s3 backend, outputs and blobs can be
// fetched straight from object storage with presigned URLs.

// Me returns who the API key is, its role and the router's storage backend.
func (c *Client) Me(ctx context.Context) (*api.Me, error) {
	var m api.Me
	return &m, c.do(ctx, http.MethodGet, "/v1/me", nil, &m)
}

func ttlQuery(ttl time.Duration) string {
	q := "?direct=1"
	if ttl > 0 {
		q += "&ttl=" + strconv.Itoa(int(ttl/time.Second))
	}
	return q
}

// OutputURL returns a presigned URL for a task output, for use by other
// tools (curl, ffmpeg, a browser). Send it a plain GET, without an
// Authorization header; Range requests work. ttl 0 uses the router's
// default (1 hour unless configured); at most 7 days. Needs the s3 backend
// (400 otherwise). Downloads through the URL are invisible to the router:
// call ConfirmDownload when a job uses after_download cleanup.
func (c *Client) OutputURL(ctx context.Context, taskID, name string, ttl time.Duration) (*api.DirectDownload, error) {
	var d api.DirectDownload
	return &d, c.do(ctx, http.MethodGet,
		"/v1/tasks/"+url.PathEscape(taskID)+"/outputs/"+url.PathEscape(name)+ttlQuery(ttl), nil, &d)
}

// BlobURL returns a presigned URL for a blob you uploaded (see OutputURL).
func (c *Client) BlobURL(ctx context.Context, key string, ttl time.Duration) (*api.DirectDownload, error) {
	var d api.DirectDownload
	return &d, c.do(ctx, http.MethodGet, "/v1/blobs/"+escapeKey(key)+ttlQuery(ttl), nil, &d)
}

// ConfirmDownload tells the router an output was fetched in full outside
// it (through OutputURL), so after_download cleanup counts it. Pass the
// SHA-256 you computed (hex) to have the router check it against the Mac's
// checksum (409 on a mismatch); "" skips that check.
func (c *Client) ConfirmDownload(ctx context.Context, taskID, name, sha256 string) error {
	return c.do(ctx, http.MethodPost,
		"/v1/tasks/"+url.PathEscape(taskID)+"/outputs/"+url.PathEscape(name)+"/downloaded",
		api.DownloadConfirm{SHA256: sha256}, nil)
}

// DownloadInfo describes a finished DownloadOutputFileInfo.
type DownloadInfo struct {
	Size int64
	// Direct: fetched straight from object storage with a presigned URL.
	Direct bool
	// SHA256 is the Mac's checksum, when known (direct downloads).
	SHA256 string
	// Verified: the file matched SHA256.
	Verified bool
	// Confirmed: the router counted the download for after_download
	// (direct downloads; relayed ones are counted by the router itself).
	// ConfirmError says why not, when it failed; the file is still good.
	Confirmed    bool
	ConfirmError error
}

// ErrChecksum: a downloaded file did not match the Mac's SHA-256.
var ErrChecksum = errors.New("SHA-256 mismatch")

// DownloadOutputFileInfo is DownloadOutputFile with details. With
// opt.Direct the output comes straight from object storage (parallel
// byte ranges on a presigned URL), is checked against the Mac's SHA-256
// (unless opt.SkipVerify) and then confirmed to the router.
func (c *Client) DownloadOutputFileInfo(ctx context.Context, taskID, name, path string, opt *TransferOptions) (*DownloadInfo, error) {
	o := opt.norm()
	if !o.Direct {
		n, err := c.downloadRelay(ctx, taskID, name, path, o)
		if err != nil {
			return nil, err
		}
		return &DownloadInfo{Size: n}, nil
	}
	d, err := c.OutputURL(ctx, taskID, name, o.URLTTL)
	if err != nil {
		return nil, err
	}
	n, err := c.downloadURL(ctx, d.URL, path, o)
	if isStatus(err, http.StatusForbidden) {
		// The URL may have expired during a long download: once more.
		if d, err = c.OutputURL(ctx, taskID, name, o.URLTTL); err != nil {
			return nil, err
		}
		n, err = c.downloadURL(ctx, d.URL, path, o)
	}
	if err != nil {
		return nil, err
	}
	info := &DownloadInfo{Size: n, Direct: true, SHA256: d.SHA256}
	if d.Size > 0 && n != d.Size {
		os.Remove(path)
		return nil, fmt.Errorf("%s: got %d bytes, want %d", path, n, d.Size)
	}
	got := ""
	if d.SHA256 != "" && !o.SkipVerify {
		if got, err = fileSHA256(path); err != nil {
			return nil, err
		}
		if got != strings.ToLower(d.SHA256) {
			os.Remove(path)
			return nil, fmt.Errorf("%w: %s has %s, the Mac recorded %s", ErrChecksum, path, got, d.SHA256)
		}
		info.Verified = true
	}
	if err := c.ConfirmDownload(ctx, taskID, name, got); err != nil {
		info.ConfirmError = err
	} else {
		info.Confirmed = true
	}
	return info, nil
}

// DownloadOutputFile saves a task output to path, in parallel byte ranges
// when the server can serve them, with progress. opt.Direct fetches it
// straight from object storage (s3 backend) and verifies its SHA-256; see
// DownloadOutputFileInfo.
func (c *Client) DownloadOutputFile(ctx context.Context, taskID, name, path string, opt *TransferOptions) (int64, error) {
	info, err := c.DownloadOutputFileInfo(ctx, taskID, name, path, opt)
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (c *Client) downloadRelay(ctx context.Context, taskID, name, path string, o TransferOptions) (int64, error) {
	return c.download(ctx, c.BaseURL+"/v1/tasks/"+url.PathEscape(taskID)+"/outputs/"+url.PathEscape(name),
		c.authHeader(), path, o)
}

// downloadURL fetches a presigned URL: no Authorization or other headers
// beyond Range, which SigV4 query signing does not cover and S3/RGW accept.
func (c *Client) downloadURL(ctx context.Context, u, path string, o TransferOptions) (int64, error) {
	return c.download(ctx, u, nil, path, o)
}

func (c *Client) download(ctx context.Context, u string, hdr http.Header, path string, o TransferOptions) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	m := transfer.NewMeter(0)
	stop := m.Report(o.ProgressEvery, o.OnProgress)
	n, err := transfer.Download(ctx, c.HTTP, u, f,
		transfer.DownloadOptions{Chunk: o.PartSize, Concurrency: o.Concurrency, Retries: o.Retries,
			Header: hdr, Meter: m})
	stop()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		var se *transfer.StatusError
		if errors.As(err, &se) {
			return 0, &Error{Status: se.Code, Message: se.Body}
		}
	}
	return n, err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
