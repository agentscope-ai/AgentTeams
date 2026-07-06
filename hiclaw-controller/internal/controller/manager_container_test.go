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
		ResourcePrefix:   auth.ResourcePrefix("agentteams-"),
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
// agentteams.io/manager, agentteams.io/controller, agentteams.io/role,
// agentteams.io/runtime) are always present and correct.
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
		"agentteams.io/manager": "default",   // system label
		"agentteams.io/role":    "manager",   // system label
		"agentteams.io/runtime": "copaw",     // system label
		"app":                   "agentteams-manager",
		v1beta1.LabelController: "real-ctl",
	}
	for k, want := range cases {
		if got := labels[k]; got != want {
			t.Errorf("label %q = %q, want %q (full=%v)", k, got, want, labels)
		}
	}
}

// TestCreateManagerContainer_SystemLabelsOverrideUserLabels verifies
// the reserved-key contract: a user putting agentteams.io/controller or
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
		"agentteams.io/role":    "evil-role",
		"agentteams.io/manager": "spoofed",
	}
	m.Spec.Runtime = "copaw"

	labels := captureManagerCreateLabels(t, m)

	if got := labels[v1beta1.LabelController]; got != "real-ctl" {
		t.Errorf("controller label got %q, want real-ctl (full=%v)", got, labels)
	}
	if got := labels["app"]; got != "agentteams-manager" {
		t.Errorf("app label got %q, want agentteams-manager", got)
	}
	if got := labels["agentteams.io/role"]; got != "manager" {
		t.Errorf("role label got %q, want manager", got)
	}
	if got := labels["agentteams.io/manager"]; got != "default" {
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
		"agentteams.io/manager",
		"agentteams.io/role",
		"agentteams.io/runtime",
		v1beta1.LabelController,
	} {
		if _, ok := labels[k]; !ok {
			t.Errorf("missing system label %q on labelless Manager (full=%v)", k, labels)
		}
	}
}

func TestCreateManagerContainerStoresStatusSpecHash(t *testing.T) {
	mockBackend := mocks.NewMockWorkerBackend()
	r := &ManagerReconciler{
		Provisioner:    mocks.NewMockManagerProvisioner(),
		EnvBuilder:     mocks.NewMockManagerEnvBuilder(),
		ResourcePrefix: auth.ResourcePrefix("hiclaw-"),
		ControllerName: "real-ctl",
		DefaultRuntime: "copaw",
	}
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.Spec.Runtime = "copaw"
	m.Spec.Image = "manager:new"
	scope := &managerScope{
		manager: m,
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
	if _, ok := mockBackend.LastCreateReq(); !ok {
		t.Fatal("expected backend Create to be called")
	}
	if want := hashAppliedManagerSpec(m.Spec); m.Status.SpecHash != want {
		t.Fatalf("Manager status specHash=%q, want %q", m.Status.SpecHash, want)
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

func TestReconcileManagerInfrastructureKeepsModelProviderOutOfProvision(t *testing.T) {
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
	if got := prov.Calls.ProvisionManager[0].Name; got != "default" {
		t.Fatalf("ProvisionManager Name=%q, want default", got)
	}
}

func TestReconcileManagerInfrastructureRestoresGatewayAuth(t *testing.T) {
	prov := mocks.NewMockManagerProvisioner()
	r := &ManagerReconciler{Provisioner: prov}
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
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
	if call.GatewayKey == "" {
		t.Fatal("EnsureManagerGatewayAuth GatewayKey is empty")
	}
}

func TestEnsureManagerContainerPresentPrefersStatusSpecHashOverLegacyAnnotation(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"
	m.Spec.Image = "manager:new"
	desiredHash := hashAppliedManagerSpec(m.Spec)
	m.Status.SpecHash = desiredHash

	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	sandboxBackend.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:            "hiclaw-manager",
			Backend:         "sandbox",
			Status:          backend.StatusRunning,
			AppliedSpecHash: "legacy-old-hash",
		}, nil
	}
	r := &ManagerReconciler{
		Backend:        backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		ResourcePrefix: auth.ResourcePrefix("hiclaw-"),
	}

	if _, err := r.ensureManagerContainerPresent(context.Background(), &managerScope{manager: m}); err != nil {
		t.Fatalf("ensureManagerContainerPresent: %v", err)
	}
	if len(sandboxBackend.Calls.Delete) != 0 || len(sandboxBackend.Calls.Create) != 0 {
		t.Fatalf("status specHash should win over legacy annotation, delete=%v create=%v", sandboxBackend.Calls.Delete, sandboxBackend.Calls.Create)
	}
	if m.Status.SpecHash != desiredHash {
		t.Fatalf("Manager status specHash=%q, want %q", m.Status.SpecHash, desiredHash)
	}
}

func TestEnsureManagerContainerPresentSandboxUnknownWaitsWithoutRecreate(t *testing.T) {
	m := &v1beta1.Manager{}
	m.Name = "default"
	m.Namespace = "hiclaw"

	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	sandboxBackend.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:      "hiclaw-manager",
			Backend:   "sandbox",
			Status:    backend.StatusUnknown,
			RawStatus: "multiple_sandboxes",
			Message:   "multiple sandboxes match",
		}, nil
	}
	r := &ManagerReconciler{
		Backend:        backend.NewRegistry([]backend.WorkerBackend{sandboxBackend}),
		ResourcePrefix: auth.ResourcePrefix("hiclaw-"),
	}

	res, err := r.ensureManagerContainerPresent(context.Background(), &managerScope{manager: m})
	if err != nil {
		t.Fatalf("ensureManagerContainerPresent: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if len(sandboxBackend.Calls.Delete) != 0 || len(sandboxBackend.Calls.Create) != 0 {
		t.Fatalf("sandbox unknown should wait without recreate, delete=%v create=%v", sandboxBackend.Calls.Delete, sandboxBackend.Calls.Create)
	}
}
