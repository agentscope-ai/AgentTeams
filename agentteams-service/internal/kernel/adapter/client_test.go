package adapter

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// capture is one request as observed by the fake controller.
type capture struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// fakeController serves one canned status and body. The reported channel is
// buffered, so the handler never blocks, and it also synchronizes the handler
// goroutine with the test (no shared struct, so -race stays quiet).
type fakeController struct {
	srv  *httptest.Server
	seen <-chan capture
}

func newFakeController(t *testing.T, status int, body string) fakeController {
	t.Helper()
	seen := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake controller: read request body: %v", err)
		}
		seen <- capture{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: raw}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return fakeController{srv: srv, seen: seen}
}

// TestProvisionRequestShape pins the frozen provision contract: method, path,
// the four internal headers and the {name, generation, spec} body.
func TestProvisionRequestShape(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, "")
	c := New(fc.srv.URL, "shared-token")
	c.Now = func() time.Time { return time.Date(2026, 11, 13, 0, 30, 0, 0, time.UTC) }
	c.RequestID = func() string { return "req-1" }

	spec := json.RawMessage(`{"runtime":"openclaw","model":"qwen3"}`)
	if err := c.Provision(context.Background(), "w1", 7, spec, "inc-1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	got := <-fc.seen
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if want := "/internal/v1/workers/w1/provision"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
	if h := got.header.Get("Authorization"); h != "Bearer shared-token" {
		t.Fatalf("Authorization = %q", h)
	}
	if h := got.header.Get(headerRequestID); h != "req-1" {
		t.Fatalf("%s = %q, want req-1", headerRequestID, h)
	}
	if h := got.header.Get(headerTimestamp); h != "2026-11-13T00:30:00Z" {
		t.Fatalf("%s = %q, want 2026-11-13T00:30:00Z", headerTimestamp, h)
	}
	if h := got.header.Get("Content-Type"); h != "application/json" {
		t.Fatalf("Content-Type = %q", h)
	}

	var payload struct {
		Name       string          `json:"name"`
		Generation int64           `json:"generation"`
		Spec       json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode provision body %q: %v", got.body, err)
	}
	if payload.Name != "w1" || payload.Generation != 7 {
		t.Fatalf("body = %+v, want name w1 generation 7", payload)
	}
	var fields map[string]string
	if err := json.Unmarshal(payload.Spec, &fields); err != nil {
		t.Fatalf("decode spec %q: %v", payload.Spec, err)
	}
	if fields["runtime"] != "openclaw" || fields["model"] != "qwen3" {
		t.Fatalf("spec = %v, want the raw spec passthrough", fields)
	}
}

// TestDeprovisionRequestShape pins the deprovision path and {name, generation}
// body (the idempotency key pair).
func TestDeprovisionRequestShape(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, "")
	c := New(fc.srv.URL, "tok")

	if err := c.Deprovision(context.Background(), "w2", 3, "inc-2"); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	got := <-fc.seen
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if want := "/internal/v1/workers/w2/deprovision"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
	var payload struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode deprovision body %q: %v", got.body, err)
	}
	if payload.Name != "w2" || payload.Generation != 3 {
		t.Fatalf("body = %+v, want name w2 generation 3", payload)
	}
}

// TestHeadersCarryReplayWindowTimestamp exercises the default clock and the
// default request id: RFC3339 UTC inside ±5min and 128-bit hex.
func TestHeadersCarryReplayWindowTimestamp(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, `{"name":"w1"}`)
	c := New(fc.srv.URL, "tok")
	before := time.Now()

	if _, err := c.Execution(context.Background(), "w1"); err != nil {
		t.Fatalf("Execution: %v", err)
	}
	got := <-fc.seen
	if got.method != http.MethodGet {
		t.Fatalf("method = %s, want GET", got.method)
	}
	if len(got.body) != 0 {
		t.Fatalf("GET body = %q, want empty", got.body)
	}

	raw := got.header.Get(headerTimestamp)
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("%s = %q is not RFC3339: %v", headerTimestamp, raw, err)
	}
	if skew := ts.Sub(before); skew < -5*time.Minute || skew > 5*time.Minute {
		t.Fatalf("%s skew = %v, want within ±5min (replay window, 58 §5)", headerTimestamp, skew)
	}
	rid := got.header.Get(headerRequestID)
	if len(rid) != 32 {
		t.Fatalf("%s = %q, want 32 hex chars", headerRequestID, rid)
	}
	if _, err := hex.DecodeString(rid); err != nil {
		t.Fatalf("%s = %q is not hex: %v", headerRequestID, rid, err)
	}
}

