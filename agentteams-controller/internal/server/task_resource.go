package server

import (
	"encoding/json"
	"net/http"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// taskResource is the public task view. Storage records and notification routes
// stay behind the API; callers use task/submission identities rather than keys.
type taskResource struct {
	PlanID        string                            `json:"planId,omitempty"`
	Nodes         []taskPlanNodeResource            `json:"nodes,omitempty"`
	ID            string                            `json:"id"`
	Team          string                            `json:"team"`
	Kind          string                            `json:"kind,omitempty"`
	Title         string                            `json:"title,omitempty"`
	Status        string                            `json:"status"`
	AssignedTo    string                            `json:"assignedTo,omitempty"`
	AssignmentID  string                            `json:"assignmentId,omitempty"`
	ParentTaskID  string                            `json:"parentTaskId,omitempty"`
	MainTaskID    string                            `json:"mainTaskId,omitempty"`
	ProjectID     string                            `json:"projectId,omitempty"`
	Spec          string                            `json:"spec,omitempty"`
	Summary       string                            `json:"summary,omitempty"`
	SubmissionID  string                            `json:"submissionId,omitempty"`
	CompletionID  string                            `json:"completionId,omitempty"`
	Review        *taskReview                       `json:"review,omitempty"`
	ResultStatus  string                            `json:"resultStatus,omitempty"`
	Artifacts     []taskFinalArtifact               `json:"artifacts,omitempty"`
	OriginEventID string                            `json:"originEventId,omitempty"`
	Notifications map[string]taskNotificationStatus `json:"notifications,omitempty"`
}

type taskPlanNodeResource struct {
	ID         string   `json:"id"`
	TaskID     string   `json:"taskId"`
	Title      string   `json:"title"`
	AssignedTo string   `json:"assignedTo"`
	Status     string   `json:"status"`
	Spec       string   `json:"spec,omitempty"`
	DependsOn  []string `json:"dependsOn"`
}

// GetTaskResource returns the storage-independent view used by `agt task get`.
// The legacy /meta endpoint remains available to existing conditional writers.
func (h *ProjectHandler) GetTaskResource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	body, err := h.oss.GetObject(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "task state unavailable")
		return
	}
	var record map[string]any
	if json.Unmarshal(body, &record) != nil || coordinationString(record, "task_id") != id || !canonicalTaskStatus(coordinationString(record, "status")) {
		writeError(w, http.StatusServiceUnavailable, "invalid task state")
		return
	}
	version := contentETag(body)
	w.Header().Set("ETag", version)
	httputil.WriteJSON(w, http.StatusOK, struct {
		Version string       `json:"version"`
		Task    taskResource `json:"task"`
	}{Version: version, Task: publicTaskResource(id, team, record)})
}

func publicTaskResource(id, team string, record map[string]any) taskResource {
	var plan taskPlan
	encodedPlan, _ := json.Marshal(record["task_plan"])
	_ = json.Unmarshal(encodedPlan, &plan)
	var nodes []taskPlanNodeResource
	if plan.ID != "" {
		byStep := map[string]string{}
		for _, node := range plan.Request.Nodes {
			byStep[node.ID] = node.Spec
		}
		items, _ := record["tasks"].([]any)
		for _, item := range items {
			node, _ := item.(map[string]any)
			var dependencies []string
			data, _ := json.Marshal(node["depends_on"])
			_ = json.Unmarshal(data, &dependencies)
			step := coordinationString(node, "step_id")
			nodes = append(nodes, taskPlanNodeResource{ID: step, TaskID: coordinationString(node, "task_id"), Title: coordinationString(node, "title"), AssignedTo: coordinationString(node, "assigned_to"), Status: coordinationString(node, "status"), Spec: byStep[step], DependsOn: dependencies})
		}
	}
	var review *taskReview
	if record["task_review"] != nil {
		data, _ := json.Marshal(record["task_review"])
		_ = json.Unmarshal(data, &review)
	}
	var completion taskCompletion
	data, _ := json.Marshal(record["task_completion"])
	_ = json.Unmarshal(data, &completion)
	var origin struct {
		EventID string `json:"event_id"`
	}
	data, _ = json.Marshal(record["task_origin"])
	_ = json.Unmarshal(data, &origin)
	var intents map[string]taskNotificationIntent
	data, _ = json.Marshal(record["task_notifications"])
	_ = json.Unmarshal(data, &intents)
	var notices map[string]taskNotificationStatus
	if len(intents) > 0 {
		notices = map[string]taskNotificationStatus{}
		for kind, intent := range intents {
			notices[kind] = taskNotificationStatus{Status: intent.Status, EventID: intent.EventID, Recipient: intent.Recipient}
		}
	}
	return taskResource{
		PlanID: plan.ID, Nodes: nodes,
		Review: review, ResultStatus: coordinationString(record, "result_status"),
		OriginEventID: origin.EventID, Notifications: notices,
		CompletionID: completion.ID, Artifacts: completion.Artifacts,
		ID: id, Team: team,
		Kind: coordinationString(record, "task_kind"), Title: coordinationString(record, "title"),
		Status: coordinationString(record, "status"), AssignedTo: coordinationString(record, "assigned_to"),
		AssignmentID: coordinationString(record, "notifyTxn"), ParentTaskID: coordinationString(record, "parent_task_id"),
		MainTaskID: coordinationString(record, "main_task_id"), ProjectID: coordinationString(record, "project_id"),
		Spec: coordinationString(record, "spec"), Summary: coordinationString(record, "summary"),
		SubmissionID: coordinationString(record, "submission_id"),
	}
}
