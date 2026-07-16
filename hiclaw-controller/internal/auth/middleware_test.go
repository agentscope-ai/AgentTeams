package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// TestResolveResourceTeam covers the authoritative worker->team resolution:
// Team CR reverse lookup (leader or worker member name) takes priority,
// since post-refactor team members have no Worker CR at all. The legacy
// "agentteams.io/team" annotation on a standalone Worker CR is only consulted
// as a defensive fallback. A worker that resolves to neither returns "".
func TestResolveResourceTeam(t *testing.T) {
	scheme := newAuthTestScheme(t)

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev"}}

	legacyAnnotatedWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "legacy-worker",
			Namespace:   "default",
			Annotations: map[string]string{"agentteams.io/team": "legacy-team"},
		},
	}

	standaloneWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "standalone-worker",
			Namespace: "default",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, legacyAnnotatedWorker, standaloneWorker).
		WithIndex(&v1beta1.Team{}, teamLeaderNameField, indexTeamLeaderNames).
		WithIndex(&v1beta1.Team{}, teamWorkerNameField, indexTeamWorkerNames).
		Build()

	mw := &Middleware{k8s: k8sClient, namespace: "default"}

	cases := []struct {
		name string
		kind string
		rsrc string
		want string
	}{
		{name: "team leader resolves via Team CR", kind: "worker", rsrc: "alpha-lead", want: "alpha-team"},
		{name: "team worker resolves via Team CR", kind: "worker", rsrc: "alpha-dev", want: "alpha-team"},
		{name: "standalone worker with no annotation resolves empty", kind: "worker", rsrc: "standalone-worker", want: ""},
		{name: "legacy annotated worker falls back to annotation", kind: "worker", rsrc: "legacy-worker", want: "legacy-team"},
		{name: "unknown worker resolves empty", kind: "worker", rsrc: "does-not-exist", want: ""},
		{name: "non-worker kind resolves empty", kind: "team", rsrc: "alpha-team", want: ""},
		{name: "empty name resolves empty", kind: "worker", rsrc: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mw.resolveResourceTeam(context.Background(), tc.kind, tc.rsrc)
			if got != tc.want {
				t.Errorf("resolveResourceTeam(%q, %q) = %q, want %q", tc.kind, tc.rsrc, got, tc.want)
			}
		})
	}
}
