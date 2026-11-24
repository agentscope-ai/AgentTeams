package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskPlanNode struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	AssignedTo string   `json:"assignedTo"`
	Spec       string   `json:"spec"`
	DependsOn  []string `json:"dependsOn"`
}

type taskPlanRequest struct {
	Nodes []taskPlanNode `json:"nodes"`
}
type taskPlan struct {
	ID        string          `json:"id"`
	ActorName string          `json:"actor_name"`
	RoomID    string          `json:"room_id,omitempty"`
	Request   taskPlanRequest `json:"request"`
}

type taskPlanAssignment struct {
	TaskID string `json:"taskId"`
	taskNotificationStatus
}

func (h *ProjectHandler) taskPlanAssignments(ctx context.Context, key string, main map[string]any) ([]taskPlanAssignment, error) {
	assignments := []taskPlanAssignment{}
	nodes, _ := main["tasks"].([]any)
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		id := coordinationString(node, "task_id")
		if !isSafeTaskID(id) {
			return nil, fmt.Errorf("invalid execution identity")
		}
		body, err := h.oss.GetObject(ctx, path.Join(path.Dir(path.Dir(key)), id, "meta.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var child map[string]any
		if err != nil || json.Unmarshal(body, &child) != nil {
			return nil, fmt.Errorf("assignment receipt unavailable")
		}
		if child["task_id"] != id || child["main_task_id"] != main["task_id"] || child["task_plan_id"] != coordinationString(coordinationMap(main, "task_plan"), "id") {
			return nil, fmt.Errorf("assignment receipt identity changed")
		}
		if state := coordinationString(child, "status"); state != "prepared" && state != "assigned" {
			continue
		}
		var notices map[string]taskNotificationIntent
		encoded, _ := json.Marshal(child["task_notifications"])
		if json.Unmarshal(encoded, &notices) != nil {
			return nil, fmt.Errorf("assignment receipt unavailable")
		}
		notice, exists := notices["assigned"]
		if !exists {
			return nil, fmt.Errorf("assignment receipt missing")
		}
		assignments = append(assignments, taskPlanAssignment{TaskID: id, taskNotificationStatus: taskNotificationStatus{Status: notice.Status, Recipient: notice.Recipient, EventID: notice.EventID}})
	}
	return assignments, nil
}

func normalizeTaskPlan(input *taskPlanRequest) error {
	if len(input.Nodes) == 0 || len(input.Nodes) > 50 {
		return fmt.Errorf("plan requires 1 to 50 execution nodes")
	}
	byID := map[string]taskPlanNode{}
	for i := range input.Nodes {
		node := &input.Nodes[i]
		if !isSafeTaskID(node.ID) || len(node.ID) > 64 || node.AssignedTo == "" || strings.TrimSpace(node.Spec) == "" {
			return fmt.Errorf("each node requires a safe local id, assignedTo and spec")
		}
		if _, exists := byID[node.ID]; exists {
			return fmt.Errorf("duplicate plan node id")
		}
		node.Title = strings.TrimSpace(node.Title)
		if node.Title == "" {
			node.Title = node.ID
		}
		if node.DependsOn == nil {
			return fmt.Errorf("each node must provide dependsOn; use [] only for independent work and list every required upstream node")
		}
		sort.Strings(node.DependsOn)
		for j, dependency := range node.DependsOn {
			if dependency == node.ID || !isSafeTaskID(dependency) || (j > 0 && dependency == node.DependsOn[j-1]) {
				return fmt.Errorf("invalid or duplicate dependency")
			}
		}
		byID[node.ID] = *node
	}
	visited, active := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		node, exists := byID[id]
		if !exists {
			return fmt.Errorf("dependency is not a node of this plan")
		}
		if active[id] {
			return fmt.Errorf("cyclic plan dependency")
		}
		if visited[id] {
			return nil
		}
		active[id] = true
		for _, dependency := range node.DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		active[id], visited[id] = false, true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	sort.Slice(input.Nodes, func(i, j int) bool { return input.Nodes[i].ID < input.Nodes[j].ID })
	return nil
}

func taskPlanExecutionID(team, main, node string) string {
	identity, _ := json.Marshal([]string{team, main, node})
	sum := sha256.Sum256(identity)
	return "exec-" + hex.EncodeToString(sum[:16])
}

// PlanTask preserves the registered request and stores all future dispatch specs
// before any room or execution side effect. Local node IDs are plan-scoped.
func (h *ProjectHandler) PlanTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, 400, "invalid main task id")
		return
	}
	var input taskPlanRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, "invalid task plan")
		return
	}
	if err := normalizeTaskPlan(&input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	if h.coordination == nil {
		writeError(w, 503, "task coordination unavailable")
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(r.Context(), key)
		var main map[string]any
		if err != nil || json.Unmarshal(body, &main) != nil {
			writeError(w, 503, "main task snapshot unavailable")
			return
		}
		if main["task_id"] != id || main["task_kind"] != "main" {
			writeError(w, 409, "plan requires a registered coordinating main task")
			return
		}
		if !h.ownsMainTask(w, r, main) {
			return
		}
		caller := authpkg.CallerFromContext(r.Context())
		leader, err := h.coordination.worker(r.Context(), caller.Username)
		if err != nil {
			writeError(w, 403, "registered Leader required")
			return
		}
		if !h.validatePlanAssignees(w, r, input.Nodes, leader.Name) {
			return
		}
		canonical, _ := json.Marshal([]any{team, id, input, leader.Name})
		sum := sha256.Sum256(canonical)
		plan := taskPlan{ID: hex.EncodeToString(sum[:]), ActorName: leader.Name, Request: input}
		if raw, exists := main["task_plan"]; exists {
			encoded, _ := json.Marshal(raw)
			var old taskPlan
			if json.Unmarshal(encoded, &old) != nil || old.ID == "" {
				writeError(w, 503, "plan receipt unavailable")
				return
			}
			if old.ID != plan.ID {
				writeError(w, 409, "main task already has a different execution plan")
				return
			}
			h.writeTaskPlan(w, r, key, id, team)
			return
		}
		nodes, _ := main["tasks"].([]any)
		if main["status"] != "planned" || len(nodes) != 0 || strings.TrimSpace(coordinationString(main, "spec")) == "" {
			writeError(w, 409, "main task is not awaiting its first execution plan")
			return
		}
		if !h.taskExecutionAdmission(w, r, key, main) {
			return
		}
		nodes = []any{}
		for _, node := range input.Nodes {
			dependencies := []string{}
			for _, dependency := range node.DependsOn {
				dependencies = append(dependencies, taskPlanExecutionID(team, id, dependency))
			}
			nodes = append(nodes, map[string]any{"task_id": taskPlanExecutionID(team, id, node.ID), "step_id": node.ID, "title": node.Title, "assigned_to": node.AssignedTo, "depends_on": dependencies, "status": "planned", "task_kind": "execution", "parent_task_id": id,
				"spec": "# Original request\n" + coordinationString(main, "spec") + "\n\n# Your assignment\n" + node.Spec})
		}
		main["task_plan"], main["tasks"], main["plan_type"], main["dispatch_mode"] = plan, nodes, "dag", "automatic"
		next, _ := json.Marshal(main)
		if len(next) > taskMetaCommitMaxBytes {
			writeError(w, 413, "plan exceeds task record limit")
			return
		}
		strict, supported := h.oss.(conditionalWriter)
		if !supported {
			writeError(w, 503, "conditional task storage required")
			return
		}
		if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			writeError(w, 503, "plan commit uncertain; retry unchanged plan")
			return
		}
		h.writeTaskPlan(w, r, key, id, team)
		return
	}
	writeError(w, 409, "main task changed while saving plan; retry unchanged plan")
}

