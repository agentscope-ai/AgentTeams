package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *HumanReconciler) reconcileHumanManagerRooms(ctx context.Context, s *humanScope) error {
	h := s.human
	l1 := h.Spec.PermissionLevel == 1
	if !l1 && r.ManagerConfig != nil && len(h.Status.ManagerRooms) > 0 {
		if err := r.ManagerConfig.UpdateManagerHumanAllowFrom(h.Status.MatrixUserID, false); err != nil {
			return fmt.Errorf("update Manager Human access: %w", err)
		}
	}
	if !l1 {
		return nil
	}
	var managers v1beta1.ManagerList
	if err := r.List(ctx, &managers, client.InNamespace(h.Namespace)); err != nil {
		return err
	}
	for _, manager := range managers.Items {
		if !manager.DeletionTimestamp.IsZero() || manager.Status.MatrixUserID == "" || manager.Status.RoomID == "" {
			continue
		}
		roomID := h.Status.ManagerRooms[manager.Name]
		if roomID != "" && containsString(h.Status.Rooms, roomID) {
			continue
		}
		roomID, err := r.Provisioner.EnsureHumanManagerRoom(ctx, service.HumanManagerRoomRequest{
			ManagerName: manager.Name, HumanName: h.Name, HumanUID: string(h.UID),
			HumanMatrixID: h.Status.MatrixUserID, RoomID: roomID,
		})
		if err != nil {
			return fmt.Errorf("provision personal Manager DM: %w", err)
		}
		if h.Status.ManagerRooms == nil {
			h.Status.ManagerRooms = make(map[string]string)
		}
		h.Status.ManagerRooms[manager.Name] = roomID
	}
	// Record the room before granting runtime access, so a later demotion
	// can revoke the grant even when room provisioning or joining was retried.
	if r.ManagerConfig != nil && len(h.Status.ManagerRooms) > 0 {
		if err := r.ManagerConfig.UpdateManagerHumanAllowFrom(h.Status.MatrixUserID, true); err != nil {
			return fmt.Errorf("update Manager Human access: %w", err)
		}
	}
	return nil
}

func (r *HumanReconciler) managerToHumanRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	var humans v1beta1.HumanList
	if err := r.List(ctx, &humans, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, human := range humans.Items {
		if human.Spec.PermissionLevel == 1 || len(human.Status.ManagerRooms) > 0 {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&human)})
		}
	}
	return requests
}
