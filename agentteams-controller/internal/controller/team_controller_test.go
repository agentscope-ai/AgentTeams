package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

func newTeamTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newTestManagerConfig(t *testing.T) (*service.ManagerConfigStore, *ossfake.Memory) {
	t.Helper()
	fake := ossfake.NewMemory()
	return service.NewManagerConfigStore(service.ManagerConfigStoreConfig{
		OSS:          fake,
		MatrixDomain: "matrix.local",
		ManagerName:  "manager",
	}), fake
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type teamReconcileRig struct {
	t           *testing.T
	client      client.Client
	deployer    *mocks.MockDeployer
	provisioner *mocks.MockProvisioner
	r           *TeamReconciler
}

func newTeamReconcileRig(t *testing.T, objs ...client.Object) *teamReconcileRig {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Team{}, &v1beta1.Human{}).
		Build()
	deployer := mocks.NewMockDeployer()
	provisioner := mocks.NewMockProvisioner()
	return &teamReconcileRig{
		t:           t,
		client:      c,
		deployer:    deployer,
		provisioner: provisioner,
		r: &TeamReconciler{
			Client:      c,
			Provisioner: provisioner,
			Deployer:    deployer,
		},
	}
}

func (rig *teamReconcileRig) reconcile(name string) (*v1beta1.Team, reconcile.Result, error) {
	rig.t.Helper()
	key := types.NamespacedName{Name: name, Namespace: "default"}
	res, err := rig.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	var out v1beta1.Team
	if getErr := rig.client.Get(context.Background(), key, &out); getErr != nil {
		rig.t.Fatalf("get team after reconcile: %v", getErr)
	}
	return &out, res, err
}

func TestReconcileTeamNormal_FailsEmptyTeamSpec(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-team",
			Namespace: "default",
		},
		Spec: v1beta1.TeamSpec{
			TeamName: "empty-team",
			Admin: &v1beta1.TeamAdminSpec{
				Name:         "admin",
				MatrixUserID: "@admin:localhost",
			},
		},
	}
	rig := newTeamReconcileRig(t, team)

	out, _, err := rig.reconcile("empty-team")
	if err == nil {
		t.Fatal("reconcile succeeded, want empty team spec failure")
	}
	want := "workerMembers must not be empty"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error=%q, want %q", err.Error(), want)
	}
	if out.Status.Phase != "Failed" {
		t.Fatalf("phase=%q, want Failed", out.Status.Phase)
	}
	if out.Status.Message != want {
		t.Fatalf("message=%q, want %q", out.Status.Message, want)
	}
	if len(rig.provisioner.Calls.ProvisionTeamRooms) != 0 {
		t.Fatalf("ProvisionTeamRooms calls=%d, want 0", len(rig.provisioner.Calls.ProvisionTeamRooms))
	}
}

func runtimeConfigCallFor(calls []service.MemberRuntimeConfigDeployRequest, runtimeName string) (service.MemberRuntimeConfigDeployRequest, bool) {
	for _, call := range calls {
		if call.RuntimeName == runtimeName {
			return call, true
		}
	}
	return service.MemberRuntimeConfigDeployRequest{}, false
}

func TestResolveTeamAdminActor_ExternalSSOHumanUsesResolvedIdentity(t *testing.T) {
	issuer := "https://sso.example.com"
	subject := "user-123"
	localpart := testSSOLocalpart(issuer, subject)
	matrixUserID := "@" + localpart + ":localhost"
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			Username: "managerConfig-alice",
			IdentitySource: &v1beta1.IdentitySourceSpec{
				Issuer:  issuer,
				Subject: subject,
			},
		},
		Status: v1beta1.HumanStatus{
			Phase:        "Active",
			MatrixUserID: matrixUserID,
		},
	}
	prov := mocks.NewMockProvisioner()
	prov.AppServiceEnabled = true
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, human),
		Provisioner: prov,
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Admin: &v1beta1.TeamAdminSpec{Name: "alice", MatrixUserID: matrixUserID},
		},
	}

	actor, err := r.resolveTeamAdminActor(context.Background(), team)
	if err != nil {
		t.Fatalf("resolveTeamAdminActor: %v", err)
	}
	if actor.MatrixUserID != matrixUserID {
		t.Fatalf("MatrixUserID=%q, want %q", actor.MatrixUserID, matrixUserID)
	}
	if actor.Username != localpart {
		t.Fatalf("Username=%q, want resolved SSO localpart %q", actor.Username, localpart)
	}
	if actor.Token != "mock-as-token-"+localpart {
		t.Fatalf("Token=%q, want AppService token for resolved SSO localpart", actor.Token)
	}
	if len(prov.Calls.LoginAppServiceUser) != 1 || prov.Calls.LoginAppServiceUser[0] != localpart {
		t.Fatalf("LoginAppServiceUser calls=%v, want [%s]", prov.Calls.LoginAppServiceUser, localpart)
	}
	if len(prov.Calls.LoginAsHuman) != 0 || len(prov.Calls.LoginWithPassword) != 0 {
		t.Fatalf("managerConfig login must not be used for SSO admin, LoginAsHuman=%v LoginWithPassword=%v",
			prov.Calls.LoginAsHuman, prov.Calls.LoginWithPassword)
	}
}

func testSSOLocalpart(issuer, subject string) string {
	digest := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(digest[:16])
}

