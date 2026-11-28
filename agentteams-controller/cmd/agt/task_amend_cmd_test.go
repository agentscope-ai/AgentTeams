package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskAmendPassesReplacementPlanToAuthorizedAPI(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]any
		if json.NewDecoder(r.Body).Decode(&input) != nil || r.URL.Path != "/api/v1/tasks/child/amend" || input["spec"] != "updated" || len(input["replacementNodes"].([]any)) != 1 {
			t.Errorf("invalid amendment: %+v", input)
		}
		_, _ = w.Write([]byte(`{"effect":"committed","task":{"id":"child","spec":"updated","status":"revision"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "fixture")
	cmd := newRootCommand("agt")
	cmd.SetArgs([]string{"task", "amend", "child", "--spec", "updated", "--note", "reassign", "--replacement-nodes", `[{"id":"new","assignedTo":"dev","spec":"new","dependsOn":[]}]`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("requests: %d", calls)
	}
}
