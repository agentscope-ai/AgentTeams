package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestTaskBeginInputsBindExactSubmittedOutputsNotCatalogHeads(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			store := ossfake.NewMemory()
			h := newProjectTestHandler(t, store, team("alpha-team"))
			root := "teams/alpha-team/shared/tasks/"
			putTask(store, root+"main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "status": "in_progress", "tasks": []map[string]any{{"task_id": "t1", "assigned_to": "dev", "depends_on": []string{"upstream"}}, {"task_id": "upstream", "status": "completed"}}})
			files := map[string]publishedTaskFile{}
			for identity, content := range map[string]string{"accepted": "Accepted version", "newer": "Newer unaccepted version", "debug": "Private intermediate"} {
				sum := sha256.Sum256([]byte(content))
				digest := hex.EncodeToString(sum[:])
				name, kind := "report.md", "output"
				if identity == "debug" {
					name, kind = "debug.txt", "intermediate"
				}
				files[identity] = publishedTaskFile{ArtifactID: identity, Name: name, Kind: kind, Version: digest, SHA256: digest, Size: int64(len(content))}
				if err := store.PutObject(t.Context(), root+"upstream/artifacts/"+digest, []byte(content)); err != nil {
					t.Fatal(err)
				}
			}
			if missing {
				delete(files, "accepted")
			}
			putTask(store, root+"upstream/meta.json", map[string]any{"task_id": "upstream", "main_task_id": "main", "project_id": "", "status": "completed", "submission_id": "s1", "task_submission": taskSubmission{ID: "s1", Request: taskSubmitRequest{Artifacts: []string{"accepted"}}}, "task_files": taskFileIndex{Artifacts: files, Heads: map[string]string{"report.md": "newer", "debug.txt": "debug"}}})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
			inputs, err := h.taskBeginInputs(r, root+"t1/meta.json", map[string]any{"task_id": "t1", "assigned_to": "dev", "main_task_id": "main", "project_id": ""})
			if missing {
				if err == nil {
					t.Fatalf("missing accepted reference silently fell back to newer head: %+v", inputs)
				}
				return
			}
			if err != nil || len(inputs) != 1 || inputs[0].SubmissionID != "s1" || len(inputs[0].Files) != 1 || inputs[0].Files[0].ArtifactID != "accepted" {
				t.Fatalf("incorrect accepted input snapshot: %+v %v", inputs, err)
			}
		})
	}
}

