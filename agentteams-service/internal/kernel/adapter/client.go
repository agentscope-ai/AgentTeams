// Package adapter is the service → controller internal HTTP client (58 §5).
//
// It drives controller-side execution and resolves public bearer tokens over
// the four frozen /internal/v1 routes:
//
//	POST /internal/v1/workers/{name}/provision
//	POST /internal/v1/workers/{name}/deprovision
//	GET  /internal/v1/workers/{name}/execution
//	POST /internal/v1/token-review
//
// Every request carries the shared bearer token, X-AT-Request-Id and an
// RFC3339 UTC X-AT-Timestamp (the controller enforces a ±5min replay window);
// HMAC dual keys are an R2 enhancement (58 §5). The client never retries:
// retry / backoff / dead-letter live in the ops.outbox dispatcher (54 §4.4),
// and the controller treats a repeated (name, generation) provision as
// success, which is what makes that retry safe.
//
// Failures are collapsed into four sentinels (ErrNotFound, ErrConflict,
// ErrUnauthenticated, ErrUpstream) so callers — the resources handlers and
// the outbox dispatcher — never have to look at status codes. Every sentinel
// is wrapped with the operation and, for transport failures, the original
// error, so errors.Is works on both.
package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// Sentinel errors. Callers map them to the frozen 58 §2 public codes:
// ErrNotFound → 404, ErrConflict → 409, ErrUnauthenticated → 401,
// ErrUpstream → 502 (upstream_unavailable).
var (
	ErrNotFound        = errors.New("adapter: not found")
	ErrConflict        = errors.New("adapter: conflict")
	ErrUnauthenticated = errors.New("adapter: unauthenticated")
	ErrUpstream        = errors.New("adapter: upstream unavailable")
)

// UpstreamError marks a controller call that failed in transport or with an
// unexpected status. It wraps ErrUpstream (errors.Is still reaches it) and
// reports Unavailable so the identity middleware can answer 502 instead of
// 401 when the controller cannot be reached.
type UpstreamError struct{ Err error }

func (e *UpstreamError) Error() string     { return e.Err.Error() }
func (e *UpstreamError) Unwrap() error     { return e.Err }
func (e *UpstreamError) Unavailable() bool { return true }

func upstream(err error) error { return &UpstreamError{Err: err} }

const (
	// defaultTimeout bounds every internal call (58 §4: 每个长操作都有界).
	// It is a timeout, not a retry: the outbox dispatcher owns retries.
	defaultTimeout = 10 * time.Second

	headerRequestID = "X-AT-Request-Id"
	headerTimestamp = "X-AT-Timestamp"

	// maxErrorSnippet bounds how much of a non-2xx body is echoed into an
	// error; the controller's error bodies are small, and the request body
	// (which can carry a bearer token for token-review) is never echoed.
	maxErrorSnippet = 512

	// maxResponseBody bounds how much of a response is decoded or drained.
	// Internal responses are a single small object; the cap keeps a broken
	// controller from streaming an unbounded body into the service.
	maxResponseBody = 1 << 20 // 1 MiB
)

// Client calls the controller internal API. It is safe for concurrent use
// once constructed: all fields are read-only after configuration.
type Client struct {
	// BaseURL is the controller internal API root, e.g.
	// http://agentteams-controller:8090. A trailing slash is tolerated.
	BaseURL string
	// Token is the shared service↔controller bearer (58 §5).
	Token string
	// HTTP is the transport; nil → a default client with a 10s timeout.
	HTTP *http.Client
	// Now supplies X-AT-Timestamp; nil → time.Now. Injectable for tests.
	Now func() time.Time
	// RequestID supplies X-AT-Request-Id; nil → crypto/rand hex. Injectable
	// for tests.
	RequestID func() string
}

// New builds a client for the given controller base URL and shared bearer
// token, with the default 10s timeout.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: defaultTimeout},
	}
}

// Execution is the observed execution state of one worker as returned by
// GET /internal/v1/workers/{name}/execution (58 §5). The controller is the
// only writer of these fields; the service uses them for create retry
// decisions and get-by-name refresh.
type Execution struct {
	Name             string `json:"name"`
	Generation       int64  `json:"generation"`
	Phase            string `json:"phase"`
	ContainerState   string `json:"containerState,omitempty"`
	MatrixUserID     string `json:"matrixUserID,omitempty"`
	RoomID           string `json:"roomID,omitempty"`
	Message          string `json:"message,omitempty"`
	LastActiveAt     string `json:"lastActiveAt,omitempty"`
	AgentStatus      string `json:"agentStatus,omitempty"`
	RunningTaskCount *int   `json:"runningTaskCount,omitempty"`
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastFinishAt     string `json:"lastFinishAt,omitempty"`
	// SpecHash is optional and reserved for the 2e lazy refresh: an empty
	// value means this controller build cannot report it, so the service
	// must not treat "hash absent" as a spec mismatch.
	SpecHash string `json:"specHash,omitempty"`
}

