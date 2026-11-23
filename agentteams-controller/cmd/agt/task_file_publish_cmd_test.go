package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTaskFilePutUsesExecutionIdentityWithoutAgentSuppliedPaths(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	data := []byte("id,value\na,1\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if raw, err := hex.DecodeString(key); err != nil || len(raw) != 16 {
			t.Errorf("invalid internal publication key: %q", key)
		}
		keys = append(keys, key)
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
		if len(keys) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
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
	for i := 0; i < 2; i++ {
		cmd := newRootCommand("agt")
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs([]string{"task", "files", "put", "t1", "--source", file, "--name", "analysis.csv"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(output.Bytes()) {
			t.Fatalf("invalid artifact output %q", output.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] == keys[2] {
		t.Fatalf("expected the retry to reuse the key and independent put to use a new key, got %v", keys)
	}
}
