package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// stubMessenger records every SendAdminMessage call for assertions and can be
// configured to fail.
type stubMessenger struct {
	sent []struct{ roomID, body string }
	err  error
}

func (s *stubMessenger) SendAdminMessage(_ context.Context, roomID, body string) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, struct{ roomID, body string }{roomID, body})
	return nil
}

func newProjectScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

type projectTestRig struct {
	client    client.Client
	oss       *ossfake.Memory
	messenger *stubMessenger
	r         *ProjectReconciler
}

func newProjectRig(t *testing.T, objs ...client.Object) *projectTestRig {
	t.Helper()
	scheme := newProjectScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Project{}, &v1beta1.Team{}).
		Build()
	mem := ossfake.NewMemory()
	msgr := &stubMessenger{}
	return &projectTestRig{
		client:    c,
		oss:       mem,
		messenger: msgr,
		r: &ProjectReconciler{
			Client:    c,
			OSS:       mem,
			Messenger: msgr,
		},
	}
}

func (rig *projectTestRig) reconcile(t *testing.T, name string) (reconcile.Result, error) {
	t.Helper()
	return rig.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
}

func (rig *projectTestRig) getProject(t *testing.T, name string) *v1beta1.Project {
	t.Helper()
	var proj v1beta1.Project
	if err := rig.client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &proj); err != nil {
		t.Fatalf("get project %s: %v", name, err)
	}
	return &proj
}

func baseProject(name, team string) *v1beta1.Project {
	return &v1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.ProjectSpec{
			Team: team,
			Repos: []v1beta1.ProjectRepo{
				{URL: "https://git.pawcommit.com/org/repo.git", Access: "rw"},
			},
		},
	}
}

func teamWithMembers(name string, members ...v1beta1.TeamMemberStatus) *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{Name: "lead"},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID: "!team-" + name + ":localhost",
			Members:    members,
		},
	}
}

// --- Happy path ---

