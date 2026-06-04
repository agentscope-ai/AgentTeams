package controller

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
)

func newTeamTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestLeaderHeartbeatEvery(t *testing.T) {
	team := &v1beta1.Team{}
	if got := leaderHeartbeatEvery(team); got != "" {
		t.Fatalf("expected empty heartbeat interval, got %q", got)
	}

	team.Spec.Leader.Heartbeat = &v1beta1.TeamLeaderHeartbeatSpec{
		Enabled: true,
		Every:   "30m",
	}
	if got := leaderHeartbeatEvery(team); got != "30m" {
		t.Fatalf("expected heartbeat interval 30m, got %q", got)
	}
}

func TestBuildDesiredMembers_LeaderAndWorkers(t *testing.T) {
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "gpt-4o"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-dev", Model: "gpt-4o"},
		{Name: "alpha-qa", Model: "gpt-4o"},
	}
	team.Status.Members = []v1beta1.TeamMemberStatus{
		{Name: "alpha-lead", Role: RoleTeamLeader.String(), Observed: true},
		{Name: "alpha-dev", Role: RoleTeamWorker.String(), Observed: true},
	}

	members := buildDesiredMembers(team, "")
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	if members[0].Role != RoleTeamLeader || members[0].Name != "alpha-lead" {
		t.Fatalf("members[0]=%+v, want leader alpha-lead", members[0])
	}
	if !members[0].IsUpdate {
		t.Errorf("leader should be IsUpdate=true (observed in Status.Members)")
	}
	if !members[1].IsUpdate {
		t.Errorf("alpha-dev should be IsUpdate=true (observed in Status.Members)")
	}
	if members[2].IsUpdate {
		t.Errorf("alpha-qa should be IsUpdate=false (not observed in Status.Members)")
	}
	for _, m := range members {
		if m.PodLabels["hiclaw.io/team"] != "alpha" {
			t.Errorf("member %s missing hiclaw.io/team label: %v", m.Name, m.PodLabels)
		}
		switch m.Role {
		case RoleTeamLeader:
			// Leader runtime is intentionally hardcoded to copaw in
			// leaderWorkerSpec() because LeaderSpec has no runtime field
			// and only the copaw team-leader agent template exists today.
			if m.Spec.Runtime != "copaw" {
				t.Errorf("leader %s runtime=%q, want copaw", m.Name, m.Spec.Runtime)
			}
		case RoleTeamWorker:
			// Worker runtime is passed through from TeamWorkerSpec.Runtime.
			// The fixture leaves it unset, so empty string is expected here;
			// the downstream backend.ResolveRuntime resolves it against
			// TeamReconciler.DefaultRuntime (from HICLAW_DEFAULT_WORKER_RUNTIME).
			if m.Spec.Runtime != "" {
				t.Errorf("worker %s runtime=%q, want \"\" (pass-through from TeamWorkerSpec)", m.Name, m.Spec.Runtime)
			}
		}
	}
}

func TestValidateNoStandaloneWorkerRuntimeConflicts(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "alpha-lead"},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "alpha-dev", WorkerName: "shared-dev"},
			},
		},
	}
	standalone := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-worker", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: "shared-dev"},
	}
	r := &TeamReconciler{Client: newTeamTestClient(t, standalone)}

	err := r.validateNoStandaloneWorkerRuntimeConflicts(context.Background(), team)
	if err == nil {
		t.Fatalf("expected runtime name conflict with standalone Worker")
	}
	if got := err.Error(); got != `team member worker[alpha-dev] runtime workerName "shared-dev" conflicts with existing standalone Worker default/existing-worker` {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestValidateNoStandaloneWorkerRuntimeConflictsRejectsCRNameConflict(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "alpha-lead", WorkerName: "team-lead"},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "alpha-dev", WorkerName: "team-dev"},
			},
		},
	}
	standalone := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: "standalone-dev"},
	}
	r := &TeamReconciler{Client: newTeamTestClient(t, standalone)}

	err := r.validateNoStandaloneWorkerRuntimeConflicts(context.Background(), team)
	if err == nil {
		t.Fatalf("expected CR name conflict with standalone Worker")
	}
	if got := err.Error(); got != `team member worker[alpha-dev] name "alpha-dev" conflicts with existing standalone Worker default/alpha-dev` {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestValidateNoStandaloneWorkerRuntimeConflictsAllowsDifferentNamespace(t *testing.T) {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "alpha-lead"},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "alpha-dev", WorkerName: "shared-dev"},
			},
		},
	}
	standalone := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-worker", Namespace: "other"},
		Spec:       v1beta1.WorkerSpec{WorkerName: "shared-dev"},
	}
	r := &TeamReconciler{Client: newTeamTestClient(t, standalone)}

	if err := r.validateNoStandaloneWorkerRuntimeConflicts(context.Background(), team); err != nil {
		t.Fatalf("validateNoStandaloneWorkerRuntimeConflicts: %v", err)
	}
}

