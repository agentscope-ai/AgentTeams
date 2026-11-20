package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskInputContext struct {
	TaskID   string             `json:"taskId"`
	Summary  string             `json:"summary,omitempty"`
	ReadOnly bool               `json:"readOnly"`
	Files    []taskFileResource `json:"files"`
}

func (h *ProjectHandler) ownedTaskExecution(w http.ResponseWriter, r *http.Request, task map[string]json.RawMessage) (taskExecution, bool) {
	caller := authpkg.CallerFromContext(r.Context())
	var execution taskExecution
	if !recoveryCaller(caller) || h.recovery == nil || !h.recovery.owns(r.Context(), caller, task) {
		writeError(w, 403, "task is not assigned to you")
		return execution, false
	}
	if caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.recovery.deps.Namespace {
		writeError(w, 403, "task execution namespace changed")
		return execution, false
	}
	if json.Unmarshal(task["execution"], &execution) != nil || execution.Generation == 0 || execution.State != "active" || execution.ProcessID == "" || execution.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
		writeError(w, 409, "task execution changed")
		return execution, false
	}
	instance, _, err := h.recovery.resolveInstance(r.Context(), caller, nil)
	if err != nil {
		writeError(w, 503, "execution identity unavailable")
		return execution, false
	}
	if instance != execution.InstanceID {
		writeError(w, 409, "task execution instance changed")
		return execution, false
	}
	return execution, true
}

// taskBeginInputs reads accepted prerequisites from the task's own scope. It
// never searches other teams or infers a Project from the task's complexity.
func (h *ProjectHandler) taskBeginInputs(r *http.Request, key string, task map[string]any) ([]taskInputContext, error) {
	inputs := []taskInputContext{}
	mainID := coordinationString(task, "main_task_id")
	if mainID == "" {
		return inputs, nil
	}
	if !isSafeTaskID(mainID) {
		return nil, errors.New("invalid main task identity")
	}
	mainKey := path.Join(path.Dir(path.Dir(key)), mainID, "meta.json")
	body, err := h.oss.GetObject(r.Context(), mainKey)
	var main struct {
		ID     string            `json:"task_id"`
		Kind   string            `json:"task_kind"`
		Status string            `json:"status"`
		Tasks  []projectTaskMeta `json:"tasks"`
	}
	if err != nil || json.Unmarshal(body, &main) != nil {
		return nil, errors.New("main task unavailable")
	}
	if main.ID != mainID || main.Kind != "main" || (main.Status != "assigned" && main.Status != "in_progress") {
		return nil, errors.New("main task is not active")
	}
	var node *projectTaskMeta
	for i := range main.Tasks {
		if main.Tasks[i].TaskID == coordinationString(task, "task_id") {
			node = &main.Tasks[i]
			break
		}
	}
	if node == nil || node.AssignedTo != coordinationString(task, "assigned_to") {
		return nil, errors.New("assignment does not match the main task")
	}
	for _, dependency := range node.DependsOn {
		if !isSafeTaskID(dependency) {
			return nil, errors.New("invalid prerequisite identity")
		}
		accepted := false
		for _, candidate := range main.Tasks {
			if candidate.TaskID == dependency && candidate.Status == "completed" {
				accepted = true
				break
			}
		}
		if !accepted {
			return nil, fmt.Errorf("prerequisite %s is not accepted in the main task", dependency)
		}
		upstreamKey := path.Join(path.Dir(path.Dir(key)), dependency, "meta.json")
		body, err := h.oss.GetObject(r.Context(), upstreamKey)
		var upstream map[string]any
		var raw map[string]json.RawMessage
		if err != nil || json.Unmarshal(body, &upstream) != nil || json.Unmarshal(body, &raw) != nil {
			return nil, fmt.Errorf("prerequisite %s unavailable", dependency)
		}
		if coordinationString(upstream, "task_id") != dependency || coordinationString(upstream, "main_task_id") != mainID || coordinationString(upstream, "project_id") != coordinationString(task, "project_id") || coordinationString(upstream, "status") != "completed" {
			return nil, fmt.Errorf("prerequisite %s is not accepted", dependency)
		}
		files, err := h.taskFileResources(r.Context(), upstreamKey, dependency, raw)
		if err != nil {
			return nil, fmt.Errorf("prerequisite %s files unavailable: %w", dependency, err)
		}
		inputs = append(inputs, taskInputContext{TaskID: dependency, Summary: coordinationString(upstream, "summary"), ReadOnly: true, Files: files})
	}
	return inputs, nil
}