func TestValidateWorkerMembers(t *testing.T) {
	tests := []struct {
		name        string
		refs        []v1beta1.TeamWorkerRef
		wantErr     string
		wantLeader  string
		wantWorkers int
	}{
		{
			name:    "empty list",
			refs:    nil,
			wantErr: "must not be empty",
		},
		{
			name: "no leader",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "w1"},
				{Name: "w2"},
			},
			wantErr: "must contain exactly one member with role",
		},
		{
			name: "multiple leaders",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "lead1", Role: "team_leader"},
				{Name: "lead2", Role: "team_leader"},
			},
			wantErr: "multiple leaders",
		},
		{
			name: "duplicate name",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "w1"},
				{Name: "w1"},
			},
			wantErr: "duplicate",
		},
		{
			name: "empty name",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: ""},
			},
			wantErr: "must not be empty",
		},
		{
			name: "valid",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "w1"},
				{Name: "w2", Role: "worker"},
			},
			wantLeader:  "lead",
			wantWorkers: 2,
		},
		{
			name: "single leader only",
			refs: []v1beta1.TeamWorkerRef{
				{Name: "solo-lead", Role: "team_leader"},
			},
			wantLeader:  "solo-lead",
			wantWorkers: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leader, workers, err := validateWorkerMembers(tc.refs)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !stringSliceContains([]string{err.Error()}, "") && err.Error() == "" {
					// always true, but let's check contains
				}
				if got := err.Error(); !contains(got, tc.wantErr) {
					t.Fatalf("error=%q, want substring %q", got, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if leader == nil || leader.Name != tc.wantLeader {
				t.Fatalf("leader=%v, want name=%q", leader, tc.wantLeader)
			}
			if len(workers) != tc.wantWorkers {
				t.Fatalf("workers=%d, want %d", len(workers), tc.wantWorkers)
			}
		})
	}
}

func TestReconcileTeamTeamReferences_HappyPath(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			SpecHash:       "leader-hash",
			Phase:          "Running",
			MatrixUserID:   "@lead:matrix.local",
			RoomID:         "!room-lead:matrix.local",
			ContainerState: "running",
			LastHeartbeat:  "2026-06-06T03:00:00Z",
			LastActiveAt:   "2026-06-06T02:59:00Z",
		},
	}
	worker1 := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			SpecHash:       "dev-hash",
			Phase:          "Running",
			MatrixUserID:   "@dev:matrix.local",
			RoomID:         "!room-dev:matrix.local",
			ContainerState: "ready",
			LastHeartbeat:  "2026-06-06T03:01:00Z",
			LastActiveAt:   "2026-06-06T02:58:00Z",
			Message:        "worker detail",
			ExposedPorts: []v1beta1.ExposedPortStatus{
				{Port: 8080, Domain: "dev.example.com"},
			},
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), worker1.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	deployer := mocks.NewMockDeployer()
	prov := mocks.NewMockProvisioner()

	r := &TeamReconciler{
		Client:      c,
		Provisioner: prov,
		Deployer:    deployer,
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	team.Status.Phase = "Pending"
	if err := c.Status().Patch(ctx, team, patchBase); err != nil {
		t.Fatalf("init status: %v", err)
	}

	patchBase = client.MergeFrom(team.DeepCopy())
	result, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}
	if result.RequeueAfter != reconcileInterval {
		t.Errorf("RequeueAfter=%v, want %v", result.RequeueAfter, reconcileInterval)
	}
	if team.Status.Phase != "Active" {
		t.Errorf("Phase=%q, want Active", team.Status.Phase)
	}
	if !team.Status.LeaderReady {
		t.Errorf("LeaderReady=false, want true")
	}
	if team.Status.ReadyWorkers != 1 {
		t.Errorf("ReadyWorkers=%d, want 1", team.Status.ReadyWorkers)
	}
	if team.Status.TotalWorkers != 1 {
		t.Errorf("TotalWorkers=%d, want 1", team.Status.TotalWorkers)
	}
	leaderStatus := team.Status.MemberByName("lead")
	if leaderStatus == nil {
		t.Fatal("leader member status missing")
	}
	if leaderStatus.Phase != "Running" || leaderStatus.ContainerState != "running" || leaderStatus.SpecHash != "leader-hash" {
		t.Fatalf("leader status = %+v, want synced phase/container/specHash", leaderStatus)
	}
	if leaderStatus.LastHeartbeat != "2026-06-06T03:00:00Z" || leaderStatus.LastActiveAt != "2026-06-06T02:59:00Z" {
		t.Fatalf("leader heartbeat/active = %q/%q, want Worker status values", leaderStatus.LastHeartbeat, leaderStatus.LastActiveAt)
	}
	devStatus := team.Status.MemberByName("dev")
	if devStatus == nil {
		t.Fatal("dev member status missing")
	}
	if devStatus.Phase != "Running" || devStatus.ContainerState != "ready" || devStatus.Message != "worker detail" {
		t.Fatalf("dev status = %+v, want synced phase/container/message", devStatus)
	}
	if devStatus.SpecHash != "dev-hash" || devStatus.LastHeartbeat != "2026-06-06T03:01:00Z" || devStatus.LastActiveAt != "2026-06-06T02:58:00Z" {
		t.Fatalf("dev hash/heartbeat/active = %q/%q/%q, want Worker status values", devStatus.SpecHash, devStatus.LastHeartbeat, devStatus.LastActiveAt)
	}
	if len(devStatus.ExposedPorts) != 1 || devStatus.ExposedPorts[0].Port != 8080 || devStatus.ExposedPorts[0].Domain != "dev.example.com" {
		t.Fatalf("dev exposed ports = %+v, want Worker exposed ports", devStatus.ExposedPorts)
	}

	// Verify coordination was injected for the leader
	if len(deployer.Calls.InjectCoordinationContext) != 1 {
		t.Fatalf("InjectCoordinationContext calls=%d, want 1", len(deployer.Calls.InjectCoordinationContext))
	}
	coordReq := deployer.Calls.InjectCoordinationContext[0]
	if coordReq.LeaderName != "lead" {
		t.Errorf("coord LeaderName=%q, want lead", coordReq.LeaderName)
	}

	// Verify worker coordination was injected
	if len(deployer.Calls.InjectWorkerCoordination) != 1 {
		t.Fatalf("InjectWorkerCoordination calls=%d, want 1", len(deployer.Calls.InjectWorkerCoordination))
	}
	workerCoord := deployer.Calls.InjectWorkerCoordination[0]
	if workerCoord.WorkerName != "dev" {
		t.Errorf("workerCoord WorkerName=%q, want dev", workerCoord.WorkerName)
	}
	if workerCoord.TeamLeaderName != "lead" {
		t.Errorf("workerCoord TeamLeaderName=%q, want lead", workerCoord.TeamLeaderName)
	}
}

