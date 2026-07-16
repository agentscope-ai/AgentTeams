package controller

import (
	"context"
	"fmt"

	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileManagerInfrastructure ensures Matrix account, Gateway consumer, MinIO user,
// Matrix room, and credentials are provisioned for the Manager. Idempotent: if already
// provisioned (MatrixUserID set), it refreshes credentials and restores gateway auth.
func (r *ManagerReconciler) reconcileManagerInfrastructure(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	m := s.manager

	if m.Status.MatrixUserID != "" {
		refreshResult, err := r.Provisioner.RefreshManagerCredentials(ctx, m.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials: %w", err)
		}

		// Gateway auth errors must propagate so controller-runtime re-queues
		// the reconcile with backoff. Previously this was swallowed as
		// "non-fatal", which masked real failures (e.g. Higress Console PUT
		// returning non-200) and left the data plane stuck with an empty
		// allowedConsumers list until a subsequent event happened to retry.
		if err := r.Provisioner.EnsureManagerGatewayAuth(ctx, m.Name, refreshResult.GatewayKey); err != nil {
			return reconcile.Result{}, fmt.Errorf("restore manager gateway auth: %w", err)
		}

		s.provResult = &service.ManagerProvisionResult{
			MatrixUserID:   m.Status.MatrixUserID,
			MatrixToken:    refreshResult.MatrixToken,
			RoomID:         m.Status.RoomID,
			GatewayKey:     refreshResult.GatewayKey,
			MinIOPassword:  refreshResult.MinIOPassword,
			MatrixPassword: refreshResult.MatrixPassword,
		}
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx)
	logger.Info("provisioning manager infrastructure", "name", m.Name)

	provResult, err := r.Provisioner.ProvisionManager(ctx, service.ManagerProvisionRequest{
		Name: m.Name,
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("provision manager: %w", err)
	}

	m.Status.MatrixUserID = provResult.MatrixUserID
	m.Status.RoomID = provResult.RoomID
	s.provResult = provResult

	return reconcile.Result{}, nil
}
