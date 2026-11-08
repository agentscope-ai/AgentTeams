package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectCancelCommandForwardsSubmissionID(t *testing.T) {
	var requestBody map[string]any
	var requestMethod string
	var requestPath string
	var requestTeam string
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethod = r.Method
		requestPath = r.URL.Path
		requestTeam = r.URL.Query().Get("team")
		decodeErr = json.NewDecoder(r.Body).Decode(&requestBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")

	cmd := projectCancelCmd()
	cmd.SetArgs([]string{
		"p1", "t1",
		"--reason", "superseded",
		"--submission-id", "submission-1",
		"--team", "alpha-team",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if decodeErr != nil {
		t.Fatalf("decode request: %v", decodeErr)
	}
	if requestMethod != http.MethodPost || requestPath != "/api/v1/projects/p1/tasks/t1/cancel" {
		t.Fatalf("request=%s %s", requestMethod, requestPath)
	}
	if requestTeam != "alpha-team" {
		t.Fatalf("team query=%q, want alpha-team", requestTeam)
	}
	if requestBody["submissionId"] != "submission-1" {
		t.Fatalf("request body=%v, want submissionId", requestBody)
	}
}

func TestGetProjectsCommandForwardsIncludeTasks(t *testing.T) {
	requested := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_id":"p1","nodes":[],"next":[],"interrupts":[]}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")

	cmd := getProjectsCmd()
	cmd.SetArgs([]string{"p1", "--include-tasks", "-o", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	req := <-requested
	if req.URL.Path != "/api/v1/projects/p1/workflow" || req.URL.Query().Get("includeTasks") != "true" {
		t.Fatalf("request URL=%s, want workflow?includeTasks=true", req.URL.String())
	}
}

func TestGetProjectsCommandIncludeTasksRequiresJSONOutput(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := getProjectsCmd()
	cmd.SetArgs([]string{"p1", "--include-tasks"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--include-tasks requires -o json") {
		t.Fatalf("error=%v, want JSON output requirement", err)
	}
	if requests != 0 {
		t.Fatalf("requests=%d, want no request for invalid flag combination", requests)
	}
}
