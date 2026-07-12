package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockAuthenticator is a test authenticator that returns a fixed identity for a known token.
type mockAuthenticator struct {
	tokens map[string]*CallerIdentity
}

func (m *mockAuthenticator) Authenticate(_ context.Context, token string) (*CallerIdentity, error) {
	if id, ok := m.tokens[token]; ok {
		cp := *id
		return &cp, nil
	}
	return nil, fmt.Errorf("invalid token")
}

// noopEnricher does nothing — identity is already complete from the mock authenticator.
type noopEnricher struct{}

func (n *noopEnricher) EnrichIdentity(_ context.Context, _ *CallerIdentity) error { return nil }

type failingEnricher struct{}

func (f *failingEnricher) EnrichIdentity(_ context.Context, _ *CallerIdentity) error {
	return fmt.Errorf("enrich failed")
}

func newTestMiddleware(tokens map[string]*CallerIdentity) *Middleware {
	return NewMiddleware(
		&mockAuthenticator{tokens: tokens},
		&noopEnricher{},
		NewAuthorizer(),
		nil, "",
	)
}

func TestAuthenticate_ValidToken(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{
		"mgr-token": {Role: RoleManager, Username: "manager"},
	})

	var gotIdentity *CallerIdentity
	handler := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = CallerFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer mgr-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotIdentity == nil || gotIdentity.Role != RoleManager {
		t.Errorf("expected manager identity, got %+v", gotIdentity)
	}
}

func TestAuthenticate_InvalidToken(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{})

	handler := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthenticate_EnrichmentFailureKeepsLegacyBehavior(t *testing.T) {
	mw := NewMiddleware(
		&mockAuthenticator{tokens: map[string]*CallerIdentity{
			"worker-token": {Role: RoleWorker, Username: "alice"},
		}},
		&failingEnricher{},
		NewAuthorizer(),
		nil, "",
	)

	called := false
	handler := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer worker-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called || w.Code != http.StatusOK {
		t.Errorf("expected local legacy path to continue, called=%v code=%d", called, w.Code)
	}
}

func TestAuthenticate_MissingHeader(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{})

	handler := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthenticate_NilAuthenticator(t *testing.T) {
	mw := NewMiddleware(nil, nil, NewAuthorizer(), nil, "")

	called := false
	handler := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called when authenticator is nil (disabled)")
	}
}

func TestRequireAuthz_ManagerAllowed(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{
		"mgr-token": {Role: RoleManager, Username: "manager"},
	})

	called := false
	handler := mw.RequireAuthz(ActionCreate, "worker", nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer mgr-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("manager should be allowed to create worker")
	}
}

func TestRequireAuthz_WorkerDeniedCreate(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{
		"worker-token": {Role: RoleWorker, Username: "alice", WorkerName: "alice"},
	})

	handler := mw.RequireAuthz(ActionCreate, "worker", nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer worker-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireAuthz_WorkerSelfReady(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{
		"worker-token": {Role: RoleWorker, Username: "alice", WorkerName: "alice"},
	})

	called := false
	nameFn := func(r *http.Request) string { return "alice" }
	handler := mw.RequireAuthz(ActionReady, "worker", nameFn)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alice/ready", nil)
	req.Header.Set("Authorization", "Bearer worker-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("worker should be allowed to report own readiness")
	}
}

func TestRequireAuthz_WorkerOtherReady(t *testing.T) {
	mw := newTestMiddleware(map[string]*CallerIdentity{
		"worker-token": {Role: RoleWorker, Username: "alice", WorkerName: "alice"},
	})

	nameFn := func(r *http.Request) string { return "bob" }
	handler := mw.RequireAuthz(ActionReady, "worker", nameFn)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/bob/ready", nil)
	req.Header.Set("Authorization", "Bearer worker-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestCallerFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	if CallerFromContext(ctx) != nil {
		t.Error("expected nil for empty context")
	}
}
