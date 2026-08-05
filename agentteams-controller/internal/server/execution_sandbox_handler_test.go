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
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	return NewExecutionSandboxHandler(cl, "agentteams-system", "ctl-a", "deepagents", sandboxpolicy.Default(), nil)
}

func deepAgentsSandboxWorker() *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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
	if got := sandbox.Labels[v1beta1.LabelController]; got != "ctl-a" {
		t.Fatalf("sandbox controller label=%q, want ctl-a", got)
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

func TestExecutionSandboxEnsureRejectsInvalidWorkerPolicyWithoutCreatingSandbox(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1beta1.DeepAgentsExecutionConfig)
	}{
		{name: "idle timeout", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.IdleTimeout = "invalid"
		}},
		{name: "max lifetime", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.MaxLifetime = "0s"
		}},
		{name: "subsecond idle timeout", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.IdleTimeout = "500ms"
		}},
		{name: "non-runtime max lifetime syntax", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.MaxLifetime = "1000ms"
		}},
		{name: "egress CIDR", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].CIDR = "invalid"
		}},
		{name: "egress protocol", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].Protocol = "ICMP"
		}},
		{name: "egress port", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].Ports = []int32{0}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			tt.mutate(&worker.Spec.RuntimeConfig.DeepAgents.Execution)
			h := newExecutionSandboxHandlerTestClient(t, worker)
			sessionID := "invalid-" + strings.ReplaceAll(tt.name, " ", "-")
			req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"`+sessionID+`"}`))
			req.SetPathValue("name", worker.Name)
			rec := httptest.NewRecorder()

			h.Ensure(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var sandbox v1beta1.ExecutionSandbox
			err := h.client.Get(context.Background(), types.NamespacedName{
				Name: executionSandboxName(worker.Name, sessionID), Namespace: "agentteams-system",
			}, &sandbox)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("invalid Worker policy created ExecutionSandbox: %v", err)
			}
		})
	}
}

func TestExecutionSandboxEnsureRejectsInvalidWorkerPolicyWithoutMutatingSandbox(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1beta1.DeepAgentsExecutionConfig)
	}{
		{name: "idle timeout", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.IdleTimeout = "invalid"
		}},
		{name: "max lifetime", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.MaxLifetime = "0s"
		}},
		{name: "subsecond idle timeout", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.IdleTimeout = "1.5s"
		}},
		{name: "non-runtime max lifetime syntax", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.MaxLifetime = "1000ms"
		}},
		{name: "egress CIDR", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].CIDR = "invalid"
		}},
		{name: "egress protocol", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].Protocol = "ICMP"
		}},
		{name: "egress port", mutate: func(execution *v1beta1.DeepAgentsExecutionConfig) {
			execution.Egress[0].Ports = []int32{0}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			name := executionSandboxName(worker.Name, "thread-hash")
			sandbox := readyExecutionSandbox(worker, name, "thread-hash")
			before := sandbox.Spec.DeepCopy()
			tt.mutate(&worker.Spec.RuntimeConfig.DeepAgents.Execution)
			h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)
			req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
			req.SetPathValue("name", worker.Name)
			rec := httptest.NewRecorder()

			h.Ensure(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var stored v1beta1.ExecutionSandbox
			if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &stored); err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqual(&stored.Spec, before) {
				t.Fatalf("invalid Worker policy mutated ExecutionSandbox: got=%#v want=%#v", stored.Spec, *before)
			}
		})
	}
}

func TestExecutionSandboxEnsureRejectsInvalidConfiguredEgressCeilingWithoutMutation(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	before := sandbox.Spec.DeepCopy()
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)
	h.egressCeilings = []v1beta1.DeepAgentsEgressRule{{CIDR: "invalid-ceiling", Ports: []int32{443}}}
	req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var stored v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "agentteams-system"}, &stored); err != nil {
		t.Fatal(err)
	}
	if !apiequality.Semantic.DeepEqual(&stored.Spec, before) {
		t.Fatalf("invalid configured ceiling mutated ExecutionSandbox: got=%#v want=%#v", stored.Spec, *before)
	}
}

func TestExecutionSandboxEnsureRejectsInvalidConfiguredEgressCeilingWithoutCreation(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	h := newExecutionSandboxHandlerTestClient(t, worker)
	h.egressCeilings = []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Protocol: "ICMP", Ports: []int32{443}}}
	req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var sandbox v1beta1.ExecutionSandbox
	err := h.client.Get(context.Background(), types.NamespacedName{
		Name: executionSandboxName(worker.Name, "thread-hash"), Namespace: "agentteams-system",
	}, &sandbox)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("invalid configured ceiling created ExecutionSandbox: %v", err)
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
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "agentteams-system", Generation: 1,
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "agentteams-system",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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

func TestExecutionSandboxDeleteAllowsOwnedSandboxAfterWorkerExecutionIsDisabled(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)

	var current v1beta1.Worker
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: worker.Name, Namespace: worker.Namespace}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.RuntimeConfig.DeepAgents.Execution.Mode = "disabled"
	if err := h.client.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/sandbox", nil)
	req.SetPathValue("name", worker.Name)
	req.SetPathValue("sessionId", "thread-hash")
	rec := httptest.NewRecorder()
	h.Delete(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want 204", rec.Code, rec.Body.String())
	}
	var deleted v1beta1.ExecutionSandbox
	err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: worker.Namespace}, &deleted)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("sandbox was not deleted: %v", err)
	}
}

func TestExecutionSandboxDeleteRejectsOwnershipAndIdentityConflictsAfterWorkerExecutionIsDisabled(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1beta1.Worker, *v1beta1.ExecutionSandbox)
	}{
		{
			name: "foreign worker controller",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) {
				worker.Labels[v1beta1.LabelController] = "ctl-b"
			},
		},
		{
			name: "foreign sandbox controller",
			mutate: func(_ *v1beta1.Worker, sandbox *v1beta1.ExecutionSandbox) {
				sandbox.Labels[v1beta1.LabelController] = "ctl-b"
			},
		},
		{
			name: "worker UID mismatch",
			mutate: func(_ *v1beta1.Worker, sandbox *v1beta1.ExecutionSandbox) {
				sandbox.Spec.WorkerRef.UID = "old-worker-uid"
			},
		},
		{
			name: "worker name identity collision",
			mutate: func(_ *v1beta1.Worker, sandbox *v1beta1.ExecutionSandbox) {
				sandbox.Spec.WorkerRef.Name = "other-worker"
			},
		},
		{
			name: "session identity collision",
			mutate: func(_ *v1beta1.Worker, sandbox *v1beta1.ExecutionSandbox) {
				sandbox.Spec.SessionID = "other-session"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			name := executionSandboxName(worker.Name, "thread-hash")
			sandbox := readyExecutionSandbox(worker, name, "thread-hash")
			tt.mutate(worker, sandbox)
			h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)

			var current v1beta1.Worker
			if err := h.client.Get(context.Background(), types.NamespacedName{Name: worker.Name, Namespace: worker.Namespace}, &current); err != nil {
				t.Fatal(err)
			}
			current.Spec.RuntimeConfig.DeepAgents.Execution.Mode = "disabled"
			if err := h.client.Update(context.Background(), &current); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodDelete, "/sandbox", nil)
			req.SetPathValue("name", worker.Name)
			req.SetPathValue("sessionId", "thread-hash")
			rec := httptest.NewRecorder()
			h.Delete(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
			}
			var stored v1beta1.ExecutionSandbox
			if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: worker.Namespace}, &stored); err != nil {
				t.Fatalf("sandbox was changed: %v", err)
			}
			if !apiequality.Semantic.DeepEqual(stored.Spec, sandbox.Spec) ||
				!apiequality.Semantic.DeepEqual(stored.Status, sandbox.Status) ||
				!apiequality.Semantic.DeepEqual(stored.Labels, sandbox.Labels) {
				t.Fatalf("sandbox was mutated: %#v", stored)
			}
		})
	}
}

func TestExecutionSandboxEnsureAndHeartbeatRejectDisabledWorker(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	name := executionSandboxName(worker.Name, "thread-hash")
	sandbox := readyExecutionSandbox(worker, name, "thread-hash")
	worker.Spec.RuntimeConfig.DeepAgents.Execution.Mode = "disabled"
	h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)

	ensure := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
	ensure.SetPathValue("name", worker.Name)
	ensureRec := httptest.NewRecorder()
	h.Ensure(ensureRec, ensure)
	if ensureRec.Code != http.StatusConflict {
		t.Fatalf("ensure status=%d body=%s, want 409", ensureRec.Code, ensureRec.Body.String())
	}

	heartbeat := httptest.NewRequest(http.MethodPost, "/heartbeat", nil)
	heartbeat.SetPathValue("name", worker.Name)
	heartbeat.SetPathValue("sessionId", "thread-hash")
	heartbeatRec := httptest.NewRecorder()
	h.Heartbeat(heartbeatRec, heartbeat)
	if heartbeatRec.Code != http.StatusConflict {
		t.Fatalf("heartbeat status=%d body=%s, want 409", heartbeatRec.Code, heartbeatRec.Body.String())
	}
	var unchanged v1beta1.ExecutionSandbox
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: worker.Namespace}, &unchanged); err != nil {
		t.Fatal(err)
	}
	if !apiequality.Semantic.DeepEqual(unchanged.Status, sandbox.Status) {
		t.Fatalf("disabled Worker heartbeat mutated sandbox: %#v", unchanged.Status)
	}
}

func TestExecutionSandboxHandlerRejectsForeignControllerObjects(t *testing.T) {
	t.Run("worker", func(t *testing.T) {
		worker := deepAgentsSandboxWorker()
		worker.Labels[v1beta1.LabelController] = "ctl-b"
		h := newExecutionSandboxHandlerTestClient(t, worker)
		req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"foreign-worker"}`))
		req.SetPathValue("name", worker.Name)
		rec := httptest.NewRecorder()

		h.Ensure(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
		}
		var sandbox v1beta1.ExecutionSandbox
		err := h.client.Get(context.Background(), types.NamespacedName{
			Name: executionSandboxName(worker.Name, "foreign-worker"), Namespace: worker.Namespace,
		}, &sandbox)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("foreign Worker created sandbox: %v", err)
		}
	})

	for _, operation := range []string{"ensure", "heartbeat", "delete"} {
		t.Run(operation+" sandbox", func(t *testing.T) {
			worker := deepAgentsSandboxWorker()
			name := executionSandboxName(worker.Name, "thread-hash")
			sandbox := readyExecutionSandbox(worker, name, "thread-hash")
			sandbox.Labels[v1beta1.LabelController] = "ctl-b"
			before := sandbox.DeepCopy()
			h := newExecutionSandboxHandlerTestClient(t, worker, sandbox)
			rec := httptest.NewRecorder()

			switch operation {
			case "ensure":
				req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"thread-hash"}`))
				req.SetPathValue("name", worker.Name)
				h.Ensure(rec, req)
			case "heartbeat":
				req := httptest.NewRequest(http.MethodPost, "/heartbeat", nil)
				req.SetPathValue("name", worker.Name)
				req.SetPathValue("sessionId", "thread-hash")
				h.Heartbeat(rec, req)
			case "delete":
				req := httptest.NewRequest(http.MethodDelete, "/sandbox", nil)
				req.SetPathValue("name", worker.Name)
				req.SetPathValue("sessionId", "thread-hash")
				h.Delete(rec, req)
			}

			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
			}
			var stored v1beta1.ExecutionSandbox
			if err := h.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: worker.Namespace}, &stored); err != nil {
				t.Fatalf("foreign sandbox was deleted: %v", err)
			}
			if !apiequality.Semantic.DeepEqual(stored.Spec, before.Spec) ||
				!apiequality.Semantic.DeepEqual(stored.Status, before.Status) || stored.Labels[v1beta1.LabelController] != "ctl-b" {
				t.Fatalf("foreign sandbox was mutated: %#v", stored)
			}
		})
	}
}

func TestExecutionSandboxHTTPRoutePropagatesControllerOwnership(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	server := NewHTTPServer(":0", ServerDeps{
		Client:               cl,
		AuthMw:               authpkg.NewMiddleware(nil, nil, nil, cl, worker.Namespace),
		KubeMode:             "incluster",
		Namespace:            worker.Namespace,
		ControllerName:       "ctl-a",
		DefaultWorkerRuntime: "deepagents",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/researcher/execution-sandboxes/ensure", strings.NewReader(`{"sessionId":"http-route"}`))
	rec := httptest.NewRecorder()

	server.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", rec.Code, rec.Body.String())
	}
	var sandbox v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: executionSandboxName(worker.Name, "http-route"), Namespace: worker.Namespace,
	}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if got := sandbox.Labels[v1beta1.LabelController]; got != "ctl-a" {
		t.Fatalf("sandbox controller label=%q, want ctl-a", got)
	}
}

func TestExecutionSandboxHandlerEmptyControllerOwnsOnlyUnlabelledObjects(t *testing.T) {
	worker := deepAgentsSandboxWorker()
	delete(worker.Labels, v1beta1.LabelController)
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	h := NewExecutionSandboxHandler(cl, worker.Namespace, "", "deepagents", sandboxpolicy.Default(), nil)
	req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"embedded"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	h.Ensure(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", rec.Code, rec.Body.String())
	}
	var sandbox v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), types.NamespacedName{
		Name: executionSandboxName(worker.Name, "embedded"), Namespace: worker.Namespace,
	}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if _, exists := sandbox.Labels[v1beta1.LabelController]; exists {
		t.Fatalf("embedded sandbox unexpectedly has controller label: %#v", sandbox.Labels)
	}
}
