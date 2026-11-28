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

func taskSubmitFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory, *authpkg.CallerIdentity, string) {
	t.Helper()
	coordination, store := coordinationFixture(t)
	putTask(store, "teams/alpha/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "team_id": "alpha", "status": "in_progress", "room_id": "!task:test", "tasks": []map[string]any{{"task_id": "t1", "assigned_to": "worker-cr", "status": "in_progress"}}})
	putTask(store, "teams/alpha/shared/tasks/t1/meta.json", map[string]any{"task_id": "t1", "task_kind": "execution", "main_task_id": "main", "project_id": "", "status": "in_progress", "assigned_to": "worker-cr", "notifyTxn": "assign-1", "spec": "Create a report.", "execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}})
	h := NewProjectHandler(coordination.deps.Client, "default", store)
	h.coordination = coordination
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", Team: "alpha"}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=report.md&kind=output", strings.NewReader("Verified report\n"))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.PublishTaskFile(w, withCaller(r, caller))
	if w.Code != 200 {
		t.Fatalf("publish fixture: %d %s", w.Code, w.Body.String())
	}
	var file publishedTaskFile
	if err := json.Unmarshal(w.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	return h, store, caller, file.ArtifactID
}

func TestTaskSubmitCommitsCandidateAndQueuesExactlyOneLeaderEvent(t *testing.T) {
	h, store, caller, artifact := taskSubmitFixture(t)
	call := func(summary string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"summary": summary, "artifacts": []string{artifact}, "report": "Detailed verified report."})
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
		r.SetPathValue("id", "t1")
		r.Header.Set("X-AgentTeams-Process-ID", "process-1")
		w := httptest.NewRecorder()
		h.SubmitTask(w, withCaller(r, caller))
		return w
	}
	first, retry := call("Report complete"), call("Report complete")
	if first.Code != 200 || retry.Code != 200 {
		t.Fatalf("submit/retry: %d %s / %d %s", first.Code, first.Body.String(), retry.Code, retry.Body.String())
	}
	var a, b struct {
		Task         taskResource `json:"task"`
		Notification struct {
			Status    string `json:"status"`
			EventID   string `json:"eventId"`
			Recipient string `json:"recipient"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Task.Status != "submitted" || a.Task.SubmissionID == "" || a.Task.SubmissionID != b.Task.SubmissionID || a.Notification.Recipient != "lead-cr" || a.Notification.Status != "queued" || a.Notification.EventID == "" || a.Notification.EventID != b.Notification.EventID {
		t.Fatalf("invalid candidate/notification: %s / %s", first.Body.String(), retry.Body.String())
	}
	for _, tc := range []struct {
		content string
		want    int
	}{{"Verified report\n", 200}, {"Different report\n", 409}} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/files?name=report.md&kind=output", strings.NewReader(tc.content))
		r.SetPathValue("id", "t1")
		r.Header.Set("X-AgentTeams-Process-ID", "process-1")
		w := httptest.NewRecorder()
		h.PublishTaskFile(w, withCaller(r, caller))
		if w.Code != tc.want {
			t.Fatalf("submitted artifact retry: %d %s", w.Code, w.Body.String())
		}
	}
	if strings.Contains(first.Body.String(), "Controller") || !strings.Contains(first.Body.String(), "Task system") {
		t.Fatalf("infrastructure exposed in tool result: %s", first.Body.String())
	}
	list := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files", nil)
	list.SetPathValue("id", "t1")
	lw := httptest.NewRecorder()
	h.GetTaskFiles(lw, withCaller(list, caller))
	var listing struct {
		Files []taskFileResource `json:"files"`
	}
	if lw.Code != 200 || json.Unmarshal(lw.Body.Bytes(), &listing) != nil || len(listing.Files) != 1 || listing.Files[0].Name != "report.md" || listing.Files[0].ArtifactID != artifact {
		t.Fatalf("submission exposed storage aliases instead of output names: %d %s", lw.Code, lw.Body.String())
	}
	data, err := store.GetObject(t.Context(), coordinationPending+a.Notification.EventID+".json")
	var receipt coordinationDelivery
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Event.Kind != "task_submitted" || receipt.Event.Recipient != "@lead:test" {
		t.Fatalf("missing Leader event: %s %v", data, err)
	}
	if changed := call("Different result"); changed.Code != 409 {
		t.Fatalf("changed retry accepted: %d %s", changed.Code, changed.Body.String())
	}
	data, err = store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	var task struct {
		History []taskTransition `json:"history"`
	}
	if err != nil || json.Unmarshal(data, &task) != nil || len(task.History) != 1 {
		t.Fatalf("duplicate submit history: %s %v", data, err)
	}
}

func TestTaskSubmitRefusesUnregisteredArtifacts(t *testing.T) {
	h, store, caller, _ := taskSubmitFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(`{"summary":"Done","artifacts":["not-registered"]}`))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, caller))
	if w.Code != 404 {
		t.Fatalf("missing artifact accepted: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	if !strings.Contains(string(data), `"status":"in_progress"`) {
		t.Fatalf("failed submit changed task: %s", data)
	}
}

func TestTaskSubmitNotificationRecoversAfterCommitWithoutCoordinator(t *testing.T) {
	h, store, caller, artifact := taskSubmitFixture(t)
	coordinator := h.coordination
	h.coordination = nil // Model the gap between Task commit and queue admission.
	body, _ := json.Marshal(map[string]any{"summary": "Done", "artifacts": []string{artifact}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, caller))
	var result struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
		NextAction   string                 `json:"nextAction"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Task.Status != "submitted" || result.Notification.Status != "pending" || result.Notification.EventID != "" || !strings.Contains(result.NextAction, "Do not resend") {
		t.Fatalf("uncertain notification misreported: %d %s", w.Code, w.Body.String())
	}
	h.coordination = coordinator
	h.recoverTaskNotifications(t.Context())
	first := h.flushTaskNotification(t.Context(), "teams/alpha/shared/tasks/t1/meta.json", "submitted")
	h.recoverTaskNotifications(t.Context())
	second := h.flushTaskNotification(t.Context(), "teams/alpha/shared/tasks/t1/meta.json", "submitted")
	if first.Status != "queued" || first.EventID == "" || first != second {
		t.Fatalf("recovery not idempotent: %+v / %+v", first, second)
	}
	entries, _, err := store.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate or missing notifications: %v %v", entries, err)
	}
}

func TestTaskSubmitRejectsStaleProcessAndCorruptArtifact(t *testing.T) {
	for _, scenario := range []string{"stale-process", "corrupt-artifact"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, caller, artifact := taskSubmitFixture(t)
			process, want := "process-1", 503
			if scenario == "stale-process" {
				process, want = "process-old", 409
			} else {
				data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
				var task struct {
					Files taskFileIndex `json:"task_files"`
				}
				_ = json.Unmarshal(data, &task)
				if err := store.PutObject(t.Context(), "teams/alpha/shared/tasks/t1/artifacts/"+task.Files.Artifacts[artifact].SHA256, []byte("corrupt")); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"summary": "Done", "artifacts": []string{artifact}})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
			r.SetPathValue("id", "t1")
			r.Header.Set("X-AgentTeams-Process-ID", process)
			w := httptest.NewRecorder()
			h.SubmitTask(w, withCaller(r, caller))
			if w.Code != want {
				t.Fatalf("submission accepted: %d %s", w.Code, w.Body.String())
			}
			data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
			if strings.Contains(string(data), `"task_submission"`) {
				t.Fatalf("failed submission persisted: %s", data)
			}
		})
	}
}

