package workflow

import (
	"strings"
	"testing"
)

func TestRenderMermaid(t *testing.T) {
	s := &Snapshot{
		Nodes: []Node{
			{ID: "t1", Name: "Task 1", Status: "completed"},
			{ID: "t2", Name: "Task 2", Status: "delegated"},
		},
		Edges: []Edge{{Source: "t1", Target: "t2"}},
		Next:  []string{"t2"},
	}
	out := RenderMermaid(s)
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
	if !strings.Contains(out, `t1["Task 1: completed"]:::completed`) {
		t.Fatalf("expected node t1 with status class, got %q", out)
	}
	if !strings.Contains(out, "t1 --> t2") {
		t.Fatalf("expected edge t1 --> t2, got %q", out)
	}
	// ready node t2 gets the ready class (overrides the status class)
	if !strings.Contains(out, `t2["Task 2: delegated"]:::ready`) {
		t.Fatalf("expected ready class on t2, got %q", out)
	}
	for _, cd := range mermaidClassDefs {
		if !strings.Contains(out, cd) {
			t.Fatalf("expected classDef %q, got %q", cd, out)
		}
	}
}

func TestRenderMermaid_StatusClasses(t *testing.T) {
	s := &Snapshot{
		Nodes: []Node{
			{ID: "a", Name: "A", Status: "pending"},
			{ID: "b", Name: "B", Status: "in-progress"},
			{ID: "c", Name: "C", Status: "revision"},
			{ID: "d", Name: "D", Status: "blocked"},
			{ID: "e", Name: "E"}, // unknown/empty status: no class
		},
	}
	out := RenderMermaid(s)
	for _, want := range []string{
		`a["A: pending"]:::pending`,
		`b["B: in-progress"]:::inProgress`,
		`c["C: revision"]:::revision`,
		`d["D: blocked"]:::blocked`,
		`e["E"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output, got %q", want, out)
		}
	}
}

func TestRenderMermaid_EmptyGraph(t *testing.T) {
	out := RenderMermaid(&Snapshot{Nodes: []Node{}, Edges: []Edge{}, Next: []string{}})
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
	if strings.Contains(out, "-->") {
		t.Fatalf("no edges expected, got %q", out)
	}
}

func TestRenderMermaid_NilSnapshot(t *testing.T) {
	// Defensive: a nil snapshot must not panic (renders header only).
	out := RenderMermaid(&Snapshot{})
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
}

// --- mermaid safety for user-controlled titles (reviewer requirement) ---

func TestRenderMermaid_MaliciousTitles(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		checks []string // substrings that MUST appear
		absent []string // substrings that MUST NOT appear
	}{
		{
			name:   "double quotes",
			title:  `say "hello" & 'world'`,
			checks: []string{`#quot;hello#quot;`},
			absent: []string{`say "hello`},
		},
		{
			name:   "embedded newline",
			title:  "line1\nline2",
			checks: []string{`line1<br>line2`},
		},
		{
			name:   "trailing backslash before closing quote",
			title:  `ends with backslash\`,
			checks: []string{`ends with backslash`},
		},
		{
			name:   "edge-like syntax",
			title:  "A --> B",
			checks: []string{`"A --> B: pending"`},
		},
		{
			name:   "brackets and parens",
			title:  "task [3] (final) ] [",
			checks: []string{`task [3] (final) ] [`},
		},
		{
			name:   "control characters",
			title:  "tab\there\x00nul",
			checks: []string{`tab here`},
		},
		{
			name:   "unicode preserved",
			title:  "任务一：设计评审",
			checks: []string{`任务一：设计评审`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{
				Nodes: []Node{{ID: "t1", Name: tc.title, Status: "pending"}},
				Next:  []string{},
			}
			out := RenderMermaid(s)
			// Structural invariant: exactly one node line, and it stays on a
			// single line with balanced quoting (a label can never spill or
			// terminate early).
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			var nodeLines []string
			for _, l := range lines {
				if strings.HasPrefix(strings.TrimSpace(l), "t1[") {
					nodeLines = append(nodeLines, l)
				}
			}
			if len(nodeLines) != 1 {
				t.Fatalf("expected exactly one node line, got %d: %q", len(nodeLines), out)
			}
			ln := nodeLines[0]
			if !strings.HasPrefix(ln, `    t1["`) {
				t.Fatalf("node line must start with quoted label: %q", ln)
			}
			for _, want := range tc.checks {
				if !strings.Contains(ln, want) {
					t.Fatalf("expected %q in node line, got %q", want, ln)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(out, bad) {
					t.Fatalf("forbidden %q present in output: %q", bad, out)
				}
			}
		})
	}
}

func TestRenderMermaid_TitleCannotBreakStructure(t *testing.T) {
	// A title containing node/edge syntax must render exactly one node and
	// must not create extra edges.
	s := &Snapshot{
		Nodes: []Node{
			{ID: "t1", Name: "x --> y\nz[\"quoted\"]", Status: "pending"},
			{ID: "t2", Name: "ok", Status: "completed"},
		},
		Edges: []Edge{{Source: "t1", Target: "t2"}},
		Next:  []string{},
	}
	out := RenderMermaid(s)
	edgeLines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "-->") && !strings.HasPrefix(strings.TrimSpace(l), "t1[") {
			edgeLines++
		}
	}
	if edgeLines != 1 {
		t.Fatalf("expected exactly 1 edge line, got %d:\n%s", edgeLines, out)
	}
}

func TestRenderMermaid_NodeIDSanitization(t *testing.T) {
	// Dots (allowed by the API's isSafeTaskID) are not reliable mermaid id
	// characters: they map to '_', and colliding ids must stay distinct.
	s := &Snapshot{
		Nodes: []Node{
			{ID: "t.1", Name: "dotted", Status: "pending"},
			{ID: "t_1", Name: "underscored", Status: "completed"},
		},
		Edges: []Edge{{Source: "t.1", Target: "t_1"}},
		Next:  []string{"t.1"},
	}
	out := RenderMermaid(s)
	// t.1 -> t_1 (first), t_1 -> t_1_2 (collision suffix); both nodes and
	// the edge must use the sanitized ids consistently.
	if !strings.Contains(out, `t_1["dotted: pending"]:::ready`) {
		t.Fatalf("dotted id not sanitized to t_1 with ready class:\n%s", out)
	}
	if !strings.Contains(out, `t_1_2["underscored: completed"]:::completed`) {
		t.Fatalf("collision id not suffixed:\n%s", out)
	}
	if !strings.Contains(out, "t_1 --> t_1_2") {
		t.Fatalf("edge must use sanitized ids:\n%s", out)
	}
}
