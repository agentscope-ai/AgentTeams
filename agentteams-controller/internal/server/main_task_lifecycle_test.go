package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestHandoffRegistersMainTaskBeforeLeaderAnalysis(t *testing.T) {
	h, store := coordinationFixture(t)
	body := `{"kind":"handoff","recipient":"@lead:test","origin_event_id":"request-1","room_id":"!handoff:test","source_route":{"target_session":"!source:test","target_user":"@human:test"},"text":"Please deliver this request","tasks":[{"taskId":"new-main","title":"A new request","spec":"Original acceptance criteria"}]}`
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	for i := 0; i < 2; i++ {
		req := withCaller(httptest.NewRequest("POST", "/api/v1/coordination/events", strings.NewReader(body)), caller)
		w := httptest.NewRecorder()
		h.PostEvent(w, req)
		if w.Code != 202 {
			t.Fatalf("handoff: %d %s", w.Code, w.Body)
		}
	}
	b, err := store.GetObject(context.Background(), "teams/alpha/shared/tasks/new-main/meta.json")
	if err != nil {
		t.Fatal("handoff did not persist a pending main task:", err)
	}
	var task map[string]any
	if err := json.Unmarshal(b, &task); err != nil {
		t.Fatal(err)
	}
	if task["status"] != "planned" || task["task_kind"] != "main" || task["assigned_to"] != "@lead:test" || task["spec"] != "Original acceptance criteria" {
		t.Fatalf("wrong pre-analysis task: %s", b)
	}
	if task["source_room_id"] != "!source:test" || task["room_id"] != nil {
		t.Fatalf("execution room invented before planning: %s", b)
	}
}

func TestExecutionCommitAdvancesMainTaskWithoutRegressingIt(t *testing.T) {
	for _, tc := range []struct{ initial, child, want string }{
		{"planned", "assigned", "assigned"},
		{"assigned", "in_progress", "in_progress"},
		{"in_progress", "assigned", "in_progress"},
		{"cancelled", "assigned", "cancelled"},
	} {
		t.Run(tc.initial+"_"+tc.child, func(t *testing.T) {
			store := ossfake.NewMemory()
			parentKey := "teams/alpha-team/shared/tasks/parent/meta.json"
			putProject(store, parentKey, map[string]any{"task_id": "parent", "task_kind": "main", "status": tc.initial, "history_seq": 40, "history": []any{map[string]any{"seq": 40}}, "tasks": []any{map[string]any{"task_id": "t1", "assigned_to": "alpha-dev", "status": "planned"}}})
			child := map[string]any{"task_id": "t1", "main_task_id": "parent", "parent_task_id": "parent", "task_kind": "execution", "status": "assigned", "assigned_to": "alpha-dev"}
			putProject(store, taskMetaKey, child)
			h := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
			child["status"] = tc.child
			caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}
			if tc.child == "in_progress" {
				caller.Role, caller.Username = authpkg.RoleWorker, "alpha-dev"
			}
			w := putTaskMetaRequest(t, h, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(child), caller)
			if w.Code != 200 {
				t.Fatalf("child commit: %d %s", w.Code, w.Body)
			}
			b, _ := store.GetObject(context.Background(), parentKey)
			var main map[string]any
			_ = json.Unmarshal(b, &main)
			if main["status"] != tc.want {
				t.Fatalf("main status=%v, want %s; %s", main["status"], tc.want, b)
			}
			if tc.initial != tc.want && main["history_seq"] != float64(41) {
				t.Fatalf("main transition lost its monotonic history cursor: %s", b)
			}
		})
	}
}

func TestMainTaskStorageTeamUsesOneSafePathSegment(t *testing.T) {
	for _, team := range []string{"alpha", "本地体验团队"} {
		if !coordinationStorageTeamSafe(team) {
			t.Fatalf("valid team rejected: %q", team)
		}
	}
	for _, team := range []string{"", ".", "..", "../alpha", "alpha/beta", "alpha\\beta", "alpha\n", " alpha"} {
		if coordinationStorageTeamSafe(team) {
			t.Fatalf("unsafe team accepted: %q", team)
		}
	}
}
