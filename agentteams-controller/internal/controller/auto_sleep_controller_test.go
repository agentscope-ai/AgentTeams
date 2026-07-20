package controller

import (
	"context"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAutoSleepControllerSleepsIdleStandaloneWorker(t *testing.T) {
	scheme := newControllerTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			IdleTimeout: "15m",
		},
		Status: v1beta1.WorkerStatus{
			LastActiveAt: "2026-05-12T10:00:00Z",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	controller := &AutoSleepController{
		Client:    k8sClient,
		Namespace: "default",
		Now:       func() time.Time { return mustParseTime(t, "2026-05-12T10:16:00Z") },
	}

	controller.reconcile(context.Background())

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q, want Sleeping", updated.Spec.DesiredState())
	}
}

func TestAutoSleepControllerSleepsIdleTeamWorker(t *testing.T) {
	scheme := newControllerTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			IdleTimeout: "15m",
		},
		Status: v1beta1.WorkerStatus{
			LastActiveAt: "2026-05-12T10:00:00Z",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: "dev", Role: RoleTeamWorker.String()}},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker, team).Build()
	controller := &AutoSleepController{
		Client:    k8sClient,
		Namespace: "default",
		Now:       func() time.Time { return mustParseTime(t, "2026-05-12T10:16:00Z") },
	}

	controller.reconcile(context.Background())

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("state=%q, want Sleeping", updated.Spec.DesiredState())
	}
}

func newControllerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return parsed
}
