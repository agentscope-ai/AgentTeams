package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/taskforge"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const tfHealthyJSON = `{"healthy":true,"version":"1.2.7","protocol":"atf"}`

const tfTestSecret = "0123456789abcdef"

// tfFakeRunner is a stub TaskForge upstream: /internal/v1/healthz answers
// with healthBody, everything else is recorded and answered with
// status/body (or bigResponse for the size-limit case).
type tfFakeRunner struct {
	mu          sync.Mutex
	healthBody  string
	status      int
	body        string
	bigResponse bool
	seen        int // non-healthz requests seen
	lastPath    string
	lastQuery   string
	lastHeaders http.Header
	lastBody    []byte
}

func (f *tfFakeRunner) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/internal/v1/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.healthBody))
		return
	}
	f.seen++
	f.lastPath = r.URL.Path
	f.lastQuery = r.URL.RawQuery
	f.lastHeaders = r.Header.Clone()
	f.lastBody, _ = io.ReadAll(r.Body)
	if f.bigResponse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, maxTaskForgeResponse+16))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.body))
}

// tfTestServer builds the full HTTP server with the minimal deps proven by
// human_files_changes_route_test.go plus the TaskForge client (nil = off).
func tfTestServer(t *testing.T, tf *taskforge.Client, upstreamURL string) *HTTPServer {
	t.Helper()
	return NewHTTPServer(":0", ServerDeps{
		Client:            fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).Build(),
		Namespace:         "default",
		AuthMw:            authpkg.NewMiddleware(nil, nil, nil, nil, ""),
		TaskForge:         tf,
		TaskForgeUpstream: upstreamURL,
	})
}

func tfDo(t *testing.T, srv *HTTPServer, method, target string, body []byte, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	if caller != nil {
		req = withCaller(req, caller)
	}
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	return rec
}

func tfAdmin() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
}

// tfOffPaths is the fixed §5.2 route table: all 14 must 404 when the
// gateway is off (task 26 §7).
var tfOffPaths = []struct {
	method, path string
}{
	{http.MethodPost, "/api/v1/tasks/t-1/claim"},
	{http.MethodPost, "/api/v1/tasks/t-1/submit"},
	{http.MethodPost, "/api/v1/tasks/t-1/accept"},
	{http.MethodPost, "/api/v1/tasks/t-1/reject"},
	{http.MethodPost, "/api/v1/tasks/t-1/cancel"},
	{http.MethodPost, "/api/v1/tasks/t-1/restore"},
	{http.MethodGet, "/api/v1/tasks/t-1/timeline"},
	{http.MethodGet, "/api/v1/tasks/t-1/checkpoints"},
	{http.MethodGet, "/api/v1/tasks/t-1/artifacts"},
	{http.MethodGet, "/api/v1/loops"},
	{http.MethodGet, "/api/v1/loops/loop-1"},
	{http.MethodGet, "/api/v1/events"},
	{http.MethodGet, "/api/v1/runner-modules/taskforge/status"},
	{http.MethodPost, "/api/v1/runner-modules/taskforge/update"},
}

// TestTaskForgeGatewayOffIs404 pins the zero-impact contract: with
// AGENTTEAMS_TASKFORGE_URL unset (deps.TaskForge nil) none of the 14
// gateway routes exists, so every one 404s exactly as before the gateway.
func TestTaskForgeGatewayOffIs404(t *testing.T) {
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).Build(),
		Namespace: "default",
		AuthMw:    authpkg.NewMiddleware(nil, nil, nil, nil, ""),
		// TaskForge intentionally nil: gateway off.
	})
	for _, p := range tfOffPaths {
		if rec := tfDo(t, srv, p.method, p.path, nil, tfAdmin()); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 with the gateway off", p.method, p.path, rec.Code)
		}
	}
}

// TestTaskForgeGatewayForwardsSignedClaims pins the open+healthy path:
// fixed path rewrite, query whitelist, signed claim headers (verified with
// the shared secret), request body byte-equality, and response passthrough.
func TestTaskForgeGatewayForwardsSignedClaims(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON, status: 200, body: `{"ok":true}`}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodPost, "/api/v1/tasks/t-1/claim?limit=10&evil=1", []byte(`{"note":"x"}`), tfAdmin())
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("forward: %d %s", rec.Code, rec.Body.String())
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.lastPath != "/internal/v1/tasks/t-1/claim" {
		t.Fatalf("upstream path = %q, want /internal/v1/tasks/t-1/claim", fr.lastPath)
	}
	if fr.lastQuery != "limit=10" {
		t.Fatalf("upstream query = %q, want limit=10 (evil dropped by the whitelist)", fr.lastQuery)
	}
	if string(fr.lastBody) != `{"note":"x"}` {
		t.Fatalf("upstream body = %q, want the exact request bytes", fr.lastBody)
	}
	if got := fr.lastHeaders.Get(taskforge.HeaderActor); got != "admin" {
		t.Fatalf("X-Atf-Actor = %q, want admin", got)
	}
	if got := fr.lastHeaders.Get(taskforge.HeaderActorType); got != string(taskforge.ActorHuman) {
		t.Fatalf("X-Atf-Actor-Type = %q, want human (admin signs as human)", got)
	}
	if got := fr.lastHeaders.Get(taskforge.HeaderRequestID); got == "" {
		t.Fatal("X-Request-Id empty; the claim must always carry a request id")
	}
	if got := fr.lastHeaders.Get(taskforge.HeaderTraceID); got == "" {
		t.Fatal("X-Trace-Id empty; the claim must always carry a trace id")
	}
	sig := fr.lastHeaders.Get(taskforge.HeaderSignature)
	if sig == "" {
		t.Fatal("missing X-Atf-Signature")
	}
	// The signature must verify against the captured headers with the shared
	// secret — the runner can authenticate the controller end to end.
	issued, err := strconv.ParseInt(fr.lastHeaders.Get(taskforge.HeaderIssuedAt), 10, 64)
	if err != nil {
		t.Fatalf("X-Atf-Issued-At = %q: %v", fr.lastHeaders.Get(taskforge.HeaderIssuedAt), err)
	}
	cl := taskforge.Claims{
		ActorType: taskforge.ActorHuman,
		ActorID:   "admin",
		RequestID: fr.lastHeaders.Get(taskforge.HeaderRequestID),
		TraceID:   fr.lastHeaders.Get(taskforge.HeaderTraceID),
		IssuedAt:  issued,
	}
	if err := taskforge.Verify(tfTestSecret, cl, sig, time.Now()); err != nil {
		t.Fatalf("captured signature does not verify with the shared secret: %v", err)
	}
}