// TestExecutionParsesContract decodes every frozen Execution field, including
// the pointer that distinguishes "0 running tasks" from "not reported".
func TestExecutionParsesContract(t *testing.T) {
	const body = `{"name":"w1","generation":9,"phase":"Running","containerState":"running",` +
		`"matrixUserID":"@w1:agentteams.local","roomID":"!room:agentteams.local","message":"reconciled",` +
		`"lastActiveAt":"2026-11-12T23:59:00Z","agentStatus":"idle","runningTaskCount":2,` +
		`"lastRunAt":"2026-11-12T23:00:00Z","lastFinishAt":"2026-11-12T23:30:00Z","specHash":"sha256:abc"}`
	fc := newFakeController(t, http.StatusOK, body)
	c := New(fc.srv.URL, "tok")

	ex, err := c.Execution(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if want := "/internal/v1/workers/w1/execution"; (<-fc.seen).path != want {
		t.Fatalf("path != %q", want)
	}
	if ex.Name != "w1" || ex.Generation != 9 || ex.Phase != "Running" {
		t.Fatalf("identity fields = %+v", ex)
	}
	if ex.ContainerState != "running" || ex.MatrixUserID != "@w1:agentteams.local" || ex.RoomID != "!room:agentteams.local" {
		t.Fatalf("runtime fields = %+v", ex)
	}
	if ex.Message != "reconciled" || ex.AgentStatus != "idle" {
		t.Fatalf("status fields = %+v", ex)
	}
	if ex.LastActiveAt != "2026-11-12T23:59:00Z" || ex.LastRunAt != "2026-11-12T23:00:00Z" || ex.LastFinishAt != "2026-11-12T23:30:00Z" {
		t.Fatalf("timestamps = %+v", ex)
	}
	if ex.RunningTaskCount == nil || *ex.RunningTaskCount != 2 {
		t.Fatalf("runningTaskCount = %v, want 2", ex.RunningTaskCount)
	}
	if ex.SpecHash != "sha256:abc" {
		t.Fatalf("specHash = %q", ex.SpecHash)
	}
}

// TestExecutionWithoutSpecHash documents the 2e reservation: an absent hash
// must read as "not supported", never as a spec mismatch.
func TestExecutionWithoutSpecHash(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, `{"name":"w1","phase":"Pending"}`)
	c := New(fc.srv.URL, "tok")

	ex, err := c.Execution(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if ex.SpecHash != "" {
		t.Fatalf("specHash = %q, want empty (unsupported)", ex.SpecHash)
	}
	if ex.RunningTaskCount != nil {
		t.Fatalf("runningTaskCount = %v, want nil (not reported)", ex.RunningTaskCount)
	}
}

// TestTokenReviewShape pins POST /internal/v1/token-review and the identity
// body mapping (CallerIdentity).
func TestTokenReviewShape(t *testing.T) {
	const body = `{"matrixUserID":"@admin:agentteams.local","humanPermissionLevel":1,"role":"admin",` +
		`"username":"admin","team":"t1","teams":["t1","t2"],"capabilities":["workers.read"],` +
		`"accessibleWorkers":["w1"],"workerName":"w1"}`
	fc := newFakeController(t, http.StatusOK, body)
	c := New(fc.srv.URL, "tok")

	id, err := c.TokenReview(context.Background(), "tok-abc")
	if err != nil {
		t.Fatalf("TokenReview: %v", err)
	}
	got := <-fc.seen
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if want := "/internal/v1/token-review"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
	var req map[string]string
	if err := json.Unmarshal(got.body, &req); err != nil {
		t.Fatalf("decode token-review body %q: %v", got.body, err)
	}
	if len(req) != 1 || req["token"] != "tok-abc" {
		t.Fatalf("body = %v, want exactly {token: tok-abc}", req)
	}

	if id.Role != identity.RoleAdmin || id.Username != "admin" || id.MatrixUserID != "@admin:agentteams.local" {
		t.Fatalf("identity = %+v", id)
	}
	if id.HumanPermissionLevel != 1 {
		t.Fatalf("humanPermissionLevel = %d", id.HumanPermissionLevel)
	}
	if !id.TeamMatches("t2") || id.TeamMatches("t3") {
		t.Fatalf("teams = %v, TeamMatches is wrong", id.Teams)
	}
	if !id.WorkerReadable("other-team", "w1") {
		t.Fatalf("accessibleWorkers = %v, WorkerReadable is wrong", id.AccessibleWorkers)
	}
	if len(id.Capabilities) != 1 || id.Capabilities[0] != "workers.read" {
		t.Fatalf("capabilities = %v", id.Capabilities)
	}
}

// TestTokenReviewInvalidToken checks the invalid-token face: nil identity plus
// ErrUnauthenticated (the middleware maps it to 401 unauthenticated).
func TestTokenReviewInvalidToken(t *testing.T) {
	fc := newFakeController(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	c := New(fc.srv.URL, "tok")

	id, err := c.TokenReview(context.Background(), "stale")
	if id != nil {
		t.Fatalf("identity = %+v, want nil", id)
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
}

// TestStatusMapping walks every operation across the frozen status mapping so
// no route drifts: 401/404/409 are typed, 5xx and other 4xx are ErrUpstream.
func TestStatusMapping(t *testing.T) {
	ops := []struct {
		name string
		call func(c *Client) error
	}{
		{"provision", func(c *Client) error { return c.Provision(context.Background(), "w1", 1, nil, "") }},
		{"deprovision", func(c *Client) error { return c.Deprovision(context.Background(), "w1", 1, "") }},
		{"execution", func(c *Client) error { _, err := c.Execution(context.Background(), "w1"); return err }},
		{"token-review", func(c *Client) error { _, err := c.TokenReview(context.Background(), "tok"); return err }},
	}
	cases := []struct {
		status int
		want   error
		text   string
	}{
		{http.StatusOK, nil, ""},
		{http.StatusInternalServerError, ErrUpstream, "500"},
		{http.StatusBadGateway, ErrUpstream, "502"},
		{http.StatusBadRequest, ErrUpstream, "400"},
		{http.StatusUnauthorized, ErrUnauthenticated, ""},
		{http.StatusNotFound, ErrNotFound, ""},
		{http.StatusConflict, ErrConflict, ""},
	}
	all := []error{ErrNotFound, ErrConflict, ErrUnauthenticated, ErrUpstream}

	for _, op := range ops {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/%d", op.name, tc.status), func(t *testing.T) {
				fc := newFakeController(t, tc.status, `{"error":"boom"}`)
				c := New(fc.srv.URL, "tok")
				err := op.call(c)
				if tc.want == nil {
					if err != nil {
						t.Fatalf("status %d: err = %v, want nil", tc.status, err)
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
				}
				if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
					t.Fatalf("status %d: err = %v, want status code %q in message", tc.status, err, tc.text)
				}
				for _, other := range all {
					if other == tc.want {
						continue
					}
					if errors.Is(err, other) {
						t.Fatalf("status %d: err = %v also matches %v", tc.status, err, other)
					}
				}
			})
		}
	}
}