func TestProjectReconcile_HappyPath(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	proj := baseProject("proj-1", "alpha-team")
	rig := newProjectRig(t, team, proj)

	// First reconcile: adds finalizer, requeues immediately via the patch.
	if _, err := rig.reconcile(t, "proj-1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "proj-1")
	if got.Status.Phase != "Ready" {
		t.Fatalf("phase = %q, want Ready: %+v", got.Status.Phase, got.Status)
	}
	if got.Status.RepoCount != 1 {
		t.Fatalf("repoCount = %d, want 1", got.Status.RepoCount)
	}
	if len(got.Status.RecordedWorkers) != 1 || got.Status.RecordedWorkers[0] != "lead-runtime" {
		t.Fatalf("recordedWorkers = %+v, want [lead-runtime]", got.Status.RecordedWorkers)
	}

	for _, condType := range []string{ConditionReposResolved, ConditionWorkersRecorded, ConditionMinIOProjected, ConditionLeaderNotified} {
		cond := got.Status.ConditionByType(condType)
		if cond == nil || cond.Status != "True" {
			t.Errorf("condition %s = %+v, want True", condType, cond)
		}
	}

	// Manifest shape assertions.
	data, err := rig.oss.GetObject(context.Background(), "shared/projects/proj-1/manifest.json")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest["id"] != "proj-1" || manifest["team"] != "alpha-team" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, hasCreds := manifest["pat"]; hasCreds {
		t.Fatalf("manifest must not carry credential material: %+v", manifest)
	}
	repos, ok := manifest["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("manifest repos = %+v, want 1 entry", manifest["repos"])
	}

	if len(rig.messenger.sent) != 1 {
		t.Fatalf("expected exactly 1 admin message sent, got %d: %+v", len(rig.messenger.sent), rig.messenger.sent)
	}
	if rig.messenger.sent[0].roomID != "!team-alpha-team:localhost" {
		t.Fatalf("unexpected roomID: %q", rig.messenger.sent[0].roomID)
	}
}

// --- Missing team ---

func TestProjectReconcile_MissingTeam(t *testing.T) {
	proj := baseProject("proj-2", "does-not-exist")
	rig := newProjectRig(t, proj)

	res, err := rig.reconcile(t, "proj-2")
	if err == nil {
		t.Fatal("expected error for missing team")
	}
	if res.RequeueAfter != reconcileRetryDelay {
		t.Fatalf("requeue = %v, want %v", res.RequeueAfter, reconcileRetryDelay)
	}
	got := rig.getProject(t, "proj-2")
	if got.Status.Phase != "Degraded" {
		t.Fatalf("phase = %q, want Degraded", got.Status.Phase)
	}
	cond := got.Status.ConditionByType(ConditionReposResolved)
	if cond == nil || cond.Status != "False" || cond.Reason != "TeamNotFound" {
		t.Fatalf("ReposResolved condition = %+v, want False/TeamNotFound", cond)
	}
	if got.Status.Message == "" {
		t.Fatalf("expected status.message to carry the error")
	}
}

// --- Empty spec.workers recorded from Team.Status.Members ---

func TestProjectReconcile_EmptyWorkersRecordedFromTeam(t *testing.T) {
	team := teamWithMembers("alpha-team",
		v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"},
		v1beta1.TeamMemberStatus{Name: "dev", RuntimeName: "dev-runtime", Role: "worker"},
	)
	proj := baseProject("proj-3", "alpha-team")
	rig := newProjectRig(t, team, proj)

	if _, err := rig.reconcile(t, "proj-3"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "proj-3")
	if len(got.Status.RecordedWorkers) != 2 {
		t.Fatalf("recordedWorkers = %+v, want 2 entries", got.Status.RecordedWorkers)
	}
	want := map[string]bool{"lead-runtime": true, "dev-runtime": true}
	for _, w := range got.Status.RecordedWorkers {
		if !want[w] {
			t.Errorf("unexpected recorded worker %q", w)
		}
	}
}

// spec.workers explicit list is honored verbatim (not overridden by Team.Status.Members).
func TestProjectReconcile_ExplicitWorkersHonored(t *testing.T) {
	team := teamWithMembers("alpha-team",
		v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"},
		v1beta1.TeamMemberStatus{Name: "dev", RuntimeName: "dev-runtime", Role: "worker"},
	)
	proj := baseProject("proj-4", "alpha-team")
	proj.Spec.Workers = []string{"dev-runtime"}
	rig := newProjectRig(t, team, proj)

	if _, err := rig.reconcile(t, "proj-4"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "proj-4")
	if len(got.Status.RecordedWorkers) != 1 || got.Status.RecordedWorkers[0] != "dev-runtime" {
		t.Fatalf("recordedWorkers = %+v, want [dev-runtime]", got.Status.RecordedWorkers)
	}
}

// --- Delete: DeletePrefix + finalizer removed even when OSS errors ---

func TestProjectReconcile_DeleteRemovesFinalizerEvenOnOSSError(t *testing.T) {
	team := teamWithMembers("alpha-team")
	proj := baseProject("proj-5", "alpha-team")
	rig := newProjectRig(t, team, proj)

	// First reconcile to add finalizer + project manifest.
	if _, err := rig.reconcile(t, "proj-5"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if _, err := rig.oss.GetObject(context.Background(), "shared/projects/proj-5/manifest.json"); err != nil {
		t.Fatalf("expected manifest to exist before delete: %v", err)
	}

	// Simulate a broken OSS backend for the delete pass.
	rig.r.OSS = &erroringOSS{Memory: rig.oss}

	got := rig.getProject(t, "proj-5")
	now := metav1.Now()
	got.DeletionTimestamp = &now
	if err := rig.client.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := rig.reconcile(t, "proj-5"); err != nil {
		t.Fatalf("delete-pass reconcile returned error (should be non-fatal): %v", err)
	}

	var after v1beta1.Project
	err := rig.client.Get(context.Background(), client.ObjectKey{Name: "proj-5", Namespace: "default"}, &after)
	if err == nil {
		t.Fatalf("expected project to be gone after finalizer removal, got %+v", after)
	}
}

// --- LeaderNotified fires exactly once across multiple reconciles ---

func TestProjectReconcile_LeaderNotifiedOnce(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	proj := baseProject("proj-6", "alpha-team")
	rig := newProjectRig(t, team, proj)

	for i := 0; i < 3; i++ {
		if _, err := rig.reconcile(t, "proj-6"); err != nil {
			t.Fatalf("reconcile #%d: %v", i, err)
		}
	}

	if len(rig.messenger.sent) != 1 {
		t.Fatalf("expected exactly 1 notification across 3 reconciles, got %d", len(rig.messenger.sent))
	}
}

// TestProjectReconcile_LeaderNotifiedRetriesUntilTeamRoomReady covers the race
// where a Project reaches MinIOProjected=True before the Team reconciler has
// asynchronously populated Status.TeamRoomID. The first successful projection
// pass must NOT mark LeaderNotified=True (there is no room to post to yet),
// and a later reconcile — once TeamRoomID is populated — must still deliver
// exactly one notification. Regression test for the bug where the leader
// notification was gated on the MinIOProjected False->True edge only, so a
// project that projected successfully before the team room existed would
// never notify the lead on any subsequent reconcile.
func TestProjectReconcile_LeaderNotifiedRetriesUntilTeamRoomReady(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	team.Status.TeamRoomID = "" // team room not provisioned yet
	proj := baseProject("proj-7", "alpha-team")
	rig := newProjectRig(t, team, proj)

	if _, err := rig.reconcile(t, "proj-7"); err != nil {
		t.Fatalf("reconcile #1: %v", err)
	}
	if len(rig.messenger.sent) != 0 {
		t.Fatalf("expected 0 notifications while team room is unprovisioned, got %d", len(rig.messenger.sent))
	}
	got := rig.getProject(t, "proj-7")
	if c := got.Status.ConditionByType(ConditionMinIOProjected); c == nil || c.Status != conditionTrue {
		t.Fatalf("expected MinIOProjected=True after reconcile #1, got %+v", c)
	}
	if c := got.Status.ConditionByType(ConditionLeaderNotified); c != nil && c.Status == conditionTrue {
		t.Fatalf("expected LeaderNotified to not be True while team room is unprovisioned, got %+v", c)
	}

	// Team room becomes available (simulating the Team reconciler catching up).
	var liveTeam v1beta1.Team
	if err := rig.client.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &liveTeam); err != nil {
		t.Fatalf("get team: %v", err)
	}
	liveTeam.Status.TeamRoomID = "!team-alpha-team:localhost"
	if err := rig.client.Status().Update(context.Background(), &liveTeam); err != nil {
		t.Fatalf("update team status: %v", err)
	}

	if _, err := rig.reconcile(t, "proj-7"); err != nil {
		t.Fatalf("reconcile #2: %v", err)
	}
	if len(rig.messenger.sent) != 1 {
		t.Fatalf("expected exactly 1 notification once team room is provisioned, got %d", len(rig.messenger.sent))
	}
	got = rig.getProject(t, "proj-7")
	if c := got.Status.ConditionByType(ConditionLeaderNotified); c == nil || c.Status != conditionTrue {
		t.Fatalf("expected LeaderNotified=True after reconcile #2, got %+v", c)
	}

	// A third reconcile must not send a duplicate notification.
	if _, err := rig.reconcile(t, "proj-7"); err != nil {
		t.Fatalf("reconcile #3: %v", err)
	}
	if len(rig.messenger.sent) != 1 {
		t.Fatalf("expected still exactly 1 notification after reconcile #3, got %d", len(rig.messenger.sent))
	}
}

// --- Completed -> DeprovisionPending ---

func TestProjectReconcile_CompletedRaisesDeprovisionPending(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	proj := baseProject("proj-7", "alpha-team")
	rig := newProjectRig(t, team, proj)

	if _, err := rig.reconcile(t, "proj-7"); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// Operator sets phase to Completed directly on the CR status (simulates
	// the REST handler's operator-set-phase path).
	got := rig.getProject(t, "proj-7")
	got.Status.Phase = "Completed"
	if err := rig.client.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("set completed: %v", err)
	}

	if _, err := rig.reconcile(t, "proj-7"); err != nil {
		t.Fatalf("reconcile after completed: %v", err)
	}

	after := rig.getProject(t, "proj-7")
	if after.Status.Phase != "Completed" {
		t.Fatalf("phase = %q, want Completed to be preserved (operator-set, never reconciler-computed)", after.Status.Phase)
	}
	cond := after.Status.ConditionByType(ConditionDeprovisionPending)
	if cond == nil || cond.Status != "True" {
		t.Fatalf("DeprovisionPending = %+v, want True", cond)
	}
}

