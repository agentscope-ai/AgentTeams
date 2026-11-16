package server

import (
	"context"
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http/httptest"
	"testing"
)

func TestMainTaskIndexIncludesNoProjectAndPlannedChildren(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "in_progress", "room_id": "!main:test", "source_room_id": "!source:test", "tasks": []any{map[string]any{"task_id": "a", "status": "assigned", "assigned_to": "worker", "depends_on": []any{}}, map[string]any{"task_id": "b", "status": "planned", "depends_on": []any{"a"}}}})
	putProject(store, "teams/alpha/shared/tasks/a/meta.json", map[string]any{"task_id": "a", "main_task_id": "main", "status": "submitted"})
	h := &ProjectHandler{oss: store}
	tasks, err := h.appendMainTasks(context.Background(), nil, []string{"teams/alpha/shared/projects/"}, nil, "")
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	byID := map[string]teamTask{}
	for _, task := range tasks {
		byID[task.TaskID] = task
	}
	if byID["a"].Status != "submitted" || byID["b"].Status != "planned" || byID["a"].ParentTaskID != "main" || byID["main"].ProjectID != "" {
		t.Fatalf("wrong Task projection: %+v", tasks)
	}
	hidden, err := h.appendMainTasks(context.Background(), nil, []string{"teams/alpha/shared/projects/"}, map[string]bool{"teams/beta/shared/projects/": true}, "")
	if err != nil || len(hidden) != 0 {
		t.Fatal("task index leaked another team's main task")
	}
}

func TestMainTaskPlanWritesAndMembershipAreProtected(t *testing.T) {
	main := []byte(`{"task_id":"main","task_kind":"main","status":"in_progress"}`)
	worker := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Team: "alpha"}
	lead := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "alpha"}
	if validateMainTaskAuthority(httptest.NewRecorder(), nil, main, worker) {
		t.Fatal("execution Worker created a coordinating plan")
	}
	if !validateMainTaskAuthority(httptest.NewRecorder(), nil, main, lead) {
		t.Fatal("Leader cannot create main task")
	}
	old, _ := json.Marshal(map[string]any{"task_id": "a", "main_task_id": "first"})
	next, _ := json.Marshal(map[string]any{"task_id": "a", "main_task_id": "second"})
	if validateMainTaskAuthority(httptest.NewRecorder(), old, next, lead) {
		t.Fatal("subtask reassigned to an unrelated main task")
	}
}
