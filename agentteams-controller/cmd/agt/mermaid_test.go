package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/workflow"
)

// TestCLIMermaidPath covers the CLI integration path: a JSON-decoded
// workflow response (map[string]any, as fetched via DoJSON) is re-marshaled
// into workflow.Snapshot and rendered — the exact sequence --mermaid runs.
func TestCLIMermaidPath(t *testing.T) {
	raw := []byte(`{
		"nodes": [
			{"id": "t1", "name": "Task 1", "status": "completed", "assignee": "@w1"},
			{"id": "t2", "name": "Task 2", "status": "delegated"}
		],
		"edges": [{"source": "t1", "target": "t2"}],
		"next": ["t2"]
	}`)
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var snap workflow.Snapshot
	buf, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(buf, &snap); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	out := workflow.RenderMermaid(&snap)
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
	if !strings.Contains(out, `t1["Task 1: completed"]:::completed`) {
		t.Fatalf("expected node t1 with status class, got %q", out)
	}
	if !strings.Contains(out, `t2["Task 2: delegated"]:::ready`) {
		t.Fatalf("expected ready class on t2, got %q", out)
	}
}