func TestReconcileTeamTeamReferences_QwenPawProjectsRuntimeRoster(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	worker1 := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{PermissionLevel: 1},
		Status: v1beta1.HumanStatus{
			Phase:           "Active",
			MatrixUserID:    "@admin:localhost",
			InitialPassword: "pw",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Admin:        &v1beta1.TeamAdminSpec{Name: "admin", MatrixUserID: "@admin:localhost"},
			HumanMembers: []v1beta1.TeamMemberSpec{{Name: "human-coord", MatrixUserID: "@human:matrix.local"}},
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), worker1.DeepCopy(), admin.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}, &v1beta1.Human{}).
		Build()

	managerConfig, _ := newTestManagerConfig(t)
	deployer := mocks.NewMockDeployer()
	r := &TeamReconciler{
		Client:        c,
		Provisioner:   mocks.NewMockProvisioner(),
		Deployer:      deployer,
		ManagerConfig: managerConfig,
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	if _, err := r.reconcileTeam(ctx, team, patchBase); err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}

	leaderReq, ok := runtimeConfigCallFor(deployer.Calls.DeployMemberRuntimeConfig, "lead")
	if !ok {
		t.Fatalf("missing leader runtime config: %#v", deployer.Calls.DeployMemberRuntimeConfig)
	}
	devReq, ok := runtimeConfigCallFor(deployer.Calls.DeployMemberRuntimeConfig, "dev")
	if !ok {
		t.Fatalf("missing dev runtime config: %#v", deployer.Calls.DeployMemberRuntimeConfig)
	}
	if leaderReq.Role != "team_leader" || devReq.Role != "worker" {
		t.Fatalf("roles = leader %q dev %q", leaderReq.Role, devReq.Role)
	}
	if leaderReq.TeamRoomID == "" || leaderReq.LeaderDMRoomID == "" {
		t.Fatalf("leader runtime config missing rooms: %#v", leaderReq)
	}
	roster := map[string]service.RuntimeConfigTeamMember{}
	for _, member := range leaderReq.TeamMembers {
		roster[member.Name] = member
	}
	if got := roster["lead"].MatrixUserID; got != "@lead:matrix.local" {
		t.Fatalf("leader roster matrixUserId=%q", got)
	}
	if got := roster["dev"].PersonalRoomID; got != "!room-dev:matrix.local" {
		t.Fatalf("dev roster personalRoomId=%q", got)
	}
	if got := roster["human-coord"].MatrixUserID; got != "@human:matrix.local" {
		t.Fatalf("human roster matrixUserId=%q", got)
	}
	if len(devReq.TeamMembers) != len(leaderReq.TeamMembers) {
		t.Fatalf("leader/dev roster sizes differ: %d vs %d", len(leaderReq.TeamMembers), len(devReq.TeamMembers))
	}
	if got := len(deployer.Calls.SyncTeamLeaderAssets); got != 0 {
		t.Fatalf("qwenpaw SyncTeamLeaderAssets calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectCoordinationContext); got != 0 {
		t.Fatalf("qwenpaw InjectCoordinationContext calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectWorkerCoordination); got != 0 {
		t.Fatalf("qwenpaw InjectWorkerCoordination calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectHeartbeatConfig); got != 0 {
		t.Fatalf("qwenpaw InjectHeartbeatConfig calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectChannelPolicy); got != 0 {
		t.Fatalf("qwenpaw InjectChannelPolicy calls=%d, want 0", got)
	}
}

func TestReconcileTeamTeamReferences_SyncsAccessibleTeamHumanStatus(t *testing.T) {
	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	worker1 := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{AccessibleTeams: []string{"team-a"}},
		Status: v1beta1.HumanStatus{
			Phase:        "Active",
			MatrixUserID: "@alice:matrix.local",
		},
	}
	bob := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"},
		Status: v1beta1.HumanStatus{
			Phase:        "Active",
			MatrixUserID: "@bob:matrix.local",
			Rooms:        []string{"!team-team-a:localhost", "!room-bob:matrix.local"},
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}
	rig := newTeamReconcileRig(t, team, leaderWorker, worker1, alice, bob)

	if _, _, err := rig.reconcile("team-a"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rig.provisioner.Calls.ProvisionTeamRooms) != 1 {
		t.Fatalf("ProvisionTeamRooms calls=%d, want 1", len(rig.provisioner.Calls.ProvisionTeamRooms))
	}
	members := rig.provisioner.Calls.ProvisionTeamRooms[0].HumanMembers
	if len(members) != 1 || members[0].Name != "alice" || members[0].MatrixUserID != "@alice:matrix.local" {
		t.Fatalf("HumanMembers=%+v, want alice from Human.spec.accessibleTeams", members)
	}

	var aliceOut v1beta1.Human
	if err := rig.client.Get(context.Background(), types.NamespacedName{Name: "alice", Namespace: "default"}, &aliceOut); err != nil {
		t.Fatalf("get alice: %v", err)
	}
	if !stringSliceContains(aliceOut.Status.Rooms, "!team-team-a:localhost") {
		t.Fatalf("alice Status.Rooms=%v, want team room", aliceOut.Status.Rooms)
	}

	var bobOut v1beta1.Human
	if err := rig.client.Get(context.Background(), types.NamespacedName{Name: "bob", Namespace: "default"}, &bobOut); err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if stringSliceContains(bobOut.Status.Rooms, "!team-team-a:localhost") {
		t.Fatalf("bob Status.Rooms=%v, want stale team room removed", bobOut.Status.Rooms)
	}
	if !stringSliceContains(bobOut.Status.Rooms, "!room-bob:matrix.local") {
		t.Fatalf("bob Status.Rooms=%v, want unrelated room preserved", bobOut.Status.Rooms)
	}
}

func TestReconcileTeamTeamReferences_EdgeMergesRuntimeTeamContext(t *testing.T) {
	ctx := context.Background()
	edgeMode := v1beta1.DeployModeEdge

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	edgeWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-worker-cr", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			WorkerName: "edge-01",
			DeployMode: &edgeMode,
			Model:      "claude-sonnet-4",
		},
		Status: v1beta1.WorkerStatus{
			Phase:        "Pending",
			MatrixUserID: "@edge-01:matrix.local",
			RoomID:       "!room-edge-01:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "edge-worker-cr", Role: "worker"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), edgeWorker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	deployer := mocks.NewMockDeployer()
	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    deployer,
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	if _, err := r.reconcileTeam(ctx, team, patchBase); err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}

	if _, ok := runtimeConfigCallFor(deployer.Calls.DeployMemberRuntimeConfig, "edge-01"); ok {
		t.Fatalf("edge worker must not receive full runtime config overwrite: %#v", deployer.Calls.DeployMemberRuntimeConfig)
	}
	if got := len(deployer.Calls.MergeMemberRuntimeTeamContext); got != 1 {
		t.Fatalf("MergeMemberRuntimeTeamContext calls=%d, want 1: %#v", got, deployer.Calls.MergeMemberRuntimeTeamContext)
	}
	req := deployer.Calls.MergeMemberRuntimeTeamContext[0]
	if req.Name != "edge-worker-cr" || req.RuntimeName != "edge-01" {
		t.Fatalf("unexpected edge merge identity: %#v", req)
	}
	if req.Runtime != runtimeRemoteManagedLocal {
		t.Fatalf("edge merge runtime=%q, want %q", req.Runtime, runtimeRemoteManagedLocal)
	}
	if req.Role != "worker" {
		t.Fatalf("edge merge role=%q, want worker", req.Role)
	}
	if req.TeamRoomID == "" || req.LeaderDMRoomID == "" {
		t.Fatalf("edge merge missing team rooms: %#v", req)
	}
	roster := map[string]service.RuntimeConfigTeamMember{}
	for _, member := range req.TeamMembers {
		roster[member.Name] = member
	}
	if got := roster["edge-worker-cr"].PersonalRoomID; got != "!room-edge-01:matrix.local" {
		t.Fatalf("edge roster personalRoomId=%q", got)
	}
	if got := roster["lead"].RuntimeName; got != "lead" {
		t.Fatalf("leader roster runtimeName=%q", got)
	}
}

func TestReconcileTeamTeamReferences_WorkerNotFound(t *testing.T) {
	ctx := context.Background()

	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "ghost"},
			},
		},
	}
	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    mocks.NewMockDeployer(),
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	_, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if team.Status.Phase != "Degraded" {
		t.Errorf("Phase=%q, want Degraded", team.Status.Phase)
	}
	if !contains(team.Status.Message, "ghost") {
		t.Errorf("Message=%q, want mention of 'ghost'", team.Status.Message)
	}
}