// TestTeamWorkerSpecToWorkerSpec_RuntimePassthrough locks in the fix for the
// regression introduced by PR #666: team_controller must not override the
// per-member Runtime field when projecting TeamWorkerSpec into WorkerSpec.
//
// Before the fix, Runtime was hardcoded to "copaw" regardless of what the
// user declared in Team.Spec.Workers[].runtime, silently breaking
// HICLAW_DEFAULT_WORKER_RUNTIME=hermes|openclaw installs and ignoring
// explicit per-worker runtime pins.
func TestTeamWorkerSpecToWorkerSpec_RuntimePassthrough(t *testing.T) {
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "gpt-4o"}

	cases := []struct {
		name    string
		runtime string
	}{
		{"explicit_hermes", "hermes"},
		{"explicit_openclaw", "openclaw"},
		{"explicit_copaw", "copaw"},
		{"empty_defers_to_fallback", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := v1beta1.TeamWorkerSpec{Name: "alpha-dev", Model: "gpt-4o", Runtime: tc.runtime}
			team.Spec.Workers = []v1beta1.TeamWorkerSpec{w}
			got := teamWorkerSpecToWorkerSpec(team, w)
			if got.Runtime != tc.runtime {
				t.Fatalf("runtime=%q, want %q (must be passed through verbatim; empty string is valid and resolved downstream by backend.ResolveRuntime)", got.Runtime, tc.runtime)
			}
		})
	}
}

func TestBuildDesiredMembers_RuntimeWorkerNamesDriveMatrixPolicy(t *testing.T) {
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Spec.TeamName = "runtime-alpha"
	team.Spec.Leader = v1beta1.LeaderSpec{
		Name:       "alpha-worker-lead",
		WorkerName: "lead",
		Model:      "gpt-4o",
	}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-worker-dev", WorkerName: "dev", Model: "gpt-4o"},
		{Name: "alpha-worker-qa", WorkerName: "qa", Model: "gpt-4o"},
	}
	team.Spec.Admin = &v1beta1.TeamAdminSpec{
		Name:         "alpha-human-yhf",
		MatrixUserID: "@yhf:example.com",
	}

	members := buildDesiredMembers(team, "")
	byName := map[string]MemberContext{}
	for _, m := range members {
		byName[m.Name] = m
	}

	if got := byName["alpha-worker-lead"].RuntimeName; got != "lead" {
		t.Fatalf("leader RuntimeName=%q, want lead", got)
	}
	if got := byName["alpha-worker-dev"].RuntimeName; got != "dev" {
		t.Fatalf("worker RuntimeName=%q, want dev", got)
	}
	if got := byName["alpha-worker-dev"].TeamLeaderName; got != "lead" {
		t.Fatalf("worker TeamLeaderName=%q, want lead", got)
	}
	for _, m := range members {
		if got := m.TeamName; got != "runtime-alpha" {
			t.Fatalf("member %s TeamName=%q, want runtime-alpha", m.Name, got)
		}
	}

	leaderAllow := byName["alpha-worker-lead"].Spec.ChannelPolicy.GroupAllowExtra
	if !stringSliceContains(leaderAllow, "dev") || !stringSliceContains(leaderAllow, "qa") {
		t.Fatalf("leader groupAllowExtra=%v, want runtime worker names dev/qa", leaderAllow)
	}
	if stringSliceContains(leaderAllow, "alpha-worker-dev") {
		t.Fatalf("leader groupAllowExtra=%v must not use CR worker name", leaderAllow)
	}
	if !stringSliceContains(leaderAllow, "@yhf:example.com") || stringSliceContains(leaderAllow, "alpha-human-yhf") {
		t.Fatalf("leader groupAllowExtra=%v must use admin MatrixUserID, not admin CR name", leaderAllow)
	}

	devAllow := byName["alpha-worker-dev"].Spec.ChannelPolicy.GroupAllowExtra
	if !stringSliceContains(devAllow, "lead") || !stringSliceContains(devAllow, "qa") {
		t.Fatalf("dev groupAllowExtra=%v, want runtime leader/peer names lead/qa", devAllow)
	}
	if stringSliceContains(devAllow, "alpha-worker-lead") || stringSliceContains(devAllow, "alpha-worker-qa") {
		t.Fatalf("dev groupAllowExtra=%v must not use CR member names", devAllow)
	}
	if !stringSliceContains(devAllow, "@yhf:example.com") || stringSliceContains(devAllow, "alpha-human-yhf") {
		t.Fatalf("dev groupAllowExtra=%v must use admin MatrixUserID, not admin CR name", devAllow)
	}
}

