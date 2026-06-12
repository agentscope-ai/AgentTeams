package controller

import (
	"context"
	"sync"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
)

// captureManagerCreateLabels exercises createManagerContainer with fully
// mocked dependencies and returns the Labels map the reconciler handed
// to the backend.Create call. Using a capturing CreateFn lets us lock
// in the exact merged Pod-label set without spinning up envtest.
func captureManagerCreateLabels(t *testing.T, mgr *v1beta1.Manager) map[string]string {
	t.Helper()
	return captureManagerCreateRequest(t, mgr, nil).Labels
}

func captureManagerCreateRequest(t *testing.T, mgr *v1beta1.Manager, defaults *backend.ResourceRequirements) backend.CreateRequest {
	t.Helper()
	mockBackend := mocks.NewMockWorkerBackend()
	var (
		mu      sync.Mutex
		capture backend.CreateRequest
	)
	mockBackend.CreateFn = func(_ context.Context, req backend.CreateRequest) (*backend.WorkerResult, error) {
		mu.Lock()
		capture = req
		mu.Unlock()
		return &backend.WorkerResult{Name: req.Name, Backend: "mock", Status: backend.StatusStarting}, nil
	}

	r := &ManagerReconciler{
		Provisioner:      mocks.NewMockManagerProvisioner(),
		EnvBuilder:       mocks.NewMockManagerEnvBuilder(),
		ResourcePrefix:   auth.ResourcePrefix("hiclaw-"),
		ControllerName:   "real-ctl",
		DefaultRuntime:   "copaw",
		ManagerResources: defaults,
	}

	scope := &managerScope{
		manager: mgr,
		provResult: &service.ManagerProvisionResult{
			MatrixUserID: "@manager:localhost",
			MatrixToken:  "mock-token",
			RoomID:       "!room:localhost",
			GatewayKey:   "gw-key",
		},
	}

	if _, err := r.createManagerContainer(context.Background(), scope, mockBackend); err != nil {
		t.Fatalf("createManagerContainer: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return capture
}

// TestCreateManagerContainer_MergesMetadataAndSpecLabels verifies the
// full three-layer composition the Manager reconciler performs: CR
// metadata.labels and CR spec.labels both reach the Pod, spec wins over
// metadata on collision, and controller-forced system labels (app,
// hiclaw.io/manager, hiclaw.io/controller, hiclaw.io/role,
// hiclaw.io/runtime) are always present and correct.
func TestCreateManagerContainer_MergesMetadataAndSpecLabels(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.ObjectMeta.Labels = map[string]string{
		"owner": "alice",
		"tier":  "metadata-tier",
	}
	m.Spec.Labels = map[string]string{
		"env":  "prod",
		"tier": "spec-tier", // overrides metadata
	}
	m.Spec.Runtime = "copaw"

	labels := captureManagerCreateLabels(t, m)

	cases := map[string]string{
		"owner":                 "alice",     // metadata.labels propagated
		"env":                   "prod",      // spec.labels propagated
		"tier":                  "spec-tier", // spec beats metadata
		"hiclaw.io/manager":     "default",   // system label
		"hiclaw.io/role":        "manager",   // system label
		"hiclaw.io/runtime":     "copaw",     // system label
		"app":                   "hiclaw-manager",
		v1beta1.LabelController: "real-ctl",
	}
	for k, want := range cases {
		if got := labels[k]; got != want {
			t.Errorf("label %q = %q, want %q (full=%v)", k, got, want, labels)
		}
	}
}

// TestCreateManagerContainer_SystemLabelsOverrideUserLabels verifies
// the reserved-key contract: a user putting hiclaw.io/controller or
// app into their CR labels (metadata or spec) cannot spoof the
// controller's identity — the system layer is applied last and wins
// silently.
func TestCreateManagerContainer_SystemLabelsOverrideUserLabels(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.ObjectMeta.Labels = map[string]string{
		v1beta1.LabelController: "metadata-attacker",
		"app":                   "evil-app",
	}
	m.Spec.Labels = map[string]string{
		v1beta1.LabelController: "spec-attacker",
		"hiclaw.io/role":        "evil-role",
		"hiclaw.io/manager":     "spoofed",
	}
	m.Spec.Runtime = "copaw"

	labels := captureManagerCreateLabels(t, m)

	if got := labels[v1beta1.LabelController]; got != "real-ctl" {
		t.Errorf("controller label got %q, want real-ctl (full=%v)", got, labels)
	}
	if got := labels["app"]; got != "hiclaw-manager" {
		t.Errorf("app label got %q, want hiclaw-manager", got)
	}
	if got := labels["hiclaw.io/role"]; got != "manager" {
		t.Errorf("role label got %q, want manager", got)
	}
	if got := labels["hiclaw.io/manager"]; got != "default" {
		t.Errorf("manager label got %q, want default", got)
	}
}

// TestCreateManagerContainer_NilLabelsSafe ensures a Manager CR with no
// user labels at all still emits exactly the system label set without
// panicking.
func TestCreateManagerContainer_NilLabelsSafe(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.Spec.Runtime = "copaw"

	labels := captureManagerCreateLabels(t, m)

	for _, k := range []string{
		"app",
		"hiclaw.io/manager",
		"hiclaw.io/role",
		"hiclaw.io/runtime",
		v1beta1.LabelController,
	} {
		if _, ok := labels[k]; !ok {
			t.Errorf("missing system label %q on labelless Manager (full=%v)", k, labels)
		}
	}
}

func TestCreateManagerContainerSpecResourcesOverrideDefaults(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.Spec.Resources = &v1beta1.AgentResourceRequirements{
		Requests: v1beta1.AgentResourceValues{CPU: "750m", Memory: "1536Mi"},
		Limits:   v1beta1.AgentResourceValues{CPU: "3", Memory: "5Gi"},
	}

	req := captureManagerCreateRequest(t, m, &backend.ResourceRequirements{
		CPURequest:    "100m",
		MemoryRequest: "256Mi",
		CPULimit:      "1",
		MemoryLimit:   "2Gi",
	})

	if req.Resources == nil {
		t.Fatal("CreateRequest.Resources = nil, want manager spec resources")
	}
	if req.Resources.CPURequest != "750m" || req.Resources.MemoryRequest != "1536Mi" ||
		req.Resources.CPULimit != "3" || req.Resources.MemoryLimit != "5Gi" {
		t.Fatalf("CreateRequest.Resources = %+v", req.Resources)
	}
}

func TestCreateManagerContainerSpecResourcesPartiallyOverrideDefaults(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.Spec.Resources = &v1beta1.AgentResourceRequirements{
		Limits: v1beta1.AgentResourceValues{CPU: "3"},
	}

	req := captureManagerCreateRequest(t, m, &backend.ResourceRequirements{
		CPURequest:    "100m",
		MemoryRequest: "256Mi",
		CPULimit:      "1",
		MemoryLimit:   "2Gi",
	})

	if req.Resources == nil {
		t.Fatal("CreateRequest.Resources = nil, want merged manager resources")
	}
	if req.Resources.CPURequest != "100m" || req.Resources.MemoryRequest != "256Mi" ||
		req.Resources.CPULimit != "3" || req.Resources.MemoryLimit != "2Gi" {
		t.Fatalf("CreateRequest.Resources = %+v", req.Resources)
	}
}

func TestCreateManagerContainerUsesDefaultResourcesWhenSpecResourcesUnset(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	defaults := &backend.ResourceRequirements{
		CPURequest:    "100m",
		MemoryRequest: "256Mi",
		CPULimit:      "1",
		MemoryLimit:   "2Gi",
	}

	req := captureManagerCreateRequest(t, m, defaults)

	if req.Resources != defaults {
		t.Fatalf("CreateRequest.Resources = %+v, want default pointer %+v", req.Resources, defaults)
	}
}

func TestReconcileManagerInfrastructurePassesModelProviderToProvision(t *testing.T) {
	prov := mocks.NewMockManagerProvisioner()
	r := &ManagerReconciler{Provisioner: prov}
	m := &v1beta1.Manager{}
	m.Name = "default"

	scope := &managerScope{
		manager:           m,
		modelProviderInfo: &gateway.ModelProviderInfo{HttpApiID: "qwen-http-api"},
	}

	if _, err := r.reconcileManagerInfrastructure(context.Background(), scope); err != nil {
		t.Fatalf("reconcileManagerInfrastructure: %v", err)
	}
	if len(prov.Calls.ProvisionManager) != 1 {
		t.Fatalf("ProvisionManager calls=%d, want 1", len(prov.Calls.ProvisionManager))
	}
	if got := prov.Calls.ProvisionManager[0].ModelProviderID; got != "qwen-http-api" {
		t.Fatalf("ProvisionManager ModelProviderID=%q, want qwen-http-api", got)
	}
}

func TestReconcileManagerInfrastructureRestoresModelProviderAuth(t *testing.T) {
	prov := mocks.NewMockManagerProvisioner()
	r := &ManagerReconciler{Provisioner: prov}
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Status.MatrixUserID = "@manager:localhost"

	scope := &managerScope{
		manager:           m,
		modelProviderInfo: &gateway.ModelProviderInfo{HttpApiID: "openai-http-api"},
	}

	if _, err := r.reconcileManagerInfrastructure(context.Background(), scope); err != nil {
		t.Fatalf("reconcileManagerInfrastructure: %v", err)
	}
	if len(prov.Calls.EnsureManagerGatewayAuth) != 1 {
		t.Fatalf("EnsureManagerGatewayAuth calls=%d, want 1", len(prov.Calls.EnsureManagerGatewayAuth))
	}
	call := prov.Calls.EnsureManagerGatewayAuth[0]
	if call.Name != "default" {
		t.Fatalf("EnsureManagerGatewayAuth name=%q, want default", call.Name)
	}
	if call.ModelProviderID != "openai-http-api" {
		t.Fatalf("EnsureManagerGatewayAuth ModelProviderID=%q, want openai-http-api", call.ModelProviderID)
	}
}
