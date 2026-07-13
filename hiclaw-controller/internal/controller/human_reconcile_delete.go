package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileHumanDelete cleans up best-effort external state before
// removing the finalizer. The human has no container, no gateway
// consumer, and no MinIO account — only Matrix room memberships and
// (in embedded mode) a humans-registry entry. We can't log in as the
// human to /leave (password may be stale), so we rely on the Tuwunel
// admin bot's force-leave-room command instead.
//
// Every external call here is non-fatal: a transient Matrix or OSS
// failure must not wedge finalizer removal, and the homeserver's
// delete_rooms_after_leave / forget_forced_upon_leave flags provide a
// safety net if any force-leave never lands.
func (r *HumanReconciler) reconcileHumanDelete(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	h := s.human
	logger.Info("deleting human", "name", h.Name)

	humanUserID := h.Status.MatrixUserID
	if humanUserID == "" {
		humanUserID = s.identity.MatrixUserID
	}
	for _, roomID := range h.Status.Rooms {
		if err := r.Provisioner.ForceLeaveRoom(ctx, humanUserID, roomID); err != nil {
			logger.Error(err, "force-leave-room failed (non-fatal)",
				"user", humanUserID, "roomID", roomID)
		}
	}

	if s.identity.Source != nil {
		if err := s.identity.Source.EnsureDeactivated(ctx, &h.Spec, &h.Status); err != nil {
			return reconcile.Result{RequeueAfter: reconcileInterval}, err
		}
	}

	if r.Legacy != nil {
		if err := r.Legacy.RemoveFromHumansRegistry(ctx, h.Name); err != nil {
			logger.Error(err, "failed to remove human from registry (non-fatal)")
		}
	}

	base := h.DeepCopy()
	controllerutil.RemoveFinalizer(h, finalizerName)
	if err := r.Patch(ctx, h, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("human deleted", "name", h.Name)
	return reconcile.Result{}, nil
}
