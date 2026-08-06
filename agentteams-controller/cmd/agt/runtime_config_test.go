package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

func TestCreateWorkerRuntimeConfigFileSendsTypedYAMLConfig(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "runtime-config.yaml")
	if err := os.WriteFile(configFile, []byte(`deepagents:
  approvals:
    fileWrites: required
    mcpDefault: required
    coordinators:
      - "@human:example.org"
  execution:
    mode: sandbox
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var received struct {
		Runtime       string                       `json:"runtime"`
		RuntimeConfig *v1beta1.WorkerRuntimeConfig `json:"runtimeConfig"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s, want POST /api/v1/workers", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"deep-worker"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := createWorkerCmd()
	cmd.SetArgs([]string{"--name", "deep-worker", "--runtime", "deepagents", "--runtime-config-file", configFile, "--no-wait"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	if received.Runtime != "deepagents" {
		t.Fatalf("runtime = %q, want deepagents", received.Runtime)
	}
	if got := received.RuntimeConfig.DeepAgents; got == nil || got.Execution.Mode != "sandbox" || got.Approvals.MCPDefault != "required" {
		t.Fatalf("runtimeConfig = %#v, want parsed DeepAgents sandbox config", received.RuntimeConfig)
	}
}

func TestUpdateWorkerRuntimeConfigFileDoesNotRequireRuntimeFlag(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "runtime-config.json")
	if err := os.WriteFile(configFile, []byte(`{"deepagents":{"approvals":{"coordinators":["@human:example.org"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers/deep-worker" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s, want PUT /api/v1/workers/deep-worker", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"deep-worker"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	cmd := updateWorkerCmd()
	cmd.SetArgs([]string{"--name", "deep-worker", "--runtime-config-file", configFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("update worker: %v", err)
	}
	if _, ok := received["runtime"]; ok {
		t.Fatalf("advanced runtime config update unexpectedly sent runtime: %s", received["runtime"])
	}
	if _, ok := received["runtimeConfig"]; !ok {
		t.Fatal("advanced runtime config update omitted runtimeConfig")
	}
}

func TestCreateWorkerDeepAgentsSandboxRequiresHumanCoordinator(t *testing.T) {
	var received struct {
		RuntimeConfig *v1beta1.WorkerRuntimeConfig `json:"runtimeConfig"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"deep-worker"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)

	t.Run("explicit coordinators set the safe defaults", func(t *testing.T) {
		cmd := createWorkerCmd()
		cmd.SetArgs([]string{"--name", "deep-worker", "--runtime", "deepagents", "--deepagents-sandbox", "--deepagents-coordinators", "@first:example.org, @second:example.org", "--no-wait"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("create worker: %v", err)
		}
		got := received.RuntimeConfig.DeepAgents
		if got.Execution.Mode != "sandbox" || got.Approvals.FileWrites != "required" || got.Approvals.MCPDefault != "required" {
			t.Fatalf("sandbox defaults = %#v, want sandbox with required approvals", got)
		}
		if want := []string{"@first:example.org", "@second:example.org"}; !reflect.DeepEqual(got.Approvals.Coordinators, want) {
			t.Fatalf("coordinators = %#v, want %#v", got.Approvals.Coordinators, want)
		}
	})

	t.Run("derives coordinator only from complete admin environment", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_ADMIN_USER", "admin-user")
		t.Setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.example.org")
		cmd := createWorkerCmd()
		cmd.SetArgs([]string{"--name", "deep-worker", "--runtime", "deepagents", "--deepagents-sandbox", "--no-wait"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("create worker: %v", err)
		}
		if got := received.RuntimeConfig.DeepAgents.Approvals.Coordinators; !reflect.DeepEqual(got, []string{"@admin-user:matrix.example.org"}) {
			t.Fatalf("derived coordinators = %#v, want @admin-user:matrix.example.org", got)
		}
	})

	t.Run("fails instead of inventing a coordinator", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_ADMIN_USER", "")
		t.Setenv("AGENTTEAMS_MATRIX_DOMAIN", "")
		cmd := createWorkerCmd()
		cmd.SetArgs([]string{"--name", "deep-worker", "--runtime", "deepagents", "--deepagents-sandbox", "--no-wait"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--deepagents-coordinators") {
			t.Fatalf("error = %v, want actionable coordinator error", err)
		}
	})
}

func TestWorkerRuntimeConfigFileRejectsInvalidDocumentsAndConflictingFlags(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		contents string
		args     []string
		wantErr  string
	}{
		{name: "empty", contents: "", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "empty"},
		{name: "malformed yaml", contents: "deepagents: [", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "malformed"},
		{name: "duplicate yaml key", contents: "deepagents:\n  approvals:\n    fileWrites: required\n    fileWrites: notRequired\n", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "duplicate"},
		{name: "second yaml document", contents: "deepagents:\n  execution:\n    mode: sandbox\n---\ndeepagents:\n  approvals:\n    mcpDefault: required\n", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "multiple YAML documents"},
		{name: "malformed second yaml document", contents: "deepagents:\n  execution:\n    mode: sandbox\n---\ndeepagents: [\n", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "malformed"},
		{name: "unknown field", contents: "deepagents:\n  unknown: true\n", args: []string{"--runtime-config-file", "CONFIG"}, wantErr: "unknown field"},
		{name: "conflicting sandbox", contents: "deepagents: {}\n", args: []string{"--runtime", "deepagents", "--runtime-config-file", "CONFIG", "--deepagents-sandbox"}, wantErr: "cannot be combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			args := make([]string, 0, len(tt.args)+2)
			for _, arg := range tt.args {
				args = append(args, strings.ReplaceAll(arg, "CONFIG", path))
			}
			cmd := updateWorkerCmd()
			cmd.SetArgs(append([]string{"--name", "deep-worker"}, args...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		cmd := updateWorkerCmd()
		cmd.SetArgs([]string{"--name", "deep-worker", "--runtime-config-file", filepath.Join(dir, "does-not-exist.yaml")})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "read --runtime-config-file") {
			t.Fatalf("error = %v, want unreadable config-file error", err)
		}
	})
}
