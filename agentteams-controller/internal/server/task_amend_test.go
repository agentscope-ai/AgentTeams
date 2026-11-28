package server

import (
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskAmendRejectsWorkerAndClosedNode(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/amend?team=alpha-team", strings.NewReader(`{"spec":"new requirement","note":"user update"}`))
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	h.AmendTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	if w.Code != 403 {
		t.Fatalf("worker amend: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskAmendValidatesRequiredChangeNote(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	for _, body := range []string{`{"spec":"changed"}`, `{"spec":"new","note":"why","status":"completed"}`} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/amend?team=alpha-team", strings.NewReader(body))
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.AmendTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead", Team: "alpha-team"}))
		if w.Code != 400 {
			t.Fatalf("invalid amend: %d %s", w.Code, w.Body.String())
		}
	}

}

func TestTaskAmendDurablyNotifiesAssigneeWithoutNewAssignment(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	key := "teams/alpha/shared/tasks/exec-one/meta.json"
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "assigned_to": "@lead:test", "status": "in_progress", "room_id": "!task:test", "tasks": []any{map[string]any{"task_id": "exec-one"}}})
	putTask(store, key, map[string]any{"task_id": "exec-one", "task_kind": "execution", "main_task_id": "main", "team_id": "alpha", "assigned_to": "worker-cr", "status": "in_progress", "spec": "old", "assignment_id": "original-assignment", "notifyTxn": "original-delivery"})
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/exec-one/amend?team=alpha", strings.NewReader(`{"spec":"new requirement","note":"user changed workflow"}`))
		r.SetPathValue("id", "exec-one")
		w := httptest.NewRecorder()
		h.AmendTask(w, withCaller(r, leader))
		return w
	}
	coordination := h.coordination
	h.coordination = nil
	first := call()
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"notification":{"status":"pending"`) {
		t.Fatalf("amendment must retain a pending wake across unavailable transport: %d %s", first.Code, first.Body.String())
	}
	sends := 0
	transaction := ""
	mx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
		if transaction != "" && transaction != r.URL.Path {
			t.Errorf("retry changed Matrix transaction: %s != %s", transaction, r.URL.Path)
		}
		transaction = r.URL.Path
		var content map[string]any
		_ = json.NewDecoder(r.Body).Decode(&content)
		mentions := coordinationMap(content, "m.mentions")["user_ids"].([]any)
		if r.Header.Get("Authorization") != "Bearer leader-token" || !strings.Contains(r.URL.Path, "/send/m.room.message/") || len(mentions) != 1 || mentions[0] != "@worker:test" || content["org.agentteams.progress_only"] != false || content["org.agentteams.coordination"] != nil || !strings.Contains(coordinationString(content, "body"), "exec-one") {
			t.Errorf("amendment did not use the ordinary directed task-room message: %s %+v", r.URL.Path, content)
		}
		if sends == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"event_id":"$amend"}`))
	}))
	defer mx.Close()
	h.coordination = coordination
	coordination.deps.MatrixConfig.ServerURL = mx.URL
	if retry := call(); retry.Code != 200 || !strings.Contains(retry.Body.String(), `"notification":{"status":"pending"`) {
		t.Fatalf("transport failure lost wake intent: %d %s", retry.Code, retry.Body.String())
	}
	for i := 0; i < 2; i++ {
		next := call()
		if next.Code != 200 || !strings.Contains(next.Body.String(), `"status":"queued"`) {
			t.Fatalf("retry failed: %d %s", next.Code, next.Body.String())
		}
	}
	body, _ := store.GetObject(t.Context(), key)
	var task map[string]any
	_ = json.Unmarshal(body, &task)
	if sends != 2 || task["assignment_id"] != "original-assignment" || task["notifyTxn"] != "original-delivery" || task["status"] != "in_progress" || len(task["history"].([]any)) != 1 {
		t.Fatalf("amend duplicated dispatch or changed execution identity: sends=%d %s", sends, body)
	}
}

func TestPendingTaskAmendDoesNotWakeClosedExecution(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	key := "teams/alpha/shared/tasks/exec-one/meta.json"
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "assigned_to": "@lead:test", "status": "in_progress", "room_id": "!task:test", "tasks": []any{map[string]any{"task_id": "exec-one"}}})
	intent := taskNotificationIntent{Status: "pending", ActorRole: leader.Role, ActorName: leader.Username, Team: leader.Team, Event: coordinationEvent{Kind: "task_assigned", TaskID: "exec-one", MainTaskID: "main", DeliveryID: "original-delivery", Text: "changed"}}
	putTask(store, key, map[string]any{"task_id": "exec-one", "task_kind": "execution", "main_task_id": "main", "team_id": "alpha", "assigned_to": "worker-cr", "status": "completed", "notifyTxn": "original-delivery", "task_notifications": map[string]taskNotificationIntent{"amended-one": intent}})
	for i := 0; i < 2; i++ {
		if result := h.flushTaskNotification(t.Context(), key, "amended-one"); result.Status != "superseded" {
			t.Fatalf("closed execution must not be woken: %+v", result)
		}
	}
	body, _ := store.GetObject(t.Context(), key)
	if !strings.Contains(string(body), `"status":"superseded"`) {
		t.Fatalf("closed notification was not retired durably: %s", body)
	}
}

func TestLeaderAmendsOwnedRunningExecutionWithoutChangingIdentity(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	key := "teams/alpha/shared/tasks/exec-one/meta.json"
	putTask(store, key, map[string]any{"task_id": "exec-one", "task_kind": "execution", "main_task_id": "main", "team_id": "alpha", "assigned_to": "worker-cr", "status": "in_progress", "spec": "old", "assignment_id": "original-assignment"})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/exec-one/amend?team=alpha", strings.NewReader(`{"spec":"new requirement","note":"user changed workflow"}`))
		r.SetPathValue("id", "exec-one")
		w := httptest.NewRecorder()
		h.AmendTask(w, withCaller(r, leader))
		if w.Code != 200 {
			t.Fatalf("leader amend: %d %s", w.Code, w.Body.String())
		}
	}
	body, _ := store.GetObject(t.Context(), key)
	var task map[string]any
	if json.Unmarshal(body, &task) != nil || task["spec"] != "new requirement" || task["status"] != "in_progress" || task["assignment_id"] != "original-assignment" || len(task["history"].([]any)) != 1 {
		t.Fatalf("bad amendment: %s", body)
	}
	leader.Username = "worker-cr"
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/exec-one/amend?team=alpha", strings.NewReader(`{"spec":"other","note":"unauthorized"}`))
	r.SetPathValue("id", "exec-one")
	w := httptest.NewRecorder()
	h.AmendTask(w, withCaller(r, leader))
	if w.Code != 403 {
		t.Fatalf("foreign leader: %d %s", w.Code, w.Body.String())
	}
}
