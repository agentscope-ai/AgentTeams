package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskPlanFileAndInlineUseOnePublicRequest(t *testing.T) {
	const input = `{"nodes":[{"id":"A","assignedTo":"worker","spec":"Original assignment","dependsOn":[]}]}`
	for _, source := range []string{"file", "json"} {
		t.Run(source, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]json.RawMessage
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/main/plan" || r.URL.Query().Get("team") != "alpha" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["nodes"] == nil {
					t.Errorf("unexpected plan request %s %s %+v", r.Method, r.URL, body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task":{"id":"main","planId":"plan","nodes":[{"id":"A","taskId":"execution-A"}]},"progress":"pending","assignments":[]}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "fixture")
			value := input
			if source == "file" {
				value = filepath.Join(t.TempDir(), "plan.json")
				if err := os.WriteFile(value, []byte(input), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := newRootCommand("agt")
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"task", "plan", "main", "--" + source, value, "--team", "alpha"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !bytes.Contains(output.Bytes(), []byte(`"progress":"pending"`)) || !bytes.Contains(output.Bytes(), []byte("execution-A")) {
				t.Fatalf("lost plan receipt: %d %s", calls, output.String())
			}
		})
	}
}

func TestTaskPlanRejectsAmbiguousOrMalformedLocalInput(t *testing.T) {
	for _, args := range [][]string{
		{"task", "plan", "main"},
		{"task", "plan", "main", "--json", "{}", "--file", "plan.json"},
		{"task", "plan", "main", "--json", "not json"},
		{"task", "plan", "../main", "--json", "{}"},
	} {
		cmd := newRootCommand("agt")
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid plan accepted: %v", args)
		}
	}
}

func TestTaskPlanOverviewUsesTheFrameworkRunIdentity(t *testing.T) {
	const overview = "目标与要求；验收标准；开发、审查、测试分工。"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["overview"] != overview || r.Header.Get("X-AgentTeams-Run-ID") != "native-run" {
			t.Errorf("lost overview or native run: %v", body)
		}
		_, _ = w.Write([]byte(`{"progress":"ready"}`))
	}))
	defer srv.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "fixture")
	t.Setenv("AGENTTEAMS_RUN_ID", "native-run")
	cmd := newRootCommand("agt")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"task", "plan", "main", "--json", `{"nodes":[]}`, "--overview", overview})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}
