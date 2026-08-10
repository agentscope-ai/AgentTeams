package auth

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
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

func TestCREnricher_TeamReferenceWorkerMemberUsesTeamRef(t *testing.T) {
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
		t.Fatalf("identity=%+v, want Team member Worker in alpha-team", identity)
	}
	if identity.WorkerName != "dev" {
		t.Fatalf("WorkerName=%q, want dev", identity.WorkerName)
	}
}

func TestCREnricher_TeamReferenceLeaderUsesTeamRef(t *testing.T) {
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
		t.Fatalf("identity=%+v, want Team-reference leader in alpha-team", identity)
	}
	if identity.WorkerName != "lead" {
		t.Fatalf("WorkerName=%q, want lead", identity.WorkerName)
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
	for _, ref := range team.Spec.WorkerMembers {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	return names
}
