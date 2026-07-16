package credprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client issues STS tokens by calling the hiclaw-credential-provider sidecar.
type Client interface {
	// Issue asks the sidecar for an STS triple matching req.
	// A non-nil error means either the HTTP call failed or the sidecar
	// returned a non-2xx status.
	Issue(ctx context.Context, req IssueRequest) (*IssueResponse, error)
	// GetKubeconfig obtains a temporary kubeconfig for the given cluster.
	GetKubeconfig(ctx context.Context, clusterID string) (*KubeconfigResponse, error)
}

// HTTPClient is an HTTP implementation of Client that talks to the sidecar
// over loopback. It is safe for concurrent use.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient creates a Client that posts to {baseURL}/issue.
// A typical baseURL is "http://127.0.0.1:17070".
// If httpClient is nil a default one with a 30 s timeout is used.
func NewHTTPClient(baseURL string, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

// Issue implements Client.
func (c *HTTPClient) Issue(ctx context.Context, req IssueRequest) (*IssueResponse, error) {
	if c.baseURL == "" {
		return nil, errors.New("credprovider: base URL not configured (AGENTTEAMS_CREDENTIAL_PROVIDER_URL)")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal issue request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/issue", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call credential-provider: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("credential-provider returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out IssueResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse issue response: %w", err)
	}
	// SecurityToken is optional: the production sidecar always returns
	// an STS triple, but the mock-credential-provider's "passthrough"
	// mode (and any future static-AK sidecar) returns a raw AK/SK pair
	// with an empty SecurityToken. Downstream callers honour the empty
	// token by emitting a 2-tuple MC_HOST_* binding (see oss.buildMCHostEnv).
	if out.AccessKeyID == "" || out.AccessKeySecret == "" {
		return nil, errors.New("credential-provider returned incomplete credentials (missing access_key_id or access_key_secret)")
	}
	return &out, nil
}