func TestReconcileMemberInfraUsesCRNameForCredentialKey(t *testing.T) {
	prov := mocks.NewMockProvisioner()
	state := &MemberState{}
	member := MemberContext{
		Name:        "alpha-worker-lead",
		RuntimeName: "leader",
		Role:        RoleTeamLeader,
	}

	if _, err := ReconcileMemberInfra(context.Background(), MemberDeps{Provisioner: prov}, member, state); err != nil {
		t.Fatalf("ReconcileMemberInfra: %v", err)
	}

	if len(prov.Calls.ProvisionWorker) != 1 {
		t.Fatalf("ProvisionWorker calls=%d, want 1", len(prov.Calls.ProvisionWorker))
	}
	req := prov.Calls.ProvisionWorker[0]
	if req.Name != "leader" {
		t.Fatalf("ProvisionWorker Name=%q, want runtime workerName leader", req.Name)
	}
	if req.CredentialName != "alpha-worker-lead" {
		t.Fatalf("ProvisionWorker CredentialName=%q, want CR name alpha-worker-lead", req.CredentialName)
	}
}

func TestReconcileMemberRefreshUsesCRNameCredentialAndRuntimeMatrixName(t *testing.T) {
	prov := mocks.NewMockProvisioner()
	state := &MemberState{}
	member := MemberContext{
		Name:                 "alpha-worker-lead",
		RuntimeName:          "leader",
		Role:                 RoleTeamLeader,
		ExistingMatrixUserID: "@leader:localhost",
	}

	if _, err := ReconcileMemberInfra(context.Background(), MemberDeps{Provisioner: prov}, member, state); err != nil {
		t.Fatalf("ReconcileMemberInfra: %v", err)
	}

	if len(prov.Calls.RefreshWorkerCredentials) != 1 {
		t.Fatalf("RefreshWorkerCredentials calls=%d, want 1", len(prov.Calls.RefreshWorkerCredentials))
	}
	call := prov.Calls.RefreshWorkerCredentials[0]
	if call.CredentialName != "alpha-worker-lead" {
		t.Fatalf("CredentialName=%q, want CR name alpha-worker-lead", call.CredentialName)
	}
	if call.WorkerName != "leader" {
		t.Fatalf("WorkerName=%q, want runtime workerName leader", call.WorkerName)
	}
}

func TestReconcileMemberDeleteUsesCRNameForCredentialDelete(t *testing.T) {
	prov := mocks.NewMockProvisioner()
	deployer := mocks.NewMockDeployer()
	member := MemberContext{
		Name:        "alpha-worker-lead",
		RuntimeName: "leader",
		Role:        RoleTeamLeader,
	}

	if err := ReconcileMemberDelete(context.Background(), MemberDeps{Provisioner: prov, Deployer: deployer}, member); err != nil {
		t.Fatalf("ReconcileMemberDelete: %v", err)
	}

	if len(prov.Calls.DeprovisionWorker) != 1 || prov.Calls.DeprovisionWorker[0].Name != "leader" {
		t.Fatalf("DeprovisionWorker calls=%v, want runtime workerName leader", prov.Calls.DeprovisionWorker)
	}
	if len(prov.Calls.DeleteWorkerCredentials) != 1 || prov.Calls.DeleteWorkerCredentials[0] != "alpha-worker-lead" {
		t.Fatalf("DeleteWorkerCredentials calls=%v, want CR name alpha-worker-lead", prov.Calls.DeleteWorkerCredentials)
	}
}

