package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// A handoff records delivery of a frozen result, not a business review by a Leader.
type taskHandoff struct {
	SubmissionID string `json:"submissionId"`
	ActorName    string `json:"actorName"`
	ActorRole    string `json:"actorRole"`
}

func taskHandoffOf(task map[string]any) *taskHandoff {
	var receipt taskHandoff
	data, _ := json.Marshal(task["task_handoff"])
	if json.Unmarshal(data, &receipt) != nil || receipt.SubmissionID == "" || receipt.SubmissionID != task["submission_id"] || receipt.ActorName == "" || (receipt.ActorRole != authpkg.RoleWorker && receipt.ActorRole != authpkg.RoleTeamLeader) {
		return nil
	}
	return &receipt
}

func taskResultCompleted(task map[string]any) bool {
	if task["status"] != "completed" || task["submission_id"] == "" {
		return false
	}
	if taskHandoffOf(task) != nil && coordinationString(task, "task_plan_id") != "" && (task["result_status"] == "SUCCESS" || task["result_status"] == "SUCCESS_WITH_NOTES") {
		return true
	}
	var review taskReview
	data, _ := json.Marshal(task["task_review"])
	return json.Unmarshal(data, &review) == nil && review.ID != "" && review.Accepted && review.Status == "completed" && review.SubmissionID == task["submission_id"]
}

func (h *ProjectHandler) directTaskHandoff(r *http.Request, key string, task map[string]any) (bool, error) {
	if coordinationString(task, "task_plan_id") == "" || task["decision_class"] == "r1" {
		return false, nil // Legacy work and explicit human-approval gates keep their existing review contract.
	}
	mainKey := path.Join(path.Dir(path.Dir(key)), coordinationString(task, "main_task_id"), "meta.json")
	main, plan, _, err := h.readTaskPlan(r.Context(), mainKey, coordinationString(task, "task_plan_id"))
	if err != nil || plan.ID == "" || main["status"] != "in_progress" || !taskReviewMembership(main, task) {
		return false, fmt.Errorf("active main-task assignment required for handoff")
	}
	if _, message := h.taskExecutionAdmissionFailure(r.Context(), mainKey, main); message != "" {
		return false, fmt.Errorf("%s", message)
	}
	return true, nil
}

func (h *ProjectHandler) projectTaskHandoff(ctx context.Context, key string, child map[string]any) (map[string]any, error) {
	if !taskResultCompleted(child) || taskHandoffOf(child) == nil {
		return nil, fmt.Errorf("completed immutable handoff required")
	}
	return h.projectTaskOutcome(ctx, key, child, taskOutcome{Status: "completed", SubmissionID: coordinationString(child, "submission_id")})
}

func (h *ProjectHandler) writeTaskHandoff(w http.ResponseWriter, r *http.Request, key, id, team string, child map[string]any) {
	mainKey := path.Join(path.Dir(path.Dir(key)), coordinationString(child, "main_task_id"), "meta.json")
	main, err := h.projectTaskHandoff(r.Context(), key, child)
	projection, dispatch := "applied", "ready"
	if err != nil {
		projection, dispatch = "pending", "pending"
	} else if h.reconcileTaskPlan(r.Context(), mainKey) != nil {
		dispatch = "pending"
	}
	var assignments []taskPlanAssignment
	if err == nil {
		if latest, _, _, readErr := h.readTaskPlan(r.Context(), mainKey, ""); readErr == nil {
			assignments, readErr = h.taskPlanAssignments(r.Context(), mainKey, latest)
			if readErr != nil {
				dispatch = "pending"
			}
		} else {
			dispatch = "pending"
		}
	}
	var notification *taskNotificationStatus
	if err == nil && coordinationReady(main) {
		notice := h.flushTaskNotification(r.Context(), mainKey, "ready")
		notification = &notice
	}
	httputil.WriteJSON(w, 200, struct {
		Task           taskResource            `json:"task"`
		MainProjection string                  `json:"mainProjection"`
		DispatchStatus string                  `json:"dispatchStatus"`
		Assignments    []taskPlanAssignment    `json:"assignments"`
		Notification   *taskNotificationStatus `json:"notification,omitempty"`
		NextAction     string                  `json:"nextAction"`
	}{publicTaskResource(id, team, child), projection, dispatch, assignments, notification, "Task system saved your immutable result and owns downstream dispatch. End this execution; do not notify the Leader, repeat assignments, or poll. Pending synchronization is retried automatically."})
}
