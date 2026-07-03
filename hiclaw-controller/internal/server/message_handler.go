package server

import (
	"encoding/json"
	"net/http"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MessageHandler handles imperative message-injection endpoints that let an
// operator (or automation) post a system-level message directly into a
// Manager's Admin DM room or a Team's leader channel, without going through
// the normal agent chat loop. See plan #17 (v1.5).
type MessageHandler struct {
	k8s         client.Client
	provisioner *service.Provisioner
	namespace   string
}

func NewMessageHandler(k8s client.Client, provisioner *service.Provisioner, namespace string) *MessageHandler {
	return &MessageHandler{k8s: k8s, provisioner: provisioner, namespace: namespace}
}

// messageRequest is the JSON body accepted by both message-injection routes.
type messageRequest struct {
	Body string `json:"body"`
}

// messageResponse is returned on success.
type messageResponse struct {
	RoomID string `json:"roomID"`
	Sent   bool   `json:"sent"`
}

// decodeMessageBody parses and validates the shared request body. Returns
// ("", false) and has already written the error response when invalid.
func decodeMessageBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return "", false
	}
	if req.Body == "" {
		httputil.WriteError(w, http.StatusBadRequest, "body is required")
		return "", false
	}
	return req.Body, true
}

// SendManagerMessage handles POST /api/v1/managers/{name}/message — injects
// a system-level message into the Manager's Admin DM room (Status.RoomID),
// bypassing the normal agent chat loop.
//
// Example:
//
//	curl -X POST https://controller.example.com/api/v1/managers/main/message \
//	  -H "Authorization: Bearer $TOKEN" \
//	  -H "Content-Type: application/json" \
//	  -d '{"body":"Please pause new task intake for the next hour."}'
func (h *MessageHandler) SendManagerMessage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "manager name is required")
		return
	}

	body, ok := decodeMessageBody(w, r)
	if !ok {
		return
	}

	var manager v1beta1.Manager
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &manager); err != nil {
		writeK8sError(w, "get manager", err)
		return
	}

	roomID := manager.Status.RoomID
	if roomID == "" {
		httputil.WriteError(w, http.StatusConflict, "manager admin DM room is not provisioned yet")
		return
	}

	if err := h.provisioner.SendAdminMessage(r.Context(), roomID, body); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "send message: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, messageResponse{RoomID: roomID, Sent: true})
}

// SendTeamMessage handles POST /api/v1/teams/{name}/message — injects a
// system-level message addressed to the team's lead. Prefers the
// leader↔admin DM room (Status.LeaderDMRoomID); falls back to the shared
// team room (Status.TeamRoomID) when the DM hasn't been provisioned yet,
// per plan #17: instructions injected this way are intended for the team
// LEAD, not broadcast to the whole team.
//
// Example:
//
//	curl -X POST https://controller.example.com/api/v1/teams/backend/message \
//	  -H "Authorization: Bearer $TOKEN" \
//	  -H "Content-Type: application/json" \
//	  -d '{"body":"Re-prioritize: ship the hotfix before the migration."}'
func (h *MessageHandler) SendTeamMessage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "team name is required")
		return
	}

	body, ok := decodeMessageBody(w, r)
	if !ok {
		return
	}

	var team v1beta1.Team
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &team); err != nil {
		writeK8sError(w, "get team", err)
		return
	}

	roomID := team.Status.LeaderDMRoomID
	if roomID == "" {
		roomID = team.Status.TeamRoomID
	}
	if roomID == "" {
		httputil.WriteError(w, http.StatusConflict, "team leader room is not provisioned yet")
		return
	}

	if err := h.provisioner.SendAdminMessage(r.Context(), roomID, body); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "send message: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, messageResponse{RoomID: roomID, Sent: true})
}
