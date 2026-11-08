package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// mcpTestUpstream records the proxied call and replays a canned response
// (one worker's qwenpaw app GET /api/mcp).
type mcpTestUpstream struct {
	method, path string
	dialed       bool
	status       int
	response     string
}

func (u *mcpTestUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.dialed = true
		u.method = r.Method
		u.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.response))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newTestWorkerMCPProxy builds the proxy over a fake K8s client (given
// teams and workers) whose worker URL points at the given httptest server
// (mimicking one worker's qwenpaw app).
func newTestWorkerMCPProxy(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *workerMCPProxy {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	p := &workerMCPProxy{
		rh:              NewResourceHandler(k8s, "default", nil, "", nil),
		kubeMode:        kubeMode,
		containerPrefix: "agentteams-worker-",
		http:            &http.Client{Timeout: workerMCPProxyTimeout},
	}
	if ts != nil {
		p.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	} else {
		p.workerBaseURL = p.defaultWorkerBaseURL
	}
	return p
}

// workerMCPRequest builds a request with the path value the mux would set
// (name), so handler validation — not URL parsing — is what gets tested.
func workerMCPRequest(name string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/placeholder/mcp", nil)
	req.SetPathValue("name", name)
	return req
}

// TestWorkerMCP_ForwardsVerbatim: an L1 (admin) reader gets the worker's
// qwenpaw app GET /api/mcp response (its self-registered MCP servers)
// passed through verbatim — no re-wrapping of fields, no projection.
func TestWorkerMCP_ForwardsVerbatim(t *testing.T) {
	const payload = `[{"key":"teamharness","enabled":true}]`
	u := &mcpTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerMCPRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodGet || u.path != "/api/mcp" {
		t.Fatalf("upstream=%s %s, want GET /api/mcp", u.method, u.path)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
}

// TestWorkerMCP_KubeMode503: kube mode answers 503 BEFORE any worker
// lookup or dial (uniform, so worker existence cannot be probed).
func TestWorkerMCP_KubeMode503(t *testing.T) {
	u := &mcpTestUpstream{status: http.StatusOK, response: `[]`}
	p := newTestWorkerMCPProxy(t, "kube", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerMCPRequest("daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
	if u.dialed {
		t.Fatal("kube mode must not dial the worker")
	}
}

// TestWorkerMCP_EntryPoint: the production wiring — ResourceHandler.WorkerMCP
// honoring the mode argument with no test-only injection.
func TestWorkerMCP_EntryPoint(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeamWithWorkers("team-a", "daily-carol")...).Build()
	handler := NewResourceHandler(k8s, "default", nil, "", nil).WorkerMCP("kube", "agentteams-worker-")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, adminCaller(workerMCPRequest("daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// TestWorkerMCP_UnknownWorker404: an unknown worker hides as 404 (W8).
func TestWorkerMCP_UnknownWorker404(t *testing.T) {
	p := newTestWorkerMCPProxy(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerMCPRequest("ghost-worker")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

// TestWorkerMCP_CrossTeam404: an out-of-scope caller (team leader or L2
// human of another team) sees 404 — worker existence is not probeable
// (W8), and the worker is never dialed.
func TestWorkerMCP_CrossTeam404(t *testing.T) {
	u := &mcpTestUpstream{status: http.StatusOK, response: `[]`}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("alpha-team", "daily-carol")...)
	for _, caller := range []*authpkg.CallerIdentity{
		{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta-team"},
		{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"beta-team"}},
	} {
		rec := httptest.NewRecorder()
		p.serve(rec, withCaller(workerMCPRequest("daily-carol"), caller))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("role=%s status=%d, want 404 for cross-team access (W8)", caller.Role, rec.Code)
		}
	}
	if u.dialed {
		t.Fatal("cross-team read must not dial the worker")
	}
}

// workerMCPStaticAuth resolves one fixed token to a fixed identity (test
// double for the SA TokenReview path).
type workerMCPStaticAuth struct {
	token string
	id    *authpkg.CallerIdentity
}

func (s *workerMCPStaticAuth) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	if token != s.token {
		return nil, errors.New("unknown token")
	}
	return s.id, nil
}

// TestWorkerMCP_HTTPOAuthChain exercises the full HTTP chain — bearer
// extraction, authentication, authorization, route registration — for the
// unauthorized outcomes (the handler tests above bypass authentication):
// no/invalid token → 401, and a worker-role token against ANOTHER worker
// → 403 (worker reads are self-only).
func TestWorkerMCP_HTTPOAuthChain(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeamWithWorkers("team-a", "daily-carol")...).Build()
	mw := authpkg.NewMiddleware(
		&workerMCPStaticAuth{token: "worker-token", id: &authpkg.CallerIdentity{
			Role: authpkg.RoleWorker, Username: "other-worker", WorkerName: "other-worker",
		}},
		nil,
		authpkg.NewAuthorizer(),
		k8s, "default",
	)
	srv := NewHTTPServer(":0", ServerDeps{
		Client:          k8s,
		Namespace:       "default",
		KubeMode:        "embedded",
		ContainerPrefix: "agentteams-worker-",
		AuthMw:          mw,
	})

	// No token -> 401.
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d, want 401", rec.Code)
	}

	// Invalid token -> 401.
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/mcp", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, bad)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status=%d, want 401", rec2.Code)
	}

	// Worker-role token against another worker -> 403 (self-only reads;
	// a worker never sees a foreign worker's MCP registry).
	self := httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/mcp", nil)
	self.Header.Set("Authorization", "Bearer worker-token")
	rec3 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec3, self)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("worker cross-target: status=%d, want 403", rec3.Code)
	}
}