func TestReconcileTeamTeamReferences_RoleAwareChannelPolicy(t *testing.T) {
	ctx := context.Background()
	managerConfig, _ := newTestManagerConfig(t)

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen", Runtime: "copaw"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	worker1 := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen",
			ChannelPolicy: &v1beta1.ChannelPolicySpec{
				GroupDenyExtra: []string{"qa"},
			},
		},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	worker2 := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@qa:matrix.local",
			RoomID:       "!room-qa:matrix.local",
		},
	}
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Alice",
			PermissionLevel: 2,
		},
		Status: v1beta1.HumanStatus{
			Phase:           "Active",
			MatrixUserID:    "@alice:matrix.local",
			InitialPassword: "pw",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Admin: &v1beta1.TeamAdminSpec{
				Name:         "alice",
				MatrixUserID: "@alice:matrix.local",
			},
			ChannelPolicy: &v1beta1.ChannelPolicySpec{
				GroupAllowExtra: []string{"external-bot"},
			},
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
				{Name: "qa"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), worker1.DeepCopy(), worker2.DeepCopy(), admin.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	deployer := mocks.NewMockDeployer()
	provisioner := mocks.NewMockProvisioner()
	provisioner.MatrixUserIDFn = func(name string) string {
		return "@" + name + ":matrix.local"
	}
	r := &TeamReconciler{
		Client:        c,
		Provisioner:   provisioner,
		Deployer:      deployer,
		ManagerConfig: managerConfig,
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	if _, err := r.reconcileTeam(ctx, team, patchBase); err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}

	if len(deployer.Calls.SyncTeamLeaderAssets) != 1 {
		t.Fatalf("SyncTeamLeaderAssets calls=%d, want 1", len(deployer.Calls.SyncTeamLeaderAssets))
	}
	if got := deployer.Calls.SyncTeamLeaderAssets[0].WorkerName; got != "lead" {
		t.Fatalf("SyncTeamLeaderAssets WorkerName=%q, want lead", got)
	}

	if got := provisioner.Calls.KickFromRoom; len(got) != 2 {
		t.Fatalf("KickFromRoom calls=%+v, want regular worker personal rooms only", got)
	} else {
		kickedRooms := map[string]string{}
		for _, call := range got {
			kickedRooms[call.RoomID] = call.UserID
		}
		for _, roomID := range []string{"!room-dev:matrix.local", "!room-qa:matrix.local"} {
			if kickedRooms[roomID] != "@manager:matrix.local" {
				t.Errorf("KickFromRoom[%q]=%q, want @manager:matrix.local", roomID, kickedRooms[roomID])
			}
		}
		if _, kicked := kickedRooms["!room-lead:matrix.local"]; kicked {
			t.Errorf("Team Leader personal room must retain Manager membership")
		}
	}

	policies := map[string]service.InjectChannelPolicyRequest{}
	for _, call := range deployer.Calls.InjectChannelPolicy {
		policies[call.WorkerName] = call
	}
	leaderPolicy := policies["lead"]
	if !stringSliceContains(leaderPolicy.GroupAllowFrom, "@manager:matrix.local") {
		t.Errorf("leader groupAllowFrom=%v, want manager", leaderPolicy.GroupAllowFrom)
	}
	if !stringSliceContains(leaderPolicy.GroupAllowFrom, "@dev:matrix.local") ||
		!stringSliceContains(leaderPolicy.GroupAllowFrom, "@qa:matrix.local") {
		t.Errorf("leader groupAllowFrom=%v, want both workers", leaderPolicy.GroupAllowFrom)
	}
	if !stringSliceContains(leaderPolicy.GroupAllowFrom, "@alice:matrix.local") ||
		!stringSliceContains(leaderPolicy.DMAllowFrom, "@alice:matrix.local") {
		t.Errorf("leader policy=%+v, want team admin in group and dm", leaderPolicy)
	}
	if !stringSliceContains(leaderPolicy.GroupAllowFrom, "@external-bot:matrix.local") {
		t.Errorf("leader groupAllowFrom=%v, want team-level external bot", leaderPolicy.GroupAllowFrom)
	}

	devPolicy := policies["dev"]
	if !stringSliceContains(devPolicy.GroupAllowFrom, "@lead:matrix.local") {
		t.Errorf("dev groupAllowFrom=%v, want leader", devPolicy.GroupAllowFrom)
	}
	if stringSliceContains(devPolicy.GroupAllowFrom, "@manager:matrix.local") {
		t.Errorf("dev groupAllowFrom=%v, must not include manager", devPolicy.GroupAllowFrom)
	}
	if stringSliceContains(devPolicy.GroupAllowFrom, "@qa:matrix.local") {
		t.Errorf("dev groupAllowFrom=%v, must not include denied peer qa", devPolicy.GroupAllowFrom)
	}
	if !stringSliceContains(devPolicy.GroupAllowFrom, "@external-bot:matrix.local") {
		t.Errorf("dev groupAllowFrom=%v, want team-level external bot", devPolicy.GroupAllowFrom)
	}

	qaPolicy := policies["qa"]
	if !stringSliceContains(qaPolicy.GroupAllowFrom, "@dev:matrix.local") {
		t.Errorf("qa groupAllowFrom=%v, want peer dev", qaPolicy.GroupAllowFrom)
	}
}

