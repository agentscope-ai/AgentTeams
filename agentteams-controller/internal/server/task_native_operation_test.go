package server

import (
	"encoding/json"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"net/http"
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
func TestNativePlanSelfAssignmentIsCorrectableWithoutOpeningLedger(t *testing.T) {
	for _, assignee := range []string{"lead-cr", "@lead:test", "user:@lead:test"} {
		t.Run(assignee, func(t *testing.T) {
			h, store, caller := nativeOperationFixture(t)
			input := planRequest()
			input.Nodes[0].AssignedTo = assignee
			args := map[string]any{"action": "plan", "taskId": "main", "nodes": input.Nodes}
			w, rejected := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
			if w.Code != 403 || rejected["effect"] != "none" || rejected["error"] != "invalid_task_plan_assignee" {
				t.Fatalf("self-assigned plan: %d %s", w.Code, w.Body.String())
			}
			keys, err := store.ListObjects(t.Context(), "control/task-native/")
			if err != nil || len(keys) != 0 {
				t.Fatalf("rejected plan left a canonical fence: %v %v", keys, err)
			}
			args["nodes"] = planRequest().Nodes
			w, corrected := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
			if w.Code != 200 || corrected["mode"] != "execute" {
				t.Fatalf("corrected plan: %d %s", w.Code, w.Body.String())
			}
		})
	}
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

func TestNativeBeginReceiptCanBeVerifiedAfterGenerationChange(t *testing.T) {
	h, store, caller := nativeOperationFixture(t)
	args := map[string]any{"action": "begin", "taskId": "main"}
	_, first := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	key := "teams/alpha/shared/tasks/main/meta.json"
	raw, _ := store.GetObject(t.Context(), key)
	var task map[string]any
	_ = json.Unmarshal(raw, &task)
	task["status"], task["notifyTxn"] = "in_progress", "same-assignment"
	task["execution"] = map[string]any{"generation": 2}
	putTask(store, key, task)
	receipt := map[string]any{"task": map[string]any{"id": "main", "status": "in_progress", "assignmentId": "same-assignment"}}
	forged := map[string]any{"action": "begin", "taskId": "main", "team": "alpha"}
	w, _ := callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": forged, "operationId": first["operationId"], "result": receipt})
	if w.Code != 409 {
		t.Fatalf("changed original arguments verified: %d %s", w.Code, w.Body.String())
	}
	w, verified := callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": args, "operationId": first["operationId"], "result": receipt})
	if w.Code != 200 || verified["mode"] != "verified" {
		t.Fatalf("verified acknowledgement stuck after restart: %d %s", w.Code, w.Body.String())
	}
	_, next := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if next["mode"] != "execute" {
		t.Fatal("new generation reused old preparation paths", next)
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

func TestNativeOperationInputRefreshPreservesTheSuccessfulNoTaskControlResult(t *testing.T) {
	h, _ := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	event := func(id, body string) map[string]any {
		return map[string]any{"event_id": id, "room_id": "!source:test", "sender": "@human:test", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": body}}
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_ = json.NewEncoder(w).Encode(map[string]any{"chunk": []any{event("$new", "Cancel pending request"), event("$original", "Build a report")}})
		} else {
			_ = json.NewEncoder(w).Encode(event("$original", "Build a report"))
		}
	}))
	defer source.Close()
	h.coordination.deps.MatrixConfig.ServerURL = source.URL
	args := map[string]any{"action": "create", "leader": "lead-cr", "title": "Report", "_nativeSource": map[string]any{"eventId": "$original", "roomId": "!source:test", "userId": "@human:test"}}
	w, started := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	response := callTaskCreate(h, caller, "$original", `{"title":"Report","leader":"lead-cr"}`)
	var control map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &control)
	if control["outcome"] != "input_changed" || control["noTaskRegistered"] != true {
		t.Fatal(response.Body.String())
	}
	w, _ = callNativeOperation(h, caller, map[string]any{"mode": "finish", "arguments": args, "operationId": started["operationId"], "result": control})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_, cached := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	result, _ := cached["result"].(map[string]any)
	if cached["mode"] != "recovered" || result["outcome"] != "input_changed" {
		t.Fatal("normal refresh became unknown", cached)
	}
	// Once the framework checkpoints the newer input, its frontier is a new
	// intent and does not keep returning the stale cached refresh forever.
	args["_nativeSource"].(map[string]any)["frontierEventId"] = "$new"
	_, next := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if next["mode"] != "execute" {
		t.Fatal(next)
	}
}

func TestNativeCreateRejectsRepeatedRequestAsAmendmentBeforeFencing(t *testing.T) {
	h, store := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	source := map[string]any{"eventId": "$first", "roomId": "!source:test", "userId": "@human:test", "relatedSources": []string{"$second"}}
	args := map[string]any{"action": "create", "leader": "lead-cr", "title": "Report", "_nativeSource": source}
	w, response := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
	if w.Code != http.StatusConflict || response["error"] != "repeated_task_request" || response["effect"] != "none" {
		t.Fatalf("expected correctable relation rejection: %d %s", w.Code, w.Body.String())
	}
	keys, err := store.ListObjects(t.Context(), "control/task-native/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("rejected relation fenced a source: %v %v", keys, err)
	}
	delete(source, "relatedSources")
	for _, event := range []string{"$first", "$second"} {
		source["eventId"] = event
		w, response := callNativeOperation(h, caller, map[string]any{"mode": "begin", "arguments": args})
		if w.Code != http.StatusOK || response["mode"] != "execute" {
			t.Fatalf("independent source %s was not admitted: %d %s", event, w.Code, w.Body.String())
		}
	}
}

func TestNativeCreateRouteRejectsUnknownLeaderWithoutFencingCorrectedHandoff(t *testing.T) {
	h, store := taskCreateFixture(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	deps := h.coordination.deps
	deps.AuthMw = authpkg.NewMiddleware(&roleAuthenticator{identity: caller}, authpkg.NewCREnricher(deps.Client, "default"), authpkg.NewAuthorizer(), deps.Client, "default")
	srv := NewHTTPServer(":0", deps)
	t.Cleanup(srv.coordinationCancel)
	post := func(leader string) (*httptest.ResponseRecorder, map[string]any) {
		data, _ := json.Marshal(map[string]any{"mode": "begin", "arguments": map[string]any{"action": "create", "leader": leader, "title": "Own request", "_nativeSource": map[string]any{"eventId": "$source", "roomId": "!source:test", "userId": "@human:test"}}})
		r := httptest.NewRequest("POST", "/api/v1/tasks/native-operations", strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer own-manager-token")
		w := httptest.NewRecorder()
		srv.Mux.ServeHTTP(w, r)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w, body
	}
	w, rejected := post("not-a-registered-leader")
	if w.Code != 403 || rejected["effect"] != "none" || rejected["error"] != "invalid_task_handoff" {
		t.Fatalf("pre-ledger rejection: %d %s", w.Code, w.Body.String())
	}
	keys, err := store.ListObjects(t.Context(), "control/task-native/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("rejected proposal left a canonical fence: %v %v", keys, err)
	}
	w, corrected := post("lead-cr")
	if w.Code != 200 || corrected["mode"] != "execute" {
		t.Fatalf("corrected registered Leader: %d %s", w.Code, w.Body.String())
	}
	resolved, status, message := h.resolveTaskHandoff(t.Context(), caller, "lead-cr", "$source", "!source:test", "@human:test", "")
	if status != 0 || resolved.RoomID != "!manager-leader:test" || coordinationString(resolved.SourceRoute, "target_session") != "!source:test" || resolved.Recipient != "@lead:test" {
		t.Fatalf("handoff and Human source room must stay distinct: %+v %d %s", resolved, status, message)
	}
}
