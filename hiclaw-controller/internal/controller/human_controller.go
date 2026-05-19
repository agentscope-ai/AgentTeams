package controller

import (
	"context"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HumanReconciler reconciles Human resources using Service-layer orchestration.
//
// Unlike Worker/Manager, a Human has no backend container and no gateway
// consumer: the reconciler's entire job is to keep a Matrix user plus a
// set of room memberships in sync with Spec.AccessibleWorkers/Teams and
// (in embedded mode) with humans-registry.json.
type HumanReconciler struct {
	client.Client

	Provisioner service.HumanProvisioner
	Legacy      *service.LegacyCompat // nil in incluster mode
}

func (r *HumanReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("human", start, reterr) }()

	logger := log.FromContext(ctx)

	var human v1beta1.Human
	if err := r.Get(ctx, req.NamespacedName, &human); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	patchBase := client.MergeFrom(human.DeepCopy())

	s := &humanScope{
		human:     &human,
		patchBase: patchBase,
	}

	// Defer status patch so every phase writes through a single merge-patch
	// at the end of the reconcile loop. We skip the patch when the object
	// is being deleted — the finalizer cleanup path calls r.Update itself
	// and the CR may no longer exist by the time the defer runs.
	defer func() {
		if !human.DeletionTimestamp.IsZero() {
			return
		}

		human.Status.Phase = computeHumanPhase(&human, reterr)
		if reterr == nil {
			human.Status.Message = ""
		} else {
			human.Status.Message = reterr.Error()
		}

		if err := r.Status().Patch(ctx, &human, patchBase); err != nil {
			logger.Error(err, "failed to patch human status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !human.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&human, finalizerName) {
			return r.reconcileHumanDelete(ctx, s)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&human, finalizerName) {
		controllerutil.AddFinalizer(&human, finalizerName)
		if err := r.Update(ctx, &human); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileHumanNormal(ctx, s)
}

// reconcileHumanNormal runs the declarative convergence loop. Phases in
// order: infrastructure (Matrix account), rooms (membership), legacy
// (humans-registry.json). Only infrastructure is fatal; the other two
// phases log errors but never return them, so a transient Matrix hiccup
// on room invite/kick does not block the next reconcile.
func (r *HumanReconciler) reconcileHumanNormal(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	if err := r.reconcileHumanInfra(ctx, s); err != nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, err
	}
	r.reconcileHumanRooms(ctx, s)
	r.reconcileHumanLegacy(ctx, s)

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *HumanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Human{}).
		Complete(r)
}