func TestReconcileTeamTeamReferences_WorkerNotProvisionedKeepsTeamActive(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	// Worker exists but has no MatrixUserID (not yet provisioned)
	unprovisionedWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status:     v1beta1.WorkerStatus{Phase: "Pending"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), unprovisionedWorker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    mocks.NewMockDeployer(),
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	result, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if team.Status.Phase != "Active" {
		t.Errorf("Phase=%q, want Active", team.Status.Phase)
	}
	if result.RequeueAfter != reconcileInterval {
		t.Errorf("RequeueAfter=%v, want %v", result.RequeueAfter, reconcileInterval)
	}
	if !team.Status.LeaderReady {
		t.Errorf("LeaderReady=false, want true")
	}
	if team.Status.ReadyWorkers != 0 {
		t.Errorf("ReadyWorkers=%d, want 0", team.Status.ReadyWorkers)
	}
	ms := team.Status.MemberByName("dev")
	if ms == nil {
		t.Fatalf("missing dev member status")
	}
	if ms.Ready {
		t.Errorf("dev Ready=true, want false")
	}
}

func TestReconcileTeamTeamReferences_WorkerRuntimePendingKeepsTeamActive(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Pending",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Pending",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy(), worker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    mocks.NewMockDeployer(),
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	_, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}
	if team.Status.Phase != "Active" {
		t.Errorf("Phase=%q, want Active", team.Status.Phase)
	}
	if team.Status.LeaderReady {
		t.Errorf("LeaderReady=true, want false")
	}
	if team.Status.ReadyWorkers != 0 {
		t.Errorf("ReadyWorkers=%d, want 0", team.Status.ReadyWorkers)
	}
}

func TestReconcileTeamTeamReferences_MemberRemoved(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
			},
		},
		Status: v1beta1.TeamStatus{
			Phase: "Active",
			Members: []v1beta1.TeamMemberStatus{
				{Name: "lead", Role: "team_leader", MatrixUserID: "@lead:matrix.local", Observed: true},
				{Name: "removed-worker", Role: "worker", MatrixUserID: "@removed:matrix.local", Observed: true},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    mocks.NewMockDeployer(),
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	_, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}

	// "removed-worker" should have been pruned from Status.Members
	if ms := team.Status.MemberByName("removed-worker"); ms != nil {
		t.Errorf("removed-worker should have been pruned from Status.Members, still present: %+v", ms)
	}
	if ms := team.Status.MemberByName("lead"); ms == nil {
		t.Errorf("lead should still be in Status.Members")
	}
}

func TestHandleDeleteTeamReferencesResetsChannelPolicyAndArchivesRoomsWithTeamAdmin(t *testing.T) {
	ctx := context.Background()
	managerConfig, _ := newTestManagerConfig(t)

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status:     v1beta1.WorkerStatus{RoomID: "!room-lead:matrix.local"},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status:     v1beta1.WorkerStatus{RoomID: "!room-dev:matrix.local"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Admin: &v1beta1.TeamAdminSpec{Name: "alice"},
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team-room:matrix.local",
			LeaderDMRoomID: "!leader-dm:matrix.local",
		},
	}
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Status:     v1beta1.HumanStatus{InitialPassword: "alice-password"},
	}

	deployer := mocks.NewMockDeployer()
	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:        newTeamTestClient(t, team.DeepCopy(), leaderWorker.DeepCopy(), worker.DeepCopy(), admin.DeepCopy()),
		Provisioner:   provisioner,
		Deployer:      deployer,
		ManagerConfig: managerConfig,
	}

	if err := r.handleDeleteTeam(ctx, team); err != nil {
		t.Fatalf("handleDeleteTeam: %v", err)
	}

	policies := map[string]service.InjectChannelPolicyRequest{}
	for _, call := range deployer.Calls.InjectChannelPolicy {
		policies[call.WorkerName] = call
	}
	for _, workerName := range []string{"lead", "dev"} {
		policy, ok := policies[workerName]
		if !ok {
			t.Fatalf("missing channel policy reset for %s; calls=%+v", workerName, deployer.Calls.InjectChannelPolicy)
		}
		if len(policy.GroupAllowFrom) != 1 || policy.GroupAllowFrom[0] != "@manager:matrix.local" {
			t.Fatalf("%s groupAllowFrom=%v, want [@manager:matrix.local]", workerName, policy.GroupAllowFrom)
		}
		if len(policy.DMAllowFrom) != 1 || policy.DMAllowFrom[0] != "@manager:matrix.local" {
			t.Fatalf("%s dmAllowFrom=%v, want [@manager:matrix.local]", workerName, policy.DMAllowFrom)
		}
	}

	for _, workerName := range []string{"lead", "dev"} {
		req, ok := runtimeConfigCallFor(deployer.Calls.DeployMemberRuntimeConfig, workerName)
		if !ok {
			t.Fatalf("missing runtime config reset for %s; calls=%+v", workerName, deployer.Calls.DeployMemberRuntimeConfig)
		}
		if !req.DropTeamContext {
			t.Fatalf("%s DropTeamContext=false, want true", workerName)
		}
		if req.Role != RoleStandalone.String() {
			t.Fatalf("%s role=%q, want standalone", workerName, req.Role)
		}
	}

	if got := provisioner.Calls.InviteToRoom; len(got) != 2 {
		t.Fatalf("InviteToRoom calls=%+v, want Manager restored to both standalone worker rooms", got)
	} else {
		invitedRooms := map[string]string{}
		for _, call := range got {
			invitedRooms[call.RoomID] = call.UserID
		}
		for _, roomID := range []string{"!room-lead:matrix.local", "!room-dev:matrix.local"} {
			if invitedRooms[roomID] != "@manager:matrix.local" {
				t.Errorf("InviteToRoom[%q]=%q, want @manager:matrix.local", roomID, invitedRooms[roomID])
			}
		}
	}

	if got := provisioner.Calls.ArchiveTeamRooms; len(got) != 1 {
		t.Fatalf("ArchiveTeamRooms calls=%v, want one call", got)
	} else {
		req := got[0]
		if req.TeamName != "team-a" || req.LeaderName != "lead" ||
			req.TeamRoomID != "!team-room:matrix.local" || req.LeaderDMRoomID != "!leader-dm:matrix.local" {
			t.Fatalf("ArchiveTeamRooms request=%+v", req)
		}
		if req.ActorToken != "mock-pw-token-alice" {
			t.Fatalf("ArchiveTeamRooms ActorToken=%q, want team admin token", req.ActorToken)
		}
	}
}

