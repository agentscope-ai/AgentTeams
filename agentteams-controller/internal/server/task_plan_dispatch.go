package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

func (h *ProjectHandler) readTaskPlan(ctx context.Context, key, planID string) (map[string]any, taskPlan, []byte, error) {
	body, err := h.oss.GetObject(ctx, key)
	var main map[string]any
	var plan taskPlan
	if err != nil || json.Unmarshal(body, &main) != nil {
		return nil, plan, nil, fmt.Errorf("main task snapshot unavailable")
	}
	encoded, _ := json.Marshal(main["task_plan"])
	if json.Unmarshal(encoded, &plan) != nil || !validArtifactDigest(plan.ID) || (planID != "" && plan.ID != planID) {
		return nil, plan, nil, fmt.Errorf("saved task plan unavailable or changed")
	}
	if main["task_kind"] != "main" || !isSafeTaskID(coordinationString(main, "task_id")) {
		return nil, plan, nil, fmt.Errorf("invalid planned main task")
	}
	return main, plan, body, nil
}

func (h *ProjectHandler) reconcileTaskPlan(ctx context.Context, key string) error {
	strict, supported := h.oss.(conditionalWriter)
	if !supported || h.coordination == nil {
		return fmt.Errorf("task plan coordination unavailable")
	}
	var main map[string]any
	var plan taskPlan
	for attempt := 0; attempt < 3; attempt++ {
		var body []byte
		var err error
		main, plan, body, err = h.readTaskPlan(ctx, key, "")
		if err != nil {
			return err
		}
		if taskStatusIsTerminal(coordinationString(main, "status")) {
			return nil
		}
		if status, message := h.taskExecutionAdmissionFailure(ctx, key, main); status != 0 {
			return fmt.Errorf("%s", message)
		}
		if strings.HasPrefix(coordinationString(main, "room_id"), "!") && plan.RoomID == main["room_id"] {
			break
		}
		room, err := h.coordination.ensureTaskPlanRoom(ctx, main, plan)
		if err != nil {
			return err
		}
		main["room_id"] = room
		plan.RoomID = room
		main["task_plan"] = plan
		next, _ := json.Marshal(main)
		if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return err
		}
		break
	}
	if !strings.HasPrefix(coordinationString(main, "room_id"), "!") {
		return oss.ErrPreconditionFailed
	}
	main, plan, _, err := h.readTaskPlan(ctx, key, plan.ID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(coordinationString(main, "room_id"), "!") {
		return oss.ErrPreconditionFailed
	}
	nodes, _ := main["tasks"].([]any)
	states := map[string]string{}
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		states[coordinationString(node, "task_id")] = coordinationString(node, "status")
	}
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		if state := coordinationString(node, "status"); state != "planned" && state != "assigned" {
			continue
		}
		encoded, _ := json.Marshal(node["depends_on"])
		var dependencies []string
		if json.Unmarshal(encoded, &dependencies) != nil {
			return fmt.Errorf("invalid planned dependencies")
		}
		ready := true
		for _, dependency := range dependencies {
			if states[dependency] != "completed" {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if err := h.dispatchTaskPlanNode(ctx, key, plan, node); err != nil {
			return err
		}
	}
	return nil
}

