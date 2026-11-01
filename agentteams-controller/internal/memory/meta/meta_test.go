package meta

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/migrate"
)

func openStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "memory-meta.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// threeAnchors creates the same three scopes the W0b graph tests use:
// human:alice, team:t1, agent:a1.
func threeAnchors(t *testing.T, s *SQLite) (Anchor, Anchor, Anchor) {
	t.Helper()
	ctx := context.Background()
	anchors := []Anchor{
		{Anchor: memory.Anchor{Kind: memory.KindHuman, ID: "alice"},
			BackendKind: "powercontext", BackendLocation: "scope:human:alice", Capabilities: `{"search":true}`, BackendVersion: "1"},
		{Anchor: memory.Anchor{Kind: memory.KindTeam, ID: "t1"},
			BackendKind: "powercontext", BackendLocation: "scope:team:t1", Capabilities: `{"search":true}`, BackendVersion: "1"},
		{Anchor: memory.Anchor{Kind: memory.KindAgent, ID: "a1"},
			BackendKind: "remelight-file", BackendLocation: "agents/a1", Capabilities: `{}`, BackendVersion: "0.4.2.1"},
	}
	var out [3]Anchor
	for i, a := range anchors {
		created, err := s.UpsertAnchor(ctx, a)
		if err != nil {
			t.Fatalf("upsert %s: %v", a.Anchor.String(), err)
		}
		if created.RowID == 0 {
			t.Fatalf("%s: RowID not set", a.Anchor.String())
		}
		out[i] = created
	}
	return out[0], out[1], out[2]
}

func TestEnsureSchema(t *testing.T) {
	s := openStore(t) // Open already ran EnsureSchema
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("second ensure_schema: %v", err)
	}
	v, err := migrate.CurrentVersion(context.Background(), s.db, migrate.ProfileLite)
	if err != nil {
		t.Fatalf("current version: %v", err)
	}
	if v != 8 {
		t.Fatalf("version = %d, want 8", v)
	}
}

