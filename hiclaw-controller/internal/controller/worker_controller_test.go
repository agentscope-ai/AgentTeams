package controller

import (
	"context"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestWorkerReconcilerLegacySkipsDecoupledTeamMemberStandaloneState(t *testing.T) {
	ctx := context.Background()
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			MatrixUserID: "@dev:matrix.local",
			RoomID:       "!room-dev:matrix.local",
		},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "lead", Role: RoleTeamLeader.String()},
				{Name: "dev"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker.DeepCopy(), team.DeepCopy()).
		Build()
	legacy, fakeOSS := newTestLegacy(t)
	prov := mocks.NewMockProvisioner()
	prov.MatrixUserIDFn = func(name string) string {
		return "@" + name + ":matrix.local"
	}
	r := &WorkerReconciler{
		Client:      c,
		Provisioner: prov,
		Legacy:      legacy,
	}

	r.reconcileLegacy(ctx, worker, &MemberState{
		ProvResult: &service.WorkerProvisionResult{MatrixUserID: "@dev:matrix.local"},
	})

	if _, err := fakeOSS.GetObject(ctx, "agents/manager/workers-registry.json"); err == nil {
		t.Fatalf("WorkerReconciler must not write standalone workers-registry rows for decoupled Team members")
	}
}
