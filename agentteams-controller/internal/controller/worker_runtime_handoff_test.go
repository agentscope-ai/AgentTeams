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
