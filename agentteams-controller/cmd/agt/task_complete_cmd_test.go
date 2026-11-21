package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskCompleteUsesTaskIdentityAndPreservesAutomaticReplyReceipt(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/main/complete" || json.NewDecoder(r.Body).Decode(&input) != nil || input["report"] != "Verified report" || len(input) != 1 {
			t.Errorf("invalid completion request: %s %s %+v", r.Method, r.URL, input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"id":"main","status":"completed","completionId":"c1"},"notification":{"status":"queued","eventId":"e1","recipient":"manager"},"nextAction":"Task system returns the final report to the original conversation; do not send again."}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "complete", "main", "--report", "Verified report"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Notification struct {
			Status string `json:"status"`
		} `json:"notification"`
		NextAction string `json:"nextAction"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Notification.Status != "queued" || result.NextAction == "" || calls != 1 {
		t.Fatalf("reply receipt lost or extra call: %d %s", calls, output.String())
	}
}

func TestTaskCompleteSelectsFinalOutputsByOpaqueArtifactID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Report    string   `json:"report"`
			Artifacts []string `json:"artifacts"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Artifacts) != 2 || input.Artifacts[0] != "artifact-one" || input.Artifacts[1] != "artifact-two" {
			t.Errorf("missing final selection: %+v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"id":"main","status":"completed","completionId":"c1"},"notification":{"status":"queued"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "complete", "main", "--report", "Verified report", "--artifact", "artifact-one", "--artifact", "artifact-two"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}