// TestBuildDesiredMembers_SpecChangedDetection locks in the per-member
// spec-change detection that prevents unnecessary container recreation. It
// covers three cases on the same reconcile:
//   - leader with a matching stored hash   → SpecChanged=false
//   - worker whose spec was mutated         → SpecChanged=true
//   - worker with no stored hash (brand new) → SpecChanged=false (initial
//     creation is driven by the backend.StatusNotFound branch, not by
//     SpecChanged — see memberSpecChanged doc for why)
//
// This is the regression guard for the bug where TeamReconciler tore down
// every pod on every reconcile because MemberContext.ObservedGeneration was
// always 0 for team members.
func TestBuildDesiredMembers_SpecChangedDetection(t *testing.T) {
	team := &v1beta1.Team{}
	team.Name = "alpha"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "gpt-4o"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-dev", Model: "gpt-4o"},
		{Name: "alpha-qa", Model: "gpt-4o"},
	}

	// Leader's stored hash matches current source spec → unchanged.
	leaderHash := hashMemberSourceSpec(team, RoleTeamLeader, "alpha-lead")

	// alpha-dev previously stored at model=gpt-3.5 → now hashed against
	// the current gpt-4o spec → should report changed.
	priorTeam := team.DeepCopy()
	priorTeam.Spec.Workers[0].Model = "gpt-3.5"
	devHashOld := hashMemberSourceSpec(priorTeam, RoleTeamWorker, "alpha-dev")

	team.Status.Members = []v1beta1.TeamMemberStatus{
		{Name: "alpha-lead", Role: RoleTeamLeader.String(), SpecHash: leaderHash},
		{Name: "alpha-dev", Role: RoleTeamWorker.String(), SpecHash: devHashOld},
	}

	members := buildDesiredMembers(team, "")
	byName := map[string]MemberContext{}
	for _, m := range members {
		byName[m.Name] = m
	}
	if byName["alpha-lead"].SpecChanged {
		t.Errorf("leader spec unchanged, want SpecChanged=false, got true")
	}
	if !byName["alpha-dev"].SpecChanged {
		t.Errorf("alpha-dev spec mutated (gpt-3.5→gpt-4o), want SpecChanged=true")
	}
	if byName["alpha-qa"].SpecChanged {
		t.Errorf("alpha-qa has no stored hash (brand new), want SpecChanged=false so initial Create via StatusNotFound is not preempted by a transient Delete")
	}
}

// TestHashMemberSourceSpec_IgnoresPeerChanges is the specific guard for the
// live-cluster bug: adding a worker rewrites every member's *derived*
// ChannelPolicy (peer mentions + admin injection), but the user-authored
// source spec is unchanged, so the hash must stay the same.
func TestHashMemberSourceSpec_IgnoresPeerChanges(t *testing.T) {
	base := &v1beta1.Team{}
	base.Name = "alpha"
	base.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "gpt-4o"}
	base.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-dev", Model: "gpt-4o"},
	}

	after := base.DeepCopy()
	after.Spec.Workers = append(after.Spec.Workers, v1beta1.TeamWorkerSpec{
		Name: "alpha-qa", Model: "gpt-4o",
	})
	after.Spec.Admin = &v1beta1.TeamAdminSpec{Name: "alice", MatrixUserID: "@alice:example.com"}
	after.Spec.HumanMembers = []v1beta1.TeamMemberSpec{{Name: "bob", MatrixUserID: "@bob:example.com", Role: "coordinator"}}

	if hashMemberSourceSpec(base, RoleTeamLeader, "alpha-lead") !=
		hashMemberSourceSpec(after, RoleTeamLeader, "alpha-lead") {
		t.Errorf("leader hash changed after adding worker+admin+member; expected stable (no user-authored change)")
	}
	if hashMemberSourceSpec(base, RoleTeamWorker, "alpha-dev") !=
		hashMemberSourceSpec(after, RoleTeamWorker, "alpha-dev") {
		t.Errorf("alpha-dev hash changed after adding peer+admin+member; expected stable")
	}

	// Sanity: a real source change DOES flip the hash.
	mutated := base.DeepCopy()
	mutated.Spec.Workers[0].Model = "gpt-3.5"
	if hashMemberSourceSpec(base, RoleTeamWorker, "alpha-dev") ==
		hashMemberSourceSpec(mutated, RoleTeamWorker, "alpha-dev") {
		t.Errorf("alpha-dev hash unchanged after model mutation; expected different")
	}
}

