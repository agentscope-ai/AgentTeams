package server

import (
	"encoding/json"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestTaskProtocolIsResolvedFromManagedPlanNotCallerClaim(t *testing.T) {
	h, store, caller, _ := taskPlanFixture(t)
	w := callPlan(h, caller, planRequest())
	if w.Code != 200 {
		t.Fatalf("plan: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Assignments []taskPlanAssignment `json:"assignments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Assignments) != 2 {
		t.Fatalf("expected two ready assignments, got %d", len(result.Assignments))
	}
	// Every assignment uses the same common protocol regardless of member runtime.
	for _, assignment := range result.Assignments {
		data, err := store.GetObject(t.Context(), coordinationPending+assignment.EventID+".json")
		var delivery coordinationDelivery
		if err != nil || json.Unmarshal(data, &delivery) != nil || delivery.Event.Protocol != "task/v1" {
			t.Fatalf("assignment protocol missing: %s %v", data, err)
		}
	}
	coord, _ := coordinationFixture(t)
	e, err := coord.resolve(t.Context(), coordinationEvent{Protocol: "task/v1", Kind: "handoff", Recipient: "@lead:test", OriginEventID: "event", SourceRoute: map[string]any{"target_session": "!source:test", "target_user": "@human:test"}}, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	if err != nil || e.Protocol != "" {
		t.Fatalf("caller forged common protocol: %+v %v", e, err)
	}
}

func TestTaskProtocolNotificationsHaveNoLegacyOperationInstructions(t *testing.T) {
	for _, kind := range []string{"handoff", "task_assigned", "task_submitted", "main_task_ready"} {
		payload := coordinationPayload(coordinationEvent{Protocol: "task/v1", Kind: kind, TaskID: "exec", MainTaskID: "main", Tasks: []mainTaskRequest{{TaskID: "main", Spec: "Original"}}})
		data, _ := json.Marshal(payload)
		for _, old := range []string{"start_task", "ack_task", "check_task", "taskflow", "complete_task"} {
			if strings.Contains(string(data), old) {
				t.Fatalf("%s retains %s: %s", kind, old, data)
			}
		}
	}
}
