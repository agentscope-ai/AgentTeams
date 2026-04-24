package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

const (
	nacosPreflightTimeout = 5 * time.Second
)

// NacosAIClient is the unified entry point for Nacos AI capabilities.
// Authentication is fully delegated to cred; the struct holds no auth state.
type NacosAIClient struct {
	serverAddr string
	namespace  string
	httpClient *http.Client
	cred       nacosCredential
}

// NewNacosAIClient constructs and connects a NacosAIClient.
//
//   - rawAddr: host:port, optionally prefixed with user:pass@ or nacos://
//   - namespace: Nacos namespace ID (empty → "public")
//   - authType: "nacos" | "sts-hiclaw" | "none" | "" (auto-detect from addr)
//   - credClient: required only for "sts-hiclaw"; pass nil otherwise
func NewNacosAIClient(
	ctx context.Context,
	rawAddr string,
	namespace string,
	authType string,
	credClient credprovider.Client,
) (*NacosAIClient, error) {
	host, port, username, password, err := parseNacosAddr(rawAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid nacos address %q: %w", rawAddr, err)
	}

	if namespace == "" {
		namespace = "public"
	}

	serverAddr := net.JoinHostPort(host, port)
	httpCl := &http.Client{Timeout: 60 * time.Second}

	// Auto-detect auth type when not explicitly set.
	if authType == "" {
		if username != "" && password != "" {
			authType = nacosAuthTypeNacos
		} else if username != "" || password != "" {
			return nil, fmt.Errorf("both username and password are required in nacos URL or env (use nacos://user:pass@host:port or set HICLAW_NACOS_USERNAME/HICLAW_NACOS_PASSWORD)")
		} else {
			authType = nacosAuthTypeNone
		}
	}

	var cred nacosCredential
	switch authType {
	case nacosAuthTypeNacos:
		if username == "" || password == "" {
			return nil, fmt.Errorf("nacos auth requires username and password")
		}
		cred = &nacosUserPassCredential{
			serverAddr: serverAddr,
			username:   username,
			password:   password,
			httpClient: httpCl,
		}
	case nacosAuthTypeSTS:
		if credClient == nil {
			return nil, fmt.Errorf("sts-hiclaw auth requires a credprovider.Client")
		}
		cred = newNacosSTSCredential(namespace, credClient)
	case nacosAuthTypeNone, "":
		cred = nacosNoneCredential{}
	default:
		return nil, fmt.Errorf("unsupported nacos auth type %q", authType)
	}

	client := &NacosAIClient{
		serverAddr: serverAddr,
		namespace:  namespace,
		httpClient: httpCl,
		cred:       cred,
	}

	if err := client.preflightConnect(ctx); err != nil {
		return nil, err
	}

	if err := cred.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("credential refresh failed: %w", err)
	}

	return client, nil
}

func (c *NacosAIClient) preflightConnect(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, nacosPreflightTimeout)
		defer cancel()
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.serverAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", c.serverAddr, err)
	}
	return conn.Close()
}

// prepareRequest refreshes credentials if needed and applies auth headers.
// Must be called before every outbound HTTP request.
func (c *NacosAIClient) prepareRequest(ctx context.Context, req *http.Request) error {
	if err := c.cred.Refresh(ctx); err != nil {
		return fmt.Errorf("credential refresh: %w", err)
	}
	c.cred.Apply(req)
	return nil
}

// ── parseNacosAddr ────────────────────────────────────────────────────────

func parseNacosAddr(raw string) (host, port, username, password string, err error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", err
	}
	if parsed.Hostname() == "" {
		return "", "", "", "", fmt.Errorf("missing host")
	}

	port = parsed.Port()
	if port == "" {
		port = "8848"
	}

	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}

	if username == "" && password == "" {
		username = os.Getenv("HICLAW_NACOS_USERNAME")
		password = os.Getenv("HICLAW_NACOS_PASSWORD")
	}

	return parsed.Hostname(), port, username, password, nil
}
