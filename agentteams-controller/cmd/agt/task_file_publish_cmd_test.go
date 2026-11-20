package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskFilePutUsesExecutionIdentityWithoutAgentSuppliedPaths(t *testing.T) {
	requests := 0
	data := []byte("id,value\na,1\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, data) {
			t.Errorf("wrong uploaded bytes: %q %v", body, err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/t1/files" || r.URL.Query().Get("name") != "analysis.csv" || r.URL.Query().Get("kind") != "intermediate" {
			t.Errorf("wrong resource: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-AgentTeams-Process-ID") != "process-one" || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing native execution identity or content type")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifactId":"artifact-one","name":"analysis.csv","kind":"intermediate","version":"v1"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_PROCESS_ID", "process-one")
	file := filepath.Join(t.TempDir(), "local.csv")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "files", "put", "t1", "--source", file, "--name", "analysis.csv"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !json.Valid(output.Bytes()) {
		t.Fatalf("requests=%d output=%q", requests, output.String())
	}
}
