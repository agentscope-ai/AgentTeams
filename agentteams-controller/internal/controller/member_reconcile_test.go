package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

// mockBackend is a minimal WorkerBackend implementation used to test
// resolveBackendForMember through a backend.Registry. It always reports
// Available so the registry resolves it for any backendRuntime via the
// DetectWorkerBackend fallback path.
type mockBackend struct{ name string }

func (m *mockBackend) Name() string {
	if m.name == "" {
		return "mock"
	}
	return m.name
}
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
	// When DeployMode is empty or "Local", remote targeting is not applied
	// and the backend the registry resolves is returned unchanged.
	mb := &mockBackend{}
	reg := backend.NewRegistry([]backend.WorkerBackend{mb})

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
			got, err := resolveBackendForMember(reg, "", m)
			if err != nil {
				t.Fatalf("resolveBackendForMember err = %v", err)
			}
			if got != mb {
				t.Errorf("expected original backend pointer, got a different instance")
			}
		})
	}
}

func TestResolveBackendForMember_SandboxDoesNotFallbackToPod(t *testing.T) {
	mb := &mockBackend{name: "k8s"}
	reg := backend.NewRegistry([]backend.WorkerBackend{mb})

	_, err := resolveBackendForMember(reg, v1beta1.BackendRuntimeSandbox, MemberContext{Name: "worker-c"})
	if err == nil {
		t.Fatal("expected unsupported sandbox backend error")
	}
	if !strings.Contains(err.Error(), "backendRuntime \"sandbox\" is not supported") {
		t.Fatalf("error=%v", err)
	}
}

func TestValidateMemberDeploymentRejectsRemote(t *testing.T) {
	if err := ValidateMemberDeployment(MemberContext{Name: "worker-a", DeployMode: v1beta1.DeployModeLocal}); err != nil {
		t.Fatalf("local deployment should be valid: %v", err)
	}

	err := ValidateMemberDeployment(MemberContext{Name: "worker-b", DeployMode: v1beta1.DeployModeRemote})
	if err == nil || !strings.Contains(err.Error(), "deployMode \"Remote\" is not supported") {
		t.Fatalf("remote deployment error=%v", err)
	}
}

func TestResolveBackendForMember_NoBackendAvailable(t *testing.T) {
	// An empty registry surfaces an error so callers can decide whether
	// to skip container management or fail loudly.
	reg := backend.NewRegistry(nil)
	m := MemberContext{Name: "worker-d"}

	if _, err := resolveBackendForMember(reg, "", m); err == nil {
		t.Fatal("expected an error when no backend is available")
	}
}

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

func TestReconcileMemberExposeSkipsUnsupportedGatewayProvider(t *testing.T) {
	current := []v1beta1.ExposedPortStatus{{Port: 8088, Domain: "console.example.com"}}
	prov := mocks.NewMockProvisioner()
	prov.ReconcileExposeFn = func(context.Context, string, []v1beta1.ExposePort, []v1beta1.ExposedPortStatus) ([]v1beta1.ExposedPortStatus, error) {
		return nil, gateway.ErrUnsupportedOp
	}
	state := &MemberState{}

	if err := ReconcileMemberExpose(context.Background(), MemberDeps{Provisioner: prov}, MemberContext{
		Name:                "alice",
		Spec:                v1beta1.WorkerSpec{Expose: []v1beta1.ExposePort{{Port: 8088}}},
		CurrentExposedPorts: current,
	}, state); err != nil {
		t.Fatalf("ReconcileMemberExpose err = %v", err)
	}
	if len(state.ExposedPorts) != 1 || state.ExposedPorts[0].Port != 8088 || state.ExposedPorts[0].Domain != "console.example.com" {
		t.Fatalf("ExposedPorts=%+v, want current exposed ports preserved", state.ExposedPorts)
	}
}

func TestCreateMemberContainerPassesMemberRoleEnv(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"},
	}

	_, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name:        "alpha-lead",
		RuntimeName: "leader-runtime",
		Role:        RoleTeamLeader,
		Spec:        v1beta1.WorkerSpec{Image: "img:latest"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer failed: %v", err)
	}

	req, ok := wb.LastCreateReq()
	if !ok {
		t.Fatal("expected backend Create to be called")
	}
	if got := req.Env["AGENTTEAMS_WORKER_ROLE"]; got != "team_leader" {
		t.Fatalf("AGENTTEAMS_WORKER_ROLE=%q, want team_leader", got)
	}
}

