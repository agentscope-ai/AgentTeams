package main

import (
	"os"
	"strings"
	"testing"

	sigyaml "sigs.k8s.io/yaml"
)

func TestRootCommandUsesInvokedBinaryName(t *testing.T) {
	for _, name := range []string{"agt", "hiclaw"} {
		if got := newRootCommand(name).Use; got != name {
			t.Fatalf("newRootCommand(%q).Use = %q", name, got)
		}
	}
}

func TestExpandPackageURI(t *testing.T) {
	t.Setenv("AGENTTEAMS_NACOS_REGISTRY_URI", "nacos://registry.example.com/public")

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "shorthand name",
			input: "worker-name",
			want:  "nacos://registry.example.com/public/worker-name",
		},
		{
			name:  "shorthand version",
			input: "worker-name/v1",
			want:  "nacos://registry.example.com/public/worker-name/v1",
		},
		{
			name:  "shorthand label latest",
			input: "worker-name/label:latest",
			want:  "nacos://registry.example.com/public/worker-name/label:latest",
		},
		{
			name:  "full nacos uri unchanged",
			input: "nacos://host:8848/public/worker-name/v1",
			want:  "nacos://host:8848/public/worker-name/v1",
		},
		{
			name:  "full http uri unchanged",
			input: "https://example.com/worker.zip",
			want:  "https://example.com/worker.zip",
		},
		{
			name:    "invalid empty segment",
			input:   "worker-name/",
			wantErr: "empty path segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandPackageURI(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expandPackageURI() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandPackageURI() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expandPackageURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateWorkerName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:  "simple",
			input: "alice",
		},
		{
			name:  "hyphenated",
			input: "dev-01",
		},
		{
			name:    "empty",
			input:   "",
			wantErr: "name is required",
		},
		{
			name:    "uppercase rejected",
			input:   "Alice",
			wantErr: "invalid worker name",
		},
		{
			name:    "underscore rejected",
			input:   "alice_dev",
			wantErr: "invalid worker name",
		},
		{
			name:    "leading hyphen rejected",
			input:   "-alice",
			wantErr: "invalid worker name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWorkerName(tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateWorkerName() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateWorkerName() unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// YAML parsing tests — exercise splitYAMLDocs + yamlResource unmarshal,
// which is the pipeline used by applyFromFiles.
// ---------------------------------------------------------------------------

func TestParseYAML_SingleWorker(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
`
	docs := splitYAMLDocs(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if res.Kind != "Worker" {
		t.Errorf("expected kind Worker, got %s", res.Kind)
	}
	if res.Metadata.Name != "alice" {
		t.Errorf("expected name alice, got %s", res.Metadata.Name)
	}
	if res.APIVersion != "agentteams.io/v1beta1" {
		t.Errorf("expected apiVersion agentteams.io/v1beta1, got %s", res.APIVersion)
	}
}

func TestBuildApplyBody_PreservesWorkerResources(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "1"
      memory: 2Gi
`
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(input), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	body := buildApplyBody(res, true)
	if body["name"] != "alice" {
		t.Fatalf("body name = %v, want alice", body["name"])
	}

	resources, ok := body["resources"].(map[string]interface{})
	if !ok {
		t.Fatalf("body resources = %T, want map[string]interface{}", body["resources"])
	}
	requests, ok := resources["requests"].(map[string]interface{})
	if !ok {
		t.Fatalf("resources.requests = %T, want map[string]interface{}", resources["requests"])
	}
	limits, ok := resources["limits"].(map[string]interface{})
	if !ok {
		t.Fatalf("resources.limits = %T, want map[string]interface{}", resources["limits"])
	}

	if requests["cpu"] != "500m" || requests["memory"] != "1Gi" {
		t.Fatalf("requests = %#v, want cpu=500m memory=1Gi", requests)
	}
	if limits["cpu"] != "1" || limits["memory"] != "2Gi" {
		t.Fatalf("limits = %#v, want cpu=1 memory=2Gi", limits)
	}
}

func TestParseYAML_MultiDocument(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  leader:
    name: alpha-lead
---
apiVersion: agentteams.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: John Doe
  permissionLevel: 2
---
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: bob
spec:
  model: qwen3.5-plus
`
	docs := splitYAMLDocs(input)
	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(docs))
	}

	expected := []struct {
		kind string
		name string
	}{
		{"Team", "alpha-team"},
		{"Human", "john"},
		{"Worker", "bob"},
	}
	for i, exp := range expected {
		var res yamlResource
		if err := sigyaml.Unmarshal([]byte(docs[i]), &res); err != nil {
			t.Fatalf("doc %d unmarshal failed: %v", i, err)
		}
		if res.Kind != exp.kind || res.Metadata.Name != exp.name {
			t.Errorf("doc %d: expected %s/%s, got %s/%s", i, exp.kind, exp.name, res.Kind, res.Metadata.Name)
		}
	}
}

func TestParseYAML_SkipsEmptyDocs(t *testing.T) {
	input := `---
---
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: test
---
---
`
	docs := splitYAMLDocs(input)
	// splitYAMLDocs should only return non-empty docs
	if len(docs) != 1 {
		t.Fatalf("expected 1 non-empty doc, got %d", len(docs))
	}
}

func TestParseYAML_MissingNameSkipped(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Worker
spec:
  model: test
`
	docs := splitYAMLDocs(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	// applyFromFiles skips resources with empty Name
	if res.Metadata.Name != "" {
		t.Fatalf("expected empty name, got %q", res.Metadata.Name)
	}
}

func TestParseYAML_MissingKindSkipped(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
metadata:
  name: alice
spec:
  model: test
`
	docs := splitYAMLDocs(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	// applyFromFiles skips resources with empty Kind
	if res.Kind != "" {
		t.Fatalf("expected empty kind, got %q", res.Kind)
	}
}

func TestParseYAML_NameInMetadataOnly(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: my-team
spec:
  leader:
    name: leader-name
  workers:
    - name: worker-name
`
	docs := splitYAMLDocs(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	// metadata.name should be "my-team", not confused with spec.leader.name
	if res.Metadata.Name != "my-team" {
		t.Errorf("expected name my-team (from metadata), got %s", res.Metadata.Name)
	}
}

func TestSplitYAMLDocs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"single doc", "kind: Worker\nname: alice", 1},
		{"two docs", "kind: Worker\n---\nkind: Human", 2},
		{"leading separator", "---\nkind: Worker", 1},
		{"trailing separator", "kind: Worker\n---", 1},
		{"empty between", "kind: Worker\n---\n---\nkind: Human", 2},
		{"all empty", "---\n---\n---", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := splitYAMLDocs(tt.input)
			if len(docs) != tt.expected {
				t.Errorf("expected %d docs, got %d", tt.expected, len(docs))
			}
		})
	}
}