func TestHandleDeleteTeamReferencesSkipsQwenPawLegacyAssets(t *testing.T) {
	ctx := context.Background()
	managerConfig, _ := newTestManagerConfig(t)

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Runtime: "qwenpaw", Model: "qwen"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team-room:matrix.local",
			LeaderDMRoomID: "!leader-dm:matrix.local",
		},
	}

	deployer := mocks.NewMockDeployer()
	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:        newTeamTestClient(t, team.DeepCopy(), leaderWorker.DeepCopy(), worker.DeepCopy()),
		Provisioner:   provisioner,
		Deployer:      deployer,
		ManagerConfig: managerConfig,
	}

	if err := r.handleDeleteTeam(ctx, team); err != nil {
		t.Fatalf("handleDeleteTeam: %v", err)
	}

	for _, workerName := range []string{"lead", "dev"} {
		req, ok := runtimeConfigCallFor(deployer.Calls.DeployMemberRuntimeConfig, workerName)
		if !ok {
			t.Fatalf("missing runtime config reset for %s; calls=%+v", workerName, deployer.Calls.DeployMemberRuntimeConfig)
		}
		if !req.DropTeamContext {
			t.Fatalf("%s DropTeamContext=false, want true", workerName)
		}
	}
	if got := len(deployer.Calls.InjectWorkerCoordination); got != 0 {
		t.Fatalf("qwenpaw InjectWorkerCoordination calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectHeartbeatConfig); got != 0 {
		t.Fatalf("qwenpaw InjectHeartbeatConfig calls=%d, want 0", got)
	}
	if got := len(deployer.Calls.InjectChannelPolicy); got != 0 {
		t.Fatalf("qwenpaw InjectChannelPolicy calls=%d, want 0", got)
	}
	if got := provisioner.Calls.ArchiveTeamRooms; len(got) != 1 {
		t.Fatalf("ArchiveTeamRooms calls=%v, want one call", got)
	}
}

func TestHandleDeleteTeamReferencesArchivesRoomsWithoutTeamAdmin(t *testing.T) {
	ctx := context.Background()
	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead", Role: "team_leader"}},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team-room:matrix.local",
			LeaderDMRoomID: "!leader-dm:matrix.local",
		},
	}

	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, team.DeepCopy(), leaderWorker.DeepCopy()),
		Provisioner: provisioner,
		Deployer:    mocks.NewMockDeployer(),
	}

	if err := r.handleDeleteTeam(ctx, team); err != nil {
		t.Fatalf("handleDeleteTeam: %v", err)
	}
	if got := provisioner.Calls.ArchiveTeamRooms; len(got) != 1 {
		t.Fatalf("ArchiveTeamRooms calls=%v, want one call", got)
	} else if got[0].ActorToken != "" {
		t.Fatalf("ArchiveTeamRooms ActorToken=%q, want empty fallback token", got[0].ActorToken)
	}
	if got := provisioner.Calls.LoginAsHuman; len(got) != 0 {
		t.Fatalf("LoginAsHuman calls=%v, want none", got)
	}
}

func TestHandleDeleteTeamReferencesUsesStatusRuntimeNameWhenLeaderWorkerMissing(t *testing.T) {
	ctx := context.Background()
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead-cr", Role: "team_leader"}},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team-room:matrix.local",
			LeaderDMRoomID: "!leader-dm:matrix.local",
			Members: []v1beta1.TeamMemberStatus{{
				Name:        "lead-cr",
				RuntimeName: "lead-runtime",
				Role:        RoleTeamLeader.String(),
			}},
		},
	}

	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, team.DeepCopy()),
		Provisioner: provisioner,
		Deployer:    mocks.NewMockDeployer(),
	}

	if err := r.handleDeleteTeam(ctx, team); err != nil {
		t.Fatalf("handleDeleteTeam: %v", err)
	}
	if got := provisioner.Calls.ArchiveTeamRooms; len(got) != 1 {
		t.Fatalf("ArchiveTeamRooms calls=%v, want one call", got)
	} else if got[0].LeaderName != "lead-runtime" {
		t.Fatalf("ArchiveTeamRooms LeaderName=%q, want lead-runtime", got[0].LeaderName)
	}
	if got, want := provisioner.Calls.DeleteTeamRoomAliases, []string{"team-a/lead-runtime"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("DeleteTeamRoomAliases calls=%v, want %v", got, want)
	}
}