// TestTaskForgeGatewayRejectsBadResourceID pins the §5.2 whitelist: a path
// value outside [A-Za-z0-9._-]{1,128} is a 400 BEFORE any upstream contact.
func TestTaskForgeGatewayRejectsBadResourceID(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON, status: 200, body: `{}`}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/tasks/t-1*evil/timeline", nil, tfAdmin())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_resource_id") {
		t.Fatalf("body = %s, want invalid_resource_id", rec.Body.String())
	}
	fr.mu.Lock()
	seen := fr.seen
	fr.mu.Unlock()
	if seen != 0 {
		t.Fatalf("upstream hit %d times, want 0 (id is checked before any upstream contact)", seen)
	}
}

// TestTaskForgeGatewayPassesUpstreamJSONError pins the §5.3 error mapping:
// an upstream 4xx/5xx whose body is a JSON envelope with error.code is
// passed through unchanged (status + body).
func TestTaskForgeGatewayPassesUpstreamJSONError(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON, status: http.StatusServiceUnavailable,
		body: `{"error":{"code":"task_not_found","message":"no such task"}}`}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/tasks/t-1/timeline", nil, tfAdmin())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want the upstream 503", rec.Code)
	}
	if want := `{"error":{"code":"task_not_found","message":"no such task"}}`; rec.Body.String() != want {
		t.Fatalf("body = %s, want the upstream envelope unchanged", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// TestTaskForgeGatewayDegradesWhenUnhealthy pins the "宁可 503" contract:
// a runner reporting unhealthy is never forwarded to; the client gets 503 +
// Retry-After: 5 + the taskforge_unavailable envelope.
func TestTaskForgeGatewayDegradesWhenUnhealthy(t *testing.T) {
	fr := &tfFakeRunner{healthBody: `{"healthy":false}`}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/loops", nil, tfAdmin())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), "taskforge_unavailable") {
		t.Fatalf("body = %s, want taskforge_unavailable", rec.Body.String())
	}
	fr.mu.Lock()
	seen := fr.seen
	fr.mu.Unlock()
	if seen != 0 {
		t.Fatalf("unhealthy upstream must not be forwarded to, seen = %d", seen)
	}
}

// TestTaskForgeGatewayRejectsOversizedResponse pins the §5.1 body limit:
// an upstream answer larger than 8 MiB is a 502 taskforge_response_too_large.
func TestTaskForgeGatewayRejectsOversizedResponse(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON, status: 200, bigResponse: true}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/events", nil, tfAdmin())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "taskforge_response_too_large") {
		t.Fatalf("body = %s, want taskforge_response_too_large", rec.Body.String())
	}
}

// TestTaskForgeGatewayStatusIsLocal pins §5.2: the status route answers
// locally from the health cache — the runner is probed (healthz) but never
// forwarded to.
func TestTaskForgeGatewayStatusIsLocal(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/runner-modules/taskforge/status", nil, tfAdmin())
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status body is not JSON: %s", rec.Body.String())
	}
	if st["enabled"] != true || st["healthy"] != true || st["version"] != "1.2.7" || st["protocol"] != "atf" {
		t.Fatalf("status = %s, want enabled/healthy/1.2.7/atf", rec.Body.String())
	}
	fr.mu.Lock()
	seen := fr.seen
	fr.mu.Unlock()
	if seen != 0 {
		t.Fatalf("status must be local (no forward), seen = %d", seen)
	}
}

// TestTaskForgeGatewayRequiresCaller pins the fail-closed guard: a request
// without an authenticated caller is a 401 before any upstream contact
// (production routes always carry a caller — RequireAuthz — so this is the
// defense against a mis-registered bare route).
func TestTaskForgeGatewayRequiresCaller(t *testing.T) {
	fr := &tfFakeRunner{healthBody: tfHealthyJSON, status: 200, body: `{}`}
	up := httptest.NewServer(http.HandlerFunc(fr.handler))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	srv := tfTestServer(t, taskforge.NewClient(base, tfTestSecret, time.Second), up.URL)

	rec := tfDo(t, srv, http.MethodGet, "/api/v1/loops", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	fr.mu.Lock()
	seen := fr.seen
	fr.mu.Unlock()
	if seen != 0 {
		t.Fatalf("no-caller request must not reach the upstream, seen = %d", seen)
	}
}