// provisionRequest is the provision payload: the desired spec for
// (name, generation), byte-identical in meaning to the res.workers row the
// service owns (58 §4.1). The controller is idempotent on (name, generation),
// so redelivery from ops.outbox is safe (58 §5).
type provisionRequest struct {
	Name        string          `json:"name"`
	Generation  int64           `json:"generation"`
	Incarnation string          `json:"incarnation,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
}

// deprovisionRequest is the deprovision payload; idempotent exactly like
// provision.
type deprovisionRequest struct {
	Name        string `json:"name"`
	Generation  int64  `json:"generation"`
	Incarnation string `json:"incarnation,omitempty"`
}

// tokenReviewRequest is the body of POST /internal/v1/token-review.
type tokenReviewRequest struct {
	Token string `json:"token"`
}

// Provision asks the controller to execute the desired spec for
// (name, generation): container, Matrix identity and storage. The route is
// idempotent on that pair, so the outbox dispatcher may redeliver it.
func (c *Client) Provision(ctx context.Context, name string, generation int64, spec json.RawMessage, incarnation string) error {
	req := provisionRequest{Name: name, Generation: generation, Incarnation: incarnation, Spec: spec}
	op := fmt.Sprintf("provision %q", name)
	return c.do(ctx, op, http.MethodPost, workerPath(name, "provision"), req, nil)
}

// Deprovision asks the controller to stop the container and revoke the
// worker's credentials. Idempotent on (name, generation).
func (c *Client) Deprovision(ctx context.Context, name string, generation int64, incarnation string) error {
	req := deprovisionRequest{Name: name, Generation: generation, Incarnation: incarnation}
	op := fmt.Sprintf("deprovision %q", name)
	return c.do(ctx, op, http.MethodPost, workerPath(name, "deprovision"), req, nil)
}

// Execution reads the controller's observed execution state for one worker.
// It is the retry-decision and get-by-name refresh source (58 §5); a 404
// means the controller has no execution for that name (soft-delete signal).
func (c *Client) Execution(ctx context.Context, name string) (*Execution, error) {
	out := &Execution{}
	op := fmt.Sprintf("execution %q", name)
	if err := c.do(ctx, op, http.MethodGet, workerPath(name, "execution"), nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// TokenReview exchanges a public bearer token for the caller identity (58 §5).
// It implements identity.Reviewer; identity.Cache memoizes the result per
// token digest for the frozen 60s TTL. An invalid or expired token is a 401
// and surfaces as ErrUnauthenticated.
func (c *Client) TokenReview(ctx context.Context, token string) (*identity.Identity, error) {
	out := &identity.Identity{}
	if err := c.do(ctx, "token-review", http.MethodPost, "/internal/v1/token-review", tokenReviewRequest{Token: token}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Compile-time contract: the HTTP client is the identity cache's reviewer.
var _ identity.Reviewer = (*Client)(nil)

// do sends one internal call. It stamps the frozen headers, maps every
// non-2xx status through statusError, and decodes a 2xx JSON body into out
// when out is non-nil. Transport failures and timeouts become ErrUpstream
// wrapping the original error, so errors.Is reaches both.
func (c *Client) do(ctx context.Context, op, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("adapter: %s: encode request: %w", op, err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, payload)
	if err != nil {
		return fmt.Errorf("adapter: %s: build request: %w", op, err)
	}
	// Frozen internal-contract headers (58 §5): shared bearer, request id,
	// RFC3339 UTC timestamp inside the controller's ±5min replay window.
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set(headerRequestID, c.requestID())
	req.Header.Set(headerTimestamp, c.now().Format(time.RFC3339))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// Network failure, timeout or context cancellation: the controller
		// was not reached. Keep the cause in the chain for operators.
		return upstream(fmt.Errorf("%w: %s: %w", ErrUpstream, op, err))
	}
	defer func() {
		// Drain so the connection returns to the pool even on the error path,
		// but never let a broken controller stream without a bound.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(op, resp.StatusCode, resp.Body)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		// A 2xx with an undecodable body is a broken upstream contract, not
		// a caller error: report it as upstream so callers fail closed.
		return upstream(fmt.Errorf("%w: %s: decode response: %w", ErrUpstream, op, err))
	}
	return nil
}

// statusError maps a non-2xx controller status to the adapter sentinels:
// 401 → ErrUnauthenticated, 404 → ErrNotFound, 409 → ErrConflict, 5xx and
// every other 4xx → ErrUpstream (status code attached). Retrying is never
// decided here.
func statusError(op string, status int, body io.Reader) error {
	switch status {
	case http.StatusUnauthorized:
		// For token-review this is the caller's public token (invalid or
		// expired, 58 §5); for the worker routes it is the shared bearer.
		return fmt.Errorf("%w: %s: controller status 401", ErrUnauthenticated, op)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s: controller status 404", ErrNotFound, op)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s: controller status 409", ErrConflict, op)
	}
	if status >= 500 {
		return upstream(fmt.Errorf("%w: %s: controller status %d%s", ErrUpstream, op, status, errorSnippet(body)))
	}
	return upstream(fmt.Errorf("%w: %s: unexpected controller status %d%s", ErrUpstream, op, status, errorSnippet(body)))
}

// errorSnippet renders a short single-line excerpt of an error body for
// operator logs. It deliberately reads a bounded amount and never a request
// body (which may carry a bearer token during token-review).
func errorSnippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorSnippet))
	if err != nil {
		return ""
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if text == "" {
		return ""
	}
	return ": " + text
}

// baseURL returns the controller root without a trailing slash, so both
// New and a directly-built Client produce well-formed URLs.
func (c *Client) baseURL() string { return strings.TrimRight(c.BaseURL, "/") }

// httpClient returns the injected transport or the 10s default.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// now returns the timestamp clock in UTC.
func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// requestID returns the per-attempt request id: injected for tests, otherwise
// 16 random bytes in hex (128 bits, same shape as the service's own ids).
func (c *Client) requestID() string {
	if c.RequestID != nil {
		return c.RequestID()
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; a degraded but unique id
		// beats failing the request (the controller only logs it).
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// workerPath builds /internal/v1/workers/{name}/{action} with the name
// escaped, so an unexpected character cannot re-target the route.
func workerPath(name, action string) string {
	return "/internal/v1/workers/" + url.PathEscape(name) + "/" + action
}
