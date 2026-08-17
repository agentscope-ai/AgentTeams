package main

import (
	"strings"
	"testing"
)

func TestWorkflowMermaid(t *testing.T) {
	resp := map[string]any{
		"nodes": []any{
			map[string]any{"id": "t1", "name": "Task 1", "status": "completed"},
			map[string]any{"id": "t2", "name": "Task 2", "status": "delegated"},
		},
		"edges": []any{
			map[string]any{"source": "t1", "target": "t2"},
		},
		"next": []any{"t2"},
	}
	out := workflowMermaid(resp)
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
	if !strings.Contains(out, `t1["Task 1: completed"]`) {
		t.Fatalf("expected node t1 with status, got %q", out)
	}
	if !strings.Contains(out, "t1 --> t2") {
		t.Fatalf("expected edge t1 --> t2, got %q", out)
	}
	// ready node t2 gets the ::ready class
	if !strings.Contains(out, `t2["Task 2: delegated"]:::ready`) {
		t.Fatalf("expected ready class on t2, got %q", out)
	}
	if !strings.Contains(out, "classDef ready") {
		t.Fatalf("expected ready classDef, got %q", out)
	}
}

func TestWorkflowMermaid_EmptyGraph(t *testing.T) {
	resp := map[string]any{
		"nodes": []any{},
		"edges": []any{},
		"next":  []any{},
	}
	out := workflowMermaid(resp)
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
}
