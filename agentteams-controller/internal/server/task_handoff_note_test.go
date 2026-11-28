package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestTaskHandoffNoteIsOptionalAndUsesTheCanonicalRecipient(t *testing.T) {
	for _, kind := range []string{"task_assigned", "main_task_ready"} {
		e := coordinationEvent{Protocol: "task/v1", Kind: kind, TaskID: "test", Recipient: "@tester:local", Handoff: "请独立验证 <button> 交互。"}
		content := coordinationMatrixNotification(e, nil)
		if content["body"] != "@tester:local 请独立验证 <button> 交互。" || !strings.Contains(content["formatted_body"].(string), "&lt;button&gt;") {
			t.Fatalf("model handoff was lost or not escaped: %v", content)
		}
		if ids := content["m.mentions"].(map[string]any)["user_ids"].([]string); len(ids) != 1 || ids[0] != e.Recipient {
			t.Fatalf("recipient changed: %v", ids)
		}
		e.Handoff = ""
		if body := coordinationMatrixNotification(e, nil)["body"].(string); body == "@tester:local " || strings.Contains(body, "请继续开始") {
			t.Fatalf("missing natural default notification: %s", body)
		}
	}
}

func TestOmittedHandoffPreservesLegacySubmissionDigestInput(t *testing.T) {
	input := taskSubmitRequest{Summary: "Done", Artifacts: []string{}, Report: "Evidence", Status: "SUCCESS"}
	body, _ := json.Marshal(input)
	if string(body) != `{"summary":"Done","artifacts":[],"report":"Evidence","status":"SUCCESS"}` {
		t.Fatalf("old submission bytes changed: %s", body)
	}
}

func TestSubmittedHandoffFlowsToDependenciesAndLeader(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	if w := callPlan(h, leader, planRequest()); w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	submit := func(node, worker, note string) {
		t.Helper()
		id := taskPlanExecutionID("alpha", "main", node)
		key := "teams/alpha/shared/tasks/" + id + "/meta.json"
		child, err := h.coordination.read(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		child["execution"] = map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}
		putTask(store, key, child)
		caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: worker, Team: "alpha"}
		call := func(action, body string) {
			r := httptest.NewRequest("POST", "/"+action, strings.NewReader(body))
			r.SetPathValue("id", id)
			r.Header.Set("X-AgentTeams-Process-ID", "process-1")
			w := httptest.NewRecorder()
			if action == "begin" {
				h.BeginTask(w, withCaller(r, caller))
			} else {
				h.SubmitTask(w, withCaller(r, caller))
			}
			if w.Code != 200 {
				t.Fatalf("%s %s: %d %s", action, node, w.Code, w.Body.String())
			}
		}
		call("begin", "")
		body, _ := json.Marshal(taskSubmitRequest{Summary: "Actual result " + node, Handoff: note, Status: "SUCCESS"})
		call("submit", string(body))
		call("submit", string(body)) // Retries reuse the same frozen note and notification.
		child, _ = h.coordination.read(t.Context(), key)
		if taskHandoffOf(child).Note != note {
			t.Fatalf("note not frozen with submission: %v", child["task_handoff"])
		}
	}
	submit("A", "worker-cr", "A inputs are ready.")
	submit("B", "second-cr", "Both inputs are ready; integrate and verify them.")
	c := taskPlanExecutionID("alpha", "main", "C")
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+c+"/meta.json")
	var notices map[string]taskNotificationIntent
	body, _ := json.Marshal(child["task_notifications"])
	_ = json.Unmarshal(body, &notices)
	e := notices["assigned"].Event
	e.Handoff = "forged transport text"
	resolved, err := h.coordination.resolve(t.Context(), e, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "second-cr", Team: "alpha"})
	if err != nil || resolved.Handoff != "Both inputs are ready; integrate and verify them." {
		t.Fatalf("downstream note not resolved from saved submission: %v %v", resolved, err)
	}
	submit("C", "worker-cr", "Integration passed; please review the final delivery.")
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	body, _ = json.Marshal(main["task_notifications"])
	_ = json.Unmarshal(body, &notices)
	resolved, err = h.coordination.resolve(t.Context(), notices["ready"].Event, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker-cr", Team: "alpha"})
	if err != nil || resolved.Handoff != "Integration passed; please review the final delivery." || resolved.Text == resolved.Handoff {
		t.Fatalf("Leader note lost or mixed with internal context: %v %v", resolved, err)
	}
}