func TestMemberRuntimeStalePrefersStatusSpecHashOverLegacySandboxAnnotation(t *testing.T) {
	member := MemberContext{
		Name:            "alice",
		BackendRuntime:  v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash: "desired-hash",
		CurrentSpecHash: "desired-hash",
		SpecChanged:     false,
	}
	result := &backend.WorkerResult{
		Name:            "alice",
		Backend:         "sandbox",
		Status:          backend.StatusRunning,
		AppliedSpecHash: "legacy-old-hash",
	}

	if memberRuntimeStale(result, member, true) {
		t.Fatal("status specHash should win over legacy annotation")
	}
}

func TestReconcileMemberContainerSpecChangedDeletesRunningSandboxWithoutHash(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "sandbox"
	wb.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:    "alice",
			Backend: "sandbox",
			Status:  backend.StatusRunning,
		}, nil
	}
	member := MemberContext{
		Name:            "alice",
		BackendRuntime:  v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash: "desired-hash",
		SpecChanged:     true,
	}

	state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}
	if _, err := ReconcileMemberContainer(context.Background(), MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{wb}),
	}, member, state); err != nil {
		t.Fatalf("ReconcileMemberContainer: %v", err)
	}
	if len(wb.Calls.Delete) != 1 || wb.Calls.Delete[0] != "alice" {
		t.Fatalf("spec-changed sandbox should be deleted before recreate, delete=%v", wb.Calls.Delete)
	}
	if len(wb.Calls.Create) != 0 {
		t.Fatalf("sandbox recreate must wait for next reconcile, create=%v", wb.Calls.Create)
	}
}

func TestReconcileMemberContainerSandboxSpecChangeWaitsAfterDelete(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "sandbox"
	if _, err := wb.Create(context.Background(), backend.CreateRequest{Name: "alice"}); err != nil {
		t.Fatalf("seed sandbox backend: %v", err)
	}
	wb.ClearCalls()

	member := MemberContext{
		Name:            "alice",
		BackendRuntime:  v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash: "new-hash",
		CurrentSpecHash: "old-hash",
		SpecChanged:     true,
	}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}

	res, err := ReconcileMemberContainer(context.Background(), MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{wb}),
	}, member, state)
	if err != nil {
		t.Fatalf("ReconcileMemberContainer: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if state.ContainerState != "stopping" {
		t.Fatalf("ContainerState=%q, want stopping", state.ContainerState)
	}
	if len(wb.Calls.Delete) != 1 || wb.Calls.Delete[0] != "alice" {
		t.Fatalf("delete calls=%v, want [alice]", wb.Calls.Delete)
	}
	if len(wb.Calls.Create) != 0 {
		t.Fatalf("sandbox recreate must wait for next reconcile, create calls=%v", wb.Calls.Create)
	}
}

func TestReconcileMemberContainerSandboxSpecChangeDeletesAmbiguousSandboxes(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "sandbox"
	wb.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:      "alice",
			Backend:   "sandbox",
			Status:    backend.StatusUnknown,
			RawStatus: "multiple_sandboxes",
			Message:   "multiple sandboxes match alice",
		}, nil
	}
	member := MemberContext{
		Name:            "alice",
		RuntimeName:     "alice-runtime",
		BackendRuntime:  v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash: "new-hash",
		CurrentSpecHash: "old-hash",
		SpecChanged:     true,
	}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}

	res, err := ReconcileMemberContainer(context.Background(), MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{wb}),
	}, member, state)
	if err != nil {
		t.Fatalf("ReconcileMemberContainer: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if state.ContainerState != "stopping" {
		t.Fatalf("ContainerState=%q, want stopping", state.ContainerState)
	}
	if len(wb.Calls.Delete) != 1 || wb.Calls.Delete[0] != "alice" {
		t.Fatalf("delete calls=%v, want [alice]", wb.Calls.Delete)
	}
	if len(wb.Calls.Create) != 0 {
		t.Fatalf("sandbox recreate must wait for next reconcile, create calls=%v", wb.Calls.Create)
	}
}

