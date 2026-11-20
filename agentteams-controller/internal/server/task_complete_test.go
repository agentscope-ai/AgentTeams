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
)

// This fixture models the input of closure: submission and Leader acceptance
// have already completed. It does not claim to test the acceptance action.
func taskCompleteFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory, *authpkg.CallerIdentity) {
	h, store, worker, artifact := taskSubmitFixture(t)
	body, _ := json.Marshal(map[string]any{"summary": "Accepted report", "artifacts": []string{artifact}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, worker))
	if w.Code != 200 {
		t.Fatalf("fixture submission: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	var child map[string]any
	_ = json.Unmarshal(data, &child)
	child["status"] = "completed"
	putTask(store, "teams/alpha/shared/tasks/t1/meta.json", child)
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "in_progress", "assigned_to": "@lead:test", "room_id": "!task:test", "tasks": []map[string]any{{"task_id": "t1", "assigned_to": "worker-cr", "status": "completed"}}, "reply_route": map[string]any{"target_user": "@manager:test"}, "requester_reply_route": map[string]any{"target_user": "@human:test", "target_session": "!source:test"}})
	return h, store, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}
}

func callTaskComplete(h *ProjectHandler, caller *authpkg.CallerIdentity, report string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"report": report})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/main/complete", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	w := httptest.NewRecorder()
	h.CompleteTask(w, withCaller(r, caller))
	return w
}

func TestTaskCompleteSavesImmutableReportAndQueuesOriginalConversation(t *testing.T) {
	h, store, leader := taskCompleteFixture(t)
	a, b := callTaskComplete(h, leader, "Verified final result."), callTaskComplete(h, leader, "Verified final result.")
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("complete/retry: %d %s / %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	var first, second struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(a.Body.Bytes(), &first)
	_ = json.Unmarshal(b.Body.Bytes(), &second)
	if first.Task.Status != "completed" || first.Notification.Status != "queued" || first.Notification.Recipient != "default" || first.Notification.EventID == "" || first.Notification != second.Notification {
		t.Fatalf("missing completion receipt: %s / %s", a.Body.String(), b.Body.String())
	}
	data, err := store.GetObject(t.Context(), coordinationPending+first.Notification.EventID+".json")
	var delivery coordinationDelivery
	if err != nil || json.Unmarshal(data, &delivery) != nil || delivery.Event.FinalReport != "Verified final result." || coordinationString(delivery.Event.RequesterRoute, "target_session") != "!source:test" {
		t.Fatalf("missing original-conversation report: %s %v", data, err)
	}
	sends := 0
	h.coordination.projectEvent = func(_ context.Context, event coordinationEvent, role, name, room, txn, text string) (string, error) {
		sends++
		if role != "manager" || name != "default" || room != "!source:test" || text != "Verified final result." {
			t.Errorf("wrong final projection: %s %s %s %s", role, name, room, text)
		}
		return "$final", nil
	}
	h.coordination.step(t.Context(), coordinationPending+first.Notification.EventID+".json")
	h.coordination.step(t.Context(), coordinationPending+first.Notification.EventID+".json")
	if sends != 1 {
		t.Fatalf("duplicate final projection: %d", sends)
	}
	if changed := callTaskComplete(h, leader, "Changed report"); changed.Code != 409 {
		t.Fatalf("completed report mutated: %d %s", changed.Code, changed.Body.String())
	}
}

func TestTaskCompleteRefusesUnacceptedMissingAndCorruptResults(t *testing.T) {
	for _, scenario := range []string{"candidate", "wrong-main", "corrupt-artifact", "worker-caller"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, leader := taskCompleteFixture(t)
			want := 409
			data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
			var child map[string]any
			_ = json.Unmarshal(data, &child)
			switch scenario {
			case "candidate":
				child["status"] = "submitted"
			case "wrong-main":
				child["main_task_id"] = "other"
			case "worker-caller":
				leader.Role, leader.Username = authpkg.RoleWorker, "worker-cr"
				want = 403
			case "corrupt-artifact":
				var task struct {
					Files taskFileIndex `json:"task_files"`
				}
				_ = json.Unmarshal(data, &task)
				for _, file := range task.Files.Artifacts {
					if err := store.PutObject(t.Context(), "teams/alpha/shared/tasks/t1/artifacts/"+file.SHA256, []byte("bad")); err != nil {
						t.Fatal(err)
					}
				}
			}
			putTask(store, "teams/alpha/shared/tasks/t1/meta.json", child)
			w := callTaskComplete(h, leader, "Final report")
			if w.Code != want {
				t.Fatalf("invalid closure accepted: %d %s", w.Code, w.Body.String())
			}
			data, _ = store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
			if strings.Contains(string(data), `"task_completion"`) {
				t.Fatalf("invalid completion persisted: %s", data)
			}
		})
	}
}

func TestTaskCompleteRecoversOriginalReplyIntentAfterInterruption(t *testing.T) {
	h, store, leader := taskCompleteFixture(t)
	coordinator := h.coordination
	h.coordination = nil
	w := callTaskComplete(h, leader, "Verified report")
	var first struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &first) != nil || first.Task.CompletionID == "" || first.Notification.Status != "pending" {
		t.Fatalf("interrupted reply misreported: %d %s", w.Code, w.Body.String())
	}
	h.coordination = coordinator
	h.recoverTaskNotifications(t.Context())
	w = callTaskComplete(h, leader, "Verified report")
	var recovered struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &recovered) != nil || recovered.Task.CompletionID != first.Task.CompletionID || recovered.Notification.Status != "queued" {
		t.Fatalf("original reply not recovered: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	var main map[string]json.RawMessage
	_ = json.Unmarshal(data, &main)
	var history []taskTransition
	_ = json.Unmarshal(main["history"], &history)
	if len(history) != 1 {
		t.Fatalf("repeated completion history: %s", data)
	}
	var forged map[string]any
	_ = json.Unmarshal(data, &forged)
	forged["requester_reply_route"] = map[string]any{"target_session": "!other:test", "target_user": "@other:test"}
	body, _ := json.Marshal(forged)
	r := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/main/meta", strings.NewReader(string(body)))
	r.SetPathValue("id", "main")
	r.Header.Set("If-Match", contentETag(data))
	ww := httptest.NewRecorder()
	h.PutTaskMeta(ww, withCaller(r, leader))
	if ww.Code != 403 {
		t.Fatalf("completed task original conversation changed: %d %s", ww.Code, ww.Body.String())
	}
}
