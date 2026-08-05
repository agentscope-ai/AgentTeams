package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type managedIdentityAuthenticator struct {
	token    string
	identity *authpkg.CallerIdentity
}

func (a managedIdentityAuthenticator) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	if token != a.token {
		return nil, errors.New("invalid token")
	}
	copy := *a.identity
	return &copy, nil
}

type managedIdentityFailingListClient struct {
	client.Client
	failManagers bool
}

func (c managedIdentityFailingListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.failManagers {
		if _, ok := list.(*v1beta1.ManagerList); ok {
			return errors.New("manager list unavailable")
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func managedIdentityScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func managedIdentityWorker(name, namespace, controller, matrixUserID string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{v1beta1.LabelController: controller},
		},
		Status: v1beta1.WorkerStatus{MatrixUserID: matrixUserID},
	}
}

func managedIdentityManager(name, namespace, controller, matrixUserID string) *v1beta1.Manager {
	return &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{v1beta1.LabelController: controller},
		},
		Status: v1beta1.ManagerStatus{MatrixUserID: matrixUserID},
	}
}

func TestManagedAgentIdentityLookupQueriesCurrentWorkerAndManagerStateWithinScope(t *testing.T) {
	const namespace = "agentteams-system"
	objects := []client.Object{
		managedIdentityWorker("researcher", namespace, "ctl-a", "@researcher:example.org"),
		managedIdentityWorker("late-worker", namespace, "ctl-a", "@late-worker:example.org"),
		managedIdentityManager("manager", namespace, "ctl-a", "@late-manager:example.org"),
		managedIdentityWorker("other-controller", namespace, "ctl-b", "@other-controller:example.org"),
		managedIdentityManager("other-namespace", "other", "ctl-a", "@other-namespace:example.org"),
	}
	cl := fake.NewClientBuilder().WithScheme(managedIdentityScheme(t)).WithObjects(objects...).Build()
	handler := NewManagedAgentIdentityHandler(cl, namespace, "ctl-a")
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "researcher"}
	lookup := func(matrixUserID string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/managed-agent-identity", strings.NewReader(`{"matrixUserId":"`+matrixUserID+`"}`))
		req.SetPathValue("name", "researcher")
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
		rec := httptest.NewRecorder()

		handler.Lookup(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("lookup %q status=%d body=%s", matrixUserID, rec.Code, rec.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	for _, tc := range []struct {
		matrixUserID string
		managed      bool
	}{
		{matrixUserID: "@late-worker:example.org", managed: true},
		{matrixUserID: "@late-manager:example.org", managed: true},
		{matrixUserID: "@other-controller:example.org", managed: false},
		{matrixUserID: "@other-namespace:example.org", managed: false},
		{matrixUserID: "@human:example.org", managed: false},
	} {
		response := lookup(tc.matrixUserID)
		if len(response) != 1 || response["managed"] != tc.managed {
			t.Fatalf("lookup %q response=%#v, want only managed=%v", tc.matrixUserID, response, tc.managed)
		}
	}

	// The handler must read live CR state for every question, not cache the
	// result it returned before this Manager appeared.
	if response := lookup("@created-later:example.org"); response["managed"] != false {
		t.Fatalf("pre-create response=%#v, want managed=false", response)
	}
	if err := cl.Create(context.Background(), managedIdentityManager("created-later", namespace, "ctl-a", "@created-later:example.org")); err != nil {
		t.Fatalf("create later Manager: %v", err)
	}
	if response := lookup("@created-later:example.org"); response["managed"] != true {
		t.Fatalf("post-create response=%#v, want managed=true", response)
	}
}

func TestManagedAgentIdentityLookupIsAuthenticatedAndWorkerSelfScoped(t *testing.T) {
	const namespace = "agentteams-system"
	scheme := managedIdentityScheme(t)
	worker := managedIdentityWorker("researcher", namespace, "ctl-a", "@researcher:example.org")
	other := managedIdentityWorker("other", namespace, "ctl-a", "@other:example.org")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker, other).Build()
	mw := authpkg.NewMiddleware(
		managedIdentityAuthenticator{
			token:    "researcher-token",
			identity: &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "researcher"},
		},
		nil,
		authpkg.NewAuthorizer(),
		cl,
		namespace,
	)
	server := NewHTTPServer(":0", ServerDeps{
		Client:         cl,
		AuthMw:         mw,
		KubeMode:       "incluster",
		Namespace:      namespace,
		ControllerName: "ctl-a",
	})
	body := `{"matrixUserId":"@other:example.org"}`

	unauthenticated := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/managed-agent-identity", strings.NewReader(body))
	unauthenticatedRec := httptest.NewRecorder()
	server.Mux.ServeHTTP(unauthenticatedRec, unauthenticated)
	if unauthenticatedRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticatedRec.Code, unauthenticatedRec.Body.String())
	}

	crossWorker := httptest.NewRequest(http.MethodPost, "/api/v1/workers/other/managed-agent-identity", strings.NewReader(body))
	crossWorker.Header.Set("Authorization", "Bearer researcher-token")
	crossWorkerRec := httptest.NewRecorder()
	server.Mux.ServeHTTP(crossWorkerRec, crossWorker)
	if crossWorkerRec.Code != http.StatusForbidden {
		t.Fatalf("cross-worker status=%d body=%s", crossWorkerRec.Code, crossWorkerRec.Body.String())
	}

	self := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/managed-agent-identity", strings.NewReader(body))
	self.Header.Set("Authorization", "Bearer researcher-token")
	selfRec := httptest.NewRecorder()
	server.Mux.ServeHTTP(selfRec, self)
	if selfRec.Code != http.StatusOK {
		t.Fatalf("self status=%d body=%s", selfRec.Code, selfRec.Body.String())
	}
}

