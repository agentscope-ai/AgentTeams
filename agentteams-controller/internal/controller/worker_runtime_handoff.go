package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const handoffPoll = 2 * time.Second

func (r *WorkerReconciler) handoffBackend(ctx context.Context, op *service.WorkerHandoff) (backend.WorkerBackend, string, error) {
	backendName, instance, ok := strings.Cut(op.Source.InstanceID, "/")
	if !ok || backendName == "" || instance == "" || r.Backend == nil {
		return nil, "", errors.New("source backend unavailable")
	}
	b, err := r.Backend.GetWorkerBackend(ctx, backendName)
	return b, instance, err
}

// A PUT or direct spec edit on a registered Worker selects the same durable
// operation as the operator endpoint before changing runtime config or deleting
// the source. Unregistered legacy deployments retain their existing lifecycle.
func (r *WorkerReconciler) beginDesiredRuntimeHandoff(ctx context.Context, w *v1beta1.Worker, m MemberContext) (bool, error) {
	if r.Backend == nil || !r.Backend.HasWorkerBackends() {
		return false, nil
	}
	var wb backend.WorkerBackend
	var err error
	if m.StatusBackendRuntime != "" {
		wb, err = resolvePreviousMemberBackend(ctx, r.Backend, m.StatusBackendRuntime)
	} else {
		wb, err = resolveBackendForMember(r.Backend, m.BackendRuntime, m)
	}
	if err != nil {
		return false, err
	}
	actual, err := wb.Status(ctx, w.Name)
	if errors.Is(err, backend.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if actual == nil {
		return false, errors.New("current Worker status unavailable")
	}
	target := backend.ResolveRuntime(m.Spec.Runtime, r.DefaultRuntime)
	if actual.Runtime == target || (actual.Status != backend.StatusRunning && actual.Status != backend.StatusStarting && actual.Status != backend.StatusReady) {
		return false, nil
	}
	if err := r.HandoffMatrix.Validate(ctx, w, target); err != nil {
		return false, err
	}
	name := w.Spec.EffectiveWorkerName(w.Name)
	matrix := service.WorkerMatrixStore{Storage: r.RuntimeStore.Storage}
	consumption, _, err := matrix.Load(ctx, name)
	if err != nil {
		return false, err
	}
	start := consumption.ActiveStart
	if start.InstanceID == "" || (actual.Runtime == "" && start.Runtime == target) {
		return false, nil
	}
	if actual.InstanceID == "" || start.InstanceID != wb.Name()+"/"+actual.InstanceID || start.Runtime != actual.Runtime {
		return false, errors.New("waiting for registration of the current Worker start before switching Harness")
	}
	image, err := service.HandoffTargetImage(w, wb, actual.Runtime, target)
	if err != nil {
		return false, err
	}
	state, etag, err := r.RuntimeStore.Load(ctx, name)
	if err != nil {
		return false, err
	}
	if w.Annotations[service.WorkerRuntimeHandoffAnnotation] == "" {
		before := w.DeepCopy()
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations[service.WorkerRuntimeHandoffAnnotation] = "true"
		if err := r.Patch(ctx, w, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	if state.Handoff == nil || state.Handoff.Phase == "completed" {
		// Reconciles of one declarative generation coalesce across restarts.
		id := fmt.Sprintf("spec-%d-%s", w.Generation, m.AppliedSpecHash)
		if _, _, err := r.RuntimeStore.Preflight(ctx, name, id, target, image, start, etag); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *WorkerReconciler) reconcileHandoffSource(ctx context.Context, w *v1beta1.Worker, op *service.WorkerHandoff, etag string, state *MemberState) (reconcile.Result, error) {
	wait := reconcile.Result{RequeueAfter: handoffPoll}
	state.Message = "Worker runtime handoff: " + op.Phase
	wb, instance, err := r.handoffBackend(ctx, op)
	if err != nil {
		return reconcile.Result{}, err
	}
	source, ok := wb.(backend.RuntimeHandoffSource)
	if !ok {
		return reconcile.Result{}, fmt.Errorf("backend %s cannot prove source termination", wb.Name())
	}
	var next string
	switch op.Phase {
	case "stopping":
		if err := r.HandoffMatrix.Validate(ctx, w, op.TargetRuntime); err != nil {
			return wait, err
		}
		stopped, err := source.StopRuntimeInstance(ctx, w.Name, instance)
		if err != nil {
			return wait, r.checkChangedSource(ctx, w, op, etag, wb, instance, err)
		}
		if !stopped {
			return wait, nil
		}
		next = "stopped"
	case "stopped":
		absent, err := source.RemoveStoppedRuntime(ctx, w.Name, instance)
		if err != nil {
			return wait, r.checkChangedSource(ctx, w, op, etag, wb, instance, err)
		}
		if !absent {
			return wait, nil
		}
		next = "creating"
	default:
		return reconcile.Result{}, fmt.Errorf("unexpected source handoff phase %q", op.Phase)
	}
	_, _, err = r.RuntimeStore.AdvanceLifecycle(ctx, w.Spec.EffectiveWorkerName(w.Name), op.ID, op.Phase, next, etag, nil)
	return wait, err
}

// The daemon may restart the same container between confirmed checkpoint,
// stop and removal. A failed mutation never authorizes a name-based retry.
func (r *WorkerReconciler) checkChangedSource(ctx context.Context, w *v1beta1.Worker, op *service.WorkerHandoff, etag string, wb backend.WorkerBackend, instance string, cause error) error {
	observer, ok := wb.(backend.RuntimeExitObserver)
	if !ok {
		return cause
	}
	exit, err := observer.ObserveRuntimeExit(ctx, w.Name, instance)
	if err != nil || !exit.Exited {
		return cause
	}
	name := w.Spec.EffectiveWorkerName(w.Name)
	if exit.Successor != "" {
		matrix := service.WorkerMatrixStore{Storage: r.RuntimeStore.Storage}
		consumption, _, err := matrix.Load(ctx, name)
		if err != nil {
			return err
		}
		start := consumption.ActiveStart
		if start.InstanceID != wb.Name()+"/"+exit.Successor || start.Runtime != op.Source.Runtime {
			return cause
		}
		_, _, err = r.RuntimeStore.RebindSource(ctx, name, op.ID, etag, op.Source, start)
		return err
	}
	if op.Phase != "stopping" {
		return cause
	}
	_, _, err = r.RuntimeStore.AdvanceLifecycle(ctx, name, op.ID, op.Phase, "stopped", etag, nil)
	return err
}

func (r *WorkerReconciler) applyHandoffTargetSpec(ctx context.Context, w *v1beta1.Worker, op *service.WorkerHandoff) error {
	base := w.DeepCopy()
	if op.TargetRuntime != op.Source.Runtime {
		w.Spec.Image = ""
	}
	w.Spec.Runtime = op.TargetRuntime
	return r.Patch(ctx, w, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// Never call the ordinary spec-change deletion path on the newly created
// target. Adopt a surviving Create only when its handoff label matches.
func (r *WorkerReconciler) reconcileHandoffTarget(ctx context.Context, w *v1beta1.Worker, d MemberDeps, m MemberContext, state *MemberState, op *service.WorkerHandoff, etag string) (reconcile.Result, error) {
	wait := reconcile.Result{RequeueAfter: handoffPoll}
	wb, _, err := r.handoffBackend(ctx, op)
	if err != nil {
		return reconcile.Result{}, err
	}
	status, err := wb.Status(ctx, w.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	if status == nil {
		return reconcile.Result{}, errors.New("target backend status unavailable")
	}
	if status.Status == backend.StatusNotFound {
		if state.ProvResult == nil {
			return wait, errors.New("target Worker provisioning unavailable")
		}
		m.Spec.Image = op.TargetImage
		m.PodLabels = maps.Clone(m.PodLabels)
		if m.PodLabels == nil {
			m.PodLabels = make(map[string]string)
		}
		m.PodLabels[backend.RuntimeHandoffLabel] = op.TargetLabel()
		m.PodLabels[backend.RuntimeHandoffSpecLabel] = m.AppliedSpecHash
		_, err := createMemberContainer(ctx, d, m, state, wb)
		return wait, err
	}
	if status.HandoffLabel != op.TargetLabel() || status.Runtime != op.TargetRuntime {
		return reconcile.Result{}, errors.New("target name occupied by another runtime instance")
	}
	if status.InstanceID == "" || (status.Status != backend.StatusRunning && status.Status != backend.StatusReady && status.Status != backend.StatusStarting) {
		return wait, nil
	}
	matrix := service.WorkerMatrixStore{Storage: r.RuntimeStore.Storage}
	name := w.Spec.EffectiveWorkerName(w.Name)
	consumption, _, err := matrix.Load(ctx, name)
	if err != nil {
		return reconcile.Result{}, err
	}
	start := consumption.ActiveStart
	if start.Runtime != op.TargetRuntime || start.InstanceID != wb.Name()+"/"+status.InstanceID || start.ProcessID == "" {
		return wait, nil
	}
	// Persist the hash of the actual Create before opening target admission.
	// Otherwise an immediately-ready idle target could complete before the
	// status write and be deleted again by the ordinary spec-change path.
	if status.HandoffSpecHash != "" && w.Status.SpecHash != status.HandoffSpecHash {
		w.Status.SpecHash = status.HandoffSpecHash
		return wait, nil
	}
	if op.Target == nil || *op.Target != start {
		_, _, err = r.RuntimeStore.AdvanceLifecycle(ctx, name, op.ID, op.Phase, "recovering", etag, &start)
		return wait, err
	}
	// The normal reconcile may maintain services and token projection again.
	// The original request label still protects adoption after a Controller crash.
	if (status.HandoffSpecHash != "" && m.AppliedSpecHash != status.HandoffSpecHash) || (m.StatusBackendRuntime != "" && m.BackendRuntime != "" && m.StatusBackendRuntime != m.BackendRuntime) {
		return wait, nil // A later edit can reconcile once this handoff finishes.
	}
	m.SpecChanged = false
	return ReconcileMemberContainer(ctx, d, m, state)
}

// Worker deletion must retain its own finalizer until a source Pod's temporary
// observation finalizer has been released, even if deletion interrupted handoff.
func (r *WorkerReconciler) finishDeletingHandoffSource(ctx context.Context, w *v1beta1.Worker) (reconcile.Result, error) {
	if r.RuntimeStore == nil || w.Annotations[service.WorkerRuntimeHandoffAnnotation] == "" {
		return reconcile.Result{}, nil
	}
	state, etag, err := r.RuntimeStore.Load(ctx, w.Spec.EffectiveWorkerName(w.Name))
	if err != nil {
		return reconcile.Result{}, err
	}
	op := state.Handoff
	if op == nil || (op.Phase != "stopping" && op.Phase != "stopped") {
		return reconcile.Result{}, nil
	}
	return r.reconcileHandoffSource(ctx, w, op, etag, &MemberState{})
}
