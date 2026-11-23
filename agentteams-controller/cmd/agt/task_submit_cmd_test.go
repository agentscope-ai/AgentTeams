package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTaskSubmitPublishesFromTaskDirectoryAndReturnsNotificationToTool(t *testing.T) {
	shared := t.TempDir()
	outputs := filepath.Join(shared, "tasks", "t1", "workspace")
	if err := os.MkdirAll(outputs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "report.md"), []byte("Verified output"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.URL.Path)
		if r.Header.Get("X-AgentTeams-Process-ID") != "process-one" {
			t.Error("missing native execution identity")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/tasks/t1/files":
			data, _ := io.ReadAll(r.Body)
			if string(data) != "Verified output" || r.URL.Query().Get("name") != "report.md" || r.URL.Query().Get("kind") != "output" {
				t.Errorf("invalid publication: %s %s", r.URL, data)
			}
			_, _ = w.Write([]byte(`{"artifactId":"artifact-one","name":"report.md"}`))
		case "/api/v1/tasks/t1/submit":
			var body struct {
				Summary   string   `json:"summary"`
				Artifacts []string `json:"artifacts"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Summary != "Report done" || len(body.Artifacts) != 1 || body.Artifacts[0] != "artifact-one" {
				t.Errorf("invalid candidate: %+v", body)
			}
			_, _ = w.Write([]byte(`{"task":{"id":"t1","status":"submitted"},"notification":{"status":"queued","eventId":"event-one"},"nextAction":"Task system has queued an automatic notification to lead-one; do not resend"}`))
		default:
			t.Errorf("unexpected resource: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_PROCESS_ID", "process-one")
	t.Setenv("TEAMHARNESS_SHARED_DIR", shared)
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "submit", "t1", "--summary", "Report done", "--file", "report.md"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Notification struct {
			Status  string `json:"status"`
			EventID string `json:"eventId"`
		} `json:"notification"`
		NextAction string `json:"nextAction"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Notification.Status != "queued" || result.Notification.EventID != "event-one" || result.NextAction == "" {
		t.Fatalf("tool output lost notification receipt: %s", output.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("unexpected requests: %v", calls)
	}
}

func TestTaskSubmitPreflightsAllOutputsBeforePublishing(t *testing.T) {
	for _, scenario := range []string{"missing", "escape", "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			shared := t.TempDir()
			outputs := filepath.Join(shared, "tasks", "t1", "workspace")
			if err := os.MkdirAll(outputs, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outputs, "valid.txt"), []byte("valid"), 0600); err != nil {
				t.Fatal(err)
			}
			second := "missing.txt"
			if scenario == "escape" {
				second = "link.txt"
				outside := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(outputs, second)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "duplicate" {
				second = "valid.txt"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("network publication occurred before preflight completed")
				w.WriteHeader(500)
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
			t.Setenv("AGENTTEAMS_PROCESS_ID", "process-one")
			t.Setenv("TEAMHARNESS_SHARED_DIR", shared)
			cmd := newRootCommand("agt")
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"task", "submit", "t1", "--summary", "Done", "--file", "valid.txt", "--file", second})
			if err := cmd.Execute(); err == nil {
				t.Fatal("invalid outputs accepted")
			}
		})
	}
}
