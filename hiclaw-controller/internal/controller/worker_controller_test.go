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

// TestWorkerMemberContext_StampsControllerAndRoleLabels verifies that a
// standalone Worker CR's derived MemberContext carries hiclaw.io/controller
// and hiclaw.io/role=standalone so the resulting Pod is symmetric with
// Team-managed members and filterable by controller instance.
func TestWorkerMemberContext_StampsControllerAndRoleLabels(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Namespace = "hiclaw"

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels[v1beta1.LabelController]; got != "ctl-x" {
		t.Fatalf("expected controller label ctl-x, got %q (labels=%v)", got, mctx.PodLabels)
	}
	if got := mctx.PodLabels["hiclaw.io/role"]; got != RoleStandalone.String() {
		t.Fatalf("expected role %q, got %q", RoleStandalone.String(), got)
	}
	if _, ok := mctx.PodLabels["hiclaw.io/team"]; ok {
		t.Fatalf("standalone worker must not carry hiclaw.io/team, got %v", mctx.PodLabels)
	}
}

// TestWorkerMemberContext_MergesMetadataAndSpecLabels verifies the
// three-layer merge: CR metadata.labels, CR spec.labels, and the
// controller-forced system labels. spec.labels wins over metadata.labels
// on collision (per project decision — per-CR spec beats per-CR
// metadata) while non-conflicting entries from both layers survive.
func TestWorkerMemberContext_MergesMetadataAndSpecLabels(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.Namespace = "hiclaw"
	w.ObjectMeta.Labels = map[string]string{
		"owner": "alice",
		"team":  "a",
	}
	w.Spec.Labels = map[string]string{
		"env":  "prod",
		"team": "b", // overrides metadata.labels["team"]
	}

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels["owner"]; got != "alice" {
		t.Fatalf("metadata.labels[owner] not propagated: %v", mctx.PodLabels)
	}
	if got := mctx.PodLabels["env"]; got != "prod" {
		t.Fatalf("spec.labels[env] not propagated: %v", mctx.PodLabels)
	}
	if got := mctx.PodLabels["team"]; got != "b" {
		t.Fatalf("spec.labels must override metadata.labels on key collision, got team=%q", got)
	}
}

// TestWorkerMemberContext_SystemLabelsOverrideUser verifies reserved
// keys are silently overridden by controller system labels. Users
// cannot spoof hiclaw.io/controller or hiclaw.io/role by stuffing them
// into metadata.labels or spec.labels — this is the "reserved-override"
// contract.
func TestWorkerMemberContext_SystemLabelsOverrideUser(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "real-ctl"}
	w := &v1beta1.Worker{}
	w.Name = "solo"
	w.ObjectMeta.Labels = map[string]string{
		v1beta1.LabelController: "metadata-attacker",
	}
	w.Spec.Labels = map[string]string{
		v1beta1.LabelController: "spec-attacker",
		"hiclaw.io/role":        "evil",
	}

	mctx := r.workerMemberContext(w)

	if got := mctx.PodLabels[v1beta1.LabelController]; got != "real-ctl" {
		t.Fatalf("system controller label must win over user, got %q (labels=%v)", got, mctx.PodLabels)
	}
	if got := mctx.PodLabels["hiclaw.io/role"]; got != RoleStandalone.String() {
		t.Fatalf("system role label must win over user, got %q", got)
	}
}

// TestWorkerMemberContext_NilLabelsSafe ensures the merge helper
// handles the common case of a Worker CR that has neither
// metadata.labels nor spec.labels without panicking or emitting stray
// empty-map entries.
func TestWorkerMemberContext_NilLabelsSafe(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}
	w := &v1beta1.Worker{}
	w.Name = "solo"

	mctx := r.workerMemberContext(w)

	if mctx.PodLabels[v1beta1.LabelController] != "ctl-x" {
		t.Fatalf("controller label missing on nil-labels Worker: %v", mctx.PodLabels)
	}
	if len(mctx.PodLabels) != 2 {
		t.Fatalf("expected exactly the 2 system labels on nil-labels Worker, got %v", mctx.PodLabels)
	}
}

// TestWorkerMemberContext_SpecChangedGate locks in the brand-new-worker
// guard. The "brand new" case is the load-bearing one: a second reconcile
// queued by the finalizer write can read a stale informer cache and see
// the just-created container as Running while ObservedGeneration is still
// 0. Without the gate, SpecChanged=true on that intervening pass causes
// ensureMemberContainerPresent to Delete (force=true → SIGKILL) the
// container right after first create.
func TestWorkerMemberContext_SpecChangedGate(t *testing.T) {
	r := &WorkerReconciler{ControllerName: "ctl-x"}

	cases := []struct {
		name     string
		gen      int64
		observed int64
		want     bool
	}{
		// Brand-new Worker: never reconciled. Must NOT report SpecChanged
		// even though Generation > ObservedGeneration — that delta is the
		// "we have never observed this resource" signal, not a user edit.
		{"brand_new", 1, 0, false},
		// First reconcile committed: no edit pending.
		{"observed_no_edit", 1, 1, false},
		// User edit after first reconcile: spec genuinely diverged.
		{"observed_with_edit", 2, 1, true},
		// Periodic resync with no spec change.
		{"resync_no_edit", 5, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &v1beta1.Worker{}
			w.Name = "solo"
			w.Generation = tc.gen
			w.Status.ObservedGeneration = tc.observed
			mctx := r.workerMemberContext(w)
			if mctx.SpecChanged != tc.want {
				t.Fatalf("SpecChanged for (gen=%d, observed=%d): got %v, want %v",
					tc.gen, tc.observed, mctx.SpecChanged, tc.want)
			}
		})
	}
}

func TestWorkerReconcileConditionsShowServiceAccountFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "gpt-4o"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker).
		WithStatusSubresource(&v1beta1.Worker{}).
		Build()
	prov := mocks.NewMockProvisioner()
	prov.EnsureServiceAccountFn = func(context.Context, string) error {
		return errors.New("service account unavailable")
	}
	r := &WorkerReconciler{
		Client:      c,
		Provisioner: prov,
		Deployer:    mocks.NewMockDeployer(),
		Backend:     backend.NewRegistry([]backend.WorkerBackend{mocks.NewMockWorkerBackend()}),
		EnvBuilder:  mocks.NewMockEnvBuilder(),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "alice", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error")
	}

	var out v1beta1.Worker
	if err := c.Get(context.Background(), client.ObjectKey{Name: "alice", Namespace: "default"}, &out); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if out.Status.MatrixUserID != "@alice:localhost" {
		t.Fatalf("MatrixUserID=%q, want provisioned user", out.Status.MatrixUserID)
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
