package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeHandoffPreflightPersistsActualStartAndSurvivesControllerRestart(t *testing.T) {
	store := ossfake.NewMemory()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "member-cr", Namespace: "test-ns"}, Spec: v1beta1.WorkerSpec{WorkerName: "stable-name", Runtime: "qwenpaw"}}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build()
	b := mocks.NewMockWorkerBackend()
	live := "current"
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		if live == "" {
			return nil, backend.ErrNotFound
		}
		return &backend.WorkerResult{Status: backend.StatusRunning, Runtime: "hermes", InstanceID: live}, nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "operator"}
	middleware := authpkg.NewMiddleware(&roleAuthenticator{identity: caller}, nil, authpkg.NewAuthorizer(), k8s, "test-ns")
	deps := ServerDeps{Client: k8s, Namespace: "test-ns", OSS: store, Backend: backend.NewRegistry([]backend.WorkerBackend{b}), AuthMw: middleware}
	controller := NewHTTPServer(":0", deps)
	t.Cleanup(controller.coordinationCancel)
	call := func(method, body, expected string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/workers/member-cr/runtime-handoff", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture")
		if expected == "*" {
			r.Header.Set("If-None-Match", "*")
		} else if expected != "" {
			r.Header.Set("If-Match", expected)
		}
		w := httptest.NewRecorder()
		controller.Mux.ServeHTTP(w, r)
		return w
	}
	req := `{"request_id":"first","target_runtime":"codex"}`
	if w := call(http.MethodPost, req, ""); w.Code != 428 {
		t.Fatalf("unconditional write %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodPost, req, "*"); w.Code != 409 {
		t.Fatalf("unregistered source %d %s", w.Code, w.Body)
	}
	matrix := service.WorkerMatrixStore{Storage: store}
	start := service.WorkerStart{InstanceID: "mock/current", Runtime: "hermes", ProcessID: "registered-process"}
	if _, _, err := matrix.Start(t.Context(), "stable-name", start, ""); err != nil {
		t.Fatal(err)
	}
	if w := call(http.MethodPost, `{"request_id":"forged","target_runtime":"codex","phase":"completed"}`, "*"); w.Code != 400 {
		t.Fatalf("caller supplied phase %d", w.Code)
	}
	if w := call(http.MethodPost, `{"request_id":"invalid","target_runtime":"copaw"}`, "*"); w.Code != 400 {
		t.Fatalf("deprecated target %d", w.Code)
	}
	first := call(http.MethodPost, req, "*")
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"phase":"preflight"`) || !strings.Contains(first.Body.String(), `"process_id":"registered-process"`) || !strings.Contains(first.Body.String(), `"runtime":"hermes"`) {
		t.Fatalf("preflight %d %s", first.Code, first.Body)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing revision")
	}
	if w := call(http.MethodPost, req, "*"); w.Code != 200 || w.Header().Get("ETag") != etag {
		t.Fatalf("lost response retry created a different operation %d %s", w.Code, w.Body)
	}
	changed := `{"request_id":"second","target_runtime":"qwenpaw"}`
	if w := call(http.MethodPost, changed, "*"); w.Code != 409 {
		t.Fatalf("conflicting preflight %d", w.Code)
	}
	if w := call(http.MethodPost, `{"request_id":"first","target_runtime":"qwenpaw"}`, etag); w.Code != 409 {
		t.Fatalf("redefined original request %d", w.Code)
	}
	live = "replaced"
	if w := call(http.MethodPost, changed, etag); w.Code != 409 {
		t.Fatalf("registered source no longer live %d", w.Code)
	}
	live = ""
	controller = NewHTTPServer(":0", deps)
	t.Cleanup(controller.coordinationCancel)
	if w := call(http.MethodGet, "", ""); w.Code != 200 || w.Header().Get("ETag") != etag || w.Body.String() != first.Body.String() {
		t.Fatalf("new controller lost record while worker offline %d %s", w.Code, w.Body)
	}
	if _, err := store.GetObject(t.Context(), "agents/stable-name/runtime/state.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetObject(t.Context(), "agents/member-cr/runtime/state.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CR name leaked into storage identity")
	}
	caller.Role = authpkg.RoleWorker
	caller.Username = "member-cr"
	caller.WorkerName = "member-cr"
	if w := call(http.MethodPost, changed, etag); w.Code != 403 {
		t.Fatalf("source worker requested replacement %d %s", w.Code, w.Body)
	}
}
