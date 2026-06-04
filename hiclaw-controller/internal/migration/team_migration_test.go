package migration

import (
	"context"
	"strings"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNS = "default"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("add v1beta1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return s
}

func legacyTeam() *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "research",
			Namespace:  testNS,
			Finalizers: []string{"hiclaw.io/cleanup"},
		},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:  "leader-bot",
				Model: "qwen-plus",
			},
			Workers: []v1beta1.TeamWorkerSpec{
				{Name: "coder", Model: "qwen-max"},
				{Name: "tester", Model: "qwen-plus"},
			},
		},
		Status: v1beta1.TeamStatus{
			Phase:          "Active",
			TeamRoomID:     "!teamRoom:local",
			LeaderDMRoomID: "!leaderDM:local",
			Members: []v1beta1.TeamMemberStatus{
				{Name: "leader-bot", RuntimeName: "leader-bot", Role: "team_leader", MatrixUserID: "@leader-bot:local", RoomID: "!leader:local", Observed: true, Ready: true},
				{Name: "coder", RuntimeName: "coder", Role: "worker", MatrixUserID: "@coder:local", RoomID: "!coder:local", Observed: true, Ready: true},
				{Name: "tester", RuntimeName: "tester", Role: "worker", MatrixUserID: "@tester:local", RoomID: "!tester:local", Observed: true, Ready: true},
			},
		},
	}
}

func TestNeedsMigration(t *testing.T) {
	m := &TeamMigrator{Enabled: true, BatchSize: 3}

	tests := []struct {
		name   string
		team   func() *v1beta1.Team
		expect bool
	}{
		{
			name:   "legacy team needs migration",
			team:   legacyTeam,
			expect: true,
		},
		{
			name: "decoupled team does not need migration",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "leader-bot", Role: "team_leader"}}
				return t
			},
			expect: false,
		},
		{
			name: "disabled annotation opts out",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Annotations = map[string]string{v1beta1.AnnotationAutoMigrate: "disabled"}
				return t
			},
			expect: false,
		},
		{
			name: "empty team (no leader/workers)",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Spec.Leader = v1beta1.LeaderSpec{}
				t.Spec.Workers = nil
				return t
			},
			expect: false,
		},
		{
			name: "non-Active team blocked by health gate",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Status.Phase = "Pending"
				return t
			},
			expect: false,
		},
		{
			name: "team with unprovisioned member (no MatrixUserID) blocked",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Status.Members[1].MatrixUserID = ""
				return t
			},
			expect: false,
		},
		{
			name: "team with missing RoomID blocked",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Status.Members[2].RoomID = ""
				return t
			},
			expect: false,
		},
		{
			name: "team with incomplete status members blocked",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Status.Members = t.Status.Members[:1] // only leader
				return t
			},
			expect: false,
		},
		{
			name: "sleeping worker (not Ready but Observed) still allows migration",
			team: func() *v1beta1.Team {
				t := legacyTeam()
				t.Status.Members[2].Ready = false // tester is sleeping
				return t
			},
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.NeedsMigration(tt.team())
			if got != tt.expect {
				t.Errorf("NeedsMigration() = %v, want %v", got, tt.expect)
			}
		})
	}

	// Disabled migrator always returns false
	disabled := &TeamMigrator{Enabled: false}
	if disabled.NeedsMigration(legacyTeam()) {
		t.Error("disabled migrator should return false")
	}
}

func TestMigrationInProgress(t *testing.T) {
	m := &TeamMigrator{Enabled: true}

	tests := []struct {
		phase  string
		expect bool
	}{
		{"", false},
		{v1beta1.MigrationPhaseWorkerCRsCreated, true},
		{v1beta1.MigrationPhaseStatusSeeded, true},
		{v1beta1.MigrationPhasePodsReparented, true},
		{v1beta1.MigrationPhaseCoordinationInjected, true},
		{v1beta1.MigrationPhaseTeamSpecPatched, true},
		{v1beta1.MigrationPhaseMigrated, false},
	}

	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			team := legacyTeam()
			if tt.phase != "" {
				team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: tt.phase}
			}
			got := m.MigrationInProgress(team)
			if got != tt.expect {
				t.Errorf("MigrationInProgress(%q) = %v, want %v", tt.phase, got, tt.expect)
			}
		})
	}
}

