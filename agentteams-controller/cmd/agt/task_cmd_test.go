package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskGetUsesPublicResourceAndSingleRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/tasks/t1" || r.URL.Query().Get("team") != "alpha" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing configured identity")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1","task":{"id":"t1","status":"submitted","submissionId":"s1"}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "get", "t1", "--team", "alpha"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !json.Valid(output.Bytes()) {
		t.Fatalf("requests=%d, output=%q", requests, output.String())
	}
}

func TestTaskGetRejectsNonTaskIdentifiersWithoutRequest(t *testing.T) {
	for _, id := range []string{"../t1", "a/b", "t1?team=other", "t1#fragment"} {
		t.Run(id, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			cmd := newRootCommand("agt")
			cmd.SetArgs([]string{"task", "get", id})
			if err := cmd.Execute(); err == nil || requests != 0 {
				t.Fatalf("must reject identifier locally; err=%v requests=%d", err, requests)
			}
		})
	}
}
