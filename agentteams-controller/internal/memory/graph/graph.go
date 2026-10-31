// Package graph is the knowledge-graph response shape (#1412 W0b,
// task book W0 item 5): the nodes/edges JSON the Knowledge API will
// serve per anchor, with the scope rules fixed before the indexer
// (W4) exists.
//
// Invariants this package pins down:
//
//   - a response is scoped to exactly one anchor; a node or edge of
//     any other anchor never appears in it;
//   - nodes come back sorted by id, edges by (from, to, kind, weight,
//     provenance): the ordering is stable across rebuilds, so a
//     response body can be ETagged in W4;
//   - the JSON field names and their order are a contract; golden
//     tests assert the exact bytes.
package graph

import (
	"sort"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

// Node is one graph vertex. Refs points at the source of truth the
// vertex was derived from (PC artifact id, ReMe path, ...); the graph
// stores refs, never content.
type Node struct {
	ID        string    `json:"id"`
	Anchor    string    `json:"anchor"` // canonical "kind:id"
	Kind      string    `json:"kind"`   // profile|memory|source|digest|daily|handoff|experience|skill
	Label     string    `json:"label"`
	Refs      []string  `json:"refs"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Edge is one directed graph edge. Anchor tags the scope the edge
// belongs to (knowledge_edges.anchor_id); it drives filtering only
// and is never rendered into the JSON.
type Edge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Kind       string  `json:"kind"` // wikilink|source|promotion|reference
	Weight     float64 `json:"weight"`
	Provenance string  `json:"provenance"`
	Anchor     string  `json:"-"`
}

// Graph is the derived projection of the knowledge tables for
// several anchors. W0b: populated directly; W4 fills it from
// knowledge_nodes / knowledge_edges.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// FilterNodes returns the nodes of anchor in id order. The result is
// always a fresh, non-nil slice (never the receiver's backing
// array); nil refs are normalised to [] so the JSON is stable.
func (g Graph) FilterNodes(anchor string) []Node {
	out := make([]Node, 0)
	for _, n := range g.Nodes {
		if n.Anchor != anchor {
			continue
		}
		if n.Refs == nil {
			n.Refs = []string{}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// FilterEdges returns the edges of anchor in (from, to, kind, weight,
// provenance) order - the edge's identity, so the ordering is total
// and stable.
func (g Graph) FilterEdges(anchor string) []Edge {
	out := make([]Edge, 0)
	for _, e := range g.Edges {
		if e.Anchor != anchor {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Weight != b.Weight {
			return a.Weight < b.Weight
		}
		return a.Provenance < b.Provenance
	})
	return out
}

// Response is one anchor's graph as served: exactly that anchor's
// nodes and edges, deterministically ordered.
type Response struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// ResponseFor renders anchor's graph. An unknown anchor yields a
// well-formed empty response: {"nodes":[],"edges":[]}.
func (g Graph) ResponseFor(anchor string) Response {
	return Response{Nodes: g.FilterNodes(anchor), Edges: g.FilterEdges(anchor)}
}

// ResponseForAnchor is the type-safe form of ResponseFor; an invalid
// anchor yields the empty response.
func (g Graph) ResponseForAnchor(a memory.Anchor) Response {
	if !a.Valid() {
		return Response{Nodes: []Node{}, Edges: []Edge{}}
	}
	return g.ResponseFor(a.String())
}
