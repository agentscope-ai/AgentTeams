package controller

import (
	"context"
	"errors"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *ManagerReconciler) reconcileManagerDelete(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	m := s.manager
	logger.Info("deleting manager", "name", m.Name)

	managerName := m.Name

	if err := r.Provisioner.LeaveAllManagerRooms(ctx, managerName); err != nil {
		logger.Error(err, "manager leave-all-rooms failed (non-fatal)")
	}
	if m.Status.RoomID != "" {
		if err := r.Provisioner.DeleteManagerRoom(ctx, m.Status.RoomID); err != nil {
			logger.Error(err, "manager room delete command failed (non-fatal)",
				"roomID", m.Status.RoomID)
		}
	}

	if err := r.Provisioner.DeprovisionManager(ctx, managerName); err != nil {
		logger.Error(err, "deprovision failed (non-fatal)")
	}

	if wb := r.managerBackend(ctx); wb != nil {
		containerName := r.managerContainerName(managerName)
		if err := wb.Delete(ctx, containerName); err != nil && !errors.Is(err, backend.ErrNotFound) {
			logger.Error(err, "failed to delete manager container (may already be removed)")
		}
	}

	if err := r.Deployer.CleanupOSSData(ctx, managerName); err != nil {
		logger.Error(err, "failed to clean up OSS agent data (non-fatal)")
	}
	if err := r.Provisioner.DeleteCredentials(ctx, managerName); err != nil {
		logger.Error(err, "failed to delete credentials (non-fatal)")
	}
	if err := r.Provisioner.DeleteManagerServiceAccount(ctx, managerName); err != nil {
		logger.Error(err, "failed to delete ServiceAccount (non-fatal)")
	}

	// Release the Matrix alias that tied this Manager to its Admin DM room.
	// The room is preserved; only the controller's stable identifier is
	// released so a future Manager CR with the same name can reclaim it.
	if err := r.Provisioner.DeleteManagerRoomAlias(ctx, managerName); err != nil {
		logger.Error(err, "failed to delete manager room alias (non-fatal)")
	}

	controllerutil.RemoveFinalizer(m, finalizerName)
	if err := r.Update(ctx, m); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("manager deleted", "name", managerName)
	return reconcile.Result{}, nil
}