func (h *ProjectHandler) writeTaskPlan(w http.ResponseWriter, r *http.Request, key, id, team string) {
	err := h.reconcileTaskPlan(r.Context(), key)
	body, readErr := h.oss.GetObject(r.Context(), key)
	var main map[string]any
	if readErr != nil || json.Unmarshal(body, &main) != nil {
		writeError(w, 503, "plan saved; retry unchanged plan to obtain its receipt")
		return
	}
	assignments, receiptErr := h.taskPlanAssignments(r.Context(), key, main)
	if receiptErr != nil {
		err = receiptErr
	}
	progress := "ready"
	message := "Task system retained your execution plan and synchronized its ready assignments. Do not delegate these nodes again."
	if err != nil {
		progress = "pending"
		message = "Task system saved your plan; room preparation or automatic dispatch is pending and will be retried. Do not recreate the main task or delegate these nodes yourself."
	}
	if taskStatusIsTerminal(coordinationString(main, "status")) {
		progress = "stopped"
		message = "Task system retained your plan; the main task is terminal and no execution will be reopened."
	}
	httputil.WriteJSON(w, 200, struct {
		Task        taskResource         `json:"task"`
		Progress    string               `json:"progress"`
		NextAction  string               `json:"nextAction"`
		Assignments []taskPlanAssignment `json:"assignments"`
	}{publicTaskResource(id, team, main), progress, message, assignments})
}

func validateTaskPlanCommit(w http.ResponseWriter, current, next []byte) bool {
	for _, field := range []string{"task_plan", "task_plan_id"} {
		a, had := rawJSONField(current, field)
		b, has := rawJSONField(next, field)
		if had != has || (had && !sameJSONValue(a, b)) {
			writeError(w, 403, "task plans and assignment identities are server-owned; use task plan")
			return false
		}
	}
	if _, had := rawJSONField(current, "task_plan"); had {
		for _, field := range []string{"tasks", "task_attempts", "room_id", "plan_type", "dispatch_mode", "status"} {
			a, had := rawJSONField(current, field)
			b, has := rawJSONField(next, field)
			if had != has || (had && !sameJSONValue(a, b)) {
				writeError(w, 403, "planned main-task projections are server-owned")
				return false
			}
		}
	}
	if _, had := rawJSONField(current, "task_plan_id"); had {
		for _, field := range []string{"task_id", "task_kind", "main_task_id", "parent_task_id", "team_id", "project_id", "assigned_to", "notifyTxn", "room_id", "spec", "spec_path", "decision_class", "revision_of"} {
			a, had := rawJSONField(current, field)
			b, has := rawJSONField(next, field)
			if had != has || (had && !sameJSONValue(a, b)) {
				writeError(w, 403, "planned execution assignment is server-owned")
				return false
			}
		}
	}
	return true
}

func (h *ProjectHandler) validatePlanAssignees(w http.ResponseWriter, r *http.Request, nodes []taskPlanNode, leader string) bool {
	caller := authpkg.CallerFromContext(r.Context())
	for i := range nodes {
		worker, err := h.coordination.worker(r.Context(), nodes[i].AssignedTo)
		if err != nil || worker.Name == leader {
			writeError(w, 403, "execution must target an independent registered Worker")
			return false
		}
		if _, err := h.coordination.memberTeam(r.Context(), worker.Name, "worker", caller.Team); err != nil {
			writeError(w, 403, "execution assignee is outside your Team")
			return false
		}
		var target coordinationEvent
		if err := h.coordination.targetWorker(&target, worker); err != nil {
			writeError(w, 409, "execution assignee is not ready for task coordination")
			return false
		}
		nodes[i].AssignedTo = worker.Name
	}
	return true
}