// mcpSanitizePayload is a realistic MCP registry list carrying a SENTINEL
// credential in EVERY documented location (env value, header value, URL
// userinfo, URL query) plus the drift shape the projection must absorb
// (an entry identified by key only, no name): none of the SENTINELs may
// appear in ANY non-L1 response, and the non-L1 body must equal
// mcpProjectPayload exactly. The L1 verbatim round-trip preserves this
// exact string — credentials and all.
const mcpSanitizePayload = `[
  {"key":"matrix","name":"Matrix","enabled":true,"transport":"stdio","command":"python","args":["-m","matrix"],"env":{"AGENTTEAMS_WORKER_MATRIX_TOKEN":"SENTINEL-matrix-token"},"headers":{"X-Api-Key":"SENTINEL-header-key"},"url":"http://bot:pass@mcp.example.com:9000/mcp"},
  {"key":"fs","name":"FS","enabled":false,"transport":"http","url":"https://mcp.example.com/sse"},
  {"key":"search","name":"Search","enabled":true,"transport":"http","url":"https://api.example.com/v1/mcp?api_key=SENTINEL-query-key"},
  {"key":"legacy-srv","enabled":true,"transport":"stdio"}
]`

// mcpProjectPayload is the exact whitelist projection of
// mcpSanitizePayload: per server only name (the last entry has no name,
// so key is the fallback), transport, enabled, and a redacted url
// (host+path — userinfo and query stripped, the redactMCPURL contract);
// env, headers, args, command, and key are omitted whole. Key order is
// Go's json.Marshal sorted-map order.
const mcpProjectPayload = `[{"enabled":true,"name":"Matrix","transport":"stdio","url":"mcp.example.com:9000/mcp"},{"enabled":false,"name":"FS","transport":"http","url":"mcp.example.com/sse"},{"enabled":true,"name":"Search","transport":"http","url":"api.example.com/v1/mcp"},{"enabled":true,"name":"legacy-srv","transport":"stdio"}]`

