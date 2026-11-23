package server

import (
	"net/http"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager responses carry the authenticated Human's room, never another
// administrator's DM. Service identities keep the infrastructure room ID.
func (h *ResourceHandler) managerResponseForCaller(r *http.Request, manager *v1beta1.Manager) ManagerResponse {
	response := managerToResponse(manager)
	caller := auth.CallerFromContext(r.Context())
	if caller == nil || caller.MatrixUserID == "" {
		return response
	}
	response.RoomID = ""
	var human v1beta1.Human
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	if err := reader.Get(r.Context(), client.ObjectKey{Name: caller.Username, Namespace: h.namespace}, &human); err != nil {
		return response
	}
	if !human.DeletionTimestamp.IsZero() || human.Spec.PermissionLevel != 1 || human.Status.MatrixUserID != caller.MatrixUserID {
		return response
	}
	if human.Labels[v1beta1.LabelSystemAdmin] == "true" {
		response.RoomID = manager.Status.RoomID
	} else {
		response.RoomID = human.Status.ManagerRooms[manager.Name]
	}
	return response
}
