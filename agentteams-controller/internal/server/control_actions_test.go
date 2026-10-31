package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// --- Control-action audit ledger (meta.json controlActions) ---

func TestControlActionAuditRecordedOnSuccess(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause",
		strings.NewReader(`{"reason":"checkpoint"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	actions, _ := meta["controlActions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("controlActions=%v, want exactly 1 entry", meta["controlActions"])
	}
	entry, _ := actions[0].(map[string]any)
	if entry["action"] != "pause" || entry["actor"] != "admin" || entry["role"] != "admin" {
		t.Fatalf("entry=%v, want pause by admin/admin", entry)
	}
	if entry["result"] != "ok" || entry["project_id"] != "p1" || entry["reason"] != "checkpoint" {
		t.Fatalf("entry=%v, want result=ok project_id=p1 reason=checkpoint", entry)
	}
	if entry["action_id"] == "" || entry["at"] == "" {
		t.Fatalf("entry missing action_id/at: %v", entry)
	}
}

func TestControlActionAuditRecordedOnError(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel",
		strings.NewReader(`{"reason":"obsolete"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	actions, _ := meta["controlActions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("controlActions=%v, want exactly 1 entry", meta["controlActions"])
	}
	entry, _ := actions[0].(map[string]any)
	if entry["action"] != "cancel" || entry["result"] != "error" {
		t.Fatalf("entry=%v, want cancel result=error", entry)
	}
	if entry["reason"] != "task not found in project" {
		t.Fatalf("entry reason=%v, want the 404 message", entry["reason"])
	}
}

func TestGetProjectEvents_KindControlActionFilter(t *testing.T) {
	store := ossfake.NewMemory()
	putEventsProject(t, store)
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause",
		strings.NewReader(`{"reason":"checkpoint"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Default feed: task-transition aggregation only, no control events.
	resp := decodeEvents(t, doGetProjectEvents(t, h, "/api/v1/projects/p1/events"))
	if len(resp.Events) == 0 {
		t.Fatalf("default feed empty")
	}
	for _, e := range resp.Events {
		if strings.HasPrefix(e.Action, "control.") {
			t.Fatalf("default feed leaked control event: %+v", e)
		}
	}

	// kind=control_action: the audit ledger family, derived from meta.
	ctrl := decodeEvents(t, doGetProjectEvents(t, h, "/api/v1/projects/p1/events?kind=control_action"))
	if len(ctrl.Events) != 1 {
		t.Fatalf("control feed=%+v, want exactly 1 event", ctrl)
	}
	e := ctrl.Events[0]
	if e.TaskID != "" || e.Actor != "admin" || e.Action != "control.pause" || e.Note != "checkpoint" {
		t.Fatalf("control event=%+v, want admin control.pause note=checkpoint", e)
	}

	// Unknown kind is rejected rather than silently ignored.
	bad := doGetProjectEvents(t, h, "/api/v1/projects/p1/events?kind=bogus")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("kind=bogus status=%d body=%s, want 400", bad.Code, bad.Body.String())
	}
}

func TestControlActionsTrimKeepsNewest(t *testing.T) {
	meta := &projectMeta{ProjectID: "p1"}
	for i := 0; i < controlActionsLimit+1; i++ {
		appendControlAction(meta, fmt.Sprintf("act-%d", i), "admin", "admin", "", "ok")
	}
	if len(meta.ControlActions) != controlActionsLimit {
		t.Fatalf("len=%d, want %d", len(meta.ControlActions), controlActionsLimit)
	}
	if meta.ControlActionsTruncatedCount != 1 {
		t.Fatalf("truncated=%d, want 1", meta.ControlActionsTruncatedCount)
	}
	if got := meta.ControlActions[len(meta.ControlActions)-1].Action; got != "act-200" {
		t.Fatalf("last action=%q, want act-200 (newest retained)", got)
	}
	if got := meta.ControlActions[0].Action; got != "act-1" {
		t.Fatalf("first action=%q, want act-1 (oldest dropped)", got)
	}
}
