package server

import (
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"net/http/httptest"
	"strings"
	"testing"
)

func callNativeOperation(h *ProjectHandler, caller *authpkg.CallerIdentity, request map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	data, _ := json.Marshal(request)
	r := httptest.NewRequest("POST", "/api/v1/tasks/native-operations", strings.NewReader(string(data)))
	w := httptest.NewRecorder()
	r.Header.Set("X-AgentTeams-Process-ID", "canonical-process")
	h.NativeTaskOperation(w, withCaller(r, caller))
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w, result
}
func TestNativeOperationUnknownSurvivesNewRequestAndDoesNotAuthorizeReplay(t *testing.T) {
	h, _, caller := nativeOperationFixture(t)
	args := map[string]any{"action": "plan", "taskId": "main", "nodes": planRequest().Nodes}
	w, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 200 || first["mode"] != "execute" {
		t.Fatalf("begin: %d %s", w.Code, w.Body.String())
	}
	w, blocked := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 200 || blocked["mode"] != "verify" || blocked["authority"] == nil {
		t.Fatalf("restart: %d %s", w.Code, w.Body.String())
	}
	args["nodes"] = []any{}
	_, blocked = callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if blocked["mode"] != "verify" {
		t.Fatal("changed model arguments bypassed unknown fence")
	}
	w, _ = callNativeOperation(h, caller, map[string]any{"mode": "admit", "arguments": map[string]any{"action": "get", "taskId": "main"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestNativeOperationCachesOnlyActualReceiptAndReusesItWithoutMutation(t *testing.T) {
	h, _, caller := nativeOperationFixture(t)
	args := map[string]any{"action": "plan", "taskId": "main", "nodes": planRequest().Nodes}
	_, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	plan := callPlan(h, caller, planRequest())
	if plan.Code != 200 {
		t.Fatal(plan.Body.String())
	}
	var receipt map[string]any
	_ = json.Unmarshal(plan.Body.Bytes(), &receipt)
	w, _ := callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": args, "operationId": first["operationId"], "result": receipt})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_, cached := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if cached["mode"] != "recovered" || cached["result"] == nil {
		t.Fatal(cached)
	}
	// Even an unknown callback from a lost finish response cannot erase the cache.
	w, _ = callNativeOperation(h, caller, map[string]any{"mode": "unknown", "arguments": args, "operationId": first["operationId"]})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_, cached = callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if cached["mode"] != "recovered" {
		t.Fatal(cached)
	}
}
func TestNativeOperationRejectsForeignScopeAndForgedReceipt(t *testing.T) {
	h, _, caller := nativeOperationFixture(t)
	args := map[string]any{"action": "plan", "taskId": "main"}
	_, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	w, _ := callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": args, "operationId": first["operationId"], "result": map[string]any{"task": map[string]any{"id": "foreign"}}})
	if w.Code < 400 {
		t.Fatal("forged receipt accepted")
	}
	foreign := *caller
	foreign.Team = "other"
	w, _ = callNativeOperation(h, &foreign, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 404 {
		t.Fatalf("foreign: %d %s", w.Code, w.Body.String())
	}
}
func TestNativeOperationCreationUsesVerifiedSourceScopeAcrossLeaderChanges(t *testing.T) {
	h, _ := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	args := map[string]any{"action": "create", "leader": "lead-cr", "title": "Own task", "_nativeSource": map[string]any{"eventId": "$source", "roomId": "!source:test", "userId": "@human:test"}}
	w, _ := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// Admission must still find this source's durable fence without a Leader.
	delete(args, "leader")
	w, result := callNativeOperation(h, caller, map[string]any{"mode": "admit", "arguments": args})
	if w.Code != 200 || result["mode"] != "verify" {
		t.Fatalf("source admission: %d %s", w.Code, w.Body.String())
	}
}

func TestNativeOperationConcurrentStartsGrantExecutionOnce(t *testing.T) {
	h, _, caller := nativeOperationFixture(t)
	input := map[string]any{"mode": "begin", "arguments": map[string]any{"action": "plan", "taskId": "main", "nodes": planRequest().Nodes}}
	results := make(chan string, 2)
	for range 2 {
		go func() {
			_, body := callNativeOperation(h, caller, input)
			mode, _ := body["mode"].(string)
			results <- mode
		}()
	}
	executed := 0
	for range 2 {
		if <-results == "execute" {
			executed++
		}
	}
	if executed != 1 {
		t.Fatalf("execution leases: %d", executed)
	}
}
func TestNativeOperationGenerationChangeDoesNotReuseNativeBeginPaths(t *testing.T) {
	h, store, caller := nativeOperationFixture(t)
	args := map[string]any{"action": "plan", "taskId": "main", "nodes": planRequest().Nodes}
	_, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	actual := callPlan(h, caller, planRequest())
	var receipt map[string]any
	_ = json.Unmarshal(actual.Body.Bytes(), &receipt)
	w, _ := callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": args, "operationId": first["operationId"], "result": receipt})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	key := "teams/alpha/shared/tasks/main/meta.json"
	raw, _ := store.GetObject(t.Context(), key)
	var task map[string]any
	_ = json.Unmarshal(raw, &task)
	task["execution"] = map[string]any{"generation": 9007199254740993}
	putTask(store, key, task)
	_, next := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if next["mode"] != "execute" {
		t.Fatal("old-generation receipt reused", next)
	}
}

func nativeOperationFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory, *authpkg.CallerIdentity) {
	t.Helper()
	h, store, caller, _ := taskPlanFixture(t)
	worker, err := h.coordination.worker(t.Context(), caller.Username)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = (&service.WorkerMatrixStore{Storage: store}).Start(t.Context(), worker.Spec.EffectiveWorkerName(worker.Name), service.WorkerStart{InstanceID: "native-instance", ProcessID: "canonical-process", Runtime: "openclaw"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return h, store, caller
}
func TestNativeOperationRejectsReplacedProcessBeforeRecordingIntent(t *testing.T) {
	h, _, caller := nativeOperationFixture(t)
	input := map[string]any{"mode": "begin", "arguments": map[string]any{"action": "plan", "taskId": "main"}}
	body, _ := json.Marshal(input)
	request := httptest.NewRequest("POST", "/api/v1/tasks/native-operations", strings.NewReader(string(body)))
	request.Header.Set("X-AgentTeams-Process-ID", "old-process")
	response := httptest.NewRecorder()
	h.NativeTaskOperation(response, withCaller(request, caller))
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	_, accepted := callNativeOperation(h, caller, input)
	if accepted["mode"] != "execute" {
		t.Fatal("replaced process poisoned current ledger", accepted)
	}
}
