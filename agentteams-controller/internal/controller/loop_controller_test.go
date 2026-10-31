package controller

import (
	"context"
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func newLoopScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	return s
}

func newLoopRig(t *testing.T, objs ...client.Object) *LoopReconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newLoopScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.Loop{}).
		Build()
	return &LoopReconciler{Client: c}
}

func testLoop() *v1beta1.Loop {
	return &v1beta1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "loop-1", Namespace: "default"},
		Spec:       v1beta1.LoopSpec{TaskID: "task-1", LoopType: "interval", Schedule: "300"},
	}
}

func testLoopRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "loop-1"}}
}

func TestLoopReconcileInitializesStatus(t *testing.T) {
	ctx := context.Background()
	r := newLoopRig(t, testLoop())

	if _, err := r.Reconcile(ctx, testLoopRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got v1beta1.Loop
	if err := r.Get(ctx, testLoopRequest().NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.State != "Pending" {
		t.Errorf("status.state = %q, want %q", got.Status.State, "Pending")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want generation %d", got.Status.ObservedGeneration, got.Generation)
	}
}

func TestLoopReconcileIdempotent(t *testing.T) {
	ctx := context.Background()
	r := newLoopRig(t, testLoop())

	if _, err := r.Reconcile(ctx, testLoopRequest()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var first v1beta1.Loop
	if err := r.Get(ctx, testLoopRequest().NamespacedName, &first); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}

	if _, err := r.Reconcile(ctx, testLoopRequest()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var second v1beta1.Loop
	if err := r.Get(ctx, testLoopRequest().NamespacedName, &second); err != nil {
		t.Fatalf("Get after second reconcile: %v", err)
	}

	if second.ResourceVersion != first.ResourceVersion {
		t.Errorf("resourceVersion changed on idempotent reconcile: %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
	if !reflect.DeepEqual(first.Status, second.Status) {
		t.Errorf("status changed on idempotent reconcile: %+v -> %+v", first.Status, second.Status)
	}
}

func TestLoopReconcileNotFound(t *testing.T) {
	r := newLoopRig(t) // no objects

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"},
	})
	if err != nil {
		t.Fatalf("expected nil error for a deleted Loop, got %v", err)
	}
}
