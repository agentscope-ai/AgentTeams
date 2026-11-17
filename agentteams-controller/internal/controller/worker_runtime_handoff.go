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

func (r *WorkerReconciler) reconcileHandoffSource(ctx context.Context, w *v1beta1.Worker, op *service.WorkerHandoff, etag string, state *MemberState) (reconcile.Result, error) {
	wait := reconcile.Result{RequeueAfter: handoffPoll}
	state.Message = "Worker runtime handoff: " + op.Phase
	if op.Phase == "draining" || op.Phase == "preflight" {
		return wait, nil
	}
	wb, instance, err := r.handoffBackend(ctx, op)
	if err != nil {
		return reconcile.Result{}, err
	}
	source, ok := wb.(backend.RuntimeHandoffSource)
	if !ok {
		return reconcile.Result{}, fmt.Errorf("backend %s cannot prove source termination", wb.Name())
	}
	var next, image string
	switch op.Phase {
	case "checkpointed":
		image = op.TargetImage
		if image == "" {
			image = source.HandoffTargetImage(op.TargetRuntime)
			if w.Spec.Image != "" && (w.Spec.Runtime == op.TargetRuntime || (w.Spec.Runtime == "" && op.Source.Runtime == op.TargetRuntime)) {
				image = w.Spec.Image
			}
		}
		if image == "" {
			return reconcile.Result{}, fmt.Errorf("no configured image for target Harness %s", op.TargetRuntime)
		}
		next = "stopping"
	case "stopping":
		stopped, err := source.StopRuntimeInstance(ctx, w.Name, instance)
		if err != nil || !stopped {
			return wait, err
		}
		next = "stopped"
	case "stopped":
		absent, err := source.RemoveStoppedRuntime(ctx, w.Name, instance)
		if err != nil || !absent {
			return wait, err
		}
		next = "creating"
	default:
		return reconcile.Result{}, fmt.Errorf("unexpected source handoff phase %q", op.Phase)
	}
	_, _, err = r.RuntimeStore.AdvanceLifecycle(ctx, w.Spec.EffectiveWorkerName(w.Name), op.ID, op.Phase, next, etag, image, nil)
	return wait, err
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
		_, _, err = r.RuntimeStore.AdvanceLifecycle(ctx, name, op.ID, op.Phase, "recovering", etag, "", &start)
		return wait, err
	}
	// The normal reconcile may maintain services and token projection again.
	// The original request label still protects adoption after a Controller crash.
	if status.HandoffSpecHash != "" && m.AppliedSpecHash != status.HandoffSpecHash {
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
