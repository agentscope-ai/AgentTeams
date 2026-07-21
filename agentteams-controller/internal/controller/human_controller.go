package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	_ "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity/externalsso"
	_ "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity/legacypassword"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
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
		username:  human.Spec.EffectiveUsername(human.Name),
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

		if reterr == nil {
			if human.Status.Phase != "Degraded" {
				human.Status.Message = ""
			}
		} else {
			human.Status.Message = reterr.Error()
		}
		human.Status.Phase = computeHumanPhase(&human, reterr)

		if err := r.Status().Patch(ctx, &human, patchBase); err != nil {
			logger.Error(err, "failed to patch human status; CR will appear to have no status",
				"name", human.Name, "phase", human.Status.Phase, "matrixUserID", human.Status.MatrixUserID)
			reterr = kerrors.NewAggregate([]error{reterr, err})
			return
		}
		logger.Info("human status patched",
			"name", human.Name, "phase", human.Status.Phase,
			"matrixUserID", human.Status.MatrixUserID, "reconcileFailed", reterr != nil)
	}()

	if !human.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&human, finalizerName) {
			if err := r.resolveHumanScope(s); err != nil && human.Status.MatrixUserID == "" {
				logger.Error(err, "failed to resolve deleting human identity; continuing best-effort cleanup", "name", human.Name)
			}
			return r.reconcileHumanDelete(ctx, s)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&human, finalizerName) {
		base := human.DeepCopy()
		controllerutil.AddFinalizer(&human, finalizerName)
		if err := r.Patch(ctx, &human, client.MergeFrom(base)); err != nil {
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
	if err := r.resolveHumanScope(s); err != nil {
		s.human.Status.Phase = "Degraded"
		s.human.Status.Message = err.Error()
		return reconcile.Result{RequeueAfter: reconcileInterval}, nil
	}
	if err := r.reconcileHumanInfra(ctx, s); err != nil {
		if errors.Is(err, matrix.ErrAppServiceNotReady) {
			log.FromContext(ctx).Info("Matrix AppService not active yet; requeueing human provisioning",
				"name", s.human.Name)
			return reconcile.Result{RequeueAfter: appServiceNotReadyRequeue}, nil
		}
		return reconcile.Result{RequeueAfter: reconcileInterval}, err
	}
	r.reconcileHumanRooms(ctx, s)
	r.reconcileHumanLegacy(ctx, s)

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *HumanReconciler) resolveHumanScope(s *humanScope) error {
	resolved, err := humanidentity.ResolveHuman(&s.human.Spec, s.human.Name, humanidentity.Deps{
		Provisioner: r.Provisioner,
	})
	if err != nil {
		return err
	}
	// Once a Matrix account exists, the derived MXID is the human's stable
	// identity. Any change to it — switching to/from SSO, editing
	// identitySource.subject, or renaming the legacy username — means a
	// different account. Re-provisioning in place would leave Status.Rooms
	// pointing at the previous user's memberships, so the rooms phase would
	// treat them as already observed and never invite/join the new user,
	// leaving a Human that looks Active but whose new identity is in no
	// rooms. Block the switch and require recreating the CR instead.
	if s.human.Status.MatrixUserID != "" && s.human.Status.MatrixUserID != resolved.MatrixUserID {
		return fmt.Errorf("identitySource changed; recreate CR to switch identity")
	}
	s.identity = resolved
	s.username = resolved.MatrixLocalpart
	if !resolved.ManagesInitialPassword {
		s.human.Status.InitialPassword = ""
	}
	return nil
}

func (r *HumanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Human{}).
		Complete(r)
}
