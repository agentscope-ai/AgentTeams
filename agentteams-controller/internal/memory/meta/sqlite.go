package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (same as internal/memory/sqlite)

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory/migrate"
)

// SQLite is the lite-profile (SQLite, memory-meta.db) implementation
// of the Store contract. The standard/scale profiles share the DDL
// (migrate) but need the SQL-family driver; that lands with W2/W5.
type SQLite struct {
	db *sql.DB
}

var (
	_ Store    = (*SQLite)(nil)
	_ TxHandle = (*txHandle)(nil)
)

// Open opens (creating if needed) the lite-profile meta store at path
// - the controller's memory-meta.db, WAL - and ensures the schema.
func Open(ctx context.Context, path string) (*SQLite, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &SQLite{db: db}
	if err := s.EnsureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database Open created.
func (s *SQLite) Close() error { return s.db.Close() }

// EnsureSchema implements Store: it applies the lite-profile
// migrations (idempotent).
func (s *SQLite) EnsureSchema(ctx context.Context) error {
	_, err := migrate.Migrate(ctx, s.db, migrate.ProfileLite)
	return err
}

// --- transactions ----------------------------------------------------

// txHandle is the write surface of one open meta transaction.
type txHandle struct {
	tx *sql.Tx
}

// Tx implements Store.Tx.
func (s *SQLite) Tx(ctx context.Context, fn func(h TxHandle) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&txHandle{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *txHandle) UpsertAnchor(ctx context.Context, a Anchor) (Anchor, error) {
	return upsertAnchor(ctx, h.tx, a)
}

func (h *txHandle) EnqueueEvent(ctx context.Context, e Event) (int64, error) {
	return enqueueEvent(ctx, h.tx, e)
}

func (h *txHandle) UpsertStorageRef(ctx context.Context, r StorageRef) error {
	return upsertStorageRef(ctx, h.tx, r)
}

func (h *txHandle) UpsertKnowledgeNode(ctx context.Context, n KnowledgeNode) error {
	return upsertKnowledgeNode(ctx, h.tx, n)
}

func (h *txHandle) UpsertKnowledgeEdge(ctx context.Context, e KnowledgeEdge) error {
	return upsertKnowledgeEdge(ctx, h.tx, e)
}

// executor is the database/sql surface shared by *sql.DB and *sql.Tx.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// --- anchor_* ----------------------------------------------------------

// UpsertAnchor implements Store.
func (s *SQLite) UpsertAnchor(ctx context.Context, a Anchor) (Anchor, error) {
	return upsertAnchor(ctx, s.db, a)
}

func upsertAnchor(ctx context.Context, q executor, a Anchor) (Anchor, error) {
	if !a.Anchor.Valid() {
		return Anchor{}, memory.ErrInvalidAnchor
	}
	if a.Capabilities == "" {
		a.Capabilities = "{}" // the column is NOT NULL
	}
	now := nowStamp()
	created := now
	if !a.CreatedAt.IsZero() {
		created = a.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO memory_anchors (owner_type, owner_id, backend_kind, backend_location, capabilities, backend_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_type, owner_id) DO UPDATE SET
			backend_kind = excluded.backend_kind,
			backend_location = excluded.backend_location,
			capabilities = excluded.capabilities,
			backend_version = excluded.backend_version,
			updated_at = excluded.updated_at`,
		string(a.Kind), a.ID, a.BackendKind, a.BackendLocation, a.Capabilities, a.BackendVersion, created, now); err != nil {
		return Anchor{}, fmt.Errorf("memory/meta: upsert anchor %s: %w", a.Anchor.String(), err)
	}
	return getAnchor(ctx, q, a.Kind, a.ID)
}

// GetAnchor implements Store.
func (s *SQLite) GetAnchor(ctx context.Context, kind memory.Kind, ownerID string) (Anchor, error) {
	return getAnchor(ctx, s.db, kind, ownerID)
}

func getAnchor(ctx context.Context, q executor, kind memory.Kind, ownerID string) (Anchor, error) {
	var (
		a         Anchor
		kindStr   string
		createdAt string
		updatedAt string
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, owner_type, owner_id, backend_kind, backend_location, capabilities, backend_version, created_at, updated_at
		FROM memory_anchors WHERE owner_type = ? AND owner_id = ?`,
		string(kind), ownerID).
		Scan(&a.RowID, &kindStr, &a.ID, &a.BackendKind, &a.BackendLocation, &a.Capabilities, &a.BackendVersion, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Anchor{}, memory.ErrNotFound
	}
	if err != nil {
		return Anchor{}, fmt.Errorf("memory/meta: get anchor: %w", err)
	}
	a.Kind = memory.Kind(kindStr)
	if a.CreatedAt, err = parseStamp(createdAt); err != nil {
		return Anchor{}, err
	}
	if a.UpdatedAt, err = parseStamp(updatedAt); err != nil {
		return Anchor{}, err
	}
	return a, nil
}

// ListAnchors implements Store.
func (s *SQLite) ListAnchors(ctx context.Context) ([]Anchor, error) {
	return listAnchors(ctx, s.db)
}

func listAnchors(ctx context.Context, q executor) ([]Anchor, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, owner_type, owner_id, backend_kind, backend_location, capabilities, backend_version, created_at, updated_at
		FROM memory_anchors ORDER BY owner_type, owner_id`)
	if err != nil {
		return nil, fmt.Errorf("memory/meta: list anchors: %w", err)
	}
	defer rows.Close()
	out := []Anchor{}
	for rows.Next() {
		var (
			a         Anchor
			kindStr   string
			createdAt string
			updatedAt string
		)
		if err := rows.Scan(&a.RowID, &kindStr, &a.ID, &a.BackendKind, &a.BackendLocation, &a.Capabilities, &a.BackendVersion, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("memory/meta: list anchors: %w", err)
		}
		a.Kind = memory.Kind(kindStr)
		if a.CreatedAt, err = parseStamp(createdAt); err != nil {
			return nil, err
		}
		if a.UpdatedAt, err = parseStamp(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAnchor implements Store.
func (s *SQLite) DeleteAnchor(ctx context.Context, kind memory.Kind, ownerID string) error {
	return deleteAnchor(ctx, s.db, kind, ownerID)
}

func deleteAnchor(ctx context.Context, q executor, kind memory.Kind, ownerID string) error {
	res, err := q.ExecContext(ctx,
		"DELETE FROM memory_anchors WHERE owner_type = ? AND owner_id = ?",
		string(kind), ownerID)
	if err != nil {
		return fmt.Errorf("memory/meta: delete anchor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// --- outbox_* ----------------------------------------------------------

// EnqueueEvent implements Store.
func (s *SQLite) EnqueueEvent(ctx context.Context, e Event) (int64, error) {
	return enqueueEvent(ctx, s.db, e)
}

func enqueueEvent(ctx context.Context, q executor, e Event) (int64, error) {
	created := nowStamp()
	if !e.CreatedAt.IsZero() {
		created = e.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO memory_outbox (aggregate_type, aggregate_id, anchor_id, event_type, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.AggregateType, e.AggregateID, e.AnchorID, e.EventType, e.Payload, created)
	if err != nil {
		return 0, fmt.Errorf("memory/meta: enqueue event: %w", err)
	}
	return res.LastInsertId()
}

const eventCols = `seq, aggregate_type, aggregate_id, anchor_id, event_type, payload, created_at, published_at, attempts, last_error`

// PendingEvents implements Store.
func (s *SQLite) PendingEvents(ctx context.Context, limit int) ([]Event, error) {
	return pendingEvents(ctx, s.db, limit)
}

func pendingEvents(ctx context.Context, q executor, limit int) ([]Event, error) {
	query := "SELECT " + eventCols + " FROM memory_outbox WHERE published_at IS NULL ORDER BY seq ASC"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("memory/meta: pending events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e           Event
			createdAt   string
			publishedAt *string
			lastError   *string
		)
		if err := rows.Scan(&e.Seq, &e.AggregateType, &e.AggregateID, &e.AnchorID, &e.EventType,
			&e.Payload, &createdAt, &publishedAt, &e.Attempts, &lastError); err != nil {
			return nil, fmt.Errorf("memory/meta: pending events: %w", err)
		}
		if e.CreatedAt, err = parseStamp(createdAt); err != nil {
			return nil, err
		}
		if publishedAt != nil {
			t, perr := parseStamp(*publishedAt)
			if perr != nil {
				return nil, perr
			}
			e.PublishedAt = &t
		}
		if lastError != nil {
			e.LastError = *lastError
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// --- storage_ref_* ------------------------------------------------------

// UpsertStorageRef implements Store.
func (s *SQLite) UpsertStorageRef(ctx context.Context, r StorageRef) error {
	return upsertStorageRef(ctx, s.db, r)
}

func upsertStorageRef(ctx context.Context, q executor, r StorageRef) error {
	now := nowStamp()
	if !r.CreatedAt.IsZero() {
		now = r.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO storage_refs (object_key, sha256, size_bytes, owner_type, owner_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (object_key, owner_type, owner_id) DO UPDATE SET
			sha256 = excluded.sha256,
			size_bytes = excluded.size_bytes`,
		r.ObjectKey, r.SHA256, r.SizeBytes, r.OwnerType, r.OwnerID, now); err != nil {
		return fmt.Errorf("memory/meta: upsert storage ref: %w", err)
	}
	return nil
}

// GetStorageRef implements Store.
func (s *SQLite) GetStorageRef(ctx context.Context, objectKey, ownerType, ownerID string) (StorageRef, error) {
	return getStorageRef(ctx, s.db, objectKey, ownerType, ownerID)
}

func getStorageRef(ctx context.Context, q executor, objectKey, ownerType, ownerID string) (StorageRef, error) {
	var (
		r         StorageRef
		createdAt string
	)
	err := q.QueryRowContext(ctx, `
		SELECT object_key, sha256, size_bytes, owner_type, owner_id, created_at
		FROM storage_refs WHERE object_key = ? AND owner_type = ? AND owner_id = ?`,
		objectKey, ownerType, ownerID).
		Scan(&r.ObjectKey, &r.SHA256, &r.SizeBytes, &r.OwnerType, &r.OwnerID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StorageRef{}, memory.ErrNotFound
	}
	if err != nil {
		return StorageRef{}, fmt.Errorf("memory/meta: get storage ref: %w", err)
	}
	if r.CreatedAt, err = parseStamp(createdAt); err != nil {
		return StorageRef{}, err
	}
	return r, nil
}

// DeleteStorageRef implements Store.
func (s *SQLite) DeleteStorageRef(ctx context.Context, objectKey, ownerType, ownerID string) error {
	return deleteStorageRef(ctx, s.db, objectKey, ownerType, ownerID)
}

func deleteStorageRef(ctx context.Context, q executor, objectKey, ownerType, ownerID string) error {
	res, err := q.ExecContext(ctx,
		"DELETE FROM storage_refs WHERE object_key = ? AND owner_type = ? AND owner_id = ?",
		objectKey, ownerType, ownerID)
	if err != nil {
		return fmt.Errorf("memory/meta: delete storage ref: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// --- knowledge_* (anchor-local) ------------------------------------------

// UpsertKnowledgeNode implements Store.
func (s *SQLite) UpsertKnowledgeNode(ctx context.Context, n KnowledgeNode) error {
	return upsertKnowledgeNode(ctx, s.db, n)
}

func upsertKnowledgeNode(ctx context.Context, q executor, n KnowledgeNode) error {
	now := nowStamp()
	if !n.UpdatedAt.IsZero() {
		now = n.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO knowledge_nodes (anchor_id, node_id, kind, path, title, version, meta, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (anchor_id, node_id) DO UPDATE SET
			kind = excluded.kind,
			path = excluded.path,
			title = excluded.title,
			version = excluded.version,
			meta = excluded.meta,
			updated_at = excluded.updated_at`,
		n.AnchorID, n.NodeID, n.Kind, n.Path, n.Title, n.Version, n.Meta, now); err != nil {
		return fmt.Errorf("memory/meta: upsert knowledge node: %w", err)
	}
	return nil
}

// UpsertKnowledgeEdge implements Store. Set semantics: a duplicated
// (anchor, src, dst, kind) tuple is a no-op.
func (s *SQLite) UpsertKnowledgeEdge(ctx context.Context, e KnowledgeEdge) error {
	return upsertKnowledgeEdge(ctx, s.db, e)
}

func upsertKnowledgeEdge(ctx context.Context, q executor, e KnowledgeEdge) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO knowledge_edges (anchor_id, src_id, dst_id, kind)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (anchor_id, src_id, dst_id, kind) DO NOTHING`,
		e.AnchorID, e.SrcID, e.DstID, e.Kind); err != nil {
		return fmt.Errorf("memory/meta: upsert knowledge edge: %w", err)
	}
	return nil
}

// ListKnowledgeNodes implements Store.
func (s *SQLite) ListKnowledgeNodes(ctx context.Context, anchorID int64) ([]KnowledgeNode, error) {
	return listKnowledgeNodes(ctx, s.db, anchorID)
}

func listKnowledgeNodes(ctx context.Context, q executor, anchorID int64) ([]KnowledgeNode, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT anchor_id, node_id, kind, path, title, version, meta, updated_at
		FROM knowledge_nodes WHERE anchor_id = ? ORDER BY node_id`, anchorID)
	if err != nil {
		return nil, fmt.Errorf("memory/meta: list knowledge nodes: %w", err)
	}
	defer rows.Close()
	out := []KnowledgeNode{}
	for rows.Next() {
		var (
			n         KnowledgeNode
			updatedAt string
		)
		if err := rows.Scan(&n.AnchorID, &n.NodeID, &n.Kind, &n.Path, &n.Title, &n.Version, &n.Meta, &updatedAt); err != nil {
			return nil, fmt.Errorf("memory/meta: list knowledge nodes: %w", err)
		}
		if n.UpdatedAt, err = parseStamp(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListKnowledgeEdges implements Store.
func (s *SQLite) ListKnowledgeEdges(ctx context.Context, anchorID int64) ([]KnowledgeEdge, error) {
	return listKnowledgeEdges(ctx, s.db, anchorID)
}

func listKnowledgeEdges(ctx context.Context, q executor, anchorID int64) ([]KnowledgeEdge, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT anchor_id, src_id, dst_id, kind
		FROM knowledge_edges WHERE anchor_id = ? ORDER BY src_id, dst_id, kind`, anchorID)
	if err != nil {
		return nil, fmt.Errorf("memory/meta: list knowledge edges: %w", err)
	}
	defer rows.Close()
	out := []KnowledgeEdge{}
	for rows.Next() {
		var e KnowledgeEdge
		if err := rows.Scan(&e.AnchorID, &e.SrcID, &e.DstID, &e.Kind); err != nil {
			return nil, fmt.Errorf("memory/meta: list knowledge edges: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// --- helpers ---------------------------------------------------------------

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseStamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory/meta: parse timestamp %q: %w", s, err)
	}
	return t, nil
}