// TestHashMemberSourceSpec_EnvChangeFlipsHash ensures user-defined env edits
// on either LeaderSpec or TeamWorkerSpec propagate through
// hashMemberSourceSpec, so the reconciler recreates the container when env
// changes.
func TestHashMemberSourceSpec_EnvChangeFlipsHash(t *testing.T) {
	base := &v1beta1.Team{}
	base.Name = "alpha"
	base.Spec.Leader = v1beta1.LeaderSpec{
		Name:  "alpha-lead",
		Model: "gpt-4o",
		Env:   map[string]string{"FOO": "1"},
	}
	base.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-dev", Model: "gpt-4o", Env: map[string]string{"BAR": "1"}},
	}

	// Leader env edit.
	leaderMut := base.DeepCopy()
	leaderMut.Spec.Leader.Env = map[string]string{"FOO": "2"}
	if hashMemberSourceSpec(base, RoleTeamLeader, "alpha-lead") ==
		hashMemberSourceSpec(leaderMut, RoleTeamLeader, "alpha-lead") {
		t.Errorf("leader hash unchanged after Env edit; expected different")
	}

	// Worker env edit.
	workerMut := base.DeepCopy()
	workerMut.Spec.Workers[0].Env = map[string]string{"BAR": "2"}
	if hashMemberSourceSpec(base, RoleTeamWorker, "alpha-dev") ==
		hashMemberSourceSpec(workerMut, RoleTeamWorker, "alpha-dev") {
		t.Errorf("alpha-dev hash unchanged after Env edit; expected different")
	}

	// Adding a key to a worker's env also flips the hash.
	workerAdd := base.DeepCopy()
	workerAdd.Spec.Workers[0].Env = map[string]string{"BAR": "1", "BAZ": "1"}
	if hashMemberSourceSpec(base, RoleTeamWorker, "alpha-dev") ==
		hashMemberSourceSpec(workerAdd, RoleTeamWorker, "alpha-dev") {
		t.Errorf("alpha-dev hash unchanged after Env key addition; expected different")
	}
}

func TestReconcileTeamNormalInjectsLeaderCoordinationAfterMemberConfig(t *testing.T) {
	ctx := context.Background()
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:       "alpha-lead",
				WorkerName: "leader",
				Model:      "qwen",
				Agents:     "custom leader AGENTS.md",
			},
			Workers: []v1beta1.TeamWorkerSpec{{
				Name:       "alpha-dev",
				WorkerName: "dev",
				Model:      "qwen",
			}},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team.DeepCopy()).
		WithStatusSubresource(&v1beta1.Team{}).
		Build()

	var calls []string
	deployer := mocks.NewMockDeployer()
	deployer.DeployWorkerConfigFn = func(ctx context.Context, req service.WorkerDeployRequest) error {
		calls = append(calls, "config:"+req.Name)
		return nil
	}
	deployer.InjectCoordinationContextFn = func(ctx context.Context, req service.CoordinationDeployRequest) error {
		calls = append(calls, "inject:"+req.LeaderName)
		if req.LeaderName != "leader" {
			t.Fatalf("LeaderName=%q, want leader", req.LeaderName)
		}
		if len(req.TeamWorkers) != 1 || req.TeamWorkers[0] != "dev" {
			t.Fatalf("TeamWorkers=%v, want [dev]", req.TeamWorkers)
		}
		return nil
	}

	r := &TeamReconciler{
		Client:      c,
		Provisioner: mocks.NewMockProvisioner(),
		Deployer:    deployer,
		Backend:     backend.NewRegistry([]backend.WorkerBackend{mocks.NewMockWorkerBackend()}),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
		AgentFSDir:  t.TempDir(),
	}
	if _, err := r.reconcileTeamNormal(ctx, team); err != nil {
		t.Fatalf("reconcileTeamNormal: %v", err)
	}

	leaderConfig := callIndex(calls, "config:leader")
	inject := callIndex(calls, "inject:leader")
	if leaderConfig == -1 || inject == -1 {
		t.Fatalf("calls=%v, want config:leader and inject:leader", calls)
	}
	if inject < leaderConfig {
		t.Fatalf("calls=%v, leader coordination injection must run after leader AGENTS.md config write", calls)
	}
	if inject != len(calls)-1 {
		t.Fatalf("calls=%v, leader coordination injection must run after all member config writes", calls)
	}
}

// registryEntry is the minimal subset of service.workersRegistry we need to
// inspect in tests — duplicated locally because the registry shape (and
// WorkerRegistryEntry fields we care about) are stable JSON contracts that
// Manager-side tooling also consumes. Keeping this in sync with the JSON
// tags in service.WorkerRegistryEntry is deliberate.
type registryEntry struct {
	MatrixUserID string   `json:"matrix_user_id"`
	RoomID       string   `json:"room_id"`
	Runtime      string   `json:"runtime"`
	Deployment   string   `json:"deployment"`
	Skills       []string `json:"skills"`
	Role         string   `json:"role"`
	TeamID       *string  `json:"team_id"`
	Image        *string  `json:"image"`
}

type registryFile struct {
	Version int                      `json:"version"`
	Workers map[string]registryEntry `json:"workers"`
}