func TestProjectReconcile_DependenciesSatisfiedWhenUpstreamReady(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	upstream := baseProject("upstream", "alpha-team")
	upstream.Status.Phase = "Ready"
	downstream := baseProject("downstream", "alpha-team")
	downstream.Spec.DependsOn = []string{"upstream"}
	rig := newProjectRig(t, team, upstream, downstream)

	if _, err := rig.reconcile(t, "downstream"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "downstream")
	if len(got.Status.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want 1 entry", got.Status.Dependencies)
	}
	dep := got.Status.Dependencies[0]
	if dep.Project != "upstream" || dep.Phase != "Ready" || !dep.Satisfied {
		t.Fatalf("dependency = %+v, want upstream Ready satisfied", dep)
	}
}

func TestProjectReconcile_DependenciesUnsatisfiedWhenUpstreamProvisioning(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	upstream := baseProject("upstream", "alpha-team")
	upstream.Status.Phase = "Provisioning"
	downstream := baseProject("downstream", "alpha-team")
	downstream.Spec.DependsOn = []string{"upstream"}
	rig := newProjectRig(t, team, upstream, downstream)

	if _, err := rig.reconcile(t, "downstream"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "downstream")
	if len(got.Status.Dependencies) != 1 || got.Status.Dependencies[0].Satisfied {
		t.Fatalf("dependencies = %+v, want unsatisfied upstream", got.Status.Dependencies)
	}
}

