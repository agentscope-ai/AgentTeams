package controller

import (
	"context"
	"errors"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRuntimeHandoffReconcileRecoversInterruptedPreflightBeforeReplacement(t *testing.T) {
	store := &service.WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	source := service.WorkerStart{InstanceID: "docker/one", ProcessID: "source-process", Runtime: "hermes"}
	if _, _, err := store.Preflight(t.Context(), "stable", "op", "codex", "codex:fixture", source, ""); err != nil {
		t.Fatal(err)
	}
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default", Annotations: map[string]string{service.WorkerRuntimeHandoffAnnotation: "true"}}, Spec: v1beta1.WorkerSpec{WorkerName: "stable", Runtime: "codex"}}
	// A fresh reconciler must leave the old container alone while draining.
	for i := 0; i < 2; i++ {
		r := &WorkerReconciler{RuntimeStore: store}
		result, err := r.reconcileNormal(t.Context(), w, &MemberState{})
		if err != nil || result.RequeueAfter <= 0 {
			t.Fatalf("missing durable requeue %v %v", result, err)
		}
	}
	state, _, err := store.Load(t.Context(), "stable")
	if err != nil || state.Handoff.Phase != "draining" || state.Handoff.Source != source {
		t.Fatalf("lost drain %v %v", state, err)
	}
}

type runtimeHandoffBackend struct {
	*mocks.MockWorkerBackend
	stopped, removed bool
	stops, removes   int
}

func (b *runtimeHandoffBackend) HandoffTargetImage(runtime string) string {
	return runtime + ":fixture"
}
func (b *runtimeHandoffBackend) StopRuntimeInstance(_ context.Context, name, instance string) (bool, error) {
	b.stops++
	if name != "worker" || instance != "source-boot" {
		return false, errors.New("wrong source")
	}
	return b.stopped, nil
}
func (b *runtimeHandoffBackend) RemoveStoppedRuntime(_ context.Context, name, instance string) (bool, error) {
	b.removes++
	if !b.stopped || name != "worker" || instance != "source-boot" {
		return false, errors.New("premature source removal")
	}
	return b.removed, nil
}

func TestRuntimeHandoffReconcileStopsSourceThenAdoptsOnlyRegisteredTarget(t *testing.T) {
	storage := ossfake.NewMemory()
	store := &service.WorkerRuntimeStore{Storage: storage}
	source := service.WorkerStart{InstanceID: "mock/source-boot", ProcessID: "source-process", Runtime: "hermes"}
	_, _, err := store.Preflight(t.Context(), "stable", "op", "codex", "codex:fixture", source, "")
	if err != nil {
		t.Fatal(err)
	}
	_, etag, err := store.BeginDrain(t.Context(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConfirmCheckpoint(t.Context(), "stable", "op", source, etag, nil); err != nil {
		t.Fatal(err)
	}
	b := &runtimeHandoffBackend{MockWorkerBackend: mocks.NewMockWorkerBackend()}
	reg := backend.NewRegistry([]backend.WorkerBackend{b})
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"}, Spec: v1beta1.WorkerSpec{WorkerName: "stable", Runtime: "hermes", Image: "old-custom:image"}}
	member := &MemberState{ProvResult: &service.WorkerProvisionResult{MatrixToken: "token"}}
	r := &WorkerReconciler{RuntimeStore: store, Backend: reg}
	phase := func(want string) (*service.WorkerHandoff, string) {
		t.Helper()
		record, etag, err := store.Load(t.Context(), "stable")
		if err != nil || record.Handoff.Phase != want {
			t.Fatalf("expected %s, record: %+v %v", want, record, err)
		}
		return record.Handoff, etag
	}
	tick := func(from, to string) {
		t.Helper()
		op, etag := phase(from)
		if _, err := r.reconcileHandoffSource(t.Context(), w, op, etag, member); err != nil {
			t.Fatal(err)
		}
		phase(to)
	}
	tick("checkpointed", "stopping")
	if b.stops != 0 {
		t.Fatal("stopped source before durable intent")
	}
	tick("stopping", "stopping")
	if b.removes != 0 {
		t.Fatal("removed source before exit proof")
	}
	b.stopped = true
	tick("stopping", "stopped")
	tick("stopped", "stopped")
	b.removed = true
	tick("stopped", "creating")
	op, etag := phase("creating")
	if op.TargetImage != "codex:fixture" {
		t.Fatal("source image reused by target")
	}
	mctx := MemberContext{Name: "worker", RuntimeName: "stable", AppliedSpecHash: "targethash", Spec: v1beta1.WorkerSpec{Runtime: "codex", Image: op.TargetImage}}
	deps := MemberDeps{Backend: reg, Provisioner: mocks.NewMockProvisioner(), EnvBuilder: mocks.NewMockEnvBuilder()}
	status := backend.WorkerResult{Status: backend.StatusNotFound}
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) { return &status, nil }
	_, err = r.reconcileHandoffTarget(t.Context(), w, deps, mctx, member, op, etag)
	if err != nil {
		t.Fatal(err)
	}
	req, ok := b.LastCreateReq()
	if !ok || req.Name != "worker" || req.Image != "codex:fixture" || req.Runtime != "codex" || req.Labels[backend.RuntimeHandoffLabel] != op.TargetLabel() || req.Labels[backend.RuntimeHandoffSpecLabel] != "targethash" {
		t.Fatalf("target create %+v", req)
	}
	status = backend.WorkerResult{Status: backend.StatusRunning, InstanceID: "new-boot", Runtime: "codex", HandoffLabel: "foreign"}
	if _, err = r.reconcileHandoffTarget(t.Context(), w, deps, mctx, member, op, etag); err == nil {
		t.Fatal("adopted foreign container")
	}
	status.HandoffLabel = op.TargetLabel()
	status.HandoffSpecHash = "targethash"
	if _, err = r.reconcileHandoffTarget(t.Context(), w, deps, mctx, member, op, etag); err != nil {
		t.Fatal(err)
	}
	phase("creating")
	target := service.WorkerStart{InstanceID: "mock/new-boot", ProcessID: "target-process", Runtime: "codex"}
	ms := service.WorkerMatrixStore{Storage: storage}
	if _, _, err = ms.Start(t.Context(), "stable", target, ""); err != nil {
		t.Fatal(err)
	}
	// Simulate a new controller after Create succeeded but its response was lost.
	r = &WorkerReconciler{RuntimeStore: store, Backend: reg}
	if _, err = r.reconcileHandoffTarget(t.Context(), w, deps, mctx, member, op, etag); err != nil {
		t.Fatal(err)
	}
	phase("creating")
	if w.Status.SpecHash != "targethash" {
		t.Fatal("target became active before its creation hash was durable")
	}
	if _, err = r.reconcileHandoffTarget(t.Context(), w, deps, mctx, member, op, etag); err != nil {
		t.Fatal(err)
	}
	op, _ = phase("recovering")
	if op.Target == nil || *op.Target != target || len(b.Calls.Create) != 1 {
		t.Fatalf("target generation or creation count lost: %+v calls=%v", op, b.Calls.Create)
	}
}

func TestRuntimeHandoffDeclarativeWorkerUpdateEntersDrainBeforeRecreate(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "ns", Generation: 3}, Spec: v1beta1.WorkerSpec{Runtime: "codex", WorkerName: "stable"}, Status: v1beta1.WorkerStatus{SpecHash: "previous", BackendRuntime: "mock"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(w).Build()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	store := &service.WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	ms := service.WorkerMatrixStore{Storage: store.Storage}
	start := service.WorkerStart{InstanceID: "mock/source", ProcessID: "source-process", Runtime: "hermes"}
	b := &runtimeHandoffBackend{MockWorkerBackend: mocks.NewMockWorkerBackend()}
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{InstanceID: "source", Status: backend.StatusRunning, Runtime: "hermes"}, nil
	}
	r := WorkerReconciler{Client: k8s, RuntimeStore: store, Backend: backend.NewRegistry([]backend.WorkerBackend{b})}
	mctx := MemberContext{StatusBackendRuntime: "mock", Spec: w.Spec, AppliedSpecHash: "desired"}
	if handled, err := r.beginDesiredRuntimeHandoff(ctx, w, mctx); err != nil || handled {
		t.Fatalf("unregistered legacy lifecycle changed %v %v", handled, err)
	}
	if _, _, err := ms.Start(ctx, "stable", start, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res, err := r.reconcileNormal(ctx, w, &MemberState{})
		if err != nil || res.RequeueAfter <= 0 {
			t.Fatalf("missing automatic handoff %v %v", res, err)
		}
	}
	op, _, err := store.Load(ctx, "stable")
	if err != nil || op.Handoff == nil || op.Handoff.Phase != "draining" || op.Handoff.Source != start || op.Handoff.TargetRuntime != "codex" || len(b.Calls.Delete) != 0 || len(b.Calls.Create) != 0 || b.stops != 0 {
		t.Fatalf("source was not protected before common reconciliation %+v %v", op, err)
	}
}

type observedHandoffBackend struct {
	*runtimeHandoffBackend
	exit backend.RuntimeExitObservation
}

func (b *observedHandoffBackend) ObserveRuntimeExit(context.Context, string, string) (backend.RuntimeExitObservation, error) {
	return b.exit, nil
}

func TestRuntimeHandoffSourceLostWaitsForIndependentExitAndVerifiedWork(t *testing.T) {
	ctx := t.Context()
	store := &service.WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	start := service.WorkerStart{InstanceID: "mock/source-boot", ProcessID: "source-process", Runtime: "hermes"}
	if _, _, err := store.Preflight(ctx, "stable", "op", "codex", "codex:fixture", start, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginDrain(ctx, "stable"); err != nil {
		t.Fatal(err)
	}
	w := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "test"}, Spec: v1beta1.WorkerSpec{WorkerName: "stable"}}
	b := &observedHandoffBackend{runtimeHandoffBackend: &runtimeHandoffBackend{MockWorkerBackend: mocks.NewMockWorkerBackend()}}
	scans := 0
	refs := []service.WorkerCheckpointRef{{Kind: "task", TeamID: "team", TaskID: "t", Generation: 1, CheckpointVersion: 2, RequiresVerification: true}}
	r := WorkerReconciler{RuntimeStore: store, Backend: backend.NewRegistry([]backend.WorkerBackend{b}), InspectLostHandoff: func(context.Context, *v1beta1.Worker, *service.WorkerHandoff) ([]service.WorkerCheckpointRef, error) {
		scans++
		return refs, nil
	}}
	tick := func(want, reason string) *service.WorkerRuntimeState {
		t.Helper()
		curr, etag, err := store.Load(ctx, "stable")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.reconcileHandoffSource(ctx, w, curr.Handoff, etag, &MemberState{}); err != nil {
			t.Fatal(err)
		}
		curr, _, err = store.Load(ctx, "stable")
		if err != nil || curr.Handoff.Phase != want || curr.Handoff.SourceLossReason != reason {
			t.Fatalf("expected %s/%s got %+v %v", want, reason, curr.Handoff, err)
		}
		return curr
	}
	tick("draining", "") // A live source can still finish its current turn.
	b.exit.Unconfirmed = true
	tick("source_lost", "source_exit_unconfirmed")
	if scans != 0 {
		t.Fatal("unconfirmed source removal triggered a work scan")
	}
	b.exit = backend.RuntimeExitObservation{Exited: true}
	state := tick("source_lost", "execution_requires_verification")
	if state.Handoff.SourceStopObservedAt == "" || !state.Handoff.CheckpointRefs[0].RequiresVerification || b.stops != 0 || b.removes != 0 {
		t.Fatal("unverified orphan was restarted")
	}
	// The authoritative task was then cancelled/reassigned. An independent
	// controller can auto-resume and prepare the target without new Matrix input.
	refs = nil
	state = tick("stopped", "")
	b.removed, b.stopped = true, true
	tick("creating", "")
	if scans != 2 || state.Handoff.TargetImage != "codex:fixture" {
		t.Fatalf("wrong source proof %+v scans=%d", state.Handoff, scans)
	}
}