// TestWorkerMCP_NonL1ProjectsSafeMetadata: every reader below L1 (L3
// worker-scoped human, L2 team leader, the worker's own SA) gets EXACTLY
// the whitelist projection — no SENTINEL credential from any documented
// location survives, no args/command/key field survives, and nothing is
// lost or invented (exact-string equality against mcpProjectPayload).
// L1 (admin) keeps the verbatim round-trip with env/headers/args intact
// (#1453 items 1, 2, 7).
func TestWorkerMCP_NonL1ProjectsSafeMetadata(t *testing.T) {
	u := &mcpTestUpstream{status: http.StatusOK, response: mcpSanitizePayload}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	nonL1 := []*authpkg.CallerIdentity{
		{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"daily-carol"}},
		{Role: authpkg.RoleTeamLeader, Username: "team-a-lead", Team: "team-a"},
		{Role: authpkg.RoleWorker, Username: "daily-carol", WorkerName: "daily-carol"},
	}
	for _, caller := range nonL1 {
		rec := httptest.NewRecorder()
		p.serve(rec, withCaller(workerMCPRequest("daily-carol"), caller))
		if rec.Code != http.StatusOK {
			t.Fatalf("role=%s status=%d, want 200", caller.Role, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "SENTINEL") {
			t.Fatalf("role=%s body leaked a credential: %s", caller.Role, body)
		}
		for _, dropped := range []string{`"env"`, `"headers"`, `"args"`, `"command"`, `"key"`} {
			if strings.Contains(body, dropped) {
				t.Fatalf("role=%s body kept a non-whitelisted field %s: %s", caller.Role, dropped, body)
			}
		}
		if body != mcpProjectPayload {
			t.Fatalf("role=%s body is not the exact projection:\n got %s\nwant %s", caller.Role, body, mcpProjectPayload)
		}
	}

	// L1 (admin): verbatim — env/headers/args and every SENTINEL intact.
	recA := httptest.NewRecorder()
	p.serve(recA, withCaller(workerMCPRequest("daily-carol"), &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if recA.Code != http.StatusOK {
		t.Fatalf("admin status=%d, want 200", recA.Code)
	}
	if recA.Body.String() != mcpSanitizePayload {
		t.Fatalf("admin body not verbatim:\n got %s\nwant %s", recA.Body.String(), mcpSanitizePayload)
	}
	for _, kept := range []string{
		`"env"`, `"headers"`, `"args"`,
		"SENTINEL-matrix-token", "SENTINEL-header-key", "SENTINEL-query-key", "bot:pass@",
	} {
		if !strings.Contains(recA.Body.String(), kept) {
			t.Fatalf("admin body lost raw field %s: %s", kept, recA.Body.String())
		}
	}
}

// TestWorkerMCP_NonL1DropsArgsAndQuerySentinels: #1453 maintainer finding
// 1, verbatim shape — a credential riding in args (["--token=SENTINEL"])
// or a URL query (?api_key=SENTINEL) must not survive to ANY non-L1
// response: args is omitted whole (no "args" key at all) and url is
// redacted to host+path by the redactMCPURL contract.
func TestWorkerMCP_NonL1DropsArgsAndQuerySentinels(t *testing.T) {
	const payload = `[{"name":"x","args":["--token=SENTINEL"],"url":"https://host.example.com/mcp?api_key=SENTINEL"}]`
	u := &mcpTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "team-a-lead", Team: "team-a"}
	rec := httptest.NewRecorder()
	p.serve(rec, withCaller(workerMCPRequest("daily-carol"), leader))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "SENTINEL") {
		t.Fatalf("non-L1 body leaked a SENTINEL credential: %s", body)
	}
	if strings.Contains(body, `"args"`) {
		t.Fatalf("non-L1 body kept the args key: %s", body)
	}
	if body != `[{"name":"x","url":"host.example.com/mcp"}]` {
		t.Fatalf("body is not the host+path projection:\n got %s", body)
	}
}

