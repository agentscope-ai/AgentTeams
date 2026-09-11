// Package workflow holds workflow-snapshot presentation helpers shared by
// the controller API and the agt CLI.
package workflow

import (
	"fmt"
	"strings"
)

// Snapshot is the minimal workflow shape needed for mermaid rendering. It
// mirrors the nodes/edges/next fields of the controller's workflow response
// (LangGraph StateSnapshot-aligned).
type Snapshot struct {
	Nodes []Node   `json:"nodes"`
	Edges []Edge   `json:"edges"`
	Next  []string `json:"next"`
}

// Node is a single workflow graph node. Status is the normalized frontend
// enum (pending | delegated | in-progress | completed | revision | blocked).
type Node struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status,omitempty"`
	Assignee string `json:"assignee,omitempty"`
}

// Edge is a dependency edge (source must complete before target).
type Edge struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Conditional bool   `json:"conditional,omitempty"`
}

// mermaidStatusClass maps the normalized node status to a mermaid classDef
// name.
var mermaidStatusClass = map[string]string{
	"pending":     "pending",
	"delegated":   "delegated",
	"in-progress": "inProgress",
	"completed":   "completed",
	"revision":    "revision",
	"blocked":     "blocked",
}

// mermaidClassDefs lists every classDef the renderer emits, in stable order.
var mermaidClassDefs = []string{
	"classDef ready fill:#d4edda,stroke:#28a745;",
	"classDef pending fill:#e9ecef,stroke:#6c757d;",
	"classDef delegated fill:#cfe2ff,stroke:#0d6efd;",
	"classDef inProgress fill:#fff3cd,stroke:#ffc107;",
	"classDef completed fill:#d4edda,stroke:#198754;",
	"classDef revision fill:#ffe5d0,stroke:#fd7e14;",
	"classDef blocked fill:#f8d7da,stroke:#dc3545;",
}

// sanitizeNodeID maps a task id to a valid, unambiguous mermaid node id.
// Mermaid flowchart ids reliably accept [A-Za-z0-9_-]; anything else (e.g.
// dots, which the API's isSafeTaskID allows) is replaced with '_'. Colliding
// ids get a numeric suffix so two different task ids can never render as one
// node.
func sanitizeNodeID(id string, used map[string]bool) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "n"
	}
	if used[s] {
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s_%d", s, i)
			if !used[cand] {
				s = cand
				break
			}
		}
	}
	used[s] = true
	return s
}

// sanitizeLabel makes a task title safe for use inside a quoted mermaid
// label. Titles are user-controlled (workflow input); the normalization
// guarantees a malformed or hostile title can never alter the rendered
// graph structure:
//   - newlines / carriage returns -> `<br>` (mermaid's line-break tag) so a
//     label can never spill onto another node line
//   - other control characters -> space
//   - double quotes -> `#quot;` (mermaid's documented entity), so a title
//     can never terminate the label string
//   - backslashes are dropped, since mermaid's quoted-text lexer may treat
//     them as escape introducers (a trailing one could swallow the closing
//     quote)
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			b.WriteString("<br>")
		case r == '"':
			b.WriteString(`#quot;`)
		case r == '\\':
			// dropped on purpose (see doc comment)
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RenderMermaid renders a workflow snapshot as a mermaid flowchart
// (flowchart LR), mirroring LangGraph's draw_mermaid helper. Each node label
// is "name: status" (sanitized — see sanitizeLabel); next/ready nodes are
// highlighted with the `ready` class, all other nodes get a status-specific
// class. Node ids are sanitized (see sanitizeNodeId) so the output is valid
// mermaid for any user-controlled title or task id.
func RenderMermaid(s *Snapshot) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	nextSet := map[string]bool{}
	for _, id := range s.Next {
		nextSet[id] = true
	}
	idMap := map[string]string{}
	used := map[string]bool{}
	idFor := func(id string) string {
		if v, ok := idMap[id]; ok {
			return v
		}
		v := sanitizeNodeID(id, used)
		idMap[id] = v
		return v
	}
	for _, n := range s.Nodes {
		label := n.Name
		if n.Status != "" {
			label += ": " + n.Status
		}
		style := ""
		if nextSet[n.ID] {
			style = ":::ready"
		} else if c, ok := mermaidStatusClass[n.Status]; ok {
			style = ":::" + c
		}
		fmt.Fprintf(&b, "    %s[\"%s\"]%s\n", idFor(n.ID), sanitizeLabel(label), style)
	}
	for _, e := range s.Edges {
		fmt.Fprintf(&b, "    %s --> %s\n", idFor(e.Source), idFor(e.Target))
	}
	for _, cd := range mermaidClassDefs {
		b.WriteString("    " + cd + "\n")
	}
	return b.String()
}