func TestTryAcquire(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	// 2 teams in-progress, batch size 3 → should acquire
	team1 := legacyTeam()
	team1.Name = "team1"
	team1.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseWorkerCRsCreated}

	team2 := legacyTeam()
	team2.Name = "team2"
	team2.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseStatusSeeded}

	team3 := legacyTeam()
	team3.Name = "team3"
	// No migration annotation

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team1, team2, team3).Build()
	m := &TeamMigrator{Client: fc, Enabled: true, BatchSize: 3}

	if !m.TryAcquire(ctx) {
		t.Error("TryAcquire should succeed with 2/3 slots used")
	}

	// Add one more → at limit
	team3.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhasePodsReparented}
	if err := fc.Update(ctx, team3); err != nil {
		t.Fatal(err)
	}
	if m.TryAcquire(ctx) {
		t.Error("TryAcquire should fail with 3/3 slots used")
	}
}

func TestStepCreateWorkerCRs(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).WithStatusSubresource(team).Build()

	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepCreateWorkerCRs(ctx, team)
	if err != nil {
		t.Fatalf("stepCreateWorkerCRs: %v", err)
	}

	// Verify Worker CRs created
	for _, name := range []string{"leader-bot", "coder", "tester"} {
		var w v1beta1.Worker
		if err := fc.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &w); err != nil {
			t.Errorf("Worker CR %q not found: %v", name, err)
		}
	}

	// Verify leader has runtime=copaw
	var leader v1beta1.Worker
	if err := fc.Get(ctx, types.NamespacedName{Name: "leader-bot", Namespace: testNS}, &leader); err != nil {
		t.Fatal(err)
	}
	if leader.Spec.Runtime != "copaw" {
		t.Errorf("leader runtime = %q, want copaw", leader.Spec.Runtime)
	}
	if leader.Spec.Model != "qwen-plus" {
		t.Errorf("leader model = %q, want qwen-plus", leader.Spec.Model)
	}
	if leader.Annotations[v1beta1.AnnotationMigrationOwned] != "true" {
		t.Errorf("leader migration-owned annotation = %q, want true", leader.Annotations[v1beta1.AnnotationMigrationOwned])
	}
	if leader.Annotations[v1beta1.AnnotationMigratedFromTeam] != "research" {
		t.Errorf("leader migrated-from-team annotation = %q, want research", leader.Annotations[v1beta1.AnnotationMigratedFromTeam])
	}
	if leader.Annotations[v1beta1.AnnotationMigratedMemberRole] != "team_leader" {
		t.Errorf("leader migrated-member-role annotation = %q, want team_leader", leader.Annotations[v1beta1.AnnotationMigratedMemberRole])
	}

	// Verify migration finalizer added
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, team); err != nil {
		t.Fatal(err)
	}
	hasFinalizer := false
	for _, f := range team.Finalizers {
		if f == v1beta1.FinalizerMigration {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("migration finalizer not added to Team")
	}

	// Verify phase annotation
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseWorkerCRsCreated {
		t.Errorf("phase = %q, want WorkerCRsCreated", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}

	// Idempotency: re-run should not fail
	err = m.stepCreateWorkerCRs(ctx, team)
	if err != nil {
		t.Fatalf("stepCreateWorkerCRs (idempotent): %v", err)
	}
}

func TestStepCreateWorkerCRsConflictsWithUnownedExistingWorker(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	existingCoder := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: testNS},
		Spec:       teamWorkerSpecToWorkerSpecForMigration(team, team.Spec.Workers[0]),
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, existingCoder).
		WithStatusSubresource(team, existingCoder).
		Build()
	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepCreateWorkerCRs(ctx, team)
	if err == nil {
		t.Fatal("stepCreateWorkerCRs should fail when an unowned Worker with the member name already exists")
	}
	if !strings.Contains(err.Error(), v1beta1.AnnotationAllowMigrationAdopt) {
		t.Fatalf("error = %q, want adoption guidance mentioning %s", err.Error(), v1beta1.AnnotationAllowMigrationAdopt)
	}
}

func TestStepCreateWorkerCRsAdoptsExplicitMatchingWorker(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	existingCoder := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coder",
			Namespace: testNS,
			Annotations: map[string]string{
				v1beta1.AnnotationAllowMigrationAdopt: "true",
			},
		},
		Spec: teamWorkerSpecToWorkerSpecForMigration(team, team.Spec.Workers[0]),
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, existingCoder).
		WithStatusSubresource(team, existingCoder).
		Build()
	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	if err := m.stepCreateWorkerCRs(ctx, team); err != nil {
		t.Fatalf("stepCreateWorkerCRs should adopt explicitly marked matching Worker: %v", err)
	}
}

