package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestTaskRoomContextUsesScopedTaskRecords(t *testing.T) {
	store := ossfake.NewMemory()
	p := newProjectTestHandler(t, store, teamWithRoster("alpha-team"), teamWithRoster("other-team"))
	h := NewTaskRecoveryHandler(p, ServerDeps{})
	putTask(store, "teams/alpha-team/shared/tasks/main/meta.json", taskMeta(map[string]any{"task_id": "main", "task_kind": "main", "room_id": "!task:test", "status": "in_progress"}))
	putTask(store, "teams/alpha-team/shared/tasks/child/meta.json", taskMeta(map[string]any{"task_id": "child", "task_kind": "execution", "room_id": "!task:test", "status": "in_progress", "assigned_to": "dev"}))
	putTask(store, "teams/other-team/shared/tasks/foreign/meta.json", taskMeta(map[string]any{"task_id": "foreign", "task_kind": "main", "room_id": "!foreign:test", "status": "in_progress"}))
	for _, tc := range []struct{ room, want string }{{"!task:test", `"executionTaskId":"child"`}, {"!foreign:test", "{}"}, {"!dm:test", "{}"}} {
		r := withCaller(httptest.NewRequest(http.MethodGet, "/api/v1/tasks/room-context?roomId="+tc.room, nil), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"})
		w := httptest.NewRecorder()
		h.GetRoomContext(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("room context %s: %d %s", tc.room, w.Code, w.Body.String())
		}
	}
}

func TestTaskRoomContextRejectsForeignNamespace(t *testing.T) {
	h := NewTaskRecoveryHandler(nil, ServerDeps{Namespace: "local"})
	r := withCaller(httptest.NewRequest(http.MethodGet, "/api/v1/tasks/room-context?roomId=!task:test", nil),
		&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Team: "alpha-team", ServiceAccountNamespace: "foreign"})
	w := httptest.NewRecorder()
	h.GetRoomContext(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign namespace: %d %s", w.Code, w.Body.String())
	}
}