func TestReconcileMemberContainerSpecChangeDeletesTransientBackendStates(t *testing.T) {
	cases := []struct {
		name      string
		status    backend.WorkerStatus
		rawStatus string
	}{
		{"claiming", backend.StatusUnknown, "Claiming"},
		{"multiple sandboxes", backend.StatusUnknown, "multiple_sandboxes"},
		{"starting", backend.StatusStarting, "Starting"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wb := mocks.NewMockWorkerBackend()
			wb.NameOverride = "sandbox"
			wb.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
				return &backend.WorkerResult{
					Name:      "alice",
					Backend:   "sandbox",
					Status:    tc.status,
					RawStatus: tc.rawStatus,
				}, nil
			}
			member := MemberContext{
				Name:            "alice",
				RuntimeName:     "alice-runtime",
				BackendRuntime:  v1beta1.BackendRuntimeSandbox,
				AppliedSpecHash: "new-hash",
				CurrentSpecHash: "old-hash",
				SpecChanged:     true,
			}
			state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}

			res, err := ReconcileMemberContainer(context.Background(), MemberDeps{
				Backend: backend.NewRegistry([]backend.WorkerBackend{wb}),
			}, member, state)
			if err != nil {
				t.Fatalf("ReconcileMemberContainer: %v", err)
			}
			if res.RequeueAfter != reconcileRetryDelay {
				t.Fatalf("RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
			}
			if state.ContainerState != "stopping" {
				t.Fatalf("ContainerState=%q, want stopping", state.ContainerState)
			}
			if len(wb.Calls.Delete) != 1 || wb.Calls.Delete[0] != "alice" {
				t.Fatalf("delete calls=%v, want [alice]", wb.Calls.Delete)
			}
			if len(wb.Calls.Create) != 0 {
				t.Fatalf("create calls=%v, want none before delete settles", wb.Calls.Create)
			}
		})
	}
}

func TestReconcileMemberContainerSpecChangeCreatesWhenBackendNotFound(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "k8s"
	wb.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{
			Name:    "alice",
			Backend: "k8s",
			Status:  backend.StatusNotFound,
		}, nil
	}
	member := MemberContext{
		Name:            "alice",
		RuntimeName:     "alice-runtime",
		AppliedSpecHash: "new-hash",
		CurrentSpecHash: "old-hash",
		SpecChanged:     true,
		Spec:            v1beta1.WorkerSpec{Image: "worker:v2"},
	}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}

	_, err := ReconcileMemberContainer(context.Background(), MemberDeps{
		Backend:     backend.NewRegistry([]backend.WorkerBackend{wb}),
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, member, state)
	if err != nil {
		t.Fatalf("ReconcileMemberContainer: %v", err)
	}
	if len(wb.Calls.Delete) != 0 {
		t.Fatalf("not-found backend should not be deleted, delete=%v", wb.Calls.Delete)
	}
	if len(wb.Calls.Create) != 1 || wb.Calls.Create[0] != "alice" {
		t.Fatalf("create calls=%v, want [alice]", wb.Calls.Create)
	}
}

func TestReconcileMemberContainerSpecChangeStatusErrorDoesNotDelete(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.NameOverride = "sandbox"
	statusErr := errors.New("status api unavailable")
	wb.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return nil, statusErr
	}
	member := MemberContext{
		Name:            "alice",
		RuntimeName:     "alice-runtime",
		BackendRuntime:  v1beta1.BackendRuntimeSandbox,
		AppliedSpecHash: "new-hash",
		CurrentSpecHash: "old-hash",
		SpecChanged:     true,
	}

	_, err := ReconcileMemberContainer(context.Background(), MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{wb}),
	}, member, &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}})
	if err == nil {
		t.Fatal("ReconcileMemberContainer should return status error")
	}
	if len(wb.Calls.Delete) != 0 {
		t.Fatalf("status error must not delete backend resources, delete=%v", wb.Calls.Delete)
	}
	if len(wb.Calls.Create) != 0 {
		t.Fatalf("status error must not create backend resources, create=%v", wb.Calls.Create)
	}
}