func TestTaskBeginReturnsPublicRecoveryCheckpoint(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	execution := taskExecution{Generation: 2, InstanceID: "instance-1", ProcessID: "process-1", State: "active", Checkpoint: &taskCheckpoint{Version: 3, CompletedSteps: []string{"parsed input"}, RemainingSteps: []string{"write report"}, Operation: &taskOperation{ID: "op1", State: "verified", Evidence: "Checked original operation"}, Artifacts: []taskArtifact{{Key: "private/backend/blob", SHA256: "private-hash"}}, ReplyTxn: "private-transaction", ReplyAttempt: &matrixReplyAttempt{User: "private-user", Device: "private-device"}}}
	putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "in_progress", "assigned_to": "dev", "task_kind": "execution", "project_id": "", "spec": "Continue assigned work", "execution": execution}))
	h.recovery = NewTaskRecoveryHandler(h, ServerDeps{})
	h.recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance-1", "qwenpaw", nil
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.BeginTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	var result struct {
		Checkpoint struct {
			Version        uint64         `json:"version"`
			CompletedSteps []string       `json:"completedSteps"`
			Operation      *taskOperation `json:"operation"`
		} `json:"checkpoint"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Checkpoint.Version != 3 || len(result.Checkpoint.CompletedSteps) != 1 || result.Checkpoint.Operation == nil || result.Checkpoint.Operation.State != "verified" || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "private/") {
		t.Fatalf("missing or private recovery context: %d %s", w.Code, w.Body.String())
	}
	execution.Checkpoint.Result = &taskArtifact{Key: "private/saved-result"}
	execution.Checkpoint.Delivery = "pending"
	putTask(store, taskMetaKey, taskMeta(map[string]any{"status": "in_progress", "assigned_to": "dev", "task_kind": "execution", "project_id": "", "spec": "Continue assigned work", "execution": execution}))
	r = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/begin", nil)
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w = httptest.NewRecorder()
	h.BeginTask(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "delivery recovery") {
		t.Fatalf("saved result restarted business execution: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskBeginPreparesExactRejectedRevisionBase(t *testing.T) {
	for _, invalid := range []string{"", "blocked-base", "foreign-main", "foreign-project", "accepted", "wrong-submission", "wrong-review", "missing-attempt", "wrong-node", "missing-correction"} {
		t.Run(invalid, func(t *testing.T) {
			store := ossfake.NewMemory()
			h := newProjectTestHandler(t, store, team("alpha-team"))
			root := "teams/alpha-team/shared/tasks/"
			data := []byte("rejected implementation that must be corrected")
			digest := sha256.Sum256(data)
			version := hex.EncodeToString(digest[:])
			artifact := publishedTaskFile{ArtifactID: "candidate-a", Name: "app.py", Kind: "output", Version: version, SHA256: version, Size: int64(len(data))}
			if err := store.PutObject(t.Context(), root+"old/artifacts/"+version, data); err != nil {
				t.Fatal(err)
			}
			review := taskReview{ID: "r1", SubmissionID: "s1", Status: "revision", Note: "Fix only the test log", ReplacementSpec: "Fix only the test log", Actor: "leader:lead"}
			old := map[string]any{"task_id": "old", "task_kind": "execution", "main_task_id": "main", "project_id": "", "status": "revision", "submission_id": "s1", "task_review": review,
				"task_submission": taskSubmission{ID: "s1", Request: taskSubmitRequest{Artifacts: []string{"candidate-a"}}},
				"task_files":      taskFileIndex{Artifacts: map[string]publishedTaskFile{"candidate-a": artifact}, Heads: map[string]string{"unsubmitted.txt": "other"}}}
			attempts := map[string]any{"old": map[string]any{"task_id": "old", "status": "revision", "submission_id": "s1", "review_id": "r1"}}
			node := map[string]any{"task_id": "replacement", "assigned_to": "dev", "revision_of": "old", "depends_on": []string{}}
			switch invalid {
			case "blocked-base":
				old["status"], review.Status = "blocked", "blocked"
				attempts["old"].(map[string]any)["status"] = "blocked"
			case "missing-correction":
				review.ReplacementSpec = ""
			case "foreign-main":
				old["main_task_id"] = "other"
			case "foreign-project":
				old["project_id"] = "other"
			case "accepted":
				review.Accepted = true
			case "wrong-submission":
				review.SubmissionID = "other"
			case "wrong-review":
				review.ID = "other"
			case "missing-attempt":
				delete(attempts, "old")
			case "wrong-node":
				node["revision_of"] = "other"
			}
			old["task_review"] = review
			putTask(store, root+"old/meta.json", old)
			putTask(store, root+"main/meta.json", map[string]any{"task_id": "main", "task_kind": "main", "status": "in_progress", "tasks": []any{node}, "task_attempts": attempts})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/replacement/begin", nil)
			inputs, err := h.taskBeginInputs(request, root+"replacement/meta.json", map[string]any{"task_id": "replacement", "main_task_id": "main", "project_id": "", "assigned_to": "dev", "revision_of": "old"})
			if invalid != "" && invalid != "blocked-base" {
				if err == nil {
					t.Fatalf("invalid revision base accepted: %+v", inputs)
				}
				return
			}
			encoded, _ := json.Marshal(inputs)
			var public []map[string]any
			_ = json.Unmarshal(encoded, &public)
			if err != nil || len(inputs) != 1 || inputs[0].TaskID != "old" || inputs[0].SubmissionID != "s1" || !inputs[0].ReadOnly || len(inputs[0].Files) != 1 || inputs[0].Files[0].ArtifactID != "candidate-a" || public[0]["purpose"] != "revisionBase" {
				t.Fatalf("exact rejected candidate not prepared: %s %v", encoded, err)
			}
			if decision, ok := public[0]["review"].(map[string]any); !ok || decision["id"] != "r1" || decision["accepted"] != false {
				t.Fatalf("rejection provenance missing: %s", encoded)
			}
		})
	}
}
