package controller

import (
	"context"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// LoopReconciler reconciles Loop resources.
//
// It is the sole writer of Loop.Status. This pass only initializes status
// (state empty -> "Pending", observedGeneration -> metadata.generation) so
// `kubectl get loops` shows a readable status column. It does not drive
// iterations, schedule anything, or touch TeamHarness state; state
// transitions (active/paused/completed/failed) are a later pass.
type LoopReconciler struct {
	client.Client
}

// Reconcile initializes Loop.Status on first observation and then keeps
// observedGeneration in sync. It is idempotent: once status.state is
// non-empty and observedGeneration matches metadata.generation, no patch
// is issued at all (the object's resourceVersion stays untouched).
func (r *LoopReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("loop", start, reterr) }()

	var loop v1beta1.Loop
	if err := r.Get(ctx, req.NamespacedName, &loop); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !loop.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	patchBase := client.MergeFrom(loop.DeepCopy())

	changed := false
	if loop.Status.State == "" {
		loop.Status.State = "Pending"
		changed = true
	}
	if loop.Status.ObservedGeneration != loop.Generation {
		loop.Status.ObservedGeneration = loop.Generation
		changed = true
	}
	if !changed {
		return reconcile.Result{}, nil
	}
	if err := r.Status().Patch(ctx, &loop, patchBase); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// SetupWithManager registers the Loop reconciler with the manager.
func (r *LoopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Loop{}).
		Complete(r)
}
