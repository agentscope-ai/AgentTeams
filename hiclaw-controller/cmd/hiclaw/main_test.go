package main

import (
	"strings"
	"testing"
)

func TestLoadResources_SingleWorker(t *testing.T) {
	yaml := `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	r := resources[0]
	if r.Kind != "Worker" {
		t.Errorf("expected kind Worker, got %s", r.Kind)
	}
	if r.Name != "alice" {
		t.Errorf("expected name alice, got %s", r.Name)
	}
	if r.APIVersion != "hiclaw.io/v1beta1" {
		t.Errorf("expected apiVersion hiclaw.io/v1beta1, got %s", r.APIVersion)
	}
}

func TestLoadResources_MultiDocument(t *testing.T) {
	yaml := `apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  leader:
    name: alpha-lead
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: John Doe
  permissionLevel: 2
---
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: bob
spec:
  model: qwen3.5-plus
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(resources))
	}
	if resources[0].Kind != "Team" || resources[0].Name != "alpha-team" {
		t.Errorf("resource 0: expected Team/alpha-team, got %s/%s", resources[0].Kind, resources[0].Name)
	}
	if resources[1].Kind != "Human" || resources[1].Name != "john" {
		t.Errorf("resource 1: expected Human/john, got %s/%s", resources[1].Kind, resources[1].Name)
	}
	if resources[2].Kind != "Worker" || resources[2].Name != "bob" {
		t.Errorf("resource 2: expected Worker/bob, got %s/%s", resources[2].Kind, resources[2].Name)
	}
}

func TestLoadResources_SkipsEmptyDocs(t *testing.T) {
	yaml := `---
---
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: test
---
---
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource (empty docs skipped), got %d", len(resources))
	}
}

func TestLoadResources_SkipsMissingName(t *testing.T) {
	yaml := `apiVersion: hiclaw.io/v1beta1
kind: Worker
spec:
  model: test
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("expected 0 resources (no name), got %d", len(resources))
	}
}

func TestLoadResources_SkipsMissingKind(t *testing.T) {
	yaml := `apiVersion: hiclaw.io/v1beta1
metadata:
  name: alice
spec:
  model: test
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("expected 0 resources (no kind), got %d", len(resources))
	}
}

func TestLoadResources_NameInMetadataOnly(t *testing.T) {
	// "name:" under spec should NOT be picked up as resource name
	yaml := `apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: my-team
spec:
  leader:
    name: leader-name
  workers:
    - name: worker-name
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	if resources[0].Name != "my-team" {
		t.Errorf("expected name my-team (from metadata), got %s", resources[0].Name)
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

func TestOrderForApply(t *testing.T) {
	resources := []resource{
		{Kind: "Human", Name: "john"},
		{Kind: "Worker", Name: "alice"},
		{Kind: "Team", Name: "alpha"},
		{Kind: "Worker", Name: "bob"},
		{Kind: "Human", Name: "jane"},
	}

	ordered := orderForApply(resources)

	// Expected order: Team -> Worker -> Human
	if ordered[0].Kind != "Team" {
		t.Errorf("expected first to be Team, got %s", ordered[0].Kind)
	}
	if ordered[1].Kind != "Worker" || ordered[2].Kind != "Worker" {
		t.Errorf("expected workers at positions 1-2, got %s, %s", ordered[1].Kind, ordered[2].Kind)
	}
	if ordered[3].Kind != "Human" || ordered[4].Kind != "Human" {
		t.Errorf("expected humans at positions 3-4, got %s, %s", ordered[3].Kind, ordered[4].Kind)
	}
}

func TestWriteTempYAML(t *testing.T) {
	content := "kind: Worker\nmetadata:\n  name: test"
	path, err := writeTempYAML(content)
	if err != nil {
		t.Fatalf("writeTempYAML failed: %v", err)
	}
	if path == "" {
		t.Fatal("writeTempYAML returned empty path")
	}
}

// Helper to create temp YAML file for tests
func writeTempYAMLForTest(t *testing.T, content string) string {
	t.Helper()
	path, err := writeTempYAML(content)
	if err != nil {
		t.Fatalf("failed to write temp YAML: %v", err)
	}
	t.Cleanup(func() {
		// os.Remove(path) // uncomment to auto-clean
		_ = path
	})
	return path
}

func TestExtractPackageField(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		expected string
	}{
		{
			name: "nacos package",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  package: nacos://admin:pass@host:8848/ns/my-spec/v1
  model: claude-sonnet-4-6
`,
			expected: "nacos://admin:pass@host:8848/ns/my-spec/v1",
		},
		{
			name: "http package",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: bob
spec:
  package: https://example.com/worker.zip
  model: qwen3.5-plus
`,
			expected: "https://example.com/worker.zip",
		},
		{
			name: "no package field",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: charlie
spec:
  model: qwen3.5-plus
`,
			expected: "",
		},
		{
			name: "no spec section",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: dave
`,
			expected: "",
		},
		{
			name: "package outside spec is ignored",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: eve
  package: should-be-ignored
spec:
  model: test
`,
			expected: "",
		},
		{
			name: "package in nested section under spec is still found",
			yaml: `apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: frank
spec:
  model: test
  package: nacos://host:8848/ns/spec
  skills:
    - github-operations
`,
			expected: "nacos://host:8848/ns/spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPackageField(tt.yaml)
			if got != tt.expected {
				t.Errorf("extractPackageField() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestLoadResources_WorkerWithInlineFields(t *testing.T) {
	yaml := `apiVersion: hiclaw.io/v1beta1
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
  agents: |
    ## Behavior
    Monitor pipelines proactively
`
	tmpFile := writeTempYAMLForTest(t, yaml)
	resources, err := loadResources([]string{tmpFile})
	if err != nil {
		t.Fatalf("loadResources failed: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	r := resources[0]
	if r.Kind != "Worker" {
		t.Errorf("expected kind Worker, got %s", r.Kind)
	}
	if r.Name != "alice" {
		t.Errorf("expected name alice, got %s", r.Name)
	}
	// Verify the raw YAML preserves inline fields
	if !strings.Contains(r.Raw, "identity:") {
		t.Error("raw YAML should contain identity field")
	}
	if !strings.Contains(r.Raw, "soul:") {
		t.Error("raw YAML should contain soul field")
	}
	if !strings.Contains(r.Raw, "agents:") {
		t.Error("raw YAML should contain agents field")
	}
}