func readRegistry(t *testing.T, fake *ossfake.Memory, managerName string) *registryFile {
	t.Helper()
	key := "agents/" + managerName + "/workers-registry.json"
	data, err := fake.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("read registry %s: %v", key, err)
	}
	var out registryFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	return &out
}

func newTestLegacy(t *testing.T) (*service.LegacyCompat, *ossfake.Memory) {
	t.Helper()
	fake := ossfake.NewMemory()
	legacy := service.NewLegacyCompat(service.LegacyConfig{
		OSS:          fake,
		MatrixDomain: "matrix.local",
		ManagerName:  "manager",
		// Leave AgentFSDir empty so LegacyCompat skips the local shared-mount
		// write that would otherwise require creating a real directory.
		AgentFSDir: "",
	})
	return legacy, fake
}

// TestReconcileLegacyMember_BuildsEntry is the regression guard for the
// test-18 failure: TeamReconciler must populate workers-registry.json with
// role=team_leader / worker and team_id=<team name> for each team member so
// manager-side skills (find-worker.sh, push-worker-skills.sh, etc.) can
// continue to resolve team members by name.
func TestReconcileLegacyMember_BuildsEntry(t *testing.T) {
	legacy, fake := newTestLegacy(t)
	r := &TeamReconciler{Legacy: legacy}

	team := &v1beta1.Team{}
	team.Name = "team-a"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "lead"}

	leaderCtx := MemberContext{
		Name: "lead",
		Role: RoleTeamLeader,
		Spec: v1beta1.WorkerSpec{Runtime: "copaw"},
	}
	leaderStatus := &v1beta1.TeamMemberStatus{Name: "lead", RoomID: "!room-lead:matrix.local"}
	r.reconcileLegacyMember(context.Background(), team, leaderCtx, leaderStatus)

	workerCtx := MemberContext{
		Name: "dev",
		Role: RoleTeamWorker,
		Spec: v1beta1.WorkerSpec{
			Runtime: "copaw",
			Image:   "dev:v1",
			Skills:  []string{"refactor"},
		},
	}
	workerStatus := &v1beta1.TeamMemberStatus{Name: "dev", RoomID: "!room-dev:matrix.local"}
	r.reconcileLegacyMember(context.Background(), team, workerCtx, workerStatus)

	reg := readRegistry(t, fake, "manager")
	if reg.Version != 1 {
		t.Fatalf("registry version=%d, want 1", reg.Version)
	}

	leader, ok := reg.Workers["lead"]
	if !ok {
		t.Fatalf("leader entry missing from registry: %+v", reg.Workers)
	}
	if leader.Role != "team_leader" {
		t.Errorf("leader role=%q, want team_leader", leader.Role)
	}
	if leader.TeamID == nil || *leader.TeamID != "team-a" {
		t.Errorf("leader team_id=%v, want team-a", leader.TeamID)
	}
	if leader.Runtime != "copaw" {
		t.Errorf("leader runtime=%q, want copaw", leader.Runtime)
	}
	if leader.RoomID != "!room-lead:matrix.local" {
		t.Errorf("leader room_id=%q, want !room-lead:matrix.local", leader.RoomID)
	}
	if leader.MatrixUserID != "@lead:matrix.local" {
		t.Errorf("leader matrix_user_id=%q, want @lead:matrix.local", leader.MatrixUserID)
	}
	if leader.Deployment != "local" {
		t.Errorf("leader deployment=%q, want local", leader.Deployment)
	}
	if leader.Image != nil {
		t.Errorf("leader image=%v, want nil (leader spec has no image)", leader.Image)
	}

	worker, ok := reg.Workers["dev"]
	if !ok {
		t.Fatalf("worker entry missing from registry: %+v", reg.Workers)
	}
	if worker.Role != "worker" {
		t.Errorf("worker role=%q, want worker", worker.Role)
	}
	if worker.TeamID == nil || *worker.TeamID != "team-a" {
		t.Errorf("worker team_id=%v, want team-a", worker.TeamID)
	}
	if worker.Image == nil || *worker.Image != "dev:v1" {
		t.Errorf("worker image=%v, want dev:v1", worker.Image)
	}
	if len(worker.Skills) != 1 || worker.Skills[0] != "refactor" {
		t.Errorf("worker skills=%v, want [refactor]", worker.Skills)
	}
}

