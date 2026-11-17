package server

import (
	"bytes"
	"context"
	"encoding/json"
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
	b := &operatorHandoffBackend{MockWorkerBackend: mocks.NewMockWorkerBackend()}
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
	encrypted := true
	deps.HandoffMatrix.CheckPlaintextRooms = func(_ context.Context, credentialName string) error {
		if credentialName != worker.Name {
			t.Fatalf("room inspection used credential identity %q", credentialName)
		}
		if encrypted {
			return service.ErrWorkerStateConflict
		}
		return nil
	}
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
	for _, target := range []string{"codex", "deepseek-harness"} {
		body := strings.Replace(req, "codex", target, 1)
		if w := call(http.MethodPost, body, "*"); w.Code != 409 {
			t.Fatalf("accepted incompatible %s target: %d %s", target, w.Code, w.Body)
		}
	}
	if pre, _, err := (service.WorkerRuntimeStore{Storage: store}).Load(t.Context(), "stable-name"); err != nil || pre.Handoff != nil {
		t.Fatalf("rejected switch already persisted a drain %+v %v", pre, err)
	}
	encrypted = false
	first := call(http.MethodPost, req, "*")
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"phase":"draining"`) || !strings.Contains(first.Body.String(), `"process_id":"registered-process"`) || !strings.Contains(first.Body.String(), `"runtime":"hermes"`) {
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
	if w := call(http.MethodPost, req, "*"); w.Code != 200 || w.Header().Get("ETag") != etag {
		t.Fatalf("offline duplicate %d %s", w.Code, w.Body)
	}
	caller.ServiceAccountNamespace = "other"
	if w := call(http.MethodGet, "", ""); w.Code != 403 {
		t.Fatalf("foreign operator response %d", w.Code)
	}
	caller.ServiceAccountNamespace = ""
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

func TestRuntimeHandoffDrainsMatrixAtPersistedBoundary(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two", "three"}}, 200)
	store := service.WorkerRuntimeStore{Storage: f.storage}
	if _, _, err := store.Preflight(t.Context(), "runtime-alice", "switch", "codex", "codex:fixture", f.start, ""); err != nil {
		t.Fatal(err)
	}
	f.expect("claim", nil, 409)
	f.expect("checkpoint", &taskCheckpoint{Version: 2, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two", "three"}}, 409)
	f.expect("checkpoint", &taskCheckpoint{Version: 2, CompletedSteps: []string{"one"}, Operation: &taskOperation{ID: "side-effect", State: "started"}}, 409)
	stepTwo := f.artifact("two", "persisted step two")
	f.expect("checkpoint", &taskCheckpoint{Version: 2, CompletedSteps: []string{"one", "two"}, RemainingSteps: []string{"three"}, Artifacts: []taskArtifact{stepTwo}}, 200)
	f.expect("checkpoint", &taskCheckpoint{Version: 3, CompletedSteps: []string{"one", "two", "three"}, Artifacts: []taskArtifact{stepTwo}}, 409)
	input, err := f.h.store.Input(t.Context(), "runtime-alice", f.key)
	if err != nil {
		t.Fatal(err)
	}
	var execution taskExecution
	if err := json.Unmarshal(input.Execution, &execution); err != nil || execution.State != "released" || execution.HandoffID != "switch" || execution.Checkpoint.Version != 2 || len(execution.Checkpoint.Artifacts) != 1 {
		t.Fatalf("lost released boundary: %+v %v", execution, err)
	}
	state, _, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil || len(state.Pending) != 1 {
		t.Fatalf("pending input discarded: %+v %v", state, err)
	}
}

func TestRuntimeHandoffBlocksClaimStraddlingHandoffWrite(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	storage := &matrixExecutionCASStore{Memory: f.storage, eventKey: f.objectKey()}
	f.h.deps.OSS = storage
	storage.before = func() {
		_, _, err := (service.WorkerRuntimeStore{Storage: f.storage}).Preflight(t.Context(), "runtime-alice", "switch", "codex", "codex:fixture", f.start, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	f.expect("claim", nil, 409)
}

func TestRuntimeHandoffDrainsTaskAfterSideEffectVerification(t *testing.T) {
	ctx := t.Context()
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alpha-dev", "status": "in_progress"}))
	project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	h := NewTaskRecoveryHandler(project, ServerDeps{Namespace: "test-ns"})
	start := service.WorkerStart{InstanceID: "docker/one", ProcessID: "proc-one", Runtime: "hermes"}
	h.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return start.InstanceID, start.Runtime, nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	call := func(action string, cp *taskCheckpoint, expected int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": action, "instance_id": start.InstanceID, "process_id": start.ProcessID, "checkpoint": cp})
		req := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/execution?team=alpha-team", bytes.NewReader(body)), caller)
		req.SetPathValue("id", "t1")
		req.Header.Set("If-Match", taskMetaETag(t, store, taskMetaKey))
		w := httptest.NewRecorder()
		h.UpdateExecution(w, req)
		if w.Code != expected {
			t.Fatalf("%s got %d want %d %s", action, w.Code, expected, w.Body)
		}
	}
	call("claim", nil, 200)
	call("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two", "three"}, Operation: &taskOperation{ID: "external", State: "started"}}, 200)
	_, _, err := (service.WorkerRuntimeStore{Storage: store}).Preflight(ctx, "alpha-dev", "switch", "codex", "codex:fixture", start, "")
	if err != nil {
		t.Fatal(err)
	}
	call("claim", nil, 409)
	call("release", &taskCheckpoint{Version: 2, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two", "three"}}, 409)
	verified := &taskCheckpoint{Version: 2, CompletedSteps: []string{"one", "two"}, RemainingSteps: []string{"three"}, Operation: &taskOperation{ID: "external", State: "verified", Evidence: "external durable receipt"}}
	caller.ServiceAccountNamespace = "other"
	call("checkpoint", verified, 403)
	caller.ServiceAccountNamespace = ""
	call("checkpoint", verified, 200)
	call("claim", nil, 409)
	data, err := store.GetObject(ctx, taskMetaKey)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Status    string
		Execution taskExecution
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Status != "in_progress" || saved.Execution.State != "released" || saved.Execution.HandoffID != "switch" || saved.Execution.Checkpoint.Operation.Evidence != "external durable receipt" {
		t.Fatalf("lost task boundary: %+v %v", saved, err)
	}
}

func TestRuntimeHandoffPreflightWithMissingBackend(t *testing.T) {
	h := NewTaskRecoveryHandler(nil, ServerDeps{})
	if _, _, err := h.runningWorkerInstance(t.Context(), &v1beta1.Worker{}); err == nil {
		t.Fatal("missing backend provided source identity")
	}
}

type operatorHandoffBackend struct{ *mocks.MockWorkerBackend }

func (b *operatorHandoffBackend) HandoffTargetImage(runtime string) string {
	return runtime + ":fixture"
}
func (b *operatorHandoffBackend) StopRuntimeInstance(context.Context, string, string) (bool, error) {
	panic("operator request must not directly stop containers")
}
func (b *operatorHandoffBackend) RemoveStoppedRuntime(context.Context, string, string) (bool, error) {
	panic("operator request must not directly remove containers")
}
