package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	return NewExecutionSandboxHandler(cl, "agentteams-system", "deepagents", sandboxpolicy.Default())
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
	if sandbox.Spec.Resources == nil || sandbox.Spec.Resources.Requests.EphemeralStorage != "256Mi" || sandbox.Spec.Resources.Limits.EphemeralStorage != "2Gi" {
		t.Fatalf("sandbox resources=%#v, want default request=256Mi limit=2Gi", sandbox.Spec.Resources)
	}
	if len(sandbox.OwnerReferences) != 1 || sandbox.OwnerReferences[0].UID != worker.UID {
		t.Fatalf("sandbox must be garbage-collected with Worker: %#v", sandbox.OwnerReferences)
	}
}

func TestExecutionSandboxEnsurePersistsWorkerEphemeralStorageOverride(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	worker.Spec.RuntimeConfig.DeepAgents.Execution.Resources = &v1beta1.ExecutionSandboxResourceRequirements{
		Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "512Mi"},
		Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"worker-override"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sandbox v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: executionSandboxName(worker.Name, "worker-override"), Namespace: "agentteams-system"}, &sandbox); err != nil {
		t.Fatalf("get ExecutionSandbox: %v", err)
	}
	if sandbox.Spec.Resources == nil || sandbox.Spec.Resources.Requests.EphemeralStorage != "512Mi" || sandbox.Spec.Resources.Limits.EphemeralStorage != "4Gi" {
		t.Fatalf("sandbox resources=%#v, want request=512Mi limit=4Gi", sandbox.Spec.Resources)
	}
}

