package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// roomTaskBinding resolves only the caller's own team; chat content cannot select a Task.
func (h *TaskRecoveryHandler) roomTaskBinding(ctx context.Context, room string) (map[string]string, error) {
	caller := authpkg.CallerFromContext(ctx)
	if caller == nil || caller.Team == "" || (caller.Role != authpkg.RoleWorker && caller.Role != authpkg.RoleTeamLeader) {
		return nil, nil
	}
	prefixes, aliases, err := h.project.taskMetaPrefixes(ctx)
	if err != nil {
		return nil, err
	}
	var binding map[string]string
	execution := ""
	for _, prefix := range prefixes {
		team := candidateTeam(prefix)
		if !callerMatchesStorageTeam(caller, team, aliases) {
			continue
		}
		children, err := h.project.oss.ListObjects(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			key, ok := metaKeyFromListResult(prefix, child)
			if !ok {
				continue
			}
			body, err := h.project.oss.GetObject(ctx, key)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var task map[string]json.RawMessage
			if err = json.Unmarshal(body, &task); err != nil {
				return nil, err
			}
			var roomID, id, kind, status string
			_ = json.Unmarshal(task["room_id"], &roomID)
			if roomID != room {
				continue
			}
			_ = json.Unmarshal(task["task_id"], &id)
			_ = json.Unmarshal(task["task_kind"], &kind)
			_ = json.Unmarshal(task["status"], &status)
			if id != path.Base(path.Dir(key)) {
				return nil, service.ErrWorkerStateInvalid
			}
			if kind == "main" {
				if binding != nil && binding["taskId"] != id {
					return nil, service.ErrWorkerStateConflict
				}
				binding = map[string]string{"taskId": id, "teamId": team}
			} else if (status == "assigned" || status == "in_progress") && taskAssignedTo(task, caller.Username, caller.WorkerName, caller.MatrixUserID) {
				if execution != "" && execution != id {
					return nil, service.ErrWorkerStateConflict
				}
				execution = id
			}
		}
	}
	if binding != nil && execution != "" {
		binding["executionTaskId"] = execution
	}
	return binding, nil
}

func (h *TaskRecoveryHandler) GetRoomContext(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.deps.Namespace) {
		writeError(w, 403, "Task room context is outside your namespace")
		return
	}
	room := r.URL.Query().Get("roomId")
	if !strings.HasPrefix(room, "!") || len(room) > 1024 {
		writeError(w, 400, "valid roomId required")
		return
	}
	binding, err := h.roomTaskBinding(r.Context(), room)
	if errors.Is(err, service.ErrWorkerStateConflict) {
		writeError(w, 409, "room Task binding is ambiguous")
		return
	}
	if err != nil {
		writeError(w, 503, "Task room context unavailable")
		return
	}
	if binding == nil {
		binding = map[string]string{}
	}
	httputil.WriteJSON(w, 200, binding)
}