func TestCreateMemberContainerConflictRequeues(t *testing.T) {
	wb := mocks.NewMockWorkerBackend()
	wb.CreateFn = func(context.Context, backend.CreateRequest) (*backend.WorkerResult, error) {
		return nil, backend.ErrConflict
	}
	state := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "matrix-token"}}

	res, err := createMemberContainer(context.Background(), MemberDeps{
		Provisioner: mocks.NewMockProvisioner(),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}, MemberContext{
		Name: "alice",
		Spec: v1beta1.WorkerSpec{Image: "worker:v2"},
	}, state, wb)
	if err != nil {
		t.Fatalf("createMemberContainer: %v", err)
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("RequeueAfter=%v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	if len(wb.Calls.Create) != 1 || wb.Calls.Create[0] != "alice" {
		t.Fatalf("create calls=%v, want [alice]", wb.Calls.Create)
	}
}

func TestReconcileMemberConfigQwenPawWritesRuntimeConfigOnly(t *testing.T) {
	deployer := mocks.NewMockDeployer()
	state := &MemberState{
		MatrixUserID: "@worker-a:matrix.local",
		RoomID:       "!worker-dm:matrix.local",
		ProvResult: &service.WorkerProvisionResult{
			MatrixToken:    "matrix-token",
			GatewayKey:     "gateway-key",
			MatrixPassword: "matrix-password",
		},
	}
	member := MemberContext{
		Name:              "worker-cr-a",
		RuntimeName:       "worker-a",
		Role:              RoleTeamWorker,
		Generation:        7,
		TeamName:          "demo-team",
		TeamLeaderName:    "leader-runtime",
		TeamRoomID:        "!team:matrix.local",
		LeaderDMRoomID:    "!leader-dm:matrix.local",
		TeamAdminName:     "admin",
		TeamAdminMatrixID: "@admin:matrix.local",
		ModelProviderInfo: &gateway.ModelProviderInfo{
			IntranetURL: "https://provider-aigw.example.com/default",
		},
		TeamMembers: []service.RuntimeConfigTeamMember{{
			Name:         "leader",
			RuntimeName:  "leader-runtime",
			Role:         RoleTeamLeader.String(),
			MatrixUserID: "@leader-runtime:matrix.local",
		}, {
			Name:         "worker-cr-a",
			RuntimeName:  "worker-a",
			Role:         RoleTeamWorker.String(),
			MatrixUserID: "@worker-a:matrix.local",
		}},
		Spec: v1beta1.WorkerSpec{
			Runtime: "qwenpaw",
			Model:   "qwen-plus",
			Package: "nacos://registry/ns/dev-worker?version=1.2.0",
			Skills:  []string{"dev-plan"},
		},
	}

	if err := ReconcileMemberConfig(context.Background(), MemberDeps{Deployer: deployer}, member, state); err != nil {
		t.Fatalf("ReconcileMemberConfig failed: %v", err)
	}

	if got := len(deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("DeployMemberRuntimeConfig calls=%d, want 1", got)
	}
	req := deployer.Calls.DeployMemberRuntimeConfig[0]
	if req.Name != "worker-cr-a" || req.RuntimeName != "worker-a" || req.Runtime != "qwenpaw" {
		t.Fatalf("unexpected runtime config request: %#v", req)
	}
	if req.MatrixUserID != "@worker-a:matrix.local" || req.PersonalRoomID != "!worker-dm:matrix.local" {
		t.Fatalf("runtime config missing member room facts: %#v", req)
	}
	if req.TeamRoomID != "!team:matrix.local" || req.LeaderRuntimeName != "leader-runtime" || req.LeaderDMRoomID != "!leader-dm:matrix.local" {
		t.Fatalf("runtime config missing team room facts: %#v", req)
	}
	if req.AIGatewayURL != "https://provider-aigw.example.com/default" {
		t.Fatalf("runtime config ai gateway URL=%q", req.AIGatewayURL)
	}
	if len(req.TeamMembers) != 2 || req.TeamMembers[0].RuntimeName != "leader-runtime" || req.TeamMembers[1].RuntimeName != "worker-a" {
		t.Fatalf("runtime config missing team roster: %#v", req.TeamMembers)
	}
	if deployPkg, writeInline, deployConfig, pushSkills, _ := deployer.CallCounts(); deployPkg != 0 || writeInline != 0 || deployConfig != 0 || pushSkills != 0 {
		t.Fatalf("qwenpaw must skip legacy deploy path, got package=%d inline=%d config=%d skills=%d",
			deployPkg, writeInline, deployConfig, pushSkills)
	}
}