func TestProjectReconcile_DependenciesSatisfiedForCompletedAndArchived(t *testing.T) {
	for _, phase := range []string{"Completed", "Archived"} {
		t.Run(phase, func(t *testing.T) {
			team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
			upstream := baseProject("upstream", "alpha-team")
			upstream.Status.Phase = phase
			downstream := baseProject("downstream", "alpha-team")
			downstream.Spec.DependsOn = []string{"upstream"}
			rig := newProjectRig(t, team, upstream, downstream)

			if _, err := rig.reconcile(t, "downstream"); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := rig.getProject(t, "downstream")
			if len(got.Status.Dependencies) != 1 || !got.Status.Dependencies[0].Satisfied || got.Status.Dependencies[0].Phase != phase {
				t.Fatalf("dependencies = %+v, want satisfied %s", got.Status.Dependencies, phase)
			}
		})
	}
}

func TestProjectReconcile_DependenciesMissingUpstream(t *testing.T) {
	team := teamWithMembers("alpha-team", v1beta1.TeamMemberStatus{Name: "lead", RuntimeName: "lead-runtime", Role: "team_leader"})
	downstream := baseProject("downstream", "alpha-team")
	downstream.Spec.DependsOn = []string{"missing-upstream"}
	rig := newProjectRig(t, team, downstream)

	if _, err := rig.reconcile(t, "downstream"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := rig.getProject(t, "downstream")
	if len(got.Status.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want 1 entry", got.Status.Dependencies)
	}
	dep := got.Status.Dependencies[0]
	if dep.Project != "missing-upstream" || dep.Phase != "Missing" || dep.Satisfied {
		t.Fatalf("dependency = %+v, want Missing unsatisfied", dep)
	}
}

// erroringOSS wraps ossfake.Memory but fails every DeletePrefix call, used to
// prove reconcileDelete removes the finalizer even when the projection
// cleanup fails (non-fatal contract, mirrors human_reconcile_delete.go).
type erroringOSS struct {
	*ossfake.Memory
}

func (e *erroringOSS) DeletePrefix(_ context.Context, _ string) error {
	return errors.New("simulated MinIO outage")
}
