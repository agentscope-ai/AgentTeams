package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestWorkerMatrixHTTPUsesSelfScopeAndActualStart(t *testing.T) {
	storage := ossfake.NewMemory()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker-cr", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{WorkerName: "storage-worker", Runtime: "qwenpaw"}, Status: v1beta1.WorkerStatus{MatrixUserID: "@worker:test"}}
	b := mocks.NewMockWorkerBackend()
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning, InstanceID: "source-start", Runtime: "hermes"}, nil
	}
	deps := ServerDeps{Namespace: "default", OSS: storage,
		Client: fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build(), Backend: backend.NewRegistry([]backend.WorkerBackend{b})}
	cached := worker.DeepCopy()
	cached.Spec.WorkerName = "obsolete-storage-name"
	deps.Client = fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(cached).Build()
	deps.APIReader = fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build()
	h := NewWorkerMatrixHandler(deps, NewTaskRecoveryHandler(nil, deps))
	workerCaller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", WorkerName: "worker-cr"}
	call := func(caller *authpkg.CallerIdentity, method, body, etag, process string) *httptest.ResponseRecorder {
		t.Helper()
		r := withCaller(httptest.NewRequest(method, "/api/v1/runtime/matrix", strings.NewReader(body)), caller)
		if etag == "*" {
			r.Header.Set("If-None-Match", "*")
		} else if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		r.Header.Set("X-AgentTeams-Process-ID", process)
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			h.Get(w, r)
		} else {
			h.Update(w, r)
		}
		return w
	}
	for _, role := range []string{authpkg.RoleHuman, authpkg.RoleManager, authpkg.RoleAdmin} {
		if r := call(&authpkg.CallerIdentity{Role: role, Username: "worker-cr"}, http.MethodGet, "", "", ""); r.Code != 403 {
			t.Fatalf("role %s obtained Worker self-state: %d %s", role, r.Code, r.Body)
		}
	}
	if r := call(&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", ServiceAccountNamespace: "other-tenant"}, http.MethodGet, "", "", ""); r.Code != 403 {
		t.Fatalf("foreign namespace obtained self-state: %d %s", r.Code, r.Body)
	}
	// A standalone Worker has no Team or Project-list permission.
	r := call(workerCaller, http.MethodGet, "", "", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"member_prefix":"agents/storage-worker"`) || !strings.Contains(r.Body.String(), `"runtime":"hermes"`) {
		t.Fatalf("self discovery used desired runtime or CR name as storage identity: %d %s", r.Code, r.Body)
	}
	body := `{"action":"start","instance_id":"mock/source-start","process_id":"process-one"}`
	if r := call(workerCaller, http.MethodPost, body, "*", "different-process"); r.Code != 400 {
		t.Fatalf("request/header process mismatch: %d %s", r.Code, r.Body)
	}
	if r := call(workerCaller, http.MethodPost, body, "", "process-one"); r.Code != 428 {
		t.Fatalf("unconditional start accepted: %d %s", r.Code, r.Body)
	}
	r = call(workerCaller, http.MethodPost, body, "*", "process-one")
	if r.Code != 200 {
		t.Fatalf("verified start: %d %s", r.Code, r.Body)
	}
	var response struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"action": "ingest", "instance_id": "mock/source-start", "process_id": "process-one",
		"consumer": service.WorkerMatrixConsumer{Runtime: "hermes", Account: "@worker:test", Format: "matrix-sync/v3", Token: "next"},
		"inputs": []service.WorkerMatrixInput{{RoomID: "!room:test", EventID: "$one", Intent: "message",
			Input: service.WorkerMatrixContent{Sender: "@human:test", Type: "m.room.message", Content: map[string]any{"body": "continue"}}}}}
	encoded, _ := json.Marshal(input)
	wrongAccount := input["consumer"].(service.WorkerMatrixConsumer)
	wrongAccount.Account = "@other:test"
	input["consumer"] = wrongAccount
	wrongEncoded, _ := json.Marshal(input)
	if r := call(workerCaller, http.MethodPost, string(wrongEncoded), response.ETag, "process-one"); r.Code != 403 {
		t.Fatalf("foreign Matrix account accepted: %d %s", r.Code, r.Body)
	}
	if r := call(workerCaller, http.MethodPost, string(encoded), response.ETag, "process-one"); r.Code != 200 {
		t.Fatalf("durable ingest: %d %s", r.Code, r.Body)
	}
	if r := call(workerCaller, http.MethodPost, string(encoded), response.ETag, "process-one"); r.Code != 409 {
		t.Fatalf("stale snapshot accepted: %d %s", r.Code, r.Body)
	}
	input["instance_id"] = "mock/old-start"
	encoded, _ = json.Marshal(input)
	if r := call(workerCaller, http.MethodPost, string(encoded), response.ETag, "process-one"); r.Code != 409 {
		t.Fatalf("old backend instance accepted: %d %s", r.Code, r.Body)
	}
	if _, err := storage.GetObject(t.Context(), "agents/worker-cr/runtime/matrix/state.json"); err == nil {
		t.Fatal("Controller wrote a second authority beneath the CR name")
	}
}

func TestWorkerMatrixHTTPRoutesKeepProjectListClosed(t *testing.T) {
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build()
	identity := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", WorkerName: "alice"}
	middleware := authpkg.NewMiddleware(&roleAuthenticator{identity: identity}, nil, authpkg.NewAuthorizer(), k8s, "default")
	b := mocks.NewMockWorkerBackend()
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning, InstanceID: "actual-start", Runtime: "hermes"}, nil
	}
	server := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossfake.NewMemory(), AuthMw: middleware, Backend: backend.NewRegistry([]backend.WorkerBackend{b})})
	call := func(method, path, body, etag string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer worker-token")
		request.Header.Set("X-AgentTeams-Process-ID", "actual-process")
		if etag == "*" {
			request.Header.Set("If-None-Match", "*")
		} else if etag != "" {
			request.Header.Set("If-Match", etag)
		}
		response := httptest.NewRecorder()
		server.Mux.ServeHTTP(response, request)
		return response
	}
	if r := call(http.MethodGet, "/api/v1/runtime/matrix", "", ""); r.Code != 200 {
		t.Fatalf("registered self route: %d %s", r.Code, r.Body)
	}
	r := call(http.MethodPost, "/api/v1/runtime/matrix", `{"action":"start","instance_id":"mock/actual-start","process_id":"actual-process"}`, "*")
	if r.Code != 200 {
		t.Fatalf("registered start route: %d %s", r.Code, r.Body)
	}
	body := `{"action":"ingest","instance_id":"mock/actual-start","process_id":"actual-process","inputs":[{"room_id":"!room:test","event_id":"$one","intent":"message","input":{"sender":"@human:test","type":"m.room.message","content":{"body":"continue"}}}]}`
	if r := call(http.MethodPost, "/api/v1/runtime/matrix", body, r.Header().Get("ETag")); r.Code != 200 {
		t.Fatalf("native callback ingest: %d %s", r.Code, r.Body)
	}
	if r := call(http.MethodGet, "/api/v1/runtime/matrix/events/"+service.WorkerMatrixEventKey("!room:test", "$one"), "", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"event_id":"$one"`) {
		t.Fatalf("registered input route: %d %s", r.Code, r.Body)
	}
	if r := call(http.MethodGet, "/api/v1/projects", "", ""); r.Code != 403 {
		t.Fatalf("self persistence widened Project list: %d %s", r.Code, r.Body)
	}
}