// TestUpstreamErrorKeepsCauseAndBody covers the operator-facing error detail:
// the operation, the status code and a bounded body snippet.
func TestUpstreamErrorKeepsCauseAndBody(t *testing.T) {
	fc := newFakeController(t, http.StatusInternalServerError, `{"error":"etcd is down"}`)
	c := New(fc.srv.URL, "tok")

	err := c.Provision(context.Background(), "w1", 1, nil, "")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	msg := err.Error()
	for _, want := range []string{"provision", `"w1"`, "500", "etcd is down"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("err = %q, want it to mention %q", msg, want)
		}
	}
}

// TestTransportFailureIsUpstream uses a closed httptest server: the controller
// is unreachable, which must be ErrUpstream (the outbox dispatcher retries).
func TestTransportFailureIsUpstream(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, "")
	fc.srv.Close() // unreachable from here on
	c := New(fc.srv.URL, "tok")

	_, err := c.Execution(context.Background(), "w1")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !strings.Contains(err.Error(), "execution") {
		t.Fatalf("err = %q, want the operation in the message", err)
	}
}

// TestCanceledContextIsUpstream checks that a canceled request is upstream
// failing, keeps context.Canceled in the chain, and never reaches the wire.
func TestCanceledContextIsUpstream(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, "")
	c := New(fc.srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Execution(ctx, "w1")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in the chain", err)
	}
	select {
	case <-fc.seen:
		t.Fatal("a canceled call still reached the controller")
	default:
	}
}

// TestClientTimeoutIsUpstream checks the injected transport timeout face.
func TestClientTimeoutIsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	c.HTTP = &http.Client{Timeout: 30 * time.Millisecond}

	_, err := c.Execution(context.Background(), "w1")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

// TestNewInstallsDefaultTimeout pins the 10s default (bounded calls).
func TestNewInstallsDefaultTimeout(t *testing.T) {
	c := New("http://agentteams-controller:8090", "tok")
	if c.HTTP == nil {
		t.Fatal("New must install a default HTTP client")
	}
	if c.HTTP.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want 10s", c.HTTP.Timeout)
	}
	if c.BaseURL != "http://agentteams-controller:8090" || c.Token != "tok" {
		t.Fatalf("client = %+v", c)
	}
}

// TestZeroValueClientUsesDefaults keeps a directly-built Client usable: nil
// HTTP falls back to the 10s client and a trailing slash must not double up.
func TestZeroValueClientUsesDefaults(t *testing.T) {
	fc := newFakeController(t, http.StatusOK, `{"name":"w1","phase":"Pending"}`)
	c := &Client{BaseURL: fc.srv.URL + "/", Token: "tok"}

	ex, err := c.Execution(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if ex.Name != "w1" {
		t.Fatalf("execution = %+v", ex)
	}
	if want := "/internal/v1/workers/w1/execution"; (<-fc.seen).path != want {
		t.Fatalf("path != %q", want)
	}
}

// TestWorkerPathEscapesName keeps an odd name from re-targeting the route.
func TestWorkerPathEscapesName(t *testing.T) {
	if got, want := workerPath("w 1", "provision"), "/internal/v1/workers/w%201/provision"; got != want {
		t.Fatalf("workerPath = %q, want %q", got, want)
	}
}
