package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// amendTaskPlan retires unfinished work without inventing a candidate or review.
// The same replacement DAG machinery retains history and reconnects consumers.
// false asks the caller to reread after a competing execution/storage CAS.
func (h *ProjectHandler) amendTaskPlan(w http.ResponseWriter, r *http.Request, key, team string, body []byte, task, main map[string]any, input taskAmendRequest) bool {
	caller := authpkg.CallerFromContext(r.Context())
	identity, _ := json.Marshal([]any{"amend-plan", team, task["task_id"], caller.Username, input})
	sum := sha256.Sum256(identity)
	revision := taskReview{ID: hex.EncodeToString(sum[:]), Status: "revision", Actor: "leader:" + caller.Username,
		Note: input.Note, ReplacementSpec: input.Spec, ReplacementNodes: input.ReplacementNodes}
	saved := coordinationMap(task, "task_revision")
	if saved != nil {
		if saved["id"] != revision.ID {
			writeError(w, 409, "execution already revised; amend its current replacement instead")
			return true
		}
	} else {
		status := coordinationString(task, "status")
		if status != "planned" && status != "assigned" && status != "in_progress" {
			writeError(w, 409, "use the exact candidate review to revise submitted or blocked work")
			return true
		}
		if h.coordination == nil {
			writeError(w, 503, "task coordination unavailable")
			return true
		}
		leader, err := h.coordination.worker(r.Context(), caller.Username)
		if err != nil {
			writeError(w, 403, "registered Leader required")
			return true
		}
		if !h.validatePlanAssignees(w, r, input.ReplacementNodes, leader.Name) {
			return true
		}
		var execution taskExecution
		encoded, _ := json.Marshal(task["execution"])
		if json.Unmarshal(encoded, &execution) != nil || unresolvedOperation(execution.Checkpoint) {
			writeError(w, 409, "current tool operation must be verified before replacing its execution")
			return true
		}
		if err := applyTaskReplacement(main, task, revision); err != nil {
			writeError(w, 409, err.Error())
			return true
		}
		preview, _ := json.Marshal(main)
		if len(preview) > taskMetaCommitMaxBytes {
			writeError(w, 413, "replacement exceeds main task record limit")
			return true
		}
		if _, exists := task["original_spec"]; !exists {
			task["original_spec"] = task["spec"]
		}
		task["status"], task["spec"], task["task_revision"] = "revision", input.Spec, revision
		appendTaskOperationHistory(task, status, "revision", "amend: "+input.Note, authzActor(caller))
		next, _ := json.Marshal(task)
		strict := h.oss.(conditionalWriter)
		if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(body)); errors.Is(err, oss.ErrPreconditionFailed) {
			return false
		} else if err != nil {
			writeError(w, 503, "execution amendment unavailable")
			return true
		}
	}
	main, err := h.projectTaskReview(r.Context(), key, task, revision)
	projection, dispatch := "applied", "ready"
	if err != nil {
		projection, dispatch = "pending", "pending"
	} else if err := h.reconcileTaskPlan(r.Context(), path.Join(path.Dir(path.Dir(key)), coordinationString(main, "task_id"), "meta.json")); err != nil {
		dispatch = "pending"
	}
	httputil.WriteJSON(w, 200, map[string]any{"task": publicTaskResource(coordinationString(task, "task_id"), team, task),
		"effect": "committed", "amendment": revision, "mainProjection": projection, "dispatchStatus": dispatch,
		"nextAction": "The original execution is retained as a revision. Continue with its replacement nodes; do not submit the retired node."})
	return true
}