func TestTaskSubmissionAndNotificationCannotBeForgedThroughLegacyMeta(t *testing.T) {
	for _, field := range []string{"task_submission", "task_notifications"} {
		t.Run(field, func(t *testing.T) {
			h, store, caller, _ := taskSubmitFixture(t)
			data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
			var task map[string]any
			_ = json.Unmarshal(data, &task)
			task[field] = map[string]any{"forged": true}
			next, _ := json.Marshal(task)
			r := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta", strings.NewReader(string(next)))
			r.SetPathValue("id", "t1")
			r.Header.Set("If-Match", contentETag(data))
			r.Header.Set("X-AgentTeams-Process-ID", "process-1")
			w := httptest.NewRecorder()
			h.PutTaskMeta(w, withCaller(r, caller))
			if w.Code != 403 {
				t.Fatalf("server receipt forged: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTaskSubmitBlockedPreservesUnconfirmedOperation(t *testing.T) {
	for _, tc := range []struct {
		name, status, process, worker string
		want                          int
	}{
		{"blocked report", "BLOCKED", "process-1", "worker-cr", 200},
		{"success remains fenced", "SUCCESS", "process-1", "worker-cr", 409},
		{"old process", "BLOCKED", "other-process", "worker-cr", 409},
		{"other worker", "BLOCKED", "process-1", "other-worker", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, caller, _ := taskSubmitFixture(t)
			key := "teams/alpha/shared/tasks/t1/meta.json"
			body, _ := store.GetObject(t.Context(), key)
			var task map[string]any
			if err := json.Unmarshal(body, &task); err != nil {
				t.Fatal(err)
			}
			execution := task["execution"].(map[string]any)
			execution["state"] = "needs_verification"
			execution["checkpoint"] = map[string]any{"version": 4, "operation": map[string]any{"id": "pending-write", "state": "started"}}
			before, _ := json.Marshal(execution)
			putTask(store, key, task)
			caller.Username = tc.worker
			var first string
			for i := 0; i < 2; i++ {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(`{"summary":"Tool result requires verification","status":"`+tc.status+`","report":"Original response unavailable; do not repeat the write."}`))
				r.SetPathValue("id", "t1")
				r.Header.Set("X-AgentTeams-Process-ID", tc.process)
				w := httptest.NewRecorder()
				h.SubmitTask(w, withCaller(r, caller))
				if w.Code != tc.want {
					t.Fatalf("submit: %d %s", w.Code, w.Body.String())
				}
				if tc.want == 200 {
					var result struct {
						Notification struct {
							EventID   string `json:"eventId"`
							Recipient string `json:"recipient"`
						} `json:"notification"`
					}
					if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Notification.EventID == "" || result.Notification.Recipient != "lead-cr" {
						t.Fatalf("missing Leader notification: %s", w.Body.String())
					}
					if i == 0 {
						first = result.Notification.EventID
					} else if first != result.Notification.EventID {
						t.Fatal("duplicate blocked report queued another event")
					}
				}
			}
			body, _ = store.GetObject(t.Context(), key)
			if err := json.Unmarshal(body, &task); err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(task["execution"])
			if string(before) != string(after) {
				t.Fatalf("unconfirmed execution changed: %s", after)
			}
		})
	}
}