func TestHandleDeleteTeamReferencesPrefersCurrentLeaderStatusByName(t *testing.T) {
	ctx := context.Background()
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "lead-b", Role: "team_leader"}},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID:     "!team-room:matrix.local",
			LeaderDMRoomID: "!leader-dm:matrix.local",
			Members: []v1beta1.TeamMemberStatus{
				{
					Name:        "lead-a",
					RuntimeName: "lead-a-runtime",
					Role:        RoleTeamLeader.String(),
				},
				{
					Name:        "lead-b",
					RuntimeName: "lead-b-runtime",
					Role:        "worker",
				},
			},
		},
	}

	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, team.DeepCopy()),
		Provisioner: provisioner,
		Deployer:    mocks.NewMockDeployer(),
	}

	if err := r.handleDeleteTeam(ctx, team); err != nil {
		t.Fatalf("handleDeleteTeam: %v", err)
	}
	if got := provisioner.Calls.ArchiveTeamRooms; len(got) != 1 {
		t.Fatalf("ArchiveTeamRooms calls=%v, want one call", got)
	} else if got[0].LeaderName != "lead-b-runtime" {
		t.Fatalf("ArchiveTeamRooms LeaderName=%q, want lead-b-runtime", got[0].LeaderName)
	}
	if got, want := provisioner.Calls.DeleteTeamRoomAliases, []string{"team-a/lead-b-runtime"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("DeleteTeamRoomAliases calls=%v, want %v", got, want)
	}
}

func TestReconcileTeamTeamReferences_HeartbeatFromTeamCR(t *testing.T) {
	ctx := context.Background()

	leaderWorker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@lead:matrix.local",
			RoomID:       "!room-lead:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			HeartbeatEvery: "30m",
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy(), leaderWorker.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	deployer := mocks.NewMockDeployer()
	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    deployer,
	}

	patchBase := client.MergeFrom(team.DeepCopy())
	_, err := r.reconcileTeam(ctx, team, patchBase)
	if err != nil {
		t.Fatalf("reconcileTeam: %v", err)
	}

	// Verify heartbeat was injected into coordination context
	if len(deployer.Calls.InjectCoordinationContext) != 1 {
		t.Fatalf("InjectCoordinationContext calls=%d, want 1", len(deployer.Calls.InjectCoordinationContext))
	}
	coordReq := deployer.Calls.InjectCoordinationContext[0]
	if coordReq.HeartbeatEvery != "30m" {
		t.Errorf("coord HeartbeatEvery=%q, want 30m", coordReq.HeartbeatEvery)
	}

	// Verify InjectHeartbeatConfig was called
	if len(deployer.Calls.InjectHeartbeatConfig) != 1 {
		t.Fatalf("InjectHeartbeatConfig calls=%d, want 1", len(deployer.Calls.InjectHeartbeatConfig))
	}
	hbReq := deployer.Calls.InjectHeartbeatConfig[0]
	if !hbReq.Enabled {
		t.Errorf("heartbeat Enabled=false, want true")
	}
	if hbReq.Every != "30m" {
		t.Errorf("heartbeat Every=%q, want 30m", hbReq.Every)
	}
	if hbReq.WorkerName != "lead" {
		t.Errorf("heartbeat WorkerName=%q, want lead", hbReq.WorkerName)
	}
}

func TestWorkerToTeamMapFunc(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: "team_leader"},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy()).
		WithIndex(&v1beta1.Team{}, TeamWorkerMembersField, func(obj client.Object) []string {
			tm, ok := obj.(*v1beta1.Team)
			if !ok {
				return nil
			}
			names := make([]string, 0, len(tm.Spec.WorkerMembers))
			for _, ref := range tm.Spec.WorkerMembers {
				if ref.Name != "" {
					names = append(names, ref.Name)
				}
			}
			return names
		}).
		Build()

	r := &TeamReconciler{Client: c}

	// Worker "dev" should map to team-a
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
	}
	reqs := r.workerToTeamRequests(context.Background(), worker)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d: %v", len(reqs), reqs)
	}
	if reqs[0].Name != "team-a" {
		t.Errorf("request Name=%q, want team-a", reqs[0].Name)
	}

	// Worker "unknown" should map to nothing
	unknown := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown", Namespace: "default"},
	}
	reqs = r.workerToTeamRequests(context.Background(), unknown)
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for unknown worker, got %d: %v", len(reqs), reqs)
	}
}

func TestWorkerStatusChangePredicateTriggersOnWorkerSpecChange(t *testing.T) {
	p := workerStatusChangePredicate()
	oldW := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Generation: 1},
		Status: v1beta1.WorkerStatus{
			ObservedGeneration: 1,
			Phase:              "Running",
			MatrixUserID:       "@dev:matrix.local",
			RoomID:             "!room-dev:matrix.local",
		},
	}
	newW := oldW.DeepCopy()
	newW.Generation = 2

	if !p.Update(event.UpdateEvent{ObjectOld: oldW, ObjectNew: newW}) {
		t.Fatalf("worker spec/generation change must enqueue owning Team so team-reference channelPolicy overlays are recalculated")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && searchSubstring(s, substr)))
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// lastLoginAsHuman returns the most recent LoginAsHuman call recorded by the
// mock, failing the test when none were made.
func lastLoginWithPassword(t *testing.T, p *mocks.MockProvisioner) (username, password string) {
	t.Helper()
	calls := p.Calls.LoginWithPassword
	if len(calls) == 0 {
		t.Fatal("expected a LoginWithPassword call, got none")
	}
	last := calls[len(calls)-1]
	return last.Username, last.Password
}

