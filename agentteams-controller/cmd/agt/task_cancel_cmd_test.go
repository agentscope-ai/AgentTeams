package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskCancelUsesPublicMainTaskEndpointAndPreservesReceipt(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/main/cancel" || r.URL.Query().Get("team") != "alpha" || json.NewDecoder(r.Body).Decode(&input) != nil || input["reason"] != "Stop request" || len(input) != 1 {
			t.Errorf("unexpected cancellation request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"id":"main","status":"cancelled"},"cancellation":{"id":"cancel-1"},"projection":"pending"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "fixture-token")
	var output bytes.Buffer
	cmd := newRootCommand("agt")
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"task", "cancel", "main", "--reason", "Stop request", "--team", "alpha"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(output.String(), `"projection":"pending"`) || !strings.Contains(output.String(), `"id":"cancel-1"`) {
		t.Fatal("cancellation receipt lost")
	}
}

func TestTaskCancelRejectsMissingReasonAndUnsafeIdentity(t *testing.T) {
	for _, args := range [][]string{{"task", "cancel", "main"}, {"task", "cancel", "../main", "--reason", "Stop"}, {"task", "cancel", "main", "--reason", " "}} {
		cmd := newRootCommand("agt")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("invalid cancellation accepted")
		}
	}
}
