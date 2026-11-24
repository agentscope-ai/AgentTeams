package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAssistantSkipSubmitsOutcomeWithoutReminderText(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]any
		if r.Method != "POST" || r.URL.Path != "/api/v1/assistant/messages" || r.URL.Query().Get("manager") != "default" || r.URL.Query().Get("user") != "alice" || json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Errorf("unexpected skip request: %s %s", r.Method, r.URL)
		}
		if input["skip"] != true || input["text"] != nil || input["reason"] != "no verified updates" || input["requestId"] != "cycle" {
			t.Errorf("invalid outcome: %+v", input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"skipped","requestId":"cycle","skippedAt":"2026-11-23T15:00:00Z"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	file := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(file, []byte(`{"owner":"current","requestId":"cycle","reason":"no verified updates","summary":"interests retained"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand("agt")
	cmd.SetArgs([]string{"assistant", "skip", "--manager", "default", "--user", "alice", "--file", file})
	if err := cmd.Execute(); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
