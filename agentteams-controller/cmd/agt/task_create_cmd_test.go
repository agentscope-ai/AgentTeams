package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskCreateGetsOriginalRouteFromNativeContext(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks" || json.NewDecoder(r.Body).Decode(&input) != nil || input["leader"] != "lead" || input["title"] != "Request" || input["spec"] != "Original criteria" || len(input) != 3 {
			t.Errorf("invalid create request: %s %s %+v", r.Method, r.URL, input)
		}
		if r.Header.Get("X-AgentTeams-Origin-Event-ID") != "$request" || r.Header.Get("X-AgentTeams-Requester-User-ID") != "@human:test" || r.Header.Get("X-AgentTeams-Requester-Room-ID") != "!source:test" {
			t.Error("missing native source identity")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"sourceEventId":"$request","task":{"id":"main","kind":"main","status":"planned"},"notification":{"status":"queued","eventId":"handoff","recipient":"lead"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_ORIGIN_EVENT_ID", "$request")
	t.Setenv("AGENTTEAMS_REQUESTER_USER_ID", "@human:test")
	t.Setenv("AGENTTEAMS_REQUESTER_ROOM_ID", "!source:test")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "create", "--leader", "lead", "--title", "Request", "--spec", "Original criteria"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !json.Valid(output.Bytes()) {
		t.Fatalf("invalid creation receipt: %d %s", calls, output.String())
	}
}
