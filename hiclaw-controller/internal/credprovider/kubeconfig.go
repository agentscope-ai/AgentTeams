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

	hiclawmetrics "github.com/hiclaw/hiclaw-controller/internal/metrics"
)

// KubeconfigRequest is the request body for POST /api/v1/kubernetes/kubeconfig.
type KubeconfigRequest struct {
	ClusterID string `json:"clusterId"`
}

// KubeconfigResponse contains the kubeconfig and its expiration.
type KubeconfigResponse struct {
	ClusterID  string `json:"clusterId"`
	Kubeconfig string `json:"kubeconfig"`
	Expiration string `json:"expiration"` // RFC3339
}

// GetKubeconfig calls the STS provider to obtain a temporary kubeconfig for the given cluster.
func (c *HTTPClient) GetKubeconfig(ctx context.Context, clusterID string) (*KubeconfigResponse, error) {
	start := time.Now()
	statusCode := 0
	var observeErr error
	defer func() {
		hiclawmetrics.ObserveUpstream("sts_provider", "get_kubeconfig", start, statusCode, observeErr)
	}()

	if c.baseURL == "" {
		observeErr = errors.New("base URL not configured")
		return nil, errors.New("credprovider: base URL not configured (AGENTTEAMS_CREDENTIAL_PROVIDER_URL)")
	}
	body, err := json.Marshal(KubeconfigRequest{ClusterID: clusterID})
	if err != nil {
		observeErr = err
		return nil, fmt.Errorf("marshal kubeconfig request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/kubernetes/kubeconfig", bytes.NewReader(body))
	if err != nil {
		observeErr = err
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		observeErr = err
		return nil, fmt.Errorf("call credential-provider: %w", err)
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("credential-provider returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out KubeconfigResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		observeErr = fmt.Errorf("%w: %w", hiclawmetrics.ErrDecodeResponse, err)
		return nil, fmt.Errorf("parse kubeconfig response: %w", err)
	}
	if out.Kubeconfig == "" {
		observeErr = hiclawmetrics.ErrInvalidUpstreamResponse
		return nil, errors.New("credential-provider returned empty kubeconfig")
	}
	return &out, nil
}
