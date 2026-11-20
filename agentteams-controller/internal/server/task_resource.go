package server

import (
	"encoding/json"
	"net/http"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// taskResource is the public task view. Storage records and notification routes
// stay behind the API; callers use task/submission identities rather than keys.
type taskResource struct {
	ID           string `json:"id"`
	Team         string `json:"team"`
	Kind         string `json:"kind,omitempty"`
	Title        string `json:"title,omitempty"`
	Status       string `json:"status"`
	AssignedTo   string `json:"assignedTo,omitempty"`
	AssignmentID string `json:"assignmentId,omitempty"`
	ParentTaskID string `json:"parentTaskId,omitempty"`
	MainTaskID   string `json:"mainTaskId,omitempty"`
	ProjectID    string `json:"projectId,omitempty"`
	Spec         string `json:"spec,omitempty"`
	Summary      string `json:"summary,omitempty"`
	SubmissionID string `json:"submissionId,omitempty"`
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
	return taskResource{
		ID: id, Team: team,
		Kind: coordinationString(record, "task_kind"), Title: coordinationString(record, "title"),
		Status: coordinationString(record, "status"), AssignedTo: coordinationString(record, "assigned_to"),
		AssignmentID: coordinationString(record, "notifyTxn"), ParentTaskID: coordinationString(record, "parent_task_id"),
		MainTaskID: coordinationString(record, "main_task_id"), ProjectID: coordinationString(record, "project_id"),
		Spec: coordinationString(record, "spec"), Summary: coordinationString(record, "summary"),
		SubmissionID: coordinationString(record, "submission_id"),
	}
}
