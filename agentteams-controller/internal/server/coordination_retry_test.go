package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func pendingReview(t *testing.T) (*CoordinationHandler, *ossfake.Memory, coordinationDelivery) {
	t.Helper()
	h, store := coordinationFixture(t)
	putProject(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{
		"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "in_progress",
		"task_plan": map[string]any{"id": "plan"}, "room_id": "!task:test",
		"tasks": []any{map[string]any{"task_id": "t1", "status": "submitted"}},
	})
	putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "main_task_id": "main", "status": "submitted",
		"assigned_to": "@worker:test", "submission_id": "submission-1",
	})
	event, err := h.resolve(t.Context(), coordinationEvent{Kind: "task_submitted", MainTaskID: "main",
		TaskID: "t1", SubmissionID: "submission-1"},
		&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", Team: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	return h, store, coordinationDelivery{Event: event, Transport: "matrix", Status: "running",
		NativeID: "matrix:$review", MatrixEventID: "$review"}
}

func TestCoordinationPhaseCallbackIsScopedAndRetryIsDurable(t *testing.T) {
	h, store, delivery := pendingReview(t)
	start := service.WorkerStart{InstanceID: "instance", ProcessID: "process", Runtime: "qwenpaw"}
	matrixStore := service.WorkerMatrixStore{Storage: store}
	if _, _, err := matrixStore.Start(t.Context(), "lead-runtime", start, ""); err != nil {
		t.Fatal(err)
	}
	key := coordinationPending + delivery.Event.ID + ".json"
	put := func() { b, _ := json.Marshal(delivery); _ = store.PutObject(t.Context(), key, b) }
	put()
	call := func(action, native, process, actor string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"action": action, "native_event_id": native, "process_id": process})
		r := httptest.NewRequest(http.MethodPost, "/phase", strings.NewReader(string(body)))
		r.SetPathValue("id", delivery.Event.ID)
		r.Header.Set("X-AgentTeams-Process-ID", process)
		w := httptest.NewRecorder()
		h.NativePhase(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: actor, Team: "alpha"}))
		return w
	}
	for _, request := range []struct {
		native, process, actor string
		status                 int
	}{
		{"$review", "process", "worker-cr", 403},
		{"$foreign", "process", "lead-cr", 409},
		{"$review", "stale", "lead-cr", 409},
	} {
		if w := call("begin", request.native, request.process, request.actor); w.Code != request.status {
			t.Fatalf("scope check %d: %s", w.Code, w.Body.String())
		}
	}
	if w := call("begin", "$review", "process", "lead-cr"); w.Code != 200 {
		t.Fatalf("begin %d: %s", w.Code, w.Body.String())
	}
	if w := call("retry", "$review", "process", "lead-cr"); w.Code != 200 {
		t.Fatalf("retry %d: %s", w.Code, w.Body.String())
	}
	b, _ := store.GetObject(t.Context(), key)
	delivery = coordinationDelivery{}
	if json.Unmarshal(b, &delivery) != nil || delivery.Replays != 1 || delivery.NativeStart != nil || delivery.NativeID != "" {
		t.Fatal("retry intent was not saved independently of the native process")
	}
	if w := call("retry", "$review", "process", "lead-cr"); w.Code != 200 {
		t.Fatalf("lost callback response %d: %s", w.Code, w.Body.String())
	}
	b, _ = store.GetObject(t.Context(), key)
	_ = json.Unmarshal(b, &delivery)
	if delivery.Replays != 1 {
		t.Fatal("duplicate callback created another delivery attempt")
	}
}

func TestCoordinationRegisteredStartReplacementRecoversWithoutEndCallback(t *testing.T) {
	h, store, delivery := pendingReview(t)
	old := service.WorkerStart{InstanceID: "old", ProcessID: "old-process", Runtime: "qwenpaw"}
	delivery.NativeStart = &old
	matrixStore := service.WorkerMatrixStore{Storage: store}
	_, etag, err := matrixStore.Start(t.Context(), "lead-runtime", old, "")
	if err != nil {
		t.Fatal(err)
	}
	if queued, err := h.recoverChangedNativeStart(t.Context(), &delivery); err != nil || queued {
		t.Fatal("a live registered start was replayed")
	}
	next := service.WorkerStart{InstanceID: "new", ProcessID: "new-process", Runtime: "qwenpaw"}
	if _, _, err := matrixStore.Start(t.Context(), "lead-runtime", next, etag); err != nil {
		t.Fatal(err)
	}
	if queued, err := h.recoverChangedNativeStart(t.Context(), &delivery); err != nil || !queued || delivery.Replays != 1 {
		t.Fatalf("lost end callback left phase stuck: %+v %v", delivery, err)
	}
}

