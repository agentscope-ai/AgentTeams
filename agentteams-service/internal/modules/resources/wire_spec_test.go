package resources

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWireSpecUsesControllerAPIShape(t *testing.T) {
	raw, err := WireSpec(Spec{
		WorkerName:       "Worker One",
		Model:            "qwen3-coder",
		ModelProvider:    "apig-prod",
		PackageRef:       "pkg://x",
		ContainerManaged: true,
		DesiredState:     "Running",
		Skills:           []string{"s1"},
		Env:              map[string]string{"A": "b"},
		MCPServers:       json.RawMessage(`[{"name":"m1"}]`),
	})
	if err != nil {
		t.Fatalf("WireSpec: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("wire JSON: %v", err)
	}
	for _, key := range []string{"workerName", "model", "modelProvider", "package", "state", "containerManaged", "skills", "env", "mcpServers"} {
		if _, ok := m[key]; !ok {
			t.Errorf("wire spec missing controller key %q: %s", key, raw)
		}
	}
	// Storage-schema keys must never leak into the adapter contract.
	for _, key := range []string{"worker_name", "model_provider", "package_ref", "desired_state", "spec_hash", "team_slug", "skills_json"} {
		if _, ok := m[key]; ok {
			t.Errorf("wire spec leaked storage key %q: %s", key, raw)
		}
	}
	if got, _ := m["containerManaged"].(bool); !got {
		t.Errorf("containerManaged = %v, want true", m["containerManaged"])
	}
}

func TestWireSpecOmissionsAndValidation(t *testing.T) {
	raw, err := WireSpec(Spec{})
	if err != nil {
		t.Fatalf("WireSpec empty: %v", err)
	}
	if string(raw) != `{"containerManaged":false}` {
		t.Fatalf("empty wire spec = %s, want only containerManaged", raw)
	}
	if _, err := WireSpec(Spec{MCPServers: json.RawMessage("{bad")}); err == nil ||
		!strings.Contains(err.Error(), "mcpServers") {
		t.Fatalf("invalid raw JSON error = %v, want mcpServers context", err)
	}
}
