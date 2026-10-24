package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func putSignalEvent(t *testing.T, store *ossfake.Memory, key string, ev signalEvent) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal signal event: %v", err)
	}
	if err := store.PutObject(context.Background(), key, data); err != nil {
		t.Fatalf("put signal event: %v", err)
	}
}

func signalTrackerEvent(eventID, taskID, ref, eventType, occurred string) signalEvent {
	return signalEvent{
		EventID:     eventID,
		TaskID:      taskID,
		EventType:   eventType,
		IsSignal:    true,
		SourceEvent: signalSource{Origin: "decision_lock"},
		Ref:         ref,
		Fingerprint: "fp-" + eventID,
		Payload:     map[string]any{"from_status": "active", "to_status": "paused", "action": "pause_task"},
		OccurredAt:  occurred,
	}
}

func getSignalEvents(t *testing.T, h *ProjectHandler, projectID, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/projects/"+projectID+"/signal-events"+query,
		nil,
	)
	req.SetPathValue("id", projectID)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectSignalEvents(rec, req)
	return rec
}

func decodeSignalEvents(t *testing.T, rec *httptest.ResponseRecorder) signalEventsResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp signalEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func seedSignalProject(t *testing.T) (*ossfake.Memory, *ProjectHandler) {
	t.Helper()
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active",
		"plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "planned",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p2", "status": "planned",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	return store, h
}

func TestGetProjectSignalEvents_FiltersByProject(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	putSignalEvent(t, store, dir+"ev_p1.json", signalTrackerEvent(
		"ev_p1", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-DECISION", "2026-10-24T10:00:00Z",
	))
	putSignalEvent(t, store, dir+"ev_t1.json", signalTrackerEvent(
		"ev_t1", "t1", "teams/alpha-team/shared/tasks/t1/meta.json",
		"SIG-VERDICT", "2026-10-24T10:01:00Z",
	))
	// Foreign project and foreign task: must not leak into p1.
	putSignalEvent(t, store, dir+"ev_p2.json", signalTrackerEvent(
		"ev_p2", "", "teams/alpha-team/shared/projects/p2/meta.json",
		"SIG-DECISION", "2026-10-24T10:02:00Z",
	))
	putSignalEvent(t, store, dir+"ev_t2.json", signalTrackerEvent(
		"ev_t2", "t2", "teams/alpha-team/shared/tasks/t2/meta.json",
		"SIG-VERDICT", "2026-10-24T10:03:00Z",
	))

	resp := decodeSignalEvents(t, getSignalEvents(t, h, "p1", ""))
	if resp.Count != 2 {
		t.Fatalf("count=%d want 2 (project-level + own task): %+v", resp.Count, resp.Events)
	}
	if resp.Events[0].EventID != "ev_p1" || resp.Events[1].EventID != "ev_t1" {
		t.Fatalf("unexpected order/ids: %+v", resp.Events)
	}
}

func TestGetProjectSignalEvents_Pagination(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	for i, at := range []string{
		"2026-10-24T10:00:00Z", "2026-10-24T10:01:00Z", "2026-10-24T10:02:00Z",
	} {
		id := "ev_" + string(rune('a'+i))
		putSignalEvent(t, store, dir+id+".json", signalTrackerEvent(
			id, "", "teams/alpha-team/shared/projects/p1/meta.json",
			"SIG-COMPLETE", at,
		))
	}
	first := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?limit=2"))
	if first.Count != 2 || first.NextCursor == "" {
		t.Fatalf("first page count=%d cursor=%q", first.Count, first.NextCursor)
	}
	second := decodeSignalEvents(t, getSignalEvents(
		t, h, "p1", "?limit=2&cursor="+first.NextCursor,
	))
	if second.Count != 1 {
		t.Fatalf("second page count=%d want 1", second.Count)
	}
	if second.Events[0].EventID == first.Events[0].EventID ||
		second.Events[0].EventID == first.Events[1].EventID {
		t.Fatalf("second page repeated an event: %+v", second.Events)
	}
}

func TestGetProjectSignalEvents_TypeAndSinceFilters(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	putSignalEvent(t, store, dir+"ev_a.json", signalTrackerEvent(
		"ev_a", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-DECISION", "2026-10-24T10:00:00Z",
	))
	putSignalEvent(t, store, dir+"ev_b.json", signalTrackerEvent(
		"ev_b", "", "teams/alpha-team/shared/projects/p1/meta.json",
		"SIG-VERDICT", "2026-10-24T10:05:00Z",
	))

	byType := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?type=SIG-VERDICT"))
	if byType.Count != 1 || byType.Events[0].EventID != "ev_b" {
		t.Fatalf("type filter: %+v", byType.Events)
	}
	bySince := decodeSignalEvents(t, getSignalEvents(t, h, "p1", "?since=2026-10-24T10:01:00Z"))
	if bySince.Count != 1 || bySince.Events[0].EventID != "ev_b" {
		t.Fatalf("since filter: %+v", bySince.Events)
	}
}

func TestGetProjectSignalEvents_UnknownProject404(t *testing.T) {
	_, h := seedSignalProject(t)
	rec := getSignalEvents(t, h, "p-missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}