func TestCoordinationRetryRetainsExactTaskAndCandidate(t *testing.T) {
	h, store, delivery := pendingReview(t)
	before, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	identity, session := delivery.Event.ID, coordinationSession(delivery.Event)
	queued, err := h.retryAfterNativeFailure(t.Context(), &delivery, "matrix:$review")
	if err != nil || !queued || delivery.NativeID != "" || delivery.Status != "queued" || delivery.Replays != 1 {
		t.Fatalf("no durable-phase retry: %+v %v", delivery, err)
	}
	if delivery.Event.ID != identity || coordinationSession(delivery.Event) != session || delivery.Event.SubmissionID != "submission-1" {
		t.Fatal("retry changed logical work identity")
	}
	after, _ := store.GetObject(context.Background(), "teams/alpha/shared/tasks/t1/meta.json")
	if string(before) != string(after) {
		t.Fatal("retry changed business state")
	}
	delivery.NativeID = "matrix:$retry"
	queued, err = h.retryAfterNativeFailure(t.Context(), &delivery, "matrix:$review")
	if err != nil || !queued || delivery.Replays != 1 || delivery.NativeID != "matrix:$retry" {
		t.Fatal("duplicate completion callback restarted a newer attempt")
	}
}

func TestCoordinationRetryDoesNotReplayCommittedOrChangedCandidate(t *testing.T) {
	for _, state := range []string{"completed", "cancelled", "revision"} {
		t.Run(state, func(t *testing.T) {
			h, store, delivery := pendingReview(t)
			putProject(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{
				"task_id": "t1", "main_task_id": "main", "status": state,
				"assigned_to": "@worker:test", "submission_id": "submission-1",
			})
			queued, err := h.retryAfterNativeFailure(t.Context(), &delivery, "matrix:$review")
			if err != nil || queued || delivery.Replays != 0 {
				t.Fatalf("terminal work was replayed: %+v %v", delivery, err)
			}
		})
	}
	h, _, delivery := pendingReview(t)
	if _, err := h.retryAfterNativeFailure(t.Context(), &delivery, "matrix:$foreign"); err == nil {
		t.Fatal("foreign native attempt was accepted")
	}
}

func TestCoordinationSlowActiveTurnNeverRetriesWithoutCompletionCallback(t *testing.T) {
	h, _, delivery := pendingReview(t)
	for range 3 {
		h.advance(t.Context(), &delivery, func() error { return nil })
	}
	if delivery.Replays != 0 || delivery.NativeID != "matrix:$review" {
		t.Fatal("polling or elapsed time retried an active native turn")
	}
}

