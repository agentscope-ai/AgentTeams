package auth

import (
	"context"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCREnricher_StandaloneWorkerKeepsCRNameAndStoresRuntimeWorkerName(t *testing.T) {
	scheme := newAuthTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-worker-testmcp",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			WorkerName: "testmcp",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker).
		Build()
	enricher := NewCREnricher(k8sClient, "default")

	identity := &CallerIdentity{
		Role:       RoleWorker,
		Username:   "alpha-worker-testmcp",
		WorkerName: "alpha-worker-testmcp",
	}
	if err := enricher.EnrichIdentity(context.Background(), identity); err != nil {
		t.Fatalf("EnrichIdentity: %v", err)
	}

	if identity.Username != "alpha-worker-testmcp" || identity.WorkerName != "testmcp" {
		t.Fatalf("identity=%+v, want CR username alpha-worker-testmcp and runtime workerName testmcp", identity)
	}
}

func newAuthTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func indexTeamWorkerMemberNames(obj client.Object) []string {
	team, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(team.Spec.WorkerMembers))
	for _, m := range team.Spec.WorkerMembers {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names
}

func authStringPtr(s string) *string { return &s }

// TestCREnricher_DecoupledTeamMemberLookedUpViaWorkerMembersIndex covers
// Gap 1: in the decoupled model the Worker CR carries no agentteams.io/team
// annotation. The enricher must reverse-resolve membership by listing
// Team CRs whose spec.workerMembers[].name matches the caller username.
func TestCREnricher_DecoupledTeamMemberLookedUpViaWorkerMembersIndex(t *testing.T) {
	scheme := newAuthTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-worker-dev", Role: "worker"},
	}

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-worker-dev",
			Namespace: "default",
			// Intentionally no agentteams.io/team annotation: decoupled
			// path must work purely via Team CR reverse-lookup.
		},
		Spec: v1beta1.WorkerSpec{WorkerName: "dev"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, worker).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()
	enricher := NewCREnricher(k8sClient, "default")

	identity := &CallerIdentity{
		Role:       RoleWorker,
		Username:   "alpha-worker-dev",
		WorkerName: "alpha-worker-dev",
	}
	if err := enricher.EnrichIdentity(context.Background(), identity); err != nil {
		t.Fatalf("EnrichIdentity: %v", err)
	}

	if identity.Team != "alpha-team" {
		t.Fatalf("Team=%q, want alpha-team (resolved via spec.workerMembers index)", identity.Team)
	}
	if identity.Role != RoleWorker {
		t.Fatalf("Role=%q, want worker (member, not leader)", identity.Role)
	}
	if identity.WorkerName != "dev" {
		t.Fatalf("WorkerName=%q, want dev (runtime name from Worker CR spec)", identity.WorkerName)
	}
}

func TestCREnricher_RemoteWorkerUsesAppliedStatusTarget(t *testing.T) {
	scheme := newAuthTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			DeployMode: authStringPtr(v1beta1.DeployModeRemote),
			TargetCluster: &v1beta1.TargetClusterSpec{
				ID:        "new-cluster",
				Namespace: "new-ns",
			},
		},
		Status: v1beta1.WorkerStatus{
			DeployMode: v1beta1.DeployModeRemote,
			TargetCluster: &v1beta1.TargetClusterSpec{
				ID:        "old-cluster",
				Namespace: "old-ns",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker).
		Build()
	enricher := NewCREnricher(k8sClient, "default")

	identity := &CallerIdentity{
		Role:                    RoleWorker,
		Username:                "alice",
		WorkerName:              "alice",
		ClusterID:               "old-cluster",
		ServiceAccountNamespace: "old-ns",
		ServiceAccountName:      "agentteams-worker-alice",
	}
	if err := enricher.EnrichIdentity(context.Background(), identity); err != nil {
		t.Fatalf("EnrichIdentity old applied target: %v", err)
	}

	identity.ClusterID = "new-cluster"
	identity.ServiceAccountNamespace = "new-ns"
	if err := enricher.EnrichIdentity(context.Background(), identity); err == nil {
		t.Fatalf("EnrichIdentity accepted unapplied spec target, want status-pinned target")
	}
}

func TestCREnricher_DecoupledTeamLeaderLookedUpViaWorkerMembersIndex(t *testing.T) {
	scheme := newAuthTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-worker-lead", Role: "team_leader"},
	}

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-worker-lead",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{WorkerName: "lead"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, worker).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()
	enricher := NewCREnricher(k8sClient, "default")

	identity := &CallerIdentity{
		Role:       RoleWorker,
		Username:   "alpha-worker-lead",
		WorkerName: "alpha-worker-lead",
	}
	if err := enricher.EnrichIdentity(context.Background(), identity); err != nil {
		t.Fatalf("EnrichIdentity: %v", err)
	}

	if identity.Role != RoleTeamLeader {
		t.Fatalf("Role=%q, want team_leader (resolved via spec.workerMembers index)", identity.Role)
	}
	if identity.Team != "alpha-team" {
		t.Fatalf("Team=%q, want alpha-team", identity.Team)
	}
	if identity.WorkerName != "lead" {
		t.Fatalf("WorkerName=%q, want lead", identity.WorkerName)
	}
}

// TestLookupWorkerTeam_StandaloneWorkerReturnsEmpty asserts the helper
// returns "" when the worker is not referenced from any Team CR — and
// therefore must be treated as standalone by all consumers.
func TestLookupWorkerTeam_StandaloneWorkerReturnsEmpty(t *testing.T) {
	scheme := newAuthTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()

	if team := LookupWorkerTeam(context.Background(), k8sClient, "default", "solo"); team != "" {
		t.Fatalf("LookupWorkerTeam(solo) = %q, want empty (standalone worker)", team)
	}
	if team, isLeader := LookupWorkerTeamRole(context.Background(), k8sClient, "default", "solo"); team != "" || isLeader {
		t.Fatalf("LookupWorkerTeamRole(solo) = (%q,%v), want (\"\",false)", team, isLeader)
	}
}

func TestLookupWorkerTeamRole_UsesWorkerMembers(t *testing.T) {
	scheme := newAuthTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "new-lead", Role: "team_leader"},
		{Name: "dev", Role: "worker"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()

	if teamName, isLeader := LookupWorkerTeamRole(context.Background(), k8sClient, "default", "new-lead"); teamName != "alpha-team" || !isLeader {
		t.Fatalf("LookupWorkerTeamRole(new-lead) = (%q,%v), want (alpha-team,true)", teamName, isLeader)
	}
	if teamName, isLeader := LookupWorkerTeamRole(context.Background(), k8sClient, "default", "dev"); teamName != "alpha-team" || isLeader {
		t.Fatalf("LookupWorkerTeamRole(dev) = (%q,%v), want (alpha-team,false)", teamName, isLeader)
	}
}