// TestResolveTeamAdminActor_SSOUsesStatusMatrixID verifies that an SSO team
// admin is authenticated by the hashed localpart from Status.MatrixUserID,
// not by the spec username. Before the fix the controller derived
// "@<name>:domain" and logged in a phantom AppService user.
func TestResolveTeamAdminActor_SSOUsesStatusMatrixID(t *testing.T) {
	ctx := context.Background()
	issuer := "https://idp.example.com"
	subject := "alice-sub"
	localpart := testSSOLocalpart(issuer, subject)
	matrixUserID := "@" + localpart + ":localhost"
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			PermissionLevel: 1,
			IdentitySource:  &v1beta1.IdentitySourceSpec{Issuer: issuer, Subject: subject},
		},
		Status: v1beta1.HumanStatus{
			Phase:        "Active",
			MatrixUserID: matrixUserID,
			// SSO Humans have no initial password.
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Admin: &v1beta1.TeamAdminSpec{Name: "alice", MatrixUserID: matrixUserID},
		},
	}

	provisioner := mocks.NewMockProvisioner()
	provisioner.AppServiceEnabled = true
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, admin.DeepCopy()),
		Provisioner: provisioner,
	}

	actor, err := r.resolveTeamAdminActor(ctx, team)
	if err != nil {
		t.Fatalf("resolveTeamAdminActor: %v", err)
	}
	if actor.MatrixUserID != matrixUserID {
		t.Fatalf("actor.MatrixUserID = %q, want %q", actor.MatrixUserID, matrixUserID)
	}
	if actor.Username != localpart {
		t.Fatalf("actor.Username = %q, want %q (hashed localpart)", actor.Username, localpart)
	}
	if len(provisioner.Calls.LoginAppServiceUser) != 1 || provisioner.Calls.LoginAppServiceUser[0] != localpart {
		t.Fatalf("LoginAppServiceUser calls = %v, want [%s]", provisioner.Calls.LoginAppServiceUser, localpart)
	}
}

// TestResolveTeamAdminActor_LegacyUnchanged is a regression guard: a
// password-authenticated admin without Status.MatrixUserID still derives the
// username-based ID and logs in with the stored initial password, exactly as
// before the SSO fix.
func TestResolveTeamAdminActor_LegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{PermissionLevel: 1},
		Status:     v1beta1.HumanStatus{InitialPassword: "stored-pw"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "admin"}},
	}

	provisioner := mocks.NewMockProvisioner()
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, admin.DeepCopy()),
		Provisioner: provisioner,
	}

	actor, err := r.resolveTeamAdminActor(ctx, team)
	if err != nil {
		t.Fatalf("resolveTeamAdminActor: %v", err)
	}
	if actor.MatrixUserID != "@admin:localhost" {
		t.Fatalf("actor.MatrixUserID = %q, want @admin:localhost", actor.MatrixUserID)
	}
	user, pw := lastLoginWithPassword(t, provisioner)
	if user != "admin" || pw != "stored-pw" {
		t.Fatalf("LoginWithPassword = (%q,%q), want (admin,stored-pw)", user, pw)
	}
}

// TestResolveTeamAdminActor_SSONotProvisionedErrors verifies that an SSO admin
// without a provisioned Matrix account is rejected instead of being resolved
// to the wrong "@username" identity.
func TestResolveTeamAdminActor_SSONotProvisionedErrors(t *testing.T) {
	ctx := context.Background()
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			PermissionLevel: 1,
			IdentitySource:  &v1beta1.IdentitySourceSpec{Issuer: "https://idp.example.com", Subject: "alice-sub"},
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "alice"}},
	}

	provisioner := mocks.NewMockProvisioner()
	provisioner.AppServiceEnabled = true
	r := &TeamReconciler{
		Client:      newTeamTestClient(t, admin.DeepCopy()),
		Provisioner: provisioner,
	}

	if _, err := r.resolveTeamAdminActor(ctx, team); err == nil {
		t.Fatal("expected error for unprovisioned SSO admin, got nil")
	}
	if len(provisioner.Calls.LoginAsHuman) != 0 {
		t.Fatalf("LoginAsHuman calls = %v, want none for unprovisioned SSO admin", provisioner.Calls.LoginAsHuman)
	}
}

// TestResolveTeamAdminActor_MatrixUserIDMismatch verifies the spec
// matrixUserId is validated against the authoritative Human identity.
func TestResolveTeamAdminActor_MatrixUserIDMismatch(t *testing.T) {
	ctx := context.Background()
	admin := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{PermissionLevel: 1},
		Status:     v1beta1.HumanStatus{MatrixUserID: "@real:matrix.local", InitialPassword: "pw"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{Admin: &v1beta1.TeamAdminSpec{Name: "alice", MatrixUserID: "@wrong:matrix.local"}},
	}

	r := &TeamReconciler{
		Client:      newTeamTestClient(t, admin.DeepCopy()),
		Provisioner: mocks.NewMockProvisioner(),
	}

	if _, err := r.resolveTeamAdminActor(ctx, team); err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
}

// TestDeriveTeamWithResolvedIdentities_BackfillsHumanMembers verifies that a
// human member's Matrix ID is taken from the provisioned Human CR (authoritative
// for SSO), while a member without a backing CR keeps its spec value.
func TestDeriveTeamWithResolvedIdentities_BackfillsHumanMembers(t *testing.T) {
	ctx := context.Background()
	coord := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "coord", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			PermissionLevel: 2,
			IdentitySource:  &v1beta1.IdentitySourceSpec{Issuer: "https://idp.example.com", Subject: "coord-sub"},
		},
		Status: v1beta1.HumanStatus{MatrixUserID: "@coord-hash-xyz:matrix.local"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			HumanMembers: []v1beta1.TeamMemberSpec{
				{Name: "coord", MatrixUserID: "@coord:matrix.local", Role: "coordinator"},
				{Name: "no-cr", MatrixUserID: "@no-cr:matrix.local", Role: "coordinator"},
			},
		},
	}

	r := &TeamReconciler{
		Client:      newTeamTestClient(t, coord.DeepCopy()),
		Provisioner: mocks.NewMockProvisioner(),
	}

	derived := r.deriveTeamWithResolvedIdentities(ctx, team, teamAdminActor{})
	if got := derived.Spec.HumanMembers[0].MatrixUserID; got != "@coord-hash-xyz:matrix.local" {
		t.Fatalf("coord member MatrixUserID = %q, want @coord-hash-xyz:matrix.local (from Human status)", got)
	}
	if got := derived.Spec.HumanMembers[1].MatrixUserID; got != "@no-cr:matrix.local" {
		t.Fatalf("no-cr member MatrixUserID = %q, want spec value preserved", got)
	}
	// Source team must remain untouched.
	if team.Spec.HumanMembers[0].MatrixUserID != "@coord:matrix.local" {
		t.Fatal("source team HumanMembers mutated; expected deep copy")
	}
}