func (h *ProjectHandler) dispatchTaskPlanNode(ctx context.Context, mainKey string, plan taskPlan, node map[string]any) error {
	main, _, _, err := h.readTaskPlan(ctx, mainKey, plan.ID)
	if err != nil {
		return err
	}
	if taskStatusIsTerminal(coordinationString(main, "status")) {
		return fmt.Errorf("main task is terminal")
	}
	if status, message := h.taskExecutionAdmissionFailure(ctx, mainKey, main); status != 0 {
		return fmt.Errorf("%s", message)
	}
	id, mainID := coordinationString(node, "task_id"), coordinationString(main, "task_id")
	if !isSafeTaskID(id) || coordinationString(node, "spec") == "" {
		return fmt.Errorf("invalid execution node")
	}
	key := path.Join(path.Dir(path.Dir(mainKey)), id, "meta.json")
	actorRole, actorName := authpkg.RoleTeamLeader, plan.ActorName
	var lastCompletion time.Time
	// Validate actual accepted dependency identities before creating work. Begin
	// repeats this check and materializes their exact immutable file versions.
	dependencies, _ := node["depends_on"].([]any)
	if dependencies == nil {
		data, _ := json.Marshal(node["depends_on"])
		_ = json.Unmarshal(data, &dependencies)
	}
	for _, dependency := range dependencies {
		name, ok := dependency.(string)
		if !ok || !isSafeTaskID(name) {
			return fmt.Errorf("invalid prerequisite")
		}
		body, err := h.oss.GetObject(ctx, path.Join(path.Dir(path.Dir(mainKey)), name, "meta.json"))
		var child map[string]any
		if err != nil || json.Unmarshal(body, &child) != nil {
			return fmt.Errorf("accepted prerequisite unavailable")
		}
		if child["main_task_id"] != mainID || !taskResultCompleted(child) || !taskReviewMembership(main, child) {
			return fmt.Errorf("prerequisite is not an accepted candidate of this main task")
		}
		if handoff := taskHandoffOf(child); handoff != nil {
			completed, err := time.Parse(time.RFC3339Nano, coordinationString(child, "submitted_at"))
			if err != nil {
				return fmt.Errorf("prerequisite completion time unavailable")
			}
			if lastCompletion.IsZero() || completed.After(lastCompletion) {
				actorRole, actorName, lastCompletion = handoff.ActorRole, handoff.ActorName, completed
			}
		}
	}
	create, ok := h.oss.(createIfAbsentWriter)
	if !ok {
		return oss.ErrConditionalUnsupported
	}
	spec := []byte(coordinationString(node, "spec"))
	if err := create.PutObjectIfNoneMatchStrict(ctx, path.Dir(key)+"/spec.md", spec); err != nil {
		if !errors.Is(err, oss.ErrPreconditionFailed) {
			return err
		}
		saved, err := h.oss.GetObject(ctx, path.Dir(key)+"/spec.md")
		if err != nil || string(saved) != string(spec) {
			return fmt.Errorf("execution specification collision")
		}
	}
	delivery := "assign-" + plan.ID[:16] + "-" + id
	child := map[string]any{"task_id": id, "task_kind": "execution", "task_plan_id": plan.ID, "main_task_id": mainID, "parent_task_id": mainID,
		"project_id": coordinationString(main, "project_id"), "team_id": coordinationString(main, "team_id"), "assigned_to": node["assigned_to"], "title": node["title"],
		"spec": node["spec"], "spec_path": "shared/tasks/" + id + "/spec.md", "room_id": main["room_id"], "status": "prepared", "notifyTxn": delivery, "decision_class": "r3",
		"task_notifications": map[string]taskNotificationIntent{"assigned": {Event: coordinationEvent{Kind: "task_assigned", TaskID: id, MainTaskID: mainID, ProjectID: coordinationString(main, "project_id"), DeliveryID: delivery}, ActorRole: actorRole, ActorName: actorName, Team: coordinationString(main, "team_id"), Status: "pending"}}}
	if class := coordinationString(node, "decision_class"); class != "" {
		child["decision_class"] = class
	}
	if previous := coordinationString(node, "revision_of"); previous != "" {
		child["revision_of"] = previous
	}
	appendTaskOperationHistory(child, "planned", "prepared", "prepare_task", "task-system")
	encoded, _ := json.Marshal(child)
	if len(encoded) > taskMetaCommitMaxBytes {
		return fmt.Errorf("execution record limit exceeded")
	}
	if err := create.PutObjectIfNoneMatchStrict(ctx, key, encoded); err != nil && !errors.Is(err, oss.ErrPreconditionFailed) {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(ctx, key)
		child = nil
		if err != nil || json.Unmarshal(body, &child) != nil {
			return fmt.Errorf("prepared execution unavailable")
		}
		if child["task_plan_id"] != plan.ID || child["main_task_id"] != mainID || child["assigned_to"] != node["assigned_to"] || child["notifyTxn"] != delivery || child["spec"] != node["spec"] {
			return fmt.Errorf("execution identity collision")
		}
		state := coordinationString(child, "status")
		if state == "in_progress" || state == "submitted" || state == "completed" || state == "revision" || state == "blocked" {
			return nil
		}
		if state != "prepared" && state != "assigned" {
			return fmt.Errorf("execution cannot be assigned")
		}
		if notice := h.flushTaskNotification(ctx, key, "assigned"); notice.Status != "queued" {
			return fmt.Errorf("execution notification pending")
		}
		if err := h.assignTaskPlanNode(ctx, mainKey, plan.ID, id); err != nil {
			return err
		}
		// flushTaskNotification may have updated the child CAS token.
		body, err = h.oss.GetObject(ctx, key)
		child = nil
		if err != nil || json.Unmarshal(body, &child) != nil {
			return fmt.Errorf("queued execution unavailable")
		}
		if child["task_plan_id"] != plan.ID || child["notifyTxn"] != delivery {
			return fmt.Errorf("queued execution identity changed")
		}
		if state := coordinationString(child, "status"); state != "prepared" {
			if state == "assigned" || state == "in_progress" || state == "submitted" || state == "completed" || state == "revision" || state == "blocked" {
				return nil
			}
			return fmt.Errorf("queued execution can no longer be activated")
		}
		child["status"] = "assigned"
		appendTaskOperationHistory(child, "prepared", "assigned", "assign_task", "task-system")
		next, _ := json.Marshal(child)
		strict := h.oss.(conditionalWriter)
		if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return err
		}
		h.coordination.wakeRunner()
		return nil
	}
	return oss.ErrPreconditionFailed
}

func (h *ProjectHandler) assignTaskPlanNode(ctx context.Context, key, planID, id string) error {
	strict := h.oss.(conditionalWriter)
	for attempt := 0; attempt < 3; attempt++ {
		main, _, body, err := h.readTaskPlan(ctx, key, planID)
		if err != nil {
			return err
		}
		if taskStatusIsTerminal(coordinationString(main, "status")) {
			return fmt.Errorf("main task is terminal")
		}
		if status, message := h.taskExecutionAdmissionFailure(ctx, key, main); status != 0 {
			return fmt.Errorf("%s", message)
		}
		nodes, _ := main["tasks"].([]any)
		found := false
		for _, item := range nodes {
			node, _ := item.(map[string]any)
			if node["task_id"] != id {
				continue
			}
			found = true
			if node["status"] == "assigned" {
				return nil
			}
			if node["status"] != "planned" {
				return fmt.Errorf("execution node is no longer awaiting assignment")
			}
			node["status"] = "assigned"
		}
		if !found {
			return fmt.Errorf("execution node missing from plan")
		}
		if main["status"] == "planned" {
			main["status"] = "assigned"
			appendTaskOperationHistory(main, "planned", "assigned", "assign_task", "task-system")
		}
		next, _ := json.Marshal(main)
		if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return oss.ErrPreconditionFailed
}
