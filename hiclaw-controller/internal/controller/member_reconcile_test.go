package controller

import (
	"context"
	"strings"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
)

func TestCreateMemberContainerAddsDockerHostGateway(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "docker"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if got, want := req.ExtraHosts, []string{dockerHostInternalExtraHost}; !equalStringSlices(got, want) {
		t.Fatalf("ExtraHosts=%v, want %v", got, want)
	}
}

func TestCreateMemberContainerDoesNotAddDockerHostGatewayForK8s(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "k8s"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if len(req.ExtraHosts) != 0 {
		t.Fatalf("ExtraHosts=%v, want empty for k8s backend", req.ExtraHosts)
	}
}

func TestCreateMemberContainerPassesSpecResources(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "k8s"
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{
			Image: "img:latest",
			Resources: &v1beta1.AgentResourceRequirements{
				Requests: v1beta1.AgentResourceValues{CPU: "250m", Memory: "512Mi"},
				Limits:   v1beta1.AgentResourceValues{CPU: "2", Memory: "4Gi"},
			},
		},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if req.Resources == nil {
		t.Fatal("CreateRequest.Resources = nil, want spec resources")
	}
	if req.Resources.CPURequest != "250m" || req.Resources.MemoryRequest != "512Mi" ||
		req.Resources.CPULimit != "2" || req.Resources.MemoryLimit != "4Gi" {
		t.Fatalf("CreateRequest.Resources = %+v", req.Resources)
	}
}

func TestCreateMemberContainerPassesSandboxWorkerDeps(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "sandbox"
	prov := mocks.NewMockProvisioner()
	prov.RequestSATokenFn = func(ctx context.Context, workerName string) (string, error) {
		return "sa-token-" + workerName, nil
	}
	envBuilder := mocks.NewMockEnvBuilder()
	envBuilder.BuildFn = func(workerName string, prov *service.WorkerProvisionResult) map[string]string {
		return map[string]string{
			"AGENTTEAMS_TEST_ENV": "true",
		}
	}
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: prov,
		EnvBuilder:  envBuilder,
	}, MemberContext{
		Name:           "alice",
		RuntimeName:    "alice",
		BackendRuntime: v1beta1.BackendRuntimeSandbox,
		Spec:           v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if req.WorkersDeps == nil {
		t.Fatal("CreateRequest.WorkersDeps = nil, want sandbox runtime deps")
	}
	if req.WorkersDeps.AuthToken != "sa-token-alice" {
		t.Fatalf("WorkersDeps.AuthToken=%q, want sa-token-alice", req.WorkersDeps.AuthToken)
	}
	if got := req.WorkersDeps.Env["AGENTTEAMS_TEST_ENV"]; got != "true" {
		t.Fatalf("WorkersDeps.Env[AGENTTEAMS_TEST_ENV]=%q, want true (all=%v)", got, req.WorkersDeps.Env)
	}
	if !hasDynamicMount(req.WorkersDeps.DynamicVolumeMounts, "/mnt/agentteams/env", "workers-deps/alice/env") {
		t.Fatalf("missing env dynamic mount: %+v", req.WorkersDeps.DynamicVolumeMounts)
	}
	if !hasDynamicMount(req.WorkersDeps.DynamicVolumeMounts, "/var/run/secrets/agentteams", "workers-deps/alice/token") {
		t.Fatalf("missing token dynamic mount: %+v", req.WorkersDeps.DynamicVolumeMounts)
	}
}