// BeginTask acknowledges a registered execution and returns its complete public
// context. Native execution claiming remains owned by the existing recovery API.
func (h *ProjectHandler) BeginTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, 400, "invalid task id")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := h.oss.GetObject(r.Context(), key)
		var task map[string]any
		var raw map[string]json.RawMessage
		if err != nil || json.Unmarshal(current, &task) != nil || json.Unmarshal(current, &raw) != nil {
			writeError(w, 503, "task snapshot unavailable")
			return
		}
		status := coordinationString(task, "status")
		if coordinationString(task, "task_id") != id || coordinationString(task, "task_kind") == "main" || (status != "assigned" && status != "in_progress") {
			writeError(w, 409, "task is not an assigned execution")
			return
		}
		if !h.taskExecutionAdmission(w, r, key, task) {
			return
		}
		execution, ok := h.ownedTaskExecution(w, r, raw)
		if !ok {
			return
		}
		spec := coordinationString(task, "spec")
		if strings.TrimSpace(spec) == "" {
			body, err := h.oss.GetObject(r.Context(), path.Dir(key)+"/spec.md")
			if err != nil || strings.TrimSpace(string(body)) == "" {
				writeError(w, 503, "task specification unavailable")
				return
			}
			spec = string(body)
		}
		inputs, err := h.taskBeginInputs(r, key, task)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if status == "assigned" {
			task["status"] = "in_progress"
			appendTaskOperationHistory(task, "assigned", "in_progress", "ack_task", authzActor(authpkg.CallerFromContext(r.Context())))
			next, err := json.Marshal(task)
			strict, supported := h.oss.(conditionalWriter)
			if err != nil || !supported {
				writeError(w, 503, "conditional task storage required")
				return
			}
			if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(current)); err != nil {
				if errors.Is(err, oss.ErrPreconditionFailed) {
					continue
				}
				writeError(w, 503, "task acknowledgement unavailable")
				return
			}
			current = next
		}
		if err := h.advanceMainTaskExecution(r.Context(), key, current); err != nil {
			writeError(w, 503, "task acknowledged; main phase update needs retry")
			return
		}
		view := publicTaskResource(id, team, task)
		view.Spec = spec
		version := contentETag(current)
		w.Header().Set("ETag", version)
		httputil.WriteJSON(w, 200, struct {
			Version    string             `json:"version"`
			Task       taskResource       `json:"task"`
			Inputs     []taskInputContext `json:"inputs"`
			Generation uint64             `json:"generation"`
		}{version, view, inputs, execution.Generation})
		return
	}
	writeError(w, 409, "task changed during acknowledgement; retry begin")
}

func appendTaskOperationHistory(task map[string]any, from, to, action, actor string) {
	history, _ := task["history"].([]any)
	seq, _ := task["history_seq"].(float64)
	if seq < float64(len(history)) {
		seq = float64(len(history))
	}
	for _, entry := range history {
		if item, ok := entry.(map[string]any); ok {
			if value, ok := item["seq"].(float64); ok && value > seq {
				seq = value
			}
		}
	}
	seq++
	history = append(history, map[string]any{"seq": seq, "from": from, "to": to, "action": action, "actor": actor, "ts": time.Now().UTC().Format(time.RFC3339)})
	if len(history) > 50 {
		history = history[len(history)-50:]
	}
	task["history_seq"], task["history"] = seq, history
}

// taskExecutionAdmission shares decision and Project gates across public execution actions.
func (h *ProjectHandler) taskExecutionAdmission(w http.ResponseWriter, r *http.Request, key string, task map[string]any) bool {
	if decision := coordinationString(task, "decision_status"); decision != "" && decision != "open" {
		writeError(w, 409, "task requires a decision before execution")
		return false
	}
	if projectID := coordinationString(task, "project_id"); projectID != "" {
		if !isSafeTaskID(projectID) {
			writeError(w, 409, "invalid project membership")
			return false
		}
		projectKey := path.Join(path.Dir(path.Dir(path.Dir(key))), "projects", projectID, "meta.json")
		body, err := h.oss.GetObject(r.Context(), projectKey)
		var project map[string]any
		if err != nil || json.Unmarshal(body, &project) != nil {
			writeError(w, 503, "project membership unavailable")
			return false
		}
		if coordinationString(project, "project_id") != projectID || coordinationString(project, "status") != "active" {
			writeError(w, 409, "project membership is not active")
			return false
		}
	}

	return true
}
