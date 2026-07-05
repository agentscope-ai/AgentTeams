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

func TestCREnricher_DecoupledWorkerMemberUsesTeamRef(t *testing.T) {
	scheme := newAuthTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-worker-dev",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			WorkerName: "dev",
		},
	}
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-worker-lead", Role: "team_leader"},
		{Name: "alpha-worker-dev"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker, team).
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

	if identity.Role != RoleWorker || identity.Team != "alpha-team" {
		t.Fatalf("identity=%+v, want decoupled worker in alpha-team", identity)
	}
	if identity.WorkerName != "dev" {
		t.Fatalf("WorkerName=%q, want dev", identity.WorkerName)
	}
}

func TestCREnricher_DecoupledLeaderUsesTeamRef(t *testing.T) {
	scheme := newAuthTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha-worker-lead",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			WorkerName: "lead",
		},
	}
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-worker-lead", Role: "team_leader"},
		{Name: "alpha-worker-dev"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker, team).
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

	if identity.Role != RoleTeamLeader || identity.Team != "alpha-team" {
		t.Fatalf("identity=%+v, want decoupled team leader in alpha-team", identity)
	}
	if identity.WorkerName != "lead" {
		t.Fatalf("WorkerName=%q, want lead", identity.WorkerName)
	}
}

func TestCREnricher_TeamMemberKeepsCRNameAndStoresRuntimeWorkerName(t *testing.T) {
	scheme := newAuthTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader = v1beta1.LeaderSpec{
		Name:       "alpha-worker-lead",
		WorkerName: "lead",
	}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{
		{Name: "alpha-worker-dev", WorkerName: "dev"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team).
		WithIndex(&v1beta1.Team{}, teamLeaderNameField, indexTeamLeaderNames).
		WithIndex(&v1beta1.Team{}, teamWorkerNameField, indexTeamWorkerNames).
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
		t.Fatalf("Team=%q, want alpha-team", identity.Team)
	}
	if identity.Username != "alpha-worker-dev" || identity.WorkerName != "dev" {
		t.Fatalf("identity=%+v, want CR username alpha-worker-dev and runtime workerName dev", identity)
	}
}

func TestCREnricher_TeamLeaderKeepsCRNameAndStoresRuntimeWorkerName(t *testing.T) {
	scheme := newAuthTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader = v1beta1.LeaderSpec{
		Name:       "alpha-worker-lead",
		WorkerName: "lead",
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team).
		WithIndex(&v1beta1.Team{}, teamLeaderNameField, indexTeamLeaderNames).
		WithIndex(&v1beta1.Team{}, teamWorkerNameField, indexTeamWorkerNames).
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

	if identity.Role != RoleTeamLeader || identity.Team != "alpha-team" {
		t.Fatalf("identity=%+v, want team leader in alpha-team", identity)
	}
	if identity.Username != "alpha-worker-lead" || identity.WorkerName != "lead" {
		t.Fatalf("identity=%+v, want CR username alpha-worker-lead and runtime workerName lead", identity)
	}
}

func TestCREnricher_RemoteWorkerRequiresMatchingTarget(t *testing.T) {
	scheme := newAuthTestScheme(t)
	remoteMode := v1beta1.DeployModeRemote
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			DeployMode: &remoteMode,
			TargetCluster: &v1beta1.TargetClusterSpec{
				ID:        "remote-cluster",
				Namespace: "remote-ns",
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
		ClusterID:               "remote-cluster",
		ServiceAccountNamespace: "remote-ns",
		ServiceAccountName:      "hiclaw-worker-alice",
	}
	if err := enricher.EnrichIdentity(context.Background(), identity); err != nil {
		t.Fatalf("EnrichIdentity: %v", err)
	}
}