func TestEnsureMemberContainerPresentBlocksLegacyPodToSandboxSwitch(t *testing.T) {
	podBackend := mocks.NewMockWorkerBackend()
	podBackend.NameOverride = "k8s"
	podBackend.StatusFn = func(ctx context.Context, name string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Name: name, Status: backend.StatusRunning}, nil
	}
	sandboxBackend := mocks.NewMockWorkerBackend()
	sandboxBackend.NameOverride = "sandbox"
	state := &MemberState{}

	_, err := ensureMemberContainerPresent(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		Backend:     backend.NewRegistry([]backend.WorkerBackend{podBackend, sandboxBackend}),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name:                 "alice",
		BackendRuntime:       v1beta1.BackendRuntimeSandbox,
		StatusBackendRuntime: "",
		Spec:                 v1beta1.WorkerSpec{},
	}, state)
	if err == nil {
		t.Fatal("expected backend switch to be blocked")
	}
	if !strings.Contains(err.Error(), "spec.backendRuntime cannot be changed until the Worker is Stopped") {
		t.Fatalf("error=%v, want backend switch guard", err)
	}
	if _, ok := sandboxBackend.LastCreateReq(); ok {
		t.Fatal("sandbox backend Create was called; want legacy pod switch blocked")
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasDynamicMount(mounts []backend.DynamicVolumeMount, mountPath, subPath string) bool {
	for _, mount := range mounts {
		if mount.MountPath == mountPath && mount.SubPath == subPath {
			return true
		}
	}
	return false
}

// mockBackend is a minimal WorkerBackend implementation used to test
// resolveBackendForMember when the backend is NOT a *backend.K8sBackend.
type mockBackend struct{}

func (m *mockBackend) Name() string                     { return "mock" }
func (m *mockBackend) DeploymentMode() string           { return "local" }
func (m *mockBackend) Available(_ context.Context) bool { return true }
func (m *mockBackend) NeedsCredentialInjection() bool   { return false }
func (m *mockBackend) Create(_ context.Context, _ backend.CreateRequest) (*backend.WorkerResult, error) {
	return nil, nil
}
func (m *mockBackend) Delete(_ context.Context, _ string) error { return nil }
func (m *mockBackend) Start(_ context.Context, _ string) error  { return nil }
func (m *mockBackend) Stop(_ context.Context, _ string) error   { return nil }
func (m *mockBackend) Status(_ context.Context, _ string) (*backend.WorkerResult, error) {
	return nil, nil
}

func TestResolveBackendForMember_LocalMode(t *testing.T) {
	// When DeployMode is empty or "Local", the original backend is returned unchanged.
	k8sB := backend.NewK8sBackendWithClient(nil, backend.K8sConfig{Namespace: "default"}, "hiclaw-worker-", nil)

	tests := []struct {
		name       string
		deployMode string
	}{
		{"empty deploy mode", ""},
		{"explicit Local mode", v1beta1.DeployModeLocal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := MemberContext{
				Name:       "worker-a",
				DeployMode: tt.deployMode,
			}
			got := resolveBackendForMember(k8sB, m)
			if got != k8sB {
				t.Errorf("expected original backend pointer, got a different instance")
			}
		})
	}
}

func TestResolveBackendForMember_RemoteMode(t *testing.T) {
	// When DeployMode="Remote" and TargetClusterID is set with a K8sBackend,
	// should return a new K8sBackend with remote target configured.
	k8sB := backend.NewK8sBackendWithClient(nil, backend.K8sConfig{Namespace: "default"}, "hiclaw-worker-", nil)

	m := MemberContext{
		Name:            "worker-b",
		DeployMode:      v1beta1.DeployModeRemote,
		TargetClusterID: "cluster-remote-1",
		TargetNamespace: "remote-ns",
	}

	got := resolveBackendForMember(k8sB, m)
	if got == k8sB {
		t.Fatal("expected a new backend instance for remote mode, got same pointer")
	}

	// Verify the returned value is still a *backend.K8sBackend.
	remoteK8s, ok := got.(*backend.K8sBackend)
	if !ok {
		t.Fatalf("expected *backend.K8sBackend, got %T", got)
	}

	// The remote backend should still report as "k8s".
	if remoteK8s.Name() != "k8s" {
		t.Errorf("remote backend Name() = %q, want %q", remoteK8s.Name(), "k8s")
	}
}

func TestResolveBackendForMember_NonK8sBackend(t *testing.T) {
	// When DeployMode="Remote" but the backend is not a *K8sBackend,
	// should return the original backend unchanged.
	mb := &mockBackend{}
	m := MemberContext{
		Name:            "worker-c",
		DeployMode:      v1beta1.DeployModeRemote,
		TargetClusterID: "cluster-remote-2",
		TargetNamespace: "remote-ns",
	}

	got := resolveBackendForMember(mb, m)
	if got != mb {
		t.Errorf("expected original mock backend, got a different instance")
	}
}

func TestResolveBackendForMember_EmptyClusterID(t *testing.T) {
	// When DeployMode="Remote" but TargetClusterID is empty,
	// should return the original backend unchanged.
	k8sB := backend.NewK8sBackendWithClient(nil, backend.K8sConfig{Namespace: "default"}, "hiclaw-worker-", nil)

	m := MemberContext{
		Name:            "worker-d",
		DeployMode:      v1beta1.DeployModeRemote,
		TargetClusterID: "",
		TargetNamespace: "remote-ns",
	}

	got := resolveBackendForMember(k8sB, m)
	if got != k8sB {
		t.Errorf("expected original backend (empty clusterID), got a different instance")
	}
}
