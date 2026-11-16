package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaskRecoveryUsesActualHarnessInsteadOfDesiredSpec(t *testing.T) {
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1beta1.WorkerSpec{Runtime: "codex"}}
	b := mocks.NewMockWorkerBackend()
	actualRuntime := "hermes"
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning, InstanceID: "actual-start", Runtime: actualRuntime}, nil
	}
	h := NewTaskRecoveryHandler(nil, ServerDeps{Namespace: "default", Client: fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build(), Backend: backend.NewRegistry([]backend.WorkerBackend{b})})
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", Team: "team"}
	id, runtime, err := h.currentInstance(t.Context(), caller)
	if err != nil || runtime != "hermes" || id != "mock/actual-start" {
		t.Fatalf("actual instance: %q %q %v", id, runtime, err)
	}
	actualRuntime = ""
	if _, _, err := h.currentInstance(t.Context(), caller); err == nil {
		t.Fatal("unverifiable legacy runtime used desired spec as proof")
	}
}

func TestTaskRecoveryFencesCheckpointAndProcess(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	putTask(store, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alpha-dev"}))
	project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	h := NewTaskRecoveryHandler(project, ServerDeps{})
	instance := "container-one/started-one"
	h.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) { return instance, "codex", nil }
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"}
	call := func(action, process string, checkpoint any) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": action, "instance_id": instance, "process_id": process, "checkpoint": checkpoint})
		r := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/execution?team=alpha-team", strings.NewReader(string(body))), caller)
		r.SetPathValue("id", "t1")
		r.Header.Set("If-Match", taskMetaETag(t, store, taskMetaKey))
		w := httptest.NewRecorder()
		h.UpdateExecution(w, r)
		return w
	}
	if r := call("claim", "process-one", nil); r.Code != 200 {
		t.Fatalf("claim: %d %s", r.Code, r.Body)
	}
	if r := call("claim", "competing-process", nil); r.Code != 409 {
		t.Fatalf("concurrent execution: %d %s", r.Code, r.Body)
	}
	artifact := []byte("step one completed once")
	sum := sha256.Sum256(artifact)
	key := "teams/alpha-team/shared/tasks/t1/checkpoints/one.txt"
	cp := map[string]any{"version": 1, "completed_steps": []string{"one"}, "remaining_steps": []string{"two", "three"}, "artifacts": []map[string]any{{"key": key, "sha256": hex.EncodeToString(sum[:]), "size": len(artifact)}}}
	if r := call("checkpoint", "process-one", cp); r.Code != 409 {
		t.Fatalf("missing artifact: %d %s", r.Code, r.Body)
	}
	if err := store.PutObject(ctx, key, artifact); err != nil {
		t.Fatal(err)
	}
	if r := call("checkpoint", "process-one", cp); r.Code != 200 {
		t.Fatalf("checkpoint: %d %s", r.Code, r.Body)
	}
	if r := call("checkpoint", "process-one", cp); r.Code != 409 {
		t.Fatalf("old checkpoint: %d %s", r.Code, r.Body)
	}
	unknown := map[string]any{"version": 2, "completed_steps": []string{"one"}, "remaining_steps": []string{"two", "three"}, "operation": map[string]any{"id": "external-write", "state": "started"}}
	if r := call("checkpoint", "process-one", unknown); r.Code != 200 {
		t.Fatalf("record operation: %d %s", r.Code, r.Body)
	}
	instance = "container-two/started-two"
	if r := call("checkpoint", "process-one", map[string]any{"version": 2}); r.Code != 409 {
		t.Fatalf("old process: %d %s", r.Code, r.Body)
	}
	if r := call("claim", "process-two", nil); r.Code != 200 {
		t.Fatalf("replacement: %d %s", r.Code, r.Body)
	}
	data, _ := store.GetObject(ctx, taskMetaKey)
	var task map[string]any
	_ = json.Unmarshal(data, &task)
	execution := task["execution"].(map[string]any)
	if task["status"] != "in_progress" || task["task_id"] != "t1" || execution["generation"] != float64(2) || execution["state"] != "needs_verification" || execution["checkpoint"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("lost task continuity: %s", data)
	}
	if r := call("checkpoint", "process-two", map[string]any{"version": 3, "completed_steps": []string{"one", "two"}}); r.Code != 409 {
		t.Fatalf("blind replay: %d %s", r.Code, r.Body)
	}
	verified := map[string]any{"version": 3, "completed_steps": []string{"one", "two"}, "remaining_steps": []string{"three"}, "operation": map[string]any{"id": "external-write", "state": "verified", "evidence": "external idempotency key returned object 123"}}
	if r := call("checkpoint", "process-two", verified); r.Code != 200 {
		t.Fatalf("verify side effect: %d %s", r.Code, r.Body)
	}
	if r := call("checkpoint", "process-two", map[string]any{"version": 4, "completed_steps": []string{"one"}}); r.Code != 409 {
		t.Fatalf("lost step two: %d %s", r.Code, r.Body)
	}
	// A generic metadata commit cannot erase the execution fence.
	delete(task, "execution")
	if r := putTaskMetaRequest(t, project, "t1", taskMetaETag(t, store, taskMetaKey), taskMetaBody(task), caller); r.Code != 403 {
		t.Fatalf("erased fence: %d %s", r.Code, r.Body)
	}
	// Cancellation stays authoritative even for the current process.
	task["status"] = "cancelled"
	putTask(store, taskMetaKey, task)
	if r := call("claim", "process-two", nil); r.Code != 409 {
		t.Fatalf("cancelled task: %d %s", r.Code, r.Body)
	}
}

