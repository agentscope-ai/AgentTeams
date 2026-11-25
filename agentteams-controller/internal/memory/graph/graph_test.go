package graph

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

var fixedTime = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// sampleGraph covers the three scopes used across the W0b tests:
// human:alice, team:t1, agent:a1.
func sampleGraph() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "n2", Anchor: "human:alice", Kind: "memory", Label: "prefers go modules", Refs: []string{"pc:entry:42"}, UpdatedAt: fixedTime},
			{ID: "a1", Anchor: "agent:a1", Kind: "digest", Label: "a1 digest", Refs: []string{"reme:digest/team.md"}, UpdatedAt: fixedTime},
			{ID: "t1", Anchor: "team:t1", Kind: "profile", Label: "t1 profile", Refs: nil, UpdatedAt: fixedTime},
			{ID: "n1", Anchor: "human:alice", Kind: "profile", Label: "alice profile", Refs: nil, UpdatedAt: fixedTime},
		},
		Edges: []Edge{
			{From: "t1", To: "t1", Kind: "reference", Weight: 1, Provenance: "pc:artifact:t1", Anchor: "team:t1"},
			{From: "n2", To: "n1", Kind: "reference", Weight: 1, Provenance: "wiki:link", Anchor: "human:alice"},
			{From: "a1", To: "a1", Kind: "source", Weight: 1, Provenance: "reme:graph", Anchor: "agent:a1"},
		},
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestResponseGolden pins the JSON field names, order and format.
func TestResponseGolden(t *testing.T) {
	g := sampleGraph()

	wantAlice := `{"nodes":[{"id":"n1","anchor":"human:alice","kind":"profile","label":"alice profile","refs":[],"updated_at":"2026-10-31T12:00:00Z"},{"id":"n2","anchor":"human:alice","kind":"memory","label":"prefers go modules","refs":["pc:entry:42"],"updated_at":"2026-10-31T12:00:00Z"}],"edges":[{"from":"n2","to":"n1","kind":"reference","weight":1,"provenance":"wiki:link"}]}`
	if got := mustMarshal(t, g.ResponseFor("human:alice")); string(got) != wantAlice {
		t.Fatalf("alice response =\n%s\nwant\n%s", got, wantAlice)
	}

	wantTeam := `{"nodes":[{"id":"t1","anchor":"team:t1","kind":"profile","label":"t1 profile","refs":[],"updated_at":"2026-10-31T12:00:00Z"}],"edges":[{"from":"t1","to":"t1","kind":"reference","weight":1,"provenance":"pc:artifact:t1"}]}`
	if got := mustMarshal(t, g.ResponseFor("team:t1")); string(got) != wantTeam {
		t.Fatalf("team response =\n%s\nwant\n%s", got, wantTeam)
	}

	// An unknown anchor is a well-formed empty response.
	if got := mustMarshal(t, g.ResponseFor("team:unknown")); string(got) != `{"nodes":[],"edges":[]}` {
		t.Fatalf("unknown anchor response = %s", got)
	}
}

// TestScopeIsolation: cross-anchor nodes and edges never appear.
func TestScopeIsolation(t *testing.T) {
	g := sampleGraph()

	expectNodes := map[string][]string{
		"human:alice": {"n1", "n2"},
		"team:t1":     {"t1"},
		"agent:a1":    {"a1"},
	}
	expectEdgeCount := map[string]int{"human:alice": 1, "team:t1": 1, "agent:a1": 1}

	for anchor, wantIDs := range expectNodes {
		resp := g.ResponseFor(anchor)
		if len(resp.Nodes) != len(wantIDs) {
			t.Fatalf("%s: got %d nodes, want %d: %+v", anchor, len(resp.Nodes), len(wantIDs), resp.Nodes)
		}
		for i, id := range wantIDs {
			if resp.Nodes[i].ID != id || resp.Nodes[i].Anchor != anchor {
				t.Fatalf("%s: node[%d] = %+v, want id %s under %s", anchor, i, resp.Nodes[i], id, anchor)
			}
		}
		if len(resp.Edges) != expectEdgeCount[anchor] {
			t.Fatalf("%s: got %d edges, want %d", anchor, len(resp.Edges), expectEdgeCount[anchor])
		}
		for _, e := range resp.Edges {
			if e.Anchor != anchor {
				t.Fatalf("%s: cross-anchor edge %+v", anchor, e)
			}
		}
		// Byte level: the other scopes' raw anchor strings must not
		// leak into the response at all.
		b := mustMarshal(t, resp)
		for other := range expectNodes {
			if other == anchor {
				continue
			}
			if strings.Contains(string(b), other) {
				t.Fatalf("%s: response leaks %q: %s", anchor, other, b)
			}
		}
	}
}

// TestOrderingStable: the response does not depend on input order.
func TestOrderingStable(t *testing.T) {
	base := mustMarshal(t, sampleGraph().ResponseFor("human:alice"))
	rev := Graph{
		Nodes: []Node{
			{ID: "t1", Anchor: "team:t1", Kind: "profile", Label: "t1 profile", UpdatedAt: fixedTime},
			{ID: "n2", Anchor: "human:alice", Kind: "memory", Label: "prefers go modules", Refs: []string{"pc:entry:42"}, UpdatedAt: fixedTime},
			{ID: "a1", Anchor: "agent:a1", Kind: "digest", Label: "a1 digest", Refs: []string{"reme:digest/team.md"}, UpdatedAt: fixedTime},
			{ID: "n1", Anchor: "human:alice", Kind: "profile", Label: "alice profile", UpdatedAt: fixedTime},
		},
		Edges: []Edge{
			{From: "a1", To: "a1", Kind: "source", Weight: 1, Provenance: "reme:graph", Anchor: "agent:a1"},
			{From: "t1", To: "t1", Kind: "reference", Weight: 1, Provenance: "pc:artifact:t1", Anchor: "team:t1"},
			{From: "n2", To: "n1", Kind: "reference", Weight: 1, Provenance: "wiki:link", Anchor: "human:alice"},
		},
	}
	if got := mustMarshal(t, rev.ResponseFor("human:alice")); !bytes.Equal(got, base) {
		t.Fatalf("reordered input changed the response:\n%s\nvs\n%s", got, base)
	}
}

func TestFilterAndAnchorForm(t *testing.T) {
	g := sampleGraph()

	nodes := g.FilterNodes("human:alice")
	if len(nodes) != 2 || nodes[0].ID != "n1" || nodes[1].ID != "n2" {
		t.Fatalf("FilterNodes = %+v", nodes)
	}
	if nodes[0].Refs == nil {
		t.Fatal("nil refs must be normalised to []")
	}

	// The type-safe form agrees with the string form.
	a, b := mustMarshal(t, g.ResponseForAnchor(memory.Anchor{Kind: memory.KindHuman, ID: "alice"})),
		mustMarshal(t, g.ResponseFor("human:alice"))
	if !bytes.Equal(a, b) {
		t.Fatalf("ResponseForAnchor = %s, want %s", a, b)
	}

	// An invalid anchor (no id) yields the empty response.
	if got := mustMarshal(t, g.ResponseForAnchor(memory.Anchor{Kind: memory.KindHuman, ID: ""})); string(got) != `{"nodes":[],"edges":[]}` {
		t.Fatalf("invalid anchor response = %s", got)
	}
}