func TestExecutionSandboxEnsureRejectsInvalidWorkerEphemeralStorageOverrides(t *testing.T) {
	tests := []struct {
		name      string
		resources *v1beta1.ExecutionSandboxResourceRequirements
	}{
		{
			name: "zero request",
			resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "0"},
			},
		},
		{
			name: "request exceeds limit",
			resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "3Gi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "2Gi"},
			},
		},
		{
			name: "limit exceeds maximum",
			resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "9Gi"},
			},
		},
		{
			name: "negative CPU request",
			resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{CPU: "-1m"},
			},
		},
		{
			name: "memory request exceeds limit",
			resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{Memory: "2Gi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{Memory: "1Gi"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			worker.Spec.RuntimeConfig.DeepAgents.Execution.Resources = tt.resources
			h := newExecutionSandboxHandlerTestClient(t, worker)
			sessionID := "invalid-" + strings.ReplaceAll(tt.name, " ", "-")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"`+sessionID+`"}`))
			req.SetPathValue("name", worker.Name)
			rec := httptest.NewRecorder()

			h.Ensure(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var sandbox v1beta1.ExecutionSandbox
			err := h.client.Get(context.Background(), types.NamespacedName{Name: executionSandboxName(worker.Name, sessionID), Namespace: "agentteams-system"}, &sandbox)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("invalid resources created ExecutionSandbox: get error=%v", err)
			}
		})
	}
}

func TestExecutionSandboxEnsureReturnsTokenOnlyAfterRunnerReady(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
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

func TestExecutionSandboxEnsureRefreshesExistingPolicyBeforeReturningReadyLease(t *testing.T) {
	tests := []struct {
		name   string
		change func(*v1beta1.Worker)
		check  func(*testing.T, v1beta1.ExecutionSandboxSpec)
	}{
		{
			name: "resources",
			change: func(worker *v1beta1.Worker) {
				worker.Spec.RuntimeConfig.DeepAgents.Execution.Resources = &v1beta1.ExecutionSandboxResourceRequirements{
					Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "512Mi"},
					Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
				}
			},
			check: func(t *testing.T, spec v1beta1.ExecutionSandboxSpec) {
				t.Helper()
				if spec.Resources == nil || spec.Resources.Requests.EphemeralStorage != "512Mi" || spec.Resources.Limits.EphemeralStorage != "4Gi" {
					t.Fatalf("resources=%#v, want request=512Mi limit=4Gi", spec.Resources)
				}
			},
		},
		{
			name: "egress",
			change: func(worker *v1beta1.Worker) {
				worker.Spec.RuntimeConfig.DeepAgents.Execution.Egress = []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.20/32", Ports: []int32{8443}}}
			},
			check: func(t *testing.T, spec v1beta1.ExecutionSandboxSpec) {
				t.Helper()
				if len(spec.Egress) != 1 || spec.Egress[0].CIDR != "10.96.0.20/32" || len(spec.Egress[0].Ports) != 1 || spec.Egress[0].Ports[0] != 8443 {
					t.Fatalf("egress=%#v, want 10.96.0.20/32:8443", spec.Egress)
				}
			},
		},
		{
			name: "idle timeout",
			change: func(worker *v1beta1.Worker) {
				worker.Spec.RuntimeConfig.DeepAgents.Execution.IdleTimeout = "45m"
			},
			check: func(t *testing.T, spec v1beta1.ExecutionSandboxSpec) {
				t.Helper()
				if spec.IdleTimeout != "45m" {
					t.Fatalf("idleTimeout=%q, want 45m", spec.IdleTimeout)
				}
			},
		},
		{
			name: "max lifetime",
			change: func(worker *v1beta1.Worker) {
				worker.Spec.RuntimeConfig.DeepAgents.Execution.MaxLifetime = "12h"
			},
			check: func(t *testing.T, spec v1beta1.ExecutionSandboxSpec) {
				t.Helper()
				if spec.MaxLifetime != "12h" {
					t.Fatalf("maxLifetime=%q, want 12h", spec.MaxLifetime)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			name := executionSandboxName(worker.Name, "thread-hash")
			sandbox := readyExecutionSandbox(worker, name, "thread-hash")
			tt.change(worker)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
				Data:       map[string][]byte{"token": []byte("runner-token-value-with-at-least-32-bytes")},
			}
			h := newExecutionSandboxHandlerTestClient(t, worker, sandbox, secret)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
			req.SetPathValue("name", worker.Name)
			rec := httptest.NewRecorder()

			h.Ensure(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s, want pending after policy refresh", rec.Code, rec.Body.String())
			}
			var response ExecutionSandboxResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Phase != "Pending" || response.Token != "" || response.Endpoint != "" {
				t.Fatalf("response=%#v, want Pending without an old-policy lease", response)
			}
			var updated v1beta1.ExecutionSandbox
			if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &updated); err != nil {
				t.Fatal(err)
			}
			tt.check(t, updated.Spec)
			if updated.Status.ObservedGeneration != 1 {
				t.Fatalf("policy refresh changed status.observedGeneration=%d", updated.Status.ObservedGeneration)
			}
		})
	}
}

func TestExecutionSandboxEnsureRejectsInvalidCurrentWorkerResourcesWithoutMutatingExistingSandbox(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	worker.Spec.RuntimeConfig.DeepAgents.Execution.Resources = &v1beta1.ExecutionSandboxResourceRequirements{
		Requests: v1beta1.ExecutionSandboxResourceValues{CPU: "750m"},
		Limits:   v1beta1.ExecutionSandboxResourceValues{CPU: "500m"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
		Data:       map[string][]byte{"token": []byte("runner-token-value-with-at-least-32-bytes")},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox, secret)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var unchanged v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Spec.Resources == nil || unchanged.Spec.Resources.Requests.EphemeralStorage != "256Mi" || unchanged.Spec.Resources.Limits.EphemeralStorage != "2Gi" {
		t.Fatalf("invalid Worker override mutated existing sandbox=%#v", unchanged.Spec)
	}
}

func TestExecutionSandboxEnsurePreservesIdentityCollisionResponse(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	sandbox.Spec.WorkerRef.UID = "different-worker-uid"
	worker.Spec.RuntimeConfig.DeepAgents.Execution.Resources = &v1beta1.ExecutionSandboxResourceRequirements{
		Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "9Gi"},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want identity collision 409", rec.Code, rec.Body.String())
	}
	var unchanged v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Spec.WorkerRef.UID != "different-worker-uid" {
		t.Fatalf("identity collision mutated existing sandbox=%#v", unchanged.Spec)
	}
}

func TestExecutionSandboxEnsureWithStaleReadyStatusReturnsPendingWithoutToken(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	sandbox.Generation = 7
	sandbox.Status.ObservedGeneration = 6
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system"},
		Data:       map[string][]byte{"token": []byte("runner-token-value-with-at-least-32-bytes")},
	}
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox, secret)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want Pending while status is stale", rec.Code, rec.Body.String())
	}
	var response ExecutionSandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Phase != "Pending" || response.Token != "" || response.Endpoint != "" {
		t.Fatalf("response=%#v, want Pending without token", response)
	}
}

func readyExecutionSandbox(worker *v1beta1.Worker, name, sessionID string) *v1beta1.ExecutionSandbox {
	return &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentteams-system", Generation: 1},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID:   sessionID,
			IdleTimeout: "30m",
			MaxLifetime: "8h",
			Resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "256Mi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "2Gi"},
			},
			Egress: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}},
		},
		Status: v1beta1.ExecutionSandboxStatus{
			ObservedGeneration: 1,
			Phase:              "Ready",
			Endpoint:           "http://runner.agentteams-system.svc:8080",
		},
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