func TestStepCreateWorkerCRsRejectsAdoptWithMismatchedSpec(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	existingCoder := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coder",
			Namespace: testNS,
			Annotations: map[string]string{
				v1beta1.AnnotationAllowMigrationAdopt: "true",
			},
		},
		Spec: v1beta1.WorkerSpec{Model: "different-model"},
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, existingCoder).
		WithStatusSubresource(team, existingCoder).
		Build()
	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepCreateWorkerCRs(ctx, team)
	if err == nil {
		t.Fatal("stepCreateWorkerCRs should reject adoption when the Worker spec differs")
	}
	if !strings.Contains(err.Error(), "spec does not match") {
		t.Fatalf("error = %q, want spec mismatch", err.Error())
	}
}

func TestStepSeedStatus(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseWorkerCRsCreated}

	// Pre-create Worker CRs with empty status
	leaderW := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "leader-bot", Namespace: testNS},
		Spec:       v1beta1.WorkerSpec{Model: "qwen-plus", Runtime: "copaw"},
	}
	coderW := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: testNS},
		Spec:       v1beta1.WorkerSpec{Model: "qwen-max"},
	}
	testerW := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "tester", Namespace: testNS},
		Spec:       v1beta1.WorkerSpec{Model: "qwen-plus"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(team, leaderW, coderW, testerW).
		WithStatusSubresource(leaderW, coderW, testerW, team).
		Build()

	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepSeedStatus(ctx, team)
	if err != nil {
		t.Fatalf("stepSeedStatus: %v", err)
	}

	// Verify status was seeded
	var w v1beta1.Worker
	if err := fc.Get(ctx, types.NamespacedName{Name: "coder", Namespace: testNS}, &w); err != nil {
		t.Fatal(err)
	}
	if w.Status.MatrixUserID != "@coder:local" {
		t.Errorf("coder matrixUserID = %q, want @coder:local", w.Status.MatrixUserID)
	}
	if w.Status.RoomID != "!coder:local" {
		t.Errorf("coder roomID = %q, want !coder:local", w.Status.RoomID)
	}
	if w.Status.Phase != "Running" {
		t.Errorf("coder phase = %q, want Running", w.Status.Phase)
	}

	// Verify Team annotation advanced
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, team); err != nil {
		t.Fatal(err)
	}
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseStatusSeeded {
		t.Errorf("phase = %q, want StatusSeeded", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}
}

func TestStepReparentPods_Docker(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseStatusSeeded}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).WithStatusSubresource(team).Build()

	// nil backend means non-K8s → skip reparent
	m := &TeamMigrator{Client: fc, Backend: nil, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepReparentPods(ctx, team)
	if err != nil {
		t.Fatalf("stepReparentPods (docker): %v", err)
	}

	// Phase should advance to PodsReparented
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, team); err != nil {
		t.Fatal(err)
	}
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhasePodsReparented {
		t.Errorf("phase = %q, want PodsReparented", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}
}

func TestStepReparentPods_K8sUsesWorkerLabel(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	team.UID = "team-uid"
	team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseStatusSeeded}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "leader-bot",
			Namespace: testNS,
			UID:       "worker-uid",
		},
		Spec: v1beta1.WorkerSpec{Model: "qwen-plus"},
	}
	oldController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hiclaw-worker-leader-bot",
			Namespace: testNS,
			Labels: map[string]string{
				"app":              "hiclaw-worker",
				"hiclaw.io/worker": "leader-bot",
				"hiclaw.io/team":   "research",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1beta1.SchemeGroupVersion.String(),
				Kind:       "Team",
				Name:       "research",
				UID:        team.UID,
				Controller: &oldController,
			}},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, worker, pod).
		WithStatusSubresource(team).
		Build()
	m := &TeamMigrator{
		Client:    fc,
		Backend:   backend.NewRegistry([]backend.WorkerBackend{fakeWorkerBackend{name: "k8s", available: true}}),
		Scheme:    scheme,
		Enabled:   true,
		BatchSize: 3,
	}

	if err := m.stepReparentPods(ctx, team); err != nil {
		t.Fatalf("stepReparentPods: %v", err)
	}

	var gotPod corev1.Pod
	if err := fc.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: testNS}, &gotPod); err != nil {
		t.Fatal(err)
	}
	if len(gotPod.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %+v, want exactly one Worker owner", gotPod.OwnerReferences)
	}
	ref := gotPod.OwnerReferences[0]
	if ref.Kind != "Worker" || ref.Name != "leader-bot" || ref.UID != worker.UID {
		t.Fatalf("ownerReference = %+v, want Worker/leader-bot uid %s", ref, worker.UID)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerReference.Controller = %v, want true", ref.Controller)
	}

	var gotTeam v1beta1.Team
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, &gotTeam); err != nil {
		t.Fatal(err)
	}
	if gotTeam.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhasePodsReparented {
		t.Errorf("phase = %q, want PodsReparented", gotTeam.Annotations[v1beta1.AnnotationMigrationPhase])
	}
}