func TestRecoverableTasksOnlyReturnsOwnAssignments(t *testing.T) {
	store := ossfake.NewMemory()
	project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"), teamWithRoster("other-team"))
	for _, tc := range []struct{ key, owner, status string }{
		{taskMetaKey, "alpha-dev", "in_progress"},
		{"teams/alpha-team/shared/tasks/other/meta.json", "someone-else", "in_progress"},
		{"teams/alpha-team/shared/tasks/done/meta.json", "alpha-dev", "completed"},
		{"teams/other-team/shared/tasks/foreign/meta.json", "alpha-dev", "in_progress"},
	} {
		parts := strings.Split(tc.key, "/")
		putTask(store, tc.key, taskMeta(map[string]any{"task_id": parts[len(parts)-2], "assigned_to": tc.owner, "status": tc.status}))
	}
	h := NewTaskRecoveryHandler(project, ServerDeps{})
	h.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return "instance", "hermes", nil
	}
	r := withCaller(httptest.NewRequest("GET", "/api/v1/tasks/recoverable", nil), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"})
	w := httptest.NewRecorder()
	h.ListRecoverableTasks(w, r)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var result struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Tasks) != 1 {
		t.Fatalf("scope leaked: %s (%v)", w.Body, err)
	}
}

func TestStandaloneWorkerDiscoveryIsEmptyWithoutInspectingStorageOrBackend(t *testing.T) {
	h := NewTaskRecoveryHandler(nil, ServerDeps{})
	r := withCaller(httptest.NewRequest("GET", "/api/v1/tasks/recoverable", nil), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "solo"})
	w := httptest.NewRecorder()
	h.ListRecoverableTasks(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tasks":[]`) {
		t.Fatalf("standalone discovery: %d %s", w.Code, w.Body)
	}
}

func TestSubmittedTaskRecoversPersistedReportWithoutReexecuting(t *testing.T) {
	for _, reportPath := range []string{"result.md", "reports/final.md"} {
		t.Run(reportPath, func(t *testing.T) {
			store := ossfake.NewMemory()
			putTask(store, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alpha-dev", "status": "submitted", "submission_id": "submission-one", "result_path": "shared/tasks/t1/" + reportPath,
				"execution": taskExecution{Generation: 1, InstanceID: "old-instance", ProcessID: "old-process", Runtime: "codex", State: "released",
					Checkpoint: &taskCheckpoint{Version: 3, CompletedSteps: []string{"one", "two", "three"}, RemainingSteps: []string{}}}}))
			project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
			h := NewTaskRecoveryHandler(project, ServerDeps{})
			h.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
				return "new-instance", "hermes", nil
			}
			claim := func() *httptest.ResponseRecorder {
				r := withCaller(httptest.NewRequest("POST", "/api/v1/tasks/t1/execution?team=alpha-team", strings.NewReader(`{"action":"claim","instance_id":"new-instance","process_id":"new-process"}`)), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"})
				r.SetPathValue("id", "t1")
				r.Header.Set("If-Match", taskMetaETag(t, store, taskMetaKey))
				w := httptest.NewRecorder()
				h.UpdateExecution(w, r)
				return w
			}
			if missing := claim(); missing.Code != 409 {
				t.Fatalf("missing report must fail closed: %d %s", missing.Code, missing.Body)
			}
			report := []byte("All steps completed once. Saved before the Worker stopped.")
			if err := store.PutObject(t.Context(), "teams/alpha-team/shared/tasks/t1/"+reportPath, report); err != nil {
				t.Fatal(err)
			}
			w := claim()
			var result struct {
				Execution   taskExecution `json:"execution"`
				SavedResult string        `json:"saved_result"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("recover saved report: %d %s", w.Code, w.Body)
			}
			cp := result.Execution.Checkpoint
			if result.SavedResult != string(report) || cp == nil || cp.Version != 4 || len(cp.CompletedSteps) != 3 || cp.Delivery != "pending" || cp.ReplyTxn == "" || cp.Result == nil {
				t.Fatalf("saved submission lost delivery: %s", w.Body)
			}
			repeated := claim()
			var retry struct {
				Execution taskExecution `json:"execution"`
			}
			if repeated.Code != 200 || json.Unmarshal(repeated.Body.Bytes(), &retry) != nil || retry.Execution.Checkpoint.ReplyTxn != cp.ReplyTxn || retry.Execution.Checkpoint.Version != cp.Version {
				t.Fatalf("delivery retry changed its identity: %d %s", repeated.Code, repeated.Body)
			}
			if err := store.PutObject(t.Context(), cp.Result.Key, []byte("changed after submission")); err != nil {
				t.Fatal(err)
			}
			if changed := claim(); changed.Code != 409 {
				t.Fatalf("changed persisted result must fail closed: %d %s", changed.Code, changed.Body)
			}
		})
	}
}