func TestCoordinationCompletionWithUnknownEffectEndsNativeAdmission(t *testing.T) {
	h, store := coordinationFixture(t)
	putProject(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{
		"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "in_progress", "room_id": "!task:test",
		"task_plan": map[string]any{"id": "plan"},
		"tasks":     []any{map[string]any{"task_id": "t1", "status": "completed"}},
	})
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
	event, err := h.resolve(t.Context(), coordinationEvent{Kind: "main_task_ready", MainTaskID: "main"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	start := service.WorkerStart{InstanceID: "instance", ProcessID: "process", Runtime: "openhuman"}
	matrixStore := service.WorkerMatrixStore{Storage: store}
	if _, _, err := matrixStore.Start(t.Context(), "lead-runtime", start, ""); err != nil {
		t.Fatal(err)
	}
	operationKey := "control/task-native/" + canonicalNativeHash([]string{"default", "worker", "lead-cr", "task", "alpha", "main"}) + ".json"
	putProject(store, operationKey, map[string]any{
		"id": "operation", "team": "alpha", "task_id": "main", "action": "complete", "state": "unknown",
	})
	delivery := coordinationDelivery{Event: event, Transport: "matrix", Status: "running",
		NativeID: "matrix:$ready", NativeStart: &start}
	h.advance(t.Context(), &delivery, func() error { return nil })
	if delivery.Status != "failed" || delivery.Replays != 0 || delivery.NativeID != "matrix:$ready" {
		t.Fatalf("an unresolved completion was left eligible for replay: %+v", delivery)
	}
	data, _ := json.Marshal(delivery)
	if err := store.PutObject(t.Context(), coordinationDone+event.ID+".json", data); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/phase", strings.NewReader(`{"action":"begin","native_event_id":"$ready","process_id":"process"}`))
	r.SetPathValue("id", event.ID)
	r.Header.Set("X-AgentTeams-Process-ID", "process")
	w := httptest.NewRecorder()
	h.NativePhase(w, withCaller(r, caller))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"active":false`) || !strings.Contains(w.Body.String(), `"failed":true`) {
		t.Fatalf("terminal native admission %d: %s", w.Code, w.Body.String())
	}
	row, err := h.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if err != nil || row["status"] != "in_progress" {
		t.Fatalf("terminating the receipt changed the main task: %v %v", row, err)
	}
}

func TestCoordinationHandoffWaitsForAllDurablePlans(t *testing.T) {
	h, store := coordinationFixture(t)
	event, err := h.resolve(t.Context(), coordinationEvent{Kind: "handoff", Recipient: "@lead:test",
		OriginEventID: "source", SourceRoute: map[string]any{"target_session": "!source:test", "target_user": "@human:test"},
		Tasks: []mainTaskRequest{{TaskID: "main-a"}, {TaskID: "main-b"}}},
		&authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"main-a", "main-b"} {
		putProject(store, "teams/alpha/shared/tasks/"+id+"/meta.json", map[string]any{
			"task_id": id, "task_kind": "main", "status": "planned",
		})
	}
	delivery := coordinationDelivery{Event: event, Transport: "matrix", Status: "running", NativeID: "matrix:$handoff"}
	h.advance(t.Context(), &delivery, func() error { return nil })
	if delivery.Status == "completed" {
		t.Fatal("Matrix delivery completed an unplanned handoff")
	}
	putProject(store, "teams/alpha/shared/tasks/main-a/meta.json", map[string]any{
		"task_id": "main-a", "task_kind": "main", "status": "in_progress", "task_plan": map[string]any{"id": "plan-a"},
	})
	ids, err := h.handoffPendingTaskIDs(t.Context(), event)
	if err != nil || len(ids) != 1 || ids[0] != "main-b" {
		t.Fatalf("remaining plans: %v %v", ids, err)
	}
	h.advance(t.Context(), &delivery, func() error { return nil })
	if delivery.Status == "completed" {
		t.Fatal("first durable plan completed a multi-task handoff")
	}
	putProject(store, "teams/alpha/shared/tasks/main-b/meta.json", map[string]any{
		"task_id": "main-b", "task_kind": "main", "status": "cancelled",
	})
	h.advance(t.Context(), &delivery, func() error { return nil })
	if delivery.Status != "completed" {
		t.Fatalf("settled handoff remains %s: %s", delivery.Status, delivery.Error)
	}
}

func TestRetryWakesNativeRecipientWithoutRepeatingPublicMessage(t *testing.T) {
	for _, runtime := range []string{"claudecode", "codex", "qwenpaw", "deepseek-harness", "openclaw"} {
		e := coordinationEvent{Protocol: "task/v1", Kind: "main_task_ready", RecipientRuntime: runtime}
		d := coordinationDelivery{}
		if d.notificationEventType(e) != "m.room.message" {
			t.Fatalf("%s lost the initial public handoff", runtime)
		}
		d.Replays = 1
		internal := d.notificationEventType(e) == "org.agentteams.coordination"
		if internal != (runtime != "openclaw") {
			t.Fatalf("%s retry internal=%t", runtime, internal)
		}
	}
}