func TestAnchorCRUD(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	alice := memory.Anchor{Kind: memory.KindHuman, ID: "alice"}

	a, err := s.UpsertAnchor(ctx, Anchor{
		Anchor:          alice,
		BackendKind:     "powercontext",
		BackendLocation: "scope:human:alice",
		Capabilities:    `{"search":true,"export":true}`,
		BackendVersion:  "1",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if a.RowID == 0 || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Fatalf("upsert anchor = %+v, want row id and timestamps", a)
	}

	// An invalid anchor (no id) is rejected before it touches the DB.
	if _, err := s.UpsertAnchor(ctx, Anchor{Anchor: memory.Anchor{Kind: memory.KindHuman}, BackendKind: "powercontext"}); !errors.Is(err, memory.ErrInvalidAnchor) {
		t.Fatalf("invalid anchor upsert = %v, want ErrInvalidAnchor", err)
	}

	// Upsert again: same row, only the version and updated_at move.
	a2, err := s.UpsertAnchor(ctx, Anchor{
		Anchor:          alice,
		BackendKind:     "powercontext",
		BackendLocation: "scope:human:alice",
		Capabilities:    `{"search":true,"export":true}`,
		BackendVersion:  "2",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if a2.RowID != a.RowID {
		t.Fatalf("re-upsert moved the row: %d -> %d", a.RowID, a2.RowID)
	}
	got, err := s.GetAnchor(ctx, memory.KindHuman, "alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BackendVersion != "2" {
		t.Fatalf("backend version = %q, want 2", got.BackendVersion)
	}

	// Empty capabilities default to {} (the column is NOT NULL).
	if _, err := s.UpsertAnchor(ctx, Anchor{
		Anchor:        memory.Anchor{Kind: memory.KindTeam, ID: "t1"},
		BackendKind:   "powercontext",
	}); err != nil {
		t.Fatalf("upsert t1: %v", err)
	}
	t1, err := s.GetAnchor(ctx, memory.KindTeam, "t1")
	if err != nil {
		t.Fatalf("get t1: %v", err)
	}
	if t1.Capabilities != "{}" {
		t.Fatalf("capabilities = %q, want {}", t1.Capabilities)
	}

	// An unknown anchor is ErrNotFound.
	if _, err := s.GetAnchor(ctx, memory.KindHuman, "nobody"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get nobody = %v, want ErrNotFound", err)
	}

	// List is deterministic: owner_type, owner_id order.
	if _, err := s.UpsertAnchor(ctx, Anchor{
		Anchor:      memory.Anchor{Kind: memory.KindAgent, ID: "a1"},
		BackendKind: "remelight-file",
	}); err != nil {
		t.Fatalf("upsert a1: %v", err)
	}
	all, err := s.ListAnchors(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	wantOrder := []string{"agent:a1", "human:alice", "team:t1"}
	if len(all) != len(wantOrder) {
		t.Fatalf("list = %d anchors, want %d", len(all), len(wantOrder))
	}
	for i, want := range wantOrder {
		if all[i].Anchor.String() != want {
			t.Fatalf("list[%d] = %s, want %s", i, all[i].Anchor.String(), want)
		}
	}

	// Delete, then it is gone (and a second delete is ErrNotFound).
	if err := s.DeleteAnchor(ctx, memory.KindHuman, "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetAnchor(ctx, memory.KindHuman, "alice"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAnchor(ctx, memory.KindHuman, "alice"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestOutboxEnqueueAndPending(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	alice, t1, _ := threeAnchors(t, s)

	seq1, err := s.EnqueueEvent(ctx, Event{
		AggregateType: "entry", AggregateID: "e1", AnchorID: alice.RowID,
		EventType: "memory.entry.created", Payload: `{"id":"e1"}`,
	})
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	seq2, err := s.EnqueueEvent(ctx, Event{
		AggregateType: "entry", AggregateID: "e2", AnchorID: t1.RowID,
		EventType: "memory.entry.updated", Payload: `{"id":"e2"}`,
	})
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	seq3, err := s.EnqueueEvent(ctx, Event{
		AggregateType: "anchor", AggregateID: "alice", AnchorID: alice.RowID,
		EventType: "memory.anchor.upserted", Payload: `{}`,
	})
	if err != nil {
		t.Fatalf("enqueue 3: %v", err)
	}
	if seq1 != 1 || seq2 != 2 || seq3 != 3 {
		t.Fatalf("seq = %d,%d,%d, want 1,2,3", seq1, seq2, seq3)
	}

	all, err := s.PendingEvents(ctx, 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(all) != 3 || all[0].Seq != 1 || all[1].Seq != 2 || all[2].Seq != 3 {
		t.Fatalf("pending = %+v, want all three in seq order", all)
	}
	for _, e := range all {
		if e.PublishedAt != nil {
			t.Fatalf("pending event %d is published", e.Seq)
		}
	}

	// Publish the middle one (the publisher is W1; the store only
	// reports what is unpublished).
	if _, err := s.db.ExecContext(ctx,
		"UPDATE memory_outbox SET published_at = ? WHERE seq = ?",
		"2026-10-31T12:00:00Z", seq2); err != nil {
		t.Fatalf("publish seq 2: %v", err)
	}
	rest, err := s.PendingEvents(ctx, 0)
	if err != nil {
		t.Fatalf("pending after publish: %v", err)
	}
	if len(rest) != 2 || rest[0].Seq != 1 || rest[1].Seq != 3 {
		t.Fatalf("pending after publish = %+v, want seq 1 and 3", rest)
	}

	// limit caps the batch.
	first, err := s.PendingEvents(ctx, 1)
	if err != nil {
		t.Fatalf("pending limit 1: %v", err)
	}
	if len(first) != 1 || first[0].Seq != 1 {
		t.Fatalf("pending limit 1 = %+v, want [seq 1]", first)
	}
}

func TestTxAtomicity(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	// A failing handler rolls the whole transaction back.
	err := s.Tx(ctx, func(h TxHandle) error {
		if _, err := h.UpsertAnchor(ctx, Anchor{
			Anchor:      memory.Anchor{Kind: memory.KindAgent, ID: "a1"},
			BackendKind: "remelight-file",
		}); err != nil {
			return err
		}
		if _, err := h.EnqueueEvent(ctx, Event{
			AggregateType: "anchor", AggregateID: "a1", AnchorID: 1,
			EventType: "memory.anchor.upserted", Payload: `{}`,
		}); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("tx error = %v, want the handler error", err)
	}
	if _, err := s.GetAnchor(ctx, memory.KindAgent, "a1"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("anchor after rollback = %v, want ErrNotFound", err)
	}
	if ev, err := s.PendingEvents(ctx, 0); err != nil || len(ev) != 0 {
		t.Fatalf("events after rollback = %+v (err %v), want none", ev, err)
	}

	// A successful handler commits everything it wrote.
	_, _, a1 := threeAnchors(t, s)
	var gotSeq int64
	if err := s.Tx(ctx, func(h TxHandle) error {
		var err error
		gotSeq, err = h.EnqueueEvent(ctx, Event{
			AggregateType: "entry", AggregateID: "e9", AnchorID: a1.RowID,
			EventType: "memory.entry.created", Payload: `{}`,
		})
		return err
	}); err != nil {
		t.Fatalf("tx commit: %v", err)
	}
	ev, err := s.PendingEvents(ctx, 0)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(ev) != 1 || ev[0].Seq != gotSeq {
		t.Fatalf("pending = %+v, want the committed event", ev)
	}
}

func TestStorageRefs(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	r := StorageRef{
		ObjectKey: "exports/human:alice/2026-10-31.md",
		SHA256:    strings.Repeat("ab", 32),
		SizeBytes: 1234,
		OwnerType: "export",
		OwnerID:   "human:alice",
	}
	if err := s.UpsertStorageRef(ctx, r); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetStorageRef(ctx, r.ObjectKey, r.OwnerType, r.OwnerID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SHA256 != r.SHA256 || got.SizeBytes != 1234 || got.CreatedAt.IsZero() {
		t.Fatalf("get = %+v", got)
	}
	created := got.CreatedAt

	// A re-upsert of the same key updates the ref, keeps created_at.
	r.SizeBytes = 4321
	if err := s.UpsertStorageRef(ctx, r); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got2, err := s.GetStorageRef(ctx, r.ObjectKey, r.OwnerType, r.OwnerID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got2.SizeBytes != 4321 || !got2.CreatedAt.Equal(created) {
		t.Fatalf("re-upsert = %+v, want updated size and unchanged created_at", got2)
	}

	if err := s.DeleteStorageRef(ctx, r.ObjectKey, r.OwnerType, r.OwnerID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetStorageRef(ctx, r.ObjectKey, r.OwnerType, r.OwnerID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteStorageRef(ctx, r.ObjectKey, r.OwnerType, r.OwnerID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestKnowledgeScopeIsolation(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	alice, t1, a1 := threeAnchors(t, s)

	nodes := []KnowledgeNode{
		{AnchorID: alice.RowID, NodeID: "n1", Kind: "profile", Path: "/profile.md", Title: "alice profile", Version: 1},
		{AnchorID: alice.RowID, NodeID: "n2", Kind: "memory", Path: "/memory/2026-10-31.md", Version: 1},
		{AnchorID: t1.RowID, NodeID: "t1", Kind: "profile", Path: "/profile.md", Title: "t1 profile", Version: 1},
		{AnchorID: a1.RowID, NodeID: "a1", Kind: "digest", Path: "/digest/team.md", Version: 1},
	}
	for _, n := range nodes {
		if err := s.UpsertKnowledgeNode(ctx, n); err != nil {
			t.Fatalf("upsert node %s: %v", n.NodeID, err)
		}
	}
	edges := []KnowledgeEdge{
		{AnchorID: alice.RowID, SrcID: "n1", DstID: "n2", Kind: "wikilink"},
		{AnchorID: t1.RowID, SrcID: "t1", DstID: "t1", Kind: "reference"},
		{AnchorID: a1.RowID, SrcID: "a1", DstID: "a1", Kind: "source"},
	}
	for _, e := range edges {
		if err := s.UpsertKnowledgeEdge(ctx, e); err != nil {
			t.Fatalf("upsert edge %s->%s: %v", e.SrcID, e.DstID, err)
		}
	}
	// A duplicated edge upsert is a no-op (set semantics).
	if err := s.UpsertKnowledgeEdge(ctx, edges[0]); err != nil {
		t.Fatalf("duplicated edge: %v", err)
	}

	// A node re-upsert updates in place (no second row).
	if err := s.UpsertKnowledgeNode(ctx, KnowledgeNode{
		AnchorID: alice.RowID, NodeID: "n2", Kind: "memory",
		Path: "/memory/2026-10-31.md", Version: 2,
	}); err != nil {
		t.Fatalf("re-upsert n2: %v", err)
	}

	// alice sees only her two nodes, in node_id order, n2 at version 2.
	an, err := s.ListKnowledgeNodes(ctx, alice.RowID)
	if err != nil {
		t.Fatalf("list alice nodes: %v", err)
	}
	if len(an) != 2 || an[0].NodeID != "n1" || an[1].NodeID != "n2" {
		t.Fatalf("alice nodes = %+v", an)
	}
	if an[1].Version != 2 {
		t.Fatalf("n2 version = %d, want 2", an[1].Version)
	}

	// team:t1 sees only its own node - none of alice's or a1's.
	tn, err := s.ListKnowledgeNodes(ctx, t1.RowID)
	if err != nil {
		t.Fatalf("list t1 nodes: %v", err)
	}
	if len(tn) != 1 || tn[0].NodeID != "t1" {
		t.Fatalf("t1 nodes = %+v, want exactly [t1]", tn)
	}

	// agent:a1 sees only its own edge.
	ae, err := s.ListKnowledgeEdges(ctx, a1.RowID)
	if err != nil {
		t.Fatalf("list a1 edges: %v", err)
	}
	if len(ae) != 1 || ae[0].SrcID != "a1" || ae[0].DstID != "a1" || ae[0].Kind != "source" {
		t.Fatalf("a1 edges = %+v, want exactly [a1->a1 source]", ae)
	}

	// alice's edges: exactly the one wikilink (the duplicate stayed a
	// no-op).
	ae2, err := s.ListKnowledgeEdges(ctx, alice.RowID)
	if err != nil {
		t.Fatalf("list alice edges: %v", err)
	}
	if len(ae2) != 1 || ae2[0].SrcID != "n1" || ae2[0].DstID != "n2" || ae2[0].Kind != "wikilink" {
		t.Fatalf("alice edges = %+v, want exactly [n1->n2 wikilink]", ae2)
	}
}
