package controller

import (
	"context"
	"errors"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestManagerReconcileConditionsShowServiceAccountFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec:       v1beta1.ManagerSpec{Model: "gpt-4o"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(manager).
		WithStatusSubresource(&v1beta1.Manager{}).
		Build()
	prov := mocks.NewMockManagerProvisioner()
	prov.EnsureManagerServiceAccountFn = func(context.Context, string) error {
		return errors.New("manager service account unavailable")
	}
	r := &ManagerReconciler{
		Client:      c,
		Provisioner: prov,
		Deployer:    mocks.NewMockManagerDeployer(),
		Backend:     backend.NewRegistry([]backend.WorkerBackend{mocks.NewMockWorkerBackend()}),
		EnvBuilder:  mocks.NewMockManagerEnvBuilder(),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "default", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error")
	}

	var out v1beta1.Manager
	if err := c.Get(context.Background(), client.ObjectKey{Name: "default", Namespace: "default"}, &out); err != nil {
		t.Fatalf("get manager: %v", err)
	}
	if out.Status.MatrixUserID != "@manager:localhost" {
		t.Fatalf("MatrixUserID=%q, want provisioned manager user", out.Status.MatrixUserID)
	}
	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionInfrastructureReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("InfrastructureReady condition=%+v, want True", cond)
	}
	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionServiceAccountReady); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceAccountFailed" {
		t.Fatalf("ServiceAccountReady condition=%+v, want False ServiceAccountFailed", cond)
	}
	if cond := meta.FindStatusCondition(out.Status.Conditions, v1beta1.ConditionReady); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition=%+v, want False", cond)
	}
	if out.Status.ObservedGeneration != 0 {
		t.Fatalf("ObservedGeneration=%d, want 0 on failed reconcile", out.Status.ObservedGeneration)
	}
}