func TestCREnricher_RemoteWorkerPrefersStatusTarget(t *testing.T) {
	scheme := newAuthTestScheme(t)
	remoteMode := v1beta1.DeployModeRemote
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice",
			Namespace: "default",
		},
		Spec: v1beta1.WorkerSpec{
			DeployMode: &remoteMode,
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

	oldTargetIdentity := &CallerIdentity{
		Role:                    RoleWorker,
		Username:                "alice",
		WorkerName:              "alice",
		ClusterID:               "old-cluster",
		ServiceAccountNamespace: "old-ns",
		ServiceAccountName:      "hiclaw-worker-alice",
	}
	if err := enricher.EnrichIdentity(context.Background(), oldTargetIdentity); err != nil {
		t.Fatalf("EnrichIdentity old target: %v", err)
	}

	newTargetIdentity := &CallerIdentity{
		Role:                    RoleWorker,
		Username:                "alice",
		WorkerName:              "alice",
		ClusterID:               "new-cluster",
		ServiceAccountNamespace: "new-ns",
		ServiceAccountName:      "hiclaw-worker-alice",
	}
	if err := enricher.EnrichIdentity(context.Background(), newTargetIdentity); err == nil {
		t.Fatal("expected spec-only new target to be rejected while status still pins old target")
	}
}

func TestCREnricher_RemoteWorkerRejectsTargetMismatch(t *testing.T) {
	scheme := newAuthTestScheme(t)
	remoteMode := v1beta1.DeployModeRemote
	localMode := v1beta1.DeployModeLocal

	tests := []struct {
		name     string
		worker   *v1beta1.Worker
		identity *CallerIdentity
	}{
		{
			name: "wrong namespace",
			worker: &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
				Spec: v1beta1.WorkerSpec{
					DeployMode:    &remoteMode,
					TargetCluster: &v1beta1.TargetClusterSpec{ID: "remote-cluster", Namespace: "remote-ns"},
				},
			},
			identity: &CallerIdentity{
				Role: RoleWorker, Username: "alice", WorkerName: "alice", ClusterID: "remote-cluster",
				ServiceAccountNamespace: "other-ns", ServiceAccountName: "hiclaw-worker-alice",
			},
		},
		{
			name: "wrong cluster",
			worker: &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
				Spec: v1beta1.WorkerSpec{
					DeployMode:    &remoteMode,
					TargetCluster: &v1beta1.TargetClusterSpec{ID: "remote-cluster", Namespace: "remote-ns"},
				},
			},
			identity: &CallerIdentity{
				Role: RoleWorker, Username: "alice", WorkerName: "alice", ClusterID: "other-cluster",
				ServiceAccountNamespace: "remote-ns", ServiceAccountName: "hiclaw-worker-alice",
			},
		},
		{
			name: "wrong service account",
			worker: &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
				Spec: v1beta1.WorkerSpec{
					DeployMode:    &remoteMode,
					TargetCluster: &v1beta1.TargetClusterSpec{ID: "remote-cluster", Namespace: "remote-ns"},
				},
			},
			identity: &CallerIdentity{
				Role: RoleWorker, Username: "alice", WorkerName: "alice", ClusterID: "remote-cluster",
				ServiceAccountNamespace: "remote-ns", ServiceAccountName: "hiclaw-worker-bob",
			},
		},
		{
			name: "local worker",
			worker: &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
				Spec:       v1beta1.WorkerSpec{DeployMode: &localMode},
			},
			identity: &CallerIdentity{
				Role: RoleWorker, Username: "alice", WorkerName: "alice", ClusterID: "remote-cluster",
				ServiceAccountNamespace: "remote-ns", ServiceAccountName: "hiclaw-worker-alice",
			},
		},
		{
			name:   "missing worker cr",
			worker: nil,
			identity: &CallerIdentity{
				Role: RoleWorker, Username: "alice", WorkerName: "alice", ClusterID: "remote-cluster",
				ServiceAccountNamespace: "remote-ns", ServiceAccountName: "hiclaw-worker-alice",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.worker != nil {
				builder = builder.WithObjects(tc.worker)
			}
			enricher := NewCREnricher(builder.Build(), "default")

			if err := enricher.EnrichIdentity(context.Background(), tc.identity); err == nil {
				t.Fatal("expected remote identity mismatch to fail")
			}
		})
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

func indexTeamLeaderNames(obj client.Object) []string {
	team, ok := obj.(*v1beta1.Team)
	if !ok || team.Spec.Leader.Name == "" {
		return nil
	}
	return []string{team.Spec.Leader.Name}
}

func indexTeamWorkerNames(obj client.Object) []string {
	team, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(team.Spec.Workers))
	for _, w := range team.Spec.Workers {
		if w.Name != "" {
			names = append(names, w.Name)
		}
	}
	return names
}

func indexTeamWorkerMemberNames(obj client.Object) []string {
	team, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(team.Spec.WorkerMembers))
	for _, ref := range team.Spec.WorkerMembers {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	return names
}
