package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type mainTaskRequest struct {
	TaskID string `json:"taskId"`
	Title  string `json:"title"`
	Spec   string `json:"spec"`
}

// The Manager assigns the request before the Leader has an execution plan.
func (h *CoordinationHandler) registerHandoffTasks(ctx context.Context, e coordinationEvent) error {
	if len(e.Tasks) == 0 {
		return nil
	}
	if e.Kind != "handoff" || len(e.Tasks) > 50 {
		return errCoordinationRejected
	}
	seen := map[string]bool{}
	for _, request := range e.Tasks {
		if !coordinationSafeID.MatchString(request.TaskID) || seen[request.TaskID] || request.Title == "" || request.Spec == "" {
			return errCoordinationRejected
		}
		seen[request.TaskID] = true
	}
	for _, request := range e.Tasks {
		key := path.Join("teams", e.StorageTeam, "shared/tasks", request.TaskID, "meta.json")
		task := map[string]any{
			"task_id": request.TaskID, "task_kind": "main", "team_id": e.StorageTeam,
			"title": request.Title, "spec": request.Spec, "status": "planned", "assigned_to": e.Recipient,
			"source_room_id": coordinationString(e.SourceRoute, "target_session"), "requester": coordinationString(e.SourceRoute, "target_user"),
			"reply_route":           map[string]any{"channel": "matrix", "target_user": e.Actor, "target_session": e.RoomID},
			"requester_reply_route": e.SourceRoute, "tasks": []any{}, "handoff_id": e.ID,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		body, _ := json.Marshal(task)
		err := h.deps.OSS.PutObjectIfAbsent(ctx, key, body)
		if errors.Is(err, oss.ErrPreconditionFailed) {
			existing, readErr := h.read(ctx, key)
			if readErr != nil {
				return readErr
			}
			if existing["handoff_id"] != e.ID || existing["spec"] != request.Spec || existing["title"] != request.Title || existing["assigned_to"] != e.Recipient {
				return oss.ErrPreconditionFailed
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// Only a durable child assignment/ACK advances the parent's execution phase.
// Workers keep writing their own task; the Controller owns this parent update.
func (h *ProjectHandler) advanceMainTaskExecution(ctx context.Context, childKey string, body []byte) error {
	var child map[string]any
	if err := json.Unmarshal(body, &child); err != nil {
		return err
	}
	id := coordinationString(child, "main_task_id")
	status := coordinationString(child, "status")
	if id == "" || (status != "assigned" && status != "in_progress") {
		return nil
	}
	if !isSafeTaskID(id) {
		return fmt.Errorf("invalid parent task")
	}
	key := path.Join(path.Dir(path.Dir(childKey)), id, "meta.json")
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := h.oss.GetObject(ctx, key)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		} // Legacy tasks may lack a coordinating record.
		if err != nil {
			return err
		}
		var main map[string]any
		if err := json.Unmarshal(raw, &main); err != nil {
			return err
		}
		current := coordinationString(main, "status")
		if main["task_kind"] != "main" || (current != "planned" && current != "assigned") || current == status {
			return nil
		}
		nodes, _ := main["tasks"].([]any)
		matched := false
		for _, item := range nodes {
			node, _ := item.(map[string]any)
			if node["task_id"] == child["task_id"] && node["assigned_to"] == child["assigned_to"] {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("execution task does not match its main plan")
		}
		main["status"] = status
		history, _ := main["history"].([]any)
		seq, _ := main["history_seq"].(float64)
		if seq < float64(len(history)) {
			seq = float64(len(history))
		}
		seq++
		main["history_seq"] = seq
		main["history"] = append(history, map[string]any{"seq": seq, "from": current, "to": status,
			"action": "execution_" + status, "actor": "controller", "ts": time.Now().UTC().Format(time.RFC3339)})
		updated, _ := json.Marshal(main)
		strict, ok := h.oss.(conditionalWriter)
		if !ok {
			return oss.ErrConditionalUnsupported
		}
		err = strict.PutObjectIfMatchStrict(ctx, key, updated, contentETag(raw))
		if err == nil {
			return nil
		}
		if !errors.Is(err, oss.ErrPreconditionFailed) {
			return err
		}
	}
	return oss.ErrPreconditionFailed
}