// TestWorkerMCP_OversizedUpstream502: a 200 body strictly longer than the
// cap is 502 (the cap+1 read keeps the (cap+1)th byte as evidence) —
// never a truncated 200 (#1453 maintainer finding 2). L1 included: the
// cap is a proxy invariant, not a per-reader mask.
func TestWorkerMCP_OversizedUpstream502(t *testing.T) {
	const prefix = `[{"name":"x","pad":"`
	const suffix = `"}`
	oversized := prefix + strings.Repeat("a", workerMCPBodyCap+1-len(prefix)-len(suffix)) + suffix
	if len(oversized) != workerMCPBodyCap+1 {
		t.Fatalf("test setup: len=%d, want %d", len(oversized), workerMCPBodyCap+1)
	}
	u := &mcpTestUpstream{status: http.StatusOK, response: oversized}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerMCPRequest("daily-carol")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 for an oversized body", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds 64 KiB") {
		t.Fatalf("body=%s, want the cap message", rec.Body.String())
	}
}

// TestWorkerMCP_ExactCapAccepted: a 200 body of EXACTLY cap bytes is the
// boundary — 200 and complete (the cap+1 read rejects strictly longer
// bodies only).
func TestWorkerMCP_ExactCapAccepted(t *testing.T) {
	const prefix = `[{"name":"x","pad":"`
	const suffix = `"}]`
	exact := prefix + strings.Repeat("a", workerMCPBodyCap-len(prefix)-len(suffix)) + suffix
	if len(exact) != workerMCPBodyCap {
		t.Fatalf("test setup: len=%d, want %d", len(exact), workerMCPBodyCap)
	}
	u := &mcpTestUpstream{status: http.StatusOK, response: exact}
	p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerMCPRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 at the exact cap", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != exact {
		t.Fatal("body is not the complete verbatim array at the exact cap")
	}
}

// TestWorkerMCP_MalformedUpstream502: a 200 body that is not a valid JSON
// array is 502 for EVERY reader — never passed through raw, never
// degraded to an empty array (#1453 maintainer finding 2: the [] fallback
// reported upstream breakage as "no servers").
func TestWorkerMCP_MalformedUpstream502(t *testing.T) {
	callers := []*authpkg.CallerIdentity{
		{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"daily-carol"}},
		{Role: authpkg.RoleTeamLeader, Username: "team-a-lead", Team: "team-a"},
		{Role: authpkg.RoleAdmin, Username: "admin"},
	}
	for _, payload := range []string{
		`{"not":"an array" and not even json`, // not JSON at all
		`{"not":"an array"}`,                  // JSON, but not an array
	} {
		u := &mcpTestUpstream{status: http.StatusOK, response: payload}
		p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
		for _, caller := range callers {
			rec := httptest.NewRecorder()
			p.serve(rec, withCaller(workerMCPRequest("daily-carol"), caller))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("payload=%q role=%s status=%d, want 502", payload, caller.Role, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "malformed JSON") {
				t.Fatalf("payload=%q role=%s body=%s, want the malformed-JSON message", payload, caller.Role, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), payload) {
				t.Fatalf("payload=%q role=%s body echoed the upstream: %s", payload, caller.Role, rec.Body.String())
			}
		}
	}
}

// TestWorkerMCP_UpstreamErrorEchoFree: ANY non-200 upstream status is a
// uniform 502 whose body never carries upstream bytes (#1453: the old
// default branch echoed the upstream body through truncateForMessage,
// and the 400/404/409/422 branch copied it verbatim).
func TestWorkerMCP_UpstreamErrorEchoFree(t *testing.T) {
	for _, status := range []int{400, 404, 409, 422, 500, 502, 503} {
		u := &mcpTestUpstream{status: status, response: `{"error":"SENTINEL-upstream-detail"}`}
		p := newTestWorkerMCPProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		p.serve(rec, adminCaller(workerMCPRequest("daily-carol")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream=%d status=%d, want 502", status, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "SENTINEL") {
			t.Fatalf("upstream=%d body echoed upstream bytes: %s", status, rec.Body.String())
		}
	}
}