func TestParseYAML_WorkerWithInlineFields(t *testing.T) {
	input := `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  identity: |
    Name: Alice
    Specialization: DevOps
  soul: |
    # Alice - DevOps Worker
    ## Role
    CI/CD pipeline management
`
	docs := splitYAMLDocs(input)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	var res yamlResource
	if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if res.Kind != "Worker" {
		t.Errorf("expected kind Worker, got %s", res.Kind)
	}
	if res.Metadata.Name != "alice" {
		t.Errorf("expected name alice, got %s", res.Metadata.Name)
	}
	if _, ok := res.Spec["identity"]; !ok {
		t.Error("spec should contain identity field")
	}
	if _, ok := res.Spec["soul"]; !ok {
		t.Error("spec should contain soul field")
	}
}

func TestParseYAML_PackageFieldInSpec(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantPkg string
	}{
		{
			name: "nacos package",
			yaml: `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  package: nacos://admin:pass@host:8848/ns/my-spec/v1
  model: claude-sonnet-4-6
`,
			wantPkg: "nacos://admin:pass@host:8848/ns/my-spec/v1",
		},
		{
			name: "http package",
			yaml: `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: bob
spec:
  package: https://example.com/worker.zip
  model: qwen3.5-plus
`,
			wantPkg: "https://example.com/worker.zip",
		},
		{
			name: "no package field",
			yaml: `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: charlie
spec:
  model: qwen3.5-plus
`,
			wantPkg: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := splitYAMLDocs(tt.yaml)
			if len(docs) != 1 {
				t.Fatalf("expected 1 doc, got %d", len(docs))
			}
			var res yamlResource
			if err := sigyaml.Unmarshal([]byte(docs[0]), &res); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			got, _ := res.Spec["package"].(string)
			if got != tt.wantPkg {
				t.Errorf("spec.package = %q, want %q", got, tt.wantPkg)
			}
		})
	}
}

func TestApplyFromFiles_Integration(t *testing.T) {
	// Verify applyFromFiles reads and parses a real temp file without panicking.
	// It will fail on the HTTP call, but we can verify the file-reading + parsing path.
	content := `apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: test
`
	tmpFile, err := os.CreateTemp("", "hiclaw-test-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	tmpFile.Close()

	// applyFromFiles will fail when trying to reach the controller API,
	// but it should get past file reading and YAML parsing without error.
	err = applyFromFiles([]string{tmpFile.Name()})
	// We expect an HTTP error, not a parse error
	if err == nil {
		t.Fatal("expected error from API call, got nil")
	}
	if strings.Contains(err.Error(), "read ") || strings.Contains(err.Error(), "parse YAML") {
		t.Fatalf("expected API error, got file/parse error: %v", err)
	}
}
