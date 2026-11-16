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

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

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