func TestStepPatchTeamSpec(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseCoordinationInjected}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).WithStatusSubresource(team).Build()
	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepPatchTeamSpec(ctx, team)
	if err != nil {
		t.Fatalf("stepPatchTeamSpec: %v", err)
	}

	// Verify workerMembers populated
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, team); err != nil {
		t.Fatal(err)
	}
	if len(team.Spec.WorkerMembers) != 3 {
		t.Fatalf("workerMembers count = %d, want 3", len(team.Spec.WorkerMembers))
	}

	// Check leader is first with correct role
	if team.Spec.WorkerMembers[0].Name != "leader-bot" || team.Spec.WorkerMembers[0].Role != "team_leader" {
		t.Errorf("first member = %+v, want leader-bot/team_leader", team.Spec.WorkerMembers[0])
	}
	if team.Spec.WorkerMembers[1].Name != "coder" || team.Spec.WorkerMembers[1].Role != "worker" {
		t.Errorf("second member = %+v, want coder/worker", team.Spec.WorkerMembers[1])
	}

	// Verify legacy fields preserved
	if team.Spec.Leader.Name != "leader-bot" {
		t.Error("legacy spec.leader should be preserved")
	}
	if len(team.Spec.Workers) != 2 {
		t.Error("legacy spec.workers should be preserved")
	}
}

func TestStepFinalize(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()
	team.Annotations = map[string]string{v1beta1.AnnotationMigrationPhase: v1beta1.MigrationPhaseTeamSpecPatched}
	team.Finalizers = append(team.Finalizers, v1beta1.FinalizerMigration)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).WithStatusSubresource(team).Build()
	m := &TeamMigrator{Client: fc, Scheme: scheme, Enabled: true, BatchSize: 3}

	err := m.stepFinalize(ctx, team)
	if err != nil {
		t.Fatalf("stepFinalize: %v", err)
	}

	// Verify annotations
	if err := fc.Get(ctx, types.NamespacedName{Name: "research", Namespace: testNS}, team); err != nil {
		t.Fatal(err)
	}
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseMigrated {
		t.Errorf("phase = %q, want Migrated", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}
	if team.Annotations[v1beta1.AnnotationMigratedAt] == "" {
		t.Error("migrated-at annotation not set")
	}

	// Verify finalizer removed
	for _, f := range team.Finalizers {
		if f == v1beta1.FinalizerMigration {
			t.Error("migration finalizer should be removed")
		}
	}
}

