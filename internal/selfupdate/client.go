package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// DefaultReleaseURL is where GitHub serves release assets:
// <base>/<version>/<file>.
const DefaultReleaseURL = "https://github.com/alee-ibrahim/use-my-mac/releases/download"

// HTTPRouter talks to the router with the agent's credential.
type HTTPRouter struct {
	Server     string // router base URL
	Credential string
	HTTP       *http.Client
}

func (h *HTTPRouter) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (h *HTTPRouter) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.Server, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Credential)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	return resp, nil
}

// Check asks the router which version to run.
func (h *HTTPRouter) Check(ctx context.Context) (api.UpdateCheck, error) {
	var c api.UpdateCheck
	resp, err := h.do(ctx, http.MethodGet, "/v1/agent/update", nil)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c)
	return c, err
}

// Report sends an update report.
func (h *HTTPRouter) Report(ctx context.Context, r api.UpdateReport) error {
	resp, err := h.do(ctx, http.MethodPost, "/v1/agent/update/report", r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// RelaySource fetches artifacts through the router.
func (h *HTTPRouter) RelaySource() Source {
	return Source{Name: "router", RelayOnly: true, Fetch: func(ctx context.Context, version, file string) ([]byte, error) {
		resp, err := h.do(ctx, http.MethodGet, "/v1/agent/update/artifacts/"+url.PathEscape(version)+"/"+url.PathEscape(file), nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return readAllLimit(resp.Body)
	}}
}

// DirectSource fetches artifacts from <base>/<version>/<file> (GitHub
// releases by default). No credentials are sent.
func DirectSource(base string, hc *http.Client) Source {
	if base == "" {
		base = DefaultReleaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return Source{Name: "direct", Fetch: func(ctx context.Context, version, file string) ([]byte, error) {
		return FetchURL(ctx, hc, strings.TrimRight(base, "/")+"/"+url.PathEscape(version)+"/"+url.PathEscape(file))
	}}
}

// FetchURL GETs a URL with the artifact size limit.
func FetchURL(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return readAllLimit(resp.Body)
}
