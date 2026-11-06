package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pluginsRouteStaticAuth resolves fixed tokens to fixed identities (a
// test double for the SA TokenReview path; a token table, because the
// route test needs more than one identity).
type pluginsRouteStaticAuth struct {
	tokens map[string]*authpkg.CallerIdentity
}

func (s *pluginsRouteStaticAuth) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	if id, ok := s.tokens[token]; ok {
		return id, nil
	}
	return nil, errors.New("unknown token")
}

// TestWorkerPluginsRoute_HTTPOAuthChain pins the mux registration that
// #1454 shipped without: the plugin registry handler merged (with its
// handler-level tests) but the route was never registered, so
// GET /api/v1/workers/{name}/plugins answered the mux's 404 everywhere.
// The invariant under test is that the route EXISTS: every assertion is
// the auth chain or the handler answering, never the mux's 404 (full-chain
// style, mirroring TestWorkerMCP_HTTPOAuthChain).
func TestWorkerPluginsRoute_HTTPOAuthChain(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeamWithWorkers("team-a", "daily-carol")...).Build()
	auth := &pluginsRouteStaticAuth{tokens: map[string]*authpkg.CallerIdentity{
		"admin-token":  {Role: authpkg.RoleAdmin, Username: "admin"},
		"worker-token": {Role: authpkg.RoleWorker, Username: "other-worker", WorkerName: "other-worker"},
	}}
	mw := authpkg.NewMiddleware(auth, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client:          k8s,
		Namespace:       "default",
		KubeMode:        "embedded",
		ContainerPrefix: "agentteams-worker-",
		AuthMw:          mw,
	})

	// No token -> 401 (an unregistered route would answer 404 here).
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/plugins", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status=%d, want 401 (404 would mean the route is missing)", rec.Code)
	}

	// Invalid token -> 401.
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/plugins", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, bad)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status=%d, want 401", rec2.Code)
	}

	// Worker-role token against ANOTHER worker -> 403 (worker reads are
	// self-only at the authorizer; the request still reached the
	// registered route — not the mux's 404).
	other := httptest.NewRequest(http.MethodGet, "/api/v1/workers/daily-carol/plugins", nil)
	other.Header.Set("Authorization", "Bearer worker-token")
	rec3 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec3, other)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("worker cross-target: status=%d, want 403", rec3.Code)
	}
}

// TestWorkerPluginsRoute_EmbeddedHandlerReachable: with a valid admin
// token the request reaches the handler — a non-qwenpaw worker answers
// 400 (the runtime guard) BEFORE any upstream dial, so the assertion is
// deterministic without a live worker.
func TestWorkerPluginsRoute_EmbeddedHandlerReachable(t *testing.T) {
	w := checkpointWorker("oc-worker")
	w.Spec.Runtime = "openclaw"
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeam("team-a", "oc-worker"), w).Build()
	auth := &pluginsRouteStaticAuth{tokens: map[string]*authpkg.CallerIdentity{
		"admin-token": {Role: authpkg.RoleAdmin, Username: "admin"},
	}}
	mw := authpkg.NewMiddleware(auth, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client:          k8s,
		Namespace:       "default",
		KubeMode:        "embedded",
		ContainerPrefix: "agentteams-worker-",
		AuthMw:          mw,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/oc-worker/plugins", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (the non-qwenpaw runtime guard); 404 would mean the route is missing", rec.Code)
	}
}

// TestWorkerPluginsRoute_KubeMode503BeforeLookup: in kube mode the route
// answers 503 for a worker that does not even exist in the client — the
// embedded-mode gate runs BEFORE any worker lookup (uniform, so worker
// existence cannot be probed). If the lookup ran first this would be a
// 404.
func TestWorkerPluginsRoute_KubeMode503BeforeLookup(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeamWithWorkers("team-a", "daily-carol")...).Build()
	auth := &pluginsRouteStaticAuth{tokens: map[string]*authpkg.CallerIdentity{
		"admin-token": {Role: authpkg.RoleAdmin, Username: "admin"},
	}}
	mw := authpkg.NewMiddleware(auth, nil, authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{
		Client:          k8s,
		Namespace:       "default",
		KubeMode:        "k8s",
		ContainerPrefix: "agentteams-worker-",
		AuthMw:          mw,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/ghost-worker/plugins", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode (404 would mean the worker lookup ran first)", rec.Code)
	}
}
