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

func TestTaskBeginAcknowledgesOnceAndReturnsAuthoritativeSpec(t *testing.T) {
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(map[string]any{
		"status": "assigned", "assigned_to": "dev", "task_kind": "execution", "project_id": "",
		"execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"},
	}))
	if err := store.PutObject(t.Context(), "teams/alpha-team/shared/tasks/t1/spec.md", []byte("Produce the exact requested report.\n")); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}
	begin := func(process string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin?team=alpha-team", nil)
		r.SetPathValue("id", "t1")
		r.Header.Set("X-AgentTeams-Process-ID", process)
		w := httptest.NewRecorder()
		h.BeginTask(w, withCaller(r, caller))
		return w
	}
	for i := 0; i < 2; i++ {
		response := begin("process-1")
		if response.Code != http.StatusOK {
			t.Fatalf("begin: %d %s", response.Code, response.Body.String())
		}
		var result struct {
			Task struct {
				Status string `json:"status"`
				Spec   string `json:"spec"`
			} `json:"task"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Task.Status != "in_progress" || result.Task.Spec != "Produce the exact requested report.\n" {
			t.Fatalf("context: %s %v", response.Body.String(), err)
		}
	}
	data, err := store.GetObject(t.Context(), taskMetaKey)
	var task struct {
		History []taskTransition `json:"history"`
	}
	if err != nil || json.Unmarshal(data, &task) != nil || len(task.History) != 1 {
		t.Fatalf("duplicate ACK transition: %s %v", data, err)
	}
	if response := begin("old-process"); response.Code != http.StatusConflict {
		t.Fatalf("stale execution: %d %s", response.Code, response.Body.String())
	}
}

func TestTaskBeginRejectsCoordinatingMainAndMissingSpec(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		want       int
	}{
		{"main task", "main", http.StatusConflict},
		{"missing spec", "execution", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "assigned", "project_id": "", "task_kind": tc.kind, "assigned_to": "dev", "execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}}))
			h := newProjectTestHandler(t, store, team("alpha-team"))
			h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
			h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
				return "instance-1", "qwenpaw", nil
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
			r.SetPathValue("id", "t1")
			r.Header.Set("X-AgentTeams-Process-ID", "process-1")
			w := httptest.NewRecorder()
			h.BeginTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
			data, _ := store.GetObject(t.Context(), taskMetaKey)
			var task map[string]any
			_ = json.Unmarshal(data, &task)
			if task["status"] != "assigned" {
				t.Fatalf("failed begin changed task state: %s", data)
			}
		})
	}
}

func TestTaskBeginRequiresAcceptedInputsAndActiveMain(t *testing.T) {
	for _, tc := range []struct {
		name, mainStatus, inputStatus string
		want                          int
	}{
		{"accepted input", "assigned", "completed", http.StatusOK},
		{"candidate input", "assigned", "submitted", http.StatusConflict},
		{"cancelled main", "cancelled", "completed", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "assigned", "assigned_to": "dev", "main_task_id": "main", "project_id": "", "task_kind": "execution", "execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}}))
			putTask(store, "teams/alpha-team/shared/tasks/main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "status": tc.mainStatus, "tasks": []map[string]any{{"task_id": "t1", "assigned_to": "dev", "depends_on": []string{"upstream"}}, {"task_id": "upstream", "assigned_to": "other-worker", "status": tc.inputStatus}}})
			putTask(store, "teams/alpha-team/shared/tasks/upstream/meta.json", map[string]any{"task_id": "upstream", "main_task_id": "main", "project_id": "", "status": tc.inputStatus, "summary": "Accepted input", "deliverables": []string{"shared/tasks/upstream/workspace/data.csv"}})
			for key, data := range map[string]string{"teams/alpha-team/shared/tasks/t1/spec.md": "Consume the accepted input.", "teams/alpha-team/shared/tasks/upstream/workspace/data.csv": "id,value\na,1\n"} {
				if err := store.PutObject(t.Context(), key, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			h := newProjectTestHandler(t, store, team("alpha-team"))
			h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
			h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
				return "instance-1", "qwenpaw", nil
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
			r.SetPathValue("id", "t1")
			r.Header.Set("X-AgentTeams-Process-ID", "process-1")
			w := httptest.NewRecorder()
			h.BeginTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK {
				var response struct {
					Inputs []struct {
						TaskID   string             `json:"taskId"`
						ReadOnly bool               `json:"readOnly"`
						Files    []taskFileResource `json:"files"`
					} `json:"inputs"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Inputs) != 1 || response.Inputs[0].TaskID != "upstream" || !response.Inputs[0].ReadOnly || len(response.Inputs[0].Files) != 1 || response.Inputs[0].Files[0].Name != "data.csv" || response.Inputs[0].Files[0].Version == "" {
					t.Fatalf("input context: %s %v", w.Body.String(), err)
				}
			}
		})
	}
}

func TestTaskBeginHonorsDecisionAndProjectPause(t *testing.T) {
	for _, tc := range []struct{ name, decision, project string }{
		{"frozen task", "frozen", ""},
		{"paused project", "open", "p1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "assigned", "project_id": tc.project, "decision_status": tc.decision, "task_kind": "execution", "assigned_to": "dev", "spec": "Do the assigned work.", "execution": map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}}))
			if tc.project != "" {
				putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{"project_id": "p1", "status": "paused"})
			}
			h := newProjectTestHandler(t, store, team("alpha-team"))
			h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
			h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
				return "instance-1", "qwenpaw", nil
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
			r.SetPathValue("id", "t1")
			r.Header.Set("X-AgentTeams-Process-ID", "process-1")
			w := httptest.NewRecorder()
			h.BeginTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
			if w.Code != http.StatusConflict {
				t.Fatalf("blocked execution accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