func TestReconcileMemberConfigEdgeWritesRemoteManagedRuntimeConfigOnly(t *testing.T) {
	deployer := mocks.NewMockDeployer()
	state := &MemberState{
		MatrixUserID: "@claude-local:matrix.local",
		RoomID:       "!worker-dm:matrix.local",
		ProvResult: &service.WorkerProvisionResult{
			MatrixToken:    "matrix-access-token",
			GatewayKey:     "gateway-key",
			MatrixPassword: "matrix-password",
		},
	}
	member := MemberContext{
		Name:        "edge-worker-cr",
		RuntimeName: "claude-local",
		Role:        RoleStandalone,
		Generation:  5,
		DeployMode:  v1beta1.DeployModeEdge,
		ModelProviderInfo: &gateway.ModelProviderInfo{
			IntranetURL: "http://aigw.internal/v1/claude",
		},
		Spec: v1beta1.WorkerSpec{
			Runtime: "openclaw",
			Model:   "claude-sonnet-4",
			Package: "oss://agents/claude-local/packages/demo.zip",
		},
	}

	if err := ReconcileMemberConfig(context.Background(), MemberDeps{Deployer: deployer}, member, state); err != nil {
		t.Fatalf("ReconcileMemberConfig failed: %v", err)
	}

	if got := len(deployer.Calls.DeployMemberRuntimeConfig); got != 1 {
		t.Fatalf("DeployMemberRuntimeConfig calls=%d, want 1", got)
	}
	req := deployer.Calls.DeployMemberRuntimeConfig[0]
	if req.Name != "edge-worker-cr" || req.RuntimeName != "claude-local" {
		t.Fatalf("unexpected runtime config identity: %#v", req)
	}
	if req.Runtime != "remote-managed-local" {
		t.Fatalf("runtime=%q, want remote-managed-local", req.Runtime)
	}
	if req.MatrixAccessToken != "matrix-access-token" {
		t.Fatalf("MatrixAccessToken=%q", req.MatrixAccessToken)
	}
	if req.GatewayKey != "gateway-key" {
		t.Fatalf("GatewayKey=%q", req.GatewayKey)
	}
	if req.AIGatewayURL != "http://aigw.internal/v1/claude" {
		t.Fatalf("AIGatewayURL=%q", req.AIGatewayURL)
	}
	if deployPkg, writeInline, deployConfig, pushSkills, _ := deployer.CallCounts(); deployPkg != 0 || writeInline != 0 || deployConfig != 0 || pushSkills != 0 {
		t.Fatalf("edge worker must skip legacy deploy path, got package=%d inline=%d config=%d skills=%d",
			deployPkg, writeInline, deployConfig, pushSkills)
	}
}

func TestReconcileMemberConfigNonQwenPawKeepsLegacyPath(t *testing.T) {
	deployer := mocks.NewMockDeployer()
	state := &MemberState{
		ProvResult: &service.WorkerProvisionResult{
			MatrixToken:    "matrix-token",
			GatewayKey:     "gateway-key",
			MatrixPassword: "matrix-password",
		},
	}
	member := MemberContext{
		Name:        "worker-a",
		RuntimeName: "worker-a",
		Role:        RoleStandalone,
		Spec: v1beta1.WorkerSpec{
			Runtime: "copaw",
			Package: "file:///tmp/pkg.zip",
			Skills:  []string{"github-operations"},
		},
	}

	if err := ReconcileMemberConfig(context.Background(), MemberDeps{Deployer: deployer}, member, state); err != nil {
		t.Fatalf("ReconcileMemberConfig failed: %v", err)
	}

	if got := len(deployer.Calls.DeployMemberRuntimeConfig); got != 0 {
		t.Fatalf("non-qwenpaw must not write runtime config, got %d calls", got)
	}
	if deployPkg, writeInline, deployConfig, pushSkills, _ := deployer.CallCounts(); deployPkg != 1 || writeInline != 1 || deployConfig != 1 || pushSkills != 1 {
		t.Fatalf("legacy deploy path call counts package=%d inline=%d config=%d skills=%d, want all 1",
			deployPkg, writeInline, deployConfig, pushSkills)
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
