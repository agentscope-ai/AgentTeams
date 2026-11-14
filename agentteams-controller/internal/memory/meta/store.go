// Package meta is the MemoryMetaStore contract (#1412 W0b,
// implementation plan 3.1): the schema-owning side of the memory meta
// database - the anchor registry, the outbox, the object-storage
// refs and the derived knowledge tables.
//
// W0b fixes the interface and ships the lite-profile (SQLite,
// memory-meta.db) implementation with minimal CRUD only: no index
// building, no outbox publishing, no GC. The standard/scale profiles
// share the same DDL (migrate) but need the SQL-family driver that
// lands with W2/W5.
//
// Scope rule: the knowledge_* reads are anchor-local; a caller never
// observes another anchor's nodes or edges.
package meta

import (
	"context"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
)

// Anchor is one memory_anchors row: the logical anchor plus the
// backend pointer the controller maintains for it. RowID is the
// registry row id (memory_anchors.id); Kind and ID come from the
// embedded memory.Anchor. Entry content never lives here - only
// where it lives.
type Anchor struct {
	memory.Anchor

	RowID           int64
	BackendKind     string // powercontext | remelight-file | adbpg
	BackendLocation string // PC scope / agent workspace / ADBPG namespace
	Capabilities    string // JSON object declared by the backend
	BackendVersion  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Event is one memory_outbox row: a domain fact that outlives the
// write that produced it. Payload carries ids, versions and summaries
// only - never full text, never credentials.
type Event struct {
	Seq           int64
	AggregateType string // entry | anchor | promotion | export
	AggregateID   string
	AnchorID      int64 // memory_anchors.id
	EventType     string // memory.entry.created | updated | deleted | promoted | ...
	Payload       string // JSON
	CreatedAt     time.Time
	PublishedAt   *time.Time // nil = pending
	Attempts      int
	LastError     string
}

// StorageRef is one storage_refs row: the single reference source for
// object-store GC (a blob is deleted only at refcount 0, W1).
type StorageRef struct {
	ObjectKey string
	SHA256    string
	SizeBytes int64
	OwnerType string // memory | knowledge | export | backup
	OwnerID   string
	CreatedAt time.Time
}

// KnowledgeNode is one knowledge_nodes row (derived index, not a
// source of truth).
type KnowledgeNode struct {
	AnchorID  int64
	NodeID    string
	Kind      string // profile|memory|source|digest|daily|handoff|experience|skill
	Path      string
	Title     string
	Version   int64 // 0 = none
	Meta      string // JSON, "" = none
	UpdatedAt time.Time
}

// KnowledgeEdge is one knowledge_edges row (derived index).
type KnowledgeEdge struct {
	AnchorID int64
	SrcID    string
	DstID    string
	Kind     string // wikilink|source|promotion|reference
}

// TxHandle is one open meta transaction: the write surface Store.Tx
// handlers get. The W1 rule "every mutation appends its outbox event
// in the same transaction" is expressible exactly through this
// handle.
type TxHandle interface {
	UpsertAnchor(ctx context.Context, a Anchor) (Anchor, error)
	EnqueueEvent(ctx context.Context, e Event) (int64, error)
	UpsertStorageRef(ctx context.Context, r StorageRef) error
	UpsertKnowledgeNode(ctx context.Context, n KnowledgeNode) error
	UpsertKnowledgeEdge(ctx context.Context, e KnowledgeEdge) error
}

// Store is the MemoryMetaStore contract: ensure_schema / transaction /
// anchor_* / outbox_* / storage_ref_* / knowledge_*.
//
// The plain (non-Tx) methods each run in their own transaction; Tx is
// for writes that must commit together (a row plus its outbox event).
type Store interface {
	// EnsureSchema applies the profile migrations (idempotent).
	EnsureSchema(ctx context.Context) error

	// Tx runs fn in one meta transaction; an error from fn rolls it
	// back.
	Tx(ctx context.Context, fn func(h TxHandle) error) error

	// anchor_*
	UpsertAnchor(ctx context.Context, a Anchor) (Anchor, error)
	GetAnchor(ctx context.Context, kind memory.Kind, ownerID string) (Anchor, error)
	ListAnchors(ctx context.Context) ([]Anchor, error)
	DeleteAnchor(ctx context.Context, kind memory.Kind, ownerID string) error

	// outbox_*
	// EnqueueEvent appends one event and returns its seq.
	EnqueueEvent(ctx context.Context, e Event) (int64, error)
	// PendingEvents returns the unpublished events in seq order;
	// limit 0 means no limit.
	PendingEvents(ctx context.Context, limit int) ([]Event, error)

	// storage_ref_*
	UpsertStorageRef(ctx context.Context, r StorageRef) error
	GetStorageRef(ctx context.Context, objectKey, ownerType, ownerID string) (StorageRef, error)
	DeleteStorageRef(ctx context.Context, objectKey, ownerType, ownerID string) error

	// knowledge_* (anchor-local)
	UpsertKnowledgeNode(ctx context.Context, n KnowledgeNode) error
	UpsertKnowledgeEdge(ctx context.Context, e KnowledgeEdge) error
	ListKnowledgeNodes(ctx context.Context, anchorID int64) ([]KnowledgeNode, error)
	ListKnowledgeEdges(ctx context.Context, anchorID int64) ([]KnowledgeEdge, error)
}
