package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskAmendRequest struct {
	Spec             string         `json:"spec"`
	Note             string         `json:"note"`
	ReplacementNodes []taskPlanNode `json:"replacementNodes,omitempty"`
}

// AmendTask changes the requirements of an unfinished execution, retaining its
// assignment, execution generation, outputs and history.
func (h *ProjectHandler) AmendTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var input taskAmendRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	var extra any
	if !isSafeTaskID(id) || decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF || strings.TrimSpace(input.Spec) == "" || strings.TrimSpace(input.Note) == "" {
		writeError(w, 400, "execution task id, spec and change note required")
		return
	}
	if len(input.ReplacementNodes) > 0 {
		plan := taskPlanRequest{Nodes: input.ReplacementNodes}
		if err := normalizeTaskPlan(&plan); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		input.ReplacementNodes = plan.Nodes
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleTeamLeader {
		writeError(w, 403, "Team Leader required")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	strict, ok := h.oss.(conditionalWriter)
	if !ok {
		writeError(w, 503, "conditional task storage required")
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(r.Context(), key)
		var task map[string]any
		if err != nil || json.Unmarshal(body, &task) != nil {
			writeError(w, 503, "execution snapshot unavailable")
			return
		}
		mainID := coordinationString(task, "main_task_id")
		if task["task_kind"] != "execution" || !isSafeTaskID(mainID) {
			writeError(w, 409, "amend requires a planned execution")
			return
		}
		mainBody, err := h.oss.GetObject(r.Context(), path.Join(path.Dir(path.Dir(key)), mainID, "meta.json"))
		var main map[string]any
		if err != nil || json.Unmarshal(mainBody, &main) != nil {
			writeError(w, 503, "main task unavailable")
			return
		}
		if !h.ownsMainTask(w, r, main) {
			return
		}
		if main["status"] == "completed" || main["status"] == "cancelled" {
			writeError(w, 409, "main task is closed")
			return
		}
		if len(input.ReplacementNodes) > 0 {
			if h.amendTaskPlan(w, r, key, team, body, task, main, input) {
				return
			}
			continue
		}
		status := coordinationString(task, "status")
		if status != "planned" && status != "assigned" && status != "in_progress" && status != "blocked" {
			writeError(w, 409, "submitted or completed executions require a revision")
			return
		}
		if task["spec"] != input.Spec {
			if _, exists := task["original_spec"]; !exists {
				task["original_spec"] = task["spec"]
			}
			task["spec"] = input.Spec
			appendTaskOperationHistory(task, status, status, "amend: "+input.Note, authzActor(caller))
			if status != "planned" && coordinationString(task, "notifyTxn") != "" {
				// Commit the wake intent with the amendment; transport can recover
				// after a crash without issuing another assignment or execution.
				digest := sha256.Sum256(append(body, []byte(input.Spec)...))
				kind := fmt.Sprintf("amended-%x", digest[:16])
				var intents map[string]taskNotificationIntent
				encoded, _ := json.Marshal(task["task_notifications"])
				_ = json.Unmarshal(encoded, &intents)
				if intents == nil {
					intents = map[string]taskNotificationIntent{}
				}
				intents[kind] = taskNotificationIntent{Status: "pending", ActorRole: caller.Role, ActorName: caller.Username, Team: caller.Team,
					Event: coordinationEvent{Kind: "task_assigned", TaskID: id, MainTaskID: mainID, ProjectID: coordinationString(task, "project_id"), DeliveryID: coordinationString(task, "notifyTxn"), Text: input.Note}}
				task["task_notifications"] = intents
				task["amend_notification"] = kind
			}
			next, err := json.Marshal(task)
			if err != nil {
				writeError(w, 503, "execution encoding unavailable")
				return
			}
			if err = strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(body)); errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			} else if err != nil {
				writeError(w, 503, "execution amendment unavailable")
				return
			}
		}
		result := map[string]any{"task": publicTaskResource(id, team, task), "effect": "committed"}
		if kind := coordinationString(task, "amend_notification"); kind != "" {
			result["notification"] = h.flushTaskNotification(r.Context(), key, kind)
		}
		httputil.WriteJSON(w, 200, result)
		return
	}
	writeError(w, 409, "execution changed; reread and retry amendment")
}