func TestReconcileLegacyMember_NoOpWhenLegacyNil(t *testing.T) {
	r := &TeamReconciler{Legacy: nil}
	team := &v1beta1.Team{}
	team.Name = "team-a"
	// Must not panic.
	r.reconcileLegacyMember(context.Background(), team, MemberContext{Name: "x", Role: RoleTeamLeader}, nil)
	r.removeLegacyMember(context.Background(), "x")
}

// TestRemoveLegacyMember_DeletesEntry covers the stale-cleanup and
// handleDelete paths: once removed, the entry disappears so manager-side
// skills no longer see a ghost worker.
func TestRemoveLegacyMember_DeletesEntry(t *testing.T) {
	legacy, fake := newTestLegacy(t)
	r := &TeamReconciler{Legacy: legacy}

	team := &v1beta1.Team{}
	team.Name = "team-a"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "lead"}
	r.reconcileLegacyMember(context.Background(), team,
		MemberContext{Name: "lead", Role: RoleTeamLeader, Spec: v1beta1.WorkerSpec{Runtime: "copaw"}},
		&v1beta1.TeamMemberStatus{Name: "lead"})

	if _, ok := readRegistry(t, fake, "manager").Workers["lead"]; !ok {
		t.Fatalf("precondition: lead should be present before removal")
	}

	r.removeLegacyMember(context.Background(), "lead")

	if _, ok := readRegistry(t, fake, "manager").Workers["lead"]; ok {
		t.Fatalf("lead still present after removeLegacyMember")
	}
}

// TestBuildDesiredMembers_StampsControllerLabelOnPodLabels verifies that when
// the TeamReconciler propagates a non-empty ControllerName into
// buildDesiredMembers, every derived MemberContext carries the
// hiclaw.io/controller PodLabel so the resulting Pod lands inside the
// owning controller instance's label-scoped informer cache.
//
// Post-refactor (PR #666) the label is stamped via MemberContext.PodLabels →
// backend.CreateRequest.Labels rather than on child Worker CRs, because
// TeamReconciler no longer materializes child Worker CRs.
func TestBuildDesiredMembers_StampsControllerLabelOnPodLabels(t *testing.T) {
	team := &v1beta1.Team{
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "lead", Model: "qwen"},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "w1", Model: "qwen"},
			},
		},
	}

	members := buildDesiredMembers(team, "ctrl-a")
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	for _, m := range members {
		if got := m.PodLabels[v1beta1.LabelController]; got != "ctrl-a" {
			t.Fatalf("member %s: expected controller label ctrl-a in PodLabels, got %q (labels=%v)", m.Name, got, m.PodLabels)
		}
		if got := m.PodLabels["hiclaw.io/team"]; got != team.Name {
			t.Fatalf("member %s: expected team label %q, got %q", m.Name, team.Name, got)
		}
		if m.PodLabels["hiclaw.io/role"] == "" {
			t.Fatalf("member %s: expected non-empty hiclaw.io/role", m.Name)
		}
	}
}

// TestBuildDesiredMembers_TeamMetadataLabelsPropagateToAllMembers verifies
// Team.metadata.labels fan out to the leader AND every worker — the
// "team-wide default" promise of the labels feature.
func TestBuildDesiredMembers_TeamMetadataLabelsPropagateToAllMembers(t *testing.T) {
	team := &v1beta1.Team{
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "lead", Model: "qwen"},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "w1", Model: "qwen"},
				{Name: "w2", Model: "qwen"},
			},
		},
	}
	team.Name = "alpha"
	team.ObjectMeta.Labels = map[string]string{"squad": "alpha", "region": "us-west"}

	members := buildDesiredMembers(team, "ctrl-a")
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	for _, m := range members {
		if got := m.PodLabels["squad"]; got != "alpha" {
			t.Errorf("member %s missing team metadata label squad=alpha, got %v", m.Name, m.PodLabels)
		}
		if got := m.PodLabels["region"]; got != "us-west" {
			t.Errorf("member %s missing team metadata label region=us-west, got %v", m.Name, m.PodLabels)
		}
	}
}

// TestBuildDesiredMembers_PerMemberLabelsOverrideTeamMetadata verifies
// that per-member spec.labels (leader.Labels / workers[i].Labels) win
// over team-wide metadata.labels on key collision — the "per-member
// beats team-wide" precedence for Team CRs.
func TestBuildDesiredMembers_PerMemberLabelsOverrideTeamMetadata(t *testing.T) {
	team := &v1beta1.Team{
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:   "lead",
				Model:  "qwen",
				Labels: map[string]string{"tier": "leader"},
			},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "w1", Model: "qwen", Labels: map[string]string{"tier": "worker"}},
			},
		},
	}
	team.Name = "alpha"
	team.ObjectMeta.Labels = map[string]string{"tier": "team-default"}

	members := buildDesiredMembers(team, "ctrl-a")
	byName := map[string]MemberContext{}
	for _, m := range members {
		byName[m.Name] = m
	}
	if got := byName["lead"].PodLabels["tier"]; got != "leader" {
		t.Errorf("leader tier=%q, want leader (per-member overrides team metadata)", got)
	}
	if got := byName["w1"].PodLabels["tier"]; got != "worker" {
		t.Errorf("w1 tier=%q, want worker (per-member overrides team metadata)", got)
	}
}

