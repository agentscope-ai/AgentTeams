package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newExecutionSandboxHandlerTestClient(t *testing.T, objects ...runtime.Object) *ExecutionSandboxHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
		Build()
	return NewExecutionSandboxHandler(cl, "agentteams-system", "deepagents")
}

func deepAgentsSandboxWorker() *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid"},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-max",
			// Empty runtime resolves through the platform default configured on
			// the handler, matching worker.defaultRuntime=deepagents.
			Runtime: "",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{
					Mode:        "sandbox",
					IdleTimeout: "30m",
					MaxLifetime: "8h",
					Egress: []v1beta1.DeepAgentsEgressRule{{
						CIDR: "10.96.0.10/32", Ports: []int32{443},
					}},
				},
			}},
		},
	}
}

func TestExecutionSandboxEnsureCreatesControllerManagedCR(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	h := newExecutionSandboxHandlerTestClient(t, worker)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", "researcher")
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ExecutionSandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Phase != "Pending" || response.Token != "" || response.Name == "" {
		t.Fatalf("response=%#v", response)
	}
	var sandbox v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: response.Name, Namespace: "agentteams-system"}, &sandbox); err != nil {
		t.Fatalf("get ExecutionSandbox: %v", err)
	}
	if sandbox.Spec.WorkerRef.UID != "worker-uid" || sandbox.Spec.SessionID != "thread-hash" {
		t.Fatalf("sandbox identity=%#v", sandbox.Spec)
	}
	if sandbox.Spec.IdleTimeout != "30m" || sandbox.Spec.MaxLifetime != "8h" || len(sandbox.Spec.Egress) != 1 {
		t.Fatalf("sandbox policy was not copied from Worker: %#v", sandbox.Spec)
	}
	if len(sandbox.OwnerReferences) != 1 || sandbox.OwnerReferences[0].UID != worker.UID {
		t.Fatalf("sandbox must be garbage-collected with Worker: %#v", sandbox.OwnerReferences)
	}
}

func TestExecutionSandboxEnsureReturnsTokenOnlyAfterRunnerReady(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID: "thread-hash",
		},
		Status: v1beta1.ExecutionSandboxStatus{
			Phase:    "Ready",
			Endpoint: "http://runner.agentteams-system.svc:8080",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
		Data:       map[string][]byte{"token": []byte("runner-token-value-with-at-least-32-bytes")},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox, secret)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", "researcher")
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response ExecutionSandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Token != "runner-token-value-with-at-least-32-bytes" || response.Endpoint != sandbox.Status.Endpoint {
		t.Fatalf("response=%#v", response)
	}
}

func TestExecutionSandboxHeartbeatAndDelete(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID: "thread-hash",
		},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)
	heartbeat := httptest.NewRequest(http.MethodPost, "/heartbeat", nil)
	heartbeat.SetPathValue("name", worker.Name)
	heartbeat.SetPathValue("sessionId", "thread-hash")
	heartbeatRec := httptest.NewRecorder()
	h.Heartbeat(heartbeatRec, heartbeat)
	if heartbeatRec.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status=%d body=%s", heartbeatRec.Code, heartbeatRec.Body.String())
	}
	var updated v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.LastHeartbeat == nil {
		t.Fatal("heartbeat did not update status.lastHeartbeat")
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandbox", nil)
	deleteReq.SetPathValue("name", worker.Name)
	deleteReq.SetPathValue("sessionId", "thread-hash")
	deleteRec := httptest.NewRecorder()
	h.Delete(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}