func TestFullMigrationFlow(t *testing.T) {
	scheme := newScheme(t)
	ctx := context.Background()

	team := legacyTeam()

	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(team).
		WithStatusSubresource(team, &v1beta1.Worker{}).
		Build()

	// Mock deployer that always succeeds
	deployer := &noopDeployer{}

	m := &TeamMigrator{
		Client:    fc,
		Backend:   nil, // Docker mode → skip Pod reparent
		Deployer:  deployer,
		Scheme:    scheme,
		Enabled:   true,
		BatchSize: 3,
	}

	// Phase 1: NotStarted → WorkerCRsCreated
	res, err := m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if !res.Requeue {
		t.Error("step 1 should requeue")
	}
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseWorkerCRsCreated {
		t.Fatalf("after step 1: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}

	// Phase 2: WorkerCRsCreated → StatusSeeded
	// Need to add status subresource for Worker CRs
	addWorkerStatusSubresource(t, fc, scheme)
	res, err = m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseStatusSeeded {
		t.Fatalf("after step 2: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}

	// Phase 3: StatusSeeded → PodsReparented (skipped, Docker mode)
	res, err = m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 3: %v", err)
	}
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhasePodsReparented {
		t.Fatalf("after step 3: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}

	// Phase 4: PodsReparented → CoordinationInjected
	res, err = m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 4: %v", err)
	}
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseCoordinationInjected {
		t.Fatalf("after step 4: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}

	// Phase 5: CoordinationInjected → TeamSpecPatched
	res, err = m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 5: %v", err)
	}
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseTeamSpecPatched {
		t.Fatalf("after step 5: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}
	if len(team.Spec.WorkerMembers) != 3 {
		t.Fatalf("after step 5: workerMembers = %d, want 3", len(team.Spec.WorkerMembers))
	}

	// Phase 6: TeamSpecPatched → Migrated
	res, err = m.Step(ctx, team)
	if err != nil {
		t.Fatalf("step 6: %v", err)
	}
	_ = res
	refreshTeam(t, fc, team)
	if team.Annotations[v1beta1.AnnotationMigrationPhase] != v1beta1.MigrationPhaseMigrated {
		t.Fatalf("after step 6: phase = %q", team.Annotations[v1beta1.AnnotationMigrationPhase])
	}
	if team.Annotations[v1beta1.AnnotationMigratedAt] == "" {
		t.Error("migrated-at not set after final step")
	}

	// Migration finalizer should be gone
	for _, f := range team.Finalizers {
		if f == v1beta1.FinalizerMigration {
			t.Error("migration finalizer should be removed after completion")
		}
	}
}

// --- Test helpers ---

type fakeWorkerBackend struct {
	name      string
	available bool
}

func (f fakeWorkerBackend) Name() string { return f.name }

func (f fakeWorkerBackend) DeploymentMode() string { return backend.DeployCloud }

func (f fakeWorkerBackend) Available(context.Context) bool { return f.available }

func (f fakeWorkerBackend) NeedsCredentialInjection() bool { return false }

func (f fakeWorkerBackend) Create(context.Context, backend.CreateRequest) (*backend.WorkerResult, error) {
	return nil, nil
}

func (f fakeWorkerBackend) Delete(context.Context, string) error { return nil }

func (f fakeWorkerBackend) Start(context.Context, string) error { return nil }

func (f fakeWorkerBackend) Stop(context.Context, string) error { return nil }

func (f fakeWorkerBackend) Status(context.Context, string) (*backend.WorkerResult, error) {
	return nil, nil
}

func refreshTeam(t *testing.T, fc client.Client, team *v1beta1.Team) {
	t.Helper()
	if err := fc.Get(context.Background(), types.NamespacedName{Name: team.Name, Namespace: team.Namespace}, team); err != nil {
		t.Fatalf("refresh team: %v", err)
	}
}

func addWorkerStatusSubresource(t *testing.T, fc client.Client, scheme *runtime.Scheme) {
	t.Helper()
	// The fake client in newer controller-runtime versions handles status subresource
	// automatically when registered via WithStatusSubresource. For Worker CRs created
	// during the test, we just need the status patch to work — the fake client handles it.
}

// noopDeployer implements service.WorkerDeployer with all no-ops.
type noopDeployer struct{}

func (d *noopDeployer) DeployPackage(_ context.Context, _, _ string, _ bool) error { return nil }
func (d *noopDeployer) WriteInlineConfigs(_ string, _ v1beta1.WorkerSpec) error    { return nil }
func (d *noopDeployer) DeployWorkerConfig(_ context.Context, _ service.WorkerDeployRequest) error {
	return nil
}
func (d *noopDeployer) PushOnDemandSkills(_ context.Context, _ string, _ []string, _ []v1beta1.RemoteSkillSource) error {
	return nil
}
func (d *noopDeployer) CleanupOSSData(_ context.Context, _ string) error { return nil }
func (d *noopDeployer) InjectCoordinationContext(_ context.Context, _ service.CoordinationDeployRequest) error {
	return nil
}
func (d *noopDeployer) InjectWorkerCoordination(_ context.Context, _ service.WorkerCoordinationRequest) error {
	return nil
}
func (d *noopDeployer) InjectHeartbeatConfig(_ context.Context, _ service.InjectHeartbeatRequest) error {
	return nil
}
func (d *noopDeployer) InjectChannelPolicy(_ context.Context, _ service.InjectChannelPolicyRequest) error {
	return nil
}
func (d *noopDeployer) EnsureTeamStorage(_ context.Context, _ string) error { return nil }