func TestManagedAgentIdentityLookupRejectsInvalidBoundsAndFailsClosedOnListError(t *testing.T) {
	const namespace = "agentteams-system"
	worker := managedIdentityWorker("researcher", namespace, "ctl-a", "@researcher:example.org")
	base := fake.NewClientBuilder().WithScheme(managedIdentityScheme(t)).WithObjects(worker).Build()
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "researcher"}

	for _, body := range []string{
		`{}`,
		`{"matrixUserId":" human "}`,
		`{"matrixUserId":"@human:example.org","extra":true}`,
		`{"matrixUserId":"@human:example.org"}{"matrixUserId":"@other:example.org"}`,
		`{"matrixUserId":"@` + strings.Repeat("a", 260) + `:example.org"}`,
		`{"matrixUserId":"@human:example.org"}` + strings.Repeat(" ", managedAgentIdentityRequestMaxBytes),
	} {
		handler := NewManagedAgentIdentityHandler(base, namespace, "ctl-a")
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.SetPathValue("name", "researcher")
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
		rec := httptest.NewRecorder()
		handler.Lookup(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body length=%d status=%d response=%s, want 400", len(body), rec.Code, rec.Body.String())
		}
	}

	handler := NewManagedAgentIdentityHandler(managedIdentityFailingListClient{Client: base, failManagers: true}, namespace, "ctl-a")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"matrixUserId":"@human:example.org"}`))
	req.SetPathValue("name", "researcher")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	rec := httptest.NewRecorder()
	handler.Lookup(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list failure status=%d body=%s, want 500", rec.Code, rec.Body.String())
	}
}

func TestManagedAgentIdentityLookupRejectsCallerOutsideControllerScope(t *testing.T) {
	const namespace = "agentteams-system"
	worker := managedIdentityWorker("researcher", namespace, "ctl-b", "@researcher:example.org")
	cl := fake.NewClientBuilder().WithScheme(managedIdentityScheme(t)).WithObjects(worker).Build()
	handler := NewManagedAgentIdentityHandler(cl, namespace, "ctl-a")
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "researcher"}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"matrixUserId":"@researcher:example.org"}`))
	req.SetPathValue("name", "researcher")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	rec := httptest.NewRecorder()

	handler.Lookup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope caller status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
}
