package taskforge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout is the per-request timeout used when the deployment does
// not configure AGENTTEAMS_TASKFORGE_TIMEOUT.
const DefaultTimeout = 10 * time.Second

// internalPathPrefix is the only path namespace the client may call. The
// gateway registers fixed routes and passes fixed internal paths; anything
// else is refused so the client can never become an open proxy.
const internalPathPrefix = "/internal/v1/"

// ErrUnavailable wraps connection failures and timeouts so every caller
// degrades uniformly (errors.Is).
var ErrUnavailable = errors.New("taskforge: upstream unavailable")

// Claim header names (design 21). Fixed by contract; TaskForge reads
// exactly these nine.
const (
	HeaderActorType  = "X-Atf-Actor-Type"
	HeaderActor      = "X-Atf-Actor"
	HeaderOnBehalfOf = "X-Atf-On-Behalf-Of"
	HeaderTeam       = "X-Atf-Team"
	HeaderRequestID  = "X-Request-Id"
	HeaderTraceID    = "X-Trace-Id"
	HeaderTokenJTI   = "X-Atf-Token-Jti"
	HeaderIssuedAt   = "X-Atf-Issued-At"
	HeaderSignature  = "X-Atf-Signature"
)

type forwardedHeaders struct {
	idempotencyKey string
	ifMatch        string
}

type forwardHeadersKey struct{}

// WithForwardedHeaders attaches the caller's Idempotency-Key / If-Match to
// the context so Do can forward them unchanged without widening its
// signature. Either value may be empty (only non-empty ones are set).
func WithForwardedHeaders(ctx context.Context, idempotencyKey, ifMatch string) context.Context {
	return context.WithValue(ctx, forwardHeadersKey{}, forwardedHeaders{idempotencyKey: idempotencyKey, ifMatch: ifMatch})
}

// Client talks to the TaskForge runner over its internal API.
type Client struct {
	BaseURL   *url.URL
	Secret    string
	HTTP      *http.Client // default 10s
	UserAgent string
}

// NewClient builds a Client. timeout <= 0 selects DefaultTimeout.
func NewClient(baseURL *url.URL, secret string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		BaseURL:   baseURL,
		Secret:    secret,
		HTTP:      &http.Client{Timeout: timeout},
		UserAgent: "agentteams-controller/taskforge-gateway",
	}
}

// Do signs cl, sets the nine claim headers, and performs the request
// against path (a fixed internal path under /internal/v1/, never
// caller-shaped). The response body is returned as-is — the caller decides
// how much to read; Do does not pre-read. Connection errors and timeouts
// are wrapped as ErrUnavailable.
//
// Hop-by-hop headers (Connection, Keep-Alive, Transfer-Encoding, Upgrade,
// Proxy-*) can never cross this boundary: the upstream request is built
// from scratch, and the only caller headers forwarded are the whitelisted
// Idempotency-Key / If-Match from the context.
func (c *Client) Do(ctx context.Context, cl Claims, method, path string, query url.Values, body []byte) (*http.Response, error) {
	if !strings.HasPrefix(path, internalPathPrefix) {
		return nil, fmt.Errorf("taskforge: refusing non-internal path %q", path)
	}
	u := *c.BaseURL
	u.Path = path
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("taskforge: %w", err)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set(HeaderActorType, string(cl.ActorType))
	req.Header.Set(HeaderActor, cl.ActorID)
	req.Header.Set(HeaderOnBehalfOf, cl.OnBehalfOf)
	req.Header.Set(HeaderTeam, cl.TeamID)
	req.Header.Set(HeaderRequestID, cl.RequestID)
	req.Header.Set(HeaderTraceID, cl.TraceID)
	req.Header.Set(HeaderTokenJTI, cl.TokenJTI)
	req.Header.Set(HeaderIssuedAt, strconv.FormatInt(cl.IssuedAt, 10))
	req.Header.Set(HeaderSignature, Sign(c.Secret, cl))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if fh, ok := ctx.Value(forwardHeadersKey{}).(forwardedHeaders); ok {
		if fh.idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", fh.idempotencyKey)
		}
		if fh.ifMatch != "" {
			req.Header.Set("If-Match", fh.ifMatch)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return resp, nil
}

// HealthStatus is the runner's self-reported health.
type HealthStatus struct {
	Healthy  bool   `json:"healthy"`
	Version  string `json:"version"`
	Protocol string `json:"protocol"`
}

// Health probes GET /internal/v1/healthz. A non-2xx answer is not an error
// — it reports healthy=false so the gateway can serve the cached
// degradation. Transport failures surface as ErrUnavailable.
func (c *Client) Health(ctx context.Context, cl Claims) (HealthStatus, error) {
	resp, err := c.Do(ctx, cl, http.MethodGet, "/internal/v1/healthz", nil, nil)
	if err != nil {
		return HealthStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HealthStatus{Healthy: false}, nil
	}
	var st HealthStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return HealthStatus{}, fmt.Errorf("taskforge: bad healthz body: %w", err)
	}
	return st, nil
}
