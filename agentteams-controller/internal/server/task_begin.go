package server

import (
	"context"
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
	TaskID       string             `json:"taskId"`
	SubmissionID string             `json:"submissionId,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Purpose      string             `json:"purpose,omitempty"`
	Review       *taskReview        `json:"review,omitempty"`
	ReadOnly     bool               `json:"readOnly"`
	Files        []taskFileResource `json:"files"`
}

// Public recovery context excludes storage keys and Matrix delivery credentials.
type taskResumeContext struct {
	Version        uint64         `json:"version"`
	CompletedSteps []string       `json:"completedSteps"`
	RemainingSteps []string       `json:"remainingSteps"`
	Operation      *taskOperation `json:"operation,omitempty"`
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

// taskBeginInputs reads accepted prerequisites and the exact rejected revision
// base from this main task. It never searches other teams or infers a Project.
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
		ID       string            `json:"task_id"`
		Kind     string            `json:"task_kind"`
		Status   string            `json:"status"`
		Tasks    []projectTaskMeta `json:"tasks"`
		Attempts map[string]struct {
			TaskID       string `json:"task_id"`
			Status       string `json:"status"`
			SubmissionID string `json:"submission_id"`
			ReviewID     string `json:"review_id"`
		} `json:"task_attempts"`
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
		files, err := h.acceptedTaskFileResources(r, upstreamKey, dependency, raw)
		if err != nil {
			return nil, fmt.Errorf("prerequisite %s files unavailable: %w", dependency, err)
		}
		inputs = append(inputs, taskInputContext{TaskID: dependency, SubmissionID: coordinationString(upstream, "submission_id"), Summary: coordinationString(upstream, "summary"), ReadOnly: true, Files: files})
	}
	if previous := coordinationString(task, "revision_of"); previous != "" {
		attempt, exists := main.Attempts[previous]
		if !isSafeTaskID(previous) || previous == node.TaskID || node.RevisionOf != previous || !exists || attempt.TaskID != previous || attempt.Status != "revision" || attempt.SubmissionID == "" || attempt.ReviewID == "" {
			return nil, errors.New("revision base does not match the main task's rejected attempt")
		}
		body, err := h.oss.GetObject(r.Context(), path.Join(path.Dir(path.Dir(key)), previous, "meta.json"))
		var base map[string]any
		if err != nil || json.Unmarshal(body, &base) != nil || coordinationString(base, "task_id") != previous || coordinationString(base, "task_kind") != "execution" || coordinationString(base, "main_task_id") != mainID || coordinationString(base, "project_id") != coordinationString(task, "project_id") || coordinationString(base, "status") != "revision" {
			return nil, errors.New("rejected revision base unavailable in this task scope")
		}
		var review taskReview
		encoded, _ := json.Marshal(base["task_review"])
		candidate := publicTaskCandidate(base)
		if json.Unmarshal(encoded, &review) != nil || review.ID != attempt.ReviewID || review.Accepted || review.Status != "revision" || review.SubmissionID != attempt.SubmissionID || candidate == nil || candidate.ID != review.SubmissionID {
			return nil, errors.New("rejected revision candidate or review identity changed")
		}
		inputs = append(inputs, taskInputContext{TaskID: previous, SubmissionID: candidate.ID, Summary: candidate.Summary, Purpose: "revisionBase", Review: &review, ReadOnly: true, Files: candidate.Artifacts})
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
		if cp := execution.Checkpoint; cp != nil && cp.Result != nil {
			writeError(w, 409, "saved task result requires delivery recovery, not new business execution")
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
		var checkpoint *taskResumeContext
		if cp := execution.Checkpoint; cp != nil {
			checkpoint = &taskResumeContext{cp.Version, cp.CompletedSteps, cp.RemainingSteps, cp.Operation}
		}
		w.Header().Set("ETag", version)
		httputil.WriteJSON(w, 200, struct {
			Version    string             `json:"version"`
			Task       taskResource       `json:"task"`
			Inputs     []taskInputContext `json:"inputs"`
			Generation uint64             `json:"generation"`
			Checkpoint *taskResumeContext `json:"checkpoint,omitempty"`
		}{version, view, inputs, execution.Generation, checkpoint})
		return
	}
	writeError(w, 409, "task changed during acknowledgement; retry begin")
}

// New submissions freeze output identities. Catalog heads also include published
// intermediates and may reference a different version, so never use them as the
// accepted dependency snapshot. Legacy tasks retain their declared-output view.
func (h *ProjectHandler) acceptedTaskFileResources(r *http.Request, key, id string, raw map[string]json.RawMessage) ([]taskFileResource, error) {
	if len(raw["task_submission"]) == 0 {
		return h.taskFileResources(r.Context(), key, id, raw)
	}
	var submission taskSubmission
	var currentID string
	_ = json.Unmarshal(raw["submission_id"], &currentID)
	if json.Unmarshal(raw["task_submission"], &submission) != nil || submission.ID == "" || submission.ID != currentID {
		return nil, errors.New("accepted submission identity unavailable")
	}
	var index taskFileIndex
	if len(submission.Request.Artifacts) > 0 && json.Unmarshal(raw["task_files"], &index) != nil {
		return nil, errors.New("accepted artifact index unavailable")
	}
	heads := map[string]string{}
	for _, identity := range submission.Request.Artifacts {
		file, exists := index.Artifacts[identity]
		if !exists || file.Kind != "output" || heads[file.Name] != "" {
			return nil, errors.New("accepted artifact reference unavailable or ambiguous")
		}
		heads[file.Name] = identity
	}
	if index.Artifacts == nil {
		index.Artifacts = map[string]publishedTaskFile{}
	}
	index.Heads = heads
	projection := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		projection[k] = v
	}
	projection["task_files"], _ = json.Marshal(index)
	return h.taskFileResources(r.Context(), key, id, projection)
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
	if status, message := h.taskExecutionAdmissionFailure(r.Context(), key, task); status != 0 {
		writeError(w, status, message)
		return false
	}
	return true
}

func (h *ProjectHandler) taskExecutionAdmissionFailure(ctx context.Context, key string, task map[string]any) (int, string) {
	if decision := coordinationString(task, "decision_status"); decision != "" && decision != "open" {
		return 409, "task requires a decision before execution"
	}
	if projectID := coordinationString(task, "project_id"); projectID != "" {
		if !isSafeTaskID(projectID) {
			return 409, "invalid project membership"
		}
		projectKey := path.Join(path.Dir(path.Dir(path.Dir(key))), "projects", projectID, "meta.json")
		body, err := h.oss.GetObject(ctx, projectKey)
		var project map[string]any
		if err != nil || json.Unmarshal(body, &project) != nil {
			return 503, "project membership unavailable"
		}
		if coordinationString(project, "project_id") != projectID || coordinationString(project, "status") != "active" {
			return 409, "project membership is not active"
		}
	}

	return 0, ""
}