// TestBuildDesiredMembers_WorkerLabelsDoNotLeakToLeader guards against
// the easiest regression: accidentally building the leader's labels
// from the wrong source slice, so that workers[i].Labels show up on the
// leader Pod (or vice versa).
func TestBuildDesiredMembers_WorkerLabelsDoNotLeakToLeader(t *testing.T) {
	team := &v1beta1.Team{
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:   "lead",
				Model:  "qwen",
				Labels: map[string]string{"role-hint": "planner"},
			},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "w1", Model: "qwen", Labels: map[string]string{"skill": "rust"}},
				{Name: "w2", Model: "qwen", Labels: map[string]string{"skill": "go"}},
			},
		},
	}
	team.Name = "alpha"

	members := buildDesiredMembers(team, "ctrl-a")
	byName := map[string]MemberContext{}
	for _, m := range members {
		byName[m.Name] = m
	}

	if _, ok := byName["lead"].PodLabels["skill"]; ok {
		t.Errorf("leader must not carry workers[].labels[skill]: %v", byName["lead"].PodLabels)
	}
	if got := byName["lead"].PodLabels["role-hint"]; got != "planner" {
		t.Errorf("leader missing its own spec.leader.labels[role-hint]: %v", byName["lead"].PodLabels)
	}
	if _, ok := byName["w1"].PodLabels["role-hint"]; ok {
		t.Errorf("w1 must not carry spec.leader.labels[role-hint]: %v", byName["w1"].PodLabels)
	}
	if got := byName["w1"].PodLabels["skill"]; got != "rust" {
		t.Errorf("w1 skill=%q, want rust", got)
	}
	if got := byName["w2"].PodLabels["skill"]; got != "go" {
		t.Errorf("w2 skill=%q, want go", got)
	}
	// Cross-worker isolation: w2's skill must not leak to w1 and vice versa.
	if byName["w1"].PodLabels["skill"] == "go" {
		t.Errorf("w1 received w2's skill label")
	}
}

// TestBuildDesiredMembers_SystemLabelsOverrideUserLabels verifies the
// reserved-key contract for Team CRs: users writing controller system
// keys into metadata.labels or per-member spec.labels are silently
// overridden by the controller's own values.
func TestBuildDesiredMembers_SystemLabelsOverrideUserLabels(t *testing.T) {
	team := &v1beta1.Team{
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:   "lead",
				Model:  "qwen",
				Labels: map[string]string{v1beta1.LabelController: "spec-attacker"},
			},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "w1", Model: "qwen", Labels: map[string]string{"hiclaw.io/role": "evil"}},
			},
		},
	}
	team.Name = "alpha"
	team.ObjectMeta.Labels = map[string]string{
		v1beta1.LabelController: "metadata-attacker",
		"hiclaw.io/team":        "other-team",
	}

	members := buildDesiredMembers(team, "real-ctl")
	byName := map[string]MemberContext{}
	for _, m := range members {
		byName[m.Name] = m
	}
	for _, name := range []string{"lead", "w1"} {
		if got := byName[name].PodLabels[v1beta1.LabelController]; got != "real-ctl" {
			t.Errorf("%s: controller label got %q, want real-ctl", name, got)
		}
		if got := byName[name].PodLabels["hiclaw.io/team"]; got != "alpha" {
			t.Errorf("%s: team label got %q, want alpha", name, got)
		}
	}
	if got := byName["lead"].PodLabels["hiclaw.io/role"]; got != RoleTeamLeader.String() {
		t.Errorf("leader role got %q, want %q", got, RoleTeamLeader.String())
	}
	if got := byName["w1"].PodLabels["hiclaw.io/role"]; got != RoleTeamWorker.String() {
		t.Errorf("w1 role got %q, want %q", got, RoleTeamWorker.String())
	}
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func callIndex(calls []string, target string) int {
	for i, call := range calls {
		if call == target {
			return i
		}
	}
	return -1
}
