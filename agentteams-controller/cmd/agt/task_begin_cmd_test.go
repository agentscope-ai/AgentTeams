package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTaskBeginPreparesAcceptedInputInOneAgentCall(t *testing.T) {
	requests := 0
	data := []byte("accepted input\n")
	digest := sha256.Sum256(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/api/v1/tasks/t1/begin":
			if r.Method != http.MethodPost || r.Header.Get("X-AgentTeams-Process-ID") != "process-1" {
				t.Errorf("missing native begin identity: %s %v", r.Method, r.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "task-v1", "task": map[string]any{"id": "t1", "team": "alpha", "status": "in_progress", "spec": "Read the accepted input."}, "generation": 1, "inputs": []map[string]any{{"taskId": "upstream", "readOnly": true, "files": []map[string]any{{"artifactId": "a1", "name": "data.csv", "version": "file-v1", "size": len(data), "sha256": hex.EncodeToString(digest[:])}}}}})
		case "/api/v1/tasks/upstream/files":
			if r.URL.Query().Get("artifactId") != "a1" || r.URL.Query().Get("version") != "file-v1" {
				t.Errorf("input is not version-bound: %s", r.URL)
			}
			w.Header().Set("ETag", `"file-v1"`)
			w.Header().Set("X-Content-SHA256", hex.EncodeToString(digest[:]))
			w.Header().Set("Content-Disposition", `attachment; filename="data.csv"`)
			_, _ = w.Write(data)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test-token")
	t.Setenv("AGENTTEAMS_PROCESS_ID", "process-1")
	shared := filepath.Join(t.TempDir(), "shared")
	t.Setenv("TEAMHARNESS_SHARED_DIR", shared)
	cmd := newRootCommand("agt")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "begin", "t1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		TaskDirectory   string `json:"taskDirectory"`
		OutputDirectory string `json:"outputDirectory"`
		Inputs          []struct {
			Files []struct {
				LocalPath string `json:"localPath"`
			} `json:"files"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	realShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || response.TaskDirectory != filepath.Join(realShared, "tasks", "t1") || response.OutputDirectory != filepath.Join(response.TaskDirectory, "workspace") || len(response.Inputs) != 1 || len(response.Inputs[0].Files) != 1 {
		t.Fatalf("incomplete prepared context: requests=%d %s", requests, output.String())
	}
	local := response.Inputs[0].Files[0].LocalPath
	got, err := os.ReadFile(local)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("input=%q %v", got, err)
	}
	if relative, err := filepath.Rel(response.TaskDirectory, local); err != nil || filepath.IsAbs(relative) || relative == ".." {
		t.Fatalf("input outside task: %s", local)
	}
	if info, err := os.Stat(local); err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("input is writable: %v %v", info, err)
	}
}
