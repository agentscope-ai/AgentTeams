// Package sqlite is the file-backed (lite-profile) implementation of
// memory.Store. Unit-test level: single process, temp file, WAL.
package sqlite

import (
	"context"
	"database/sql"

	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (same as internal/server)
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/memory"
	"github.com/google/uuid"
)

// schemaStatements creates the store's tables; each statement is
// executed separately so no driver multi-statement behaviour is
// relied upon.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS entries (
		id TEXT PRIMARY KEY,
		anchor_kind TEXT NOT NULL,
		anchor_id TEXT NOT NULL,
		content_type TEXT NOT NULL,
		text TEXT NOT NULL,
		revision INTEGER NOT NULL,
		etag TEXT NOT NULL,
		provenance TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		deleted_at TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS entries_anchor_idx ON entries (anchor_kind, anchor_id, deleted_at)`,
	`CREATE TABLE IF NOT EXISTS idempotency (
		key TEXT PRIMARY KEY,
		entry_id TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
}

const entryCols = `id, anchor_kind, anchor_id, content_type, text, revision, etag, provenance, created_at, updated_at, deleted_at`

// errIsBusy reports whether the error is a SQLite busy condition
// (SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT, worth a bounded retry). The
// driver renders them under several spellings, so match the known
// ones.
func errIsBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked") ||
		strings.Contains(s, "SQLITE_BUSY")
}

// Store is a file-backed memory.Store over one SQLite database.
type Store struct {
	db *sql.DB
}

var _ memory.Store = (*Store)(nil)

// Open opens (creating if needed) the SQLite store at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("memory/sqlite: create schema: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// --- row plumbing ---------------------------------------------------

type rowData struct {
	ID, AnchorKind, AnchorID, ContentType, Text string
	Revision                                    int64
	ETag, Provenance, CreatedAt, UpdatedAt      *string
	DeletedAt                                   *string
}

func (r *rowData) scan(scan func(dest ...any) error) error {
	return scan(&r.ID, &r.AnchorKind, &r.AnchorID, &r.ContentType, &r.Text,
		&r.Revision, &r.ETag, &r.Provenance, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (r *rowData) Deleted() bool { return r.DeletedAt != nil }

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func parseRow(r *rowData) (memory.Entry, error) {
	createdAt, err := time.Parse(time.RFC3339Nano, *r.CreatedAt)
	if err != nil {
		return memory.Entry{}, fmt.Errorf("memory/sqlite: parse created_at: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, *r.UpdatedAt)
	if err != nil {
		return memory.Entry{}, fmt.Errorf("memory/sqlite: parse updated_at: %w", err)
	}
	var p memory.Provenance
	if r.Provenance != nil && *r.Provenance != "" && *r.Provenance != "{}" {
		if err := json.Unmarshal([]byte(*r.Provenance), &p); err != nil {
			return memory.Entry{}, fmt.Errorf("memory/sqlite: parse provenance: %w", err)
		}
	}
	if r.DeletedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *r.DeletedAt); err != nil {
			return memory.Entry{}, fmt.Errorf("memory/sqlite: parse deleted_at: %w", err)
		}
	}
	return memory.Entry{
		ID:          r.ID,
		Anchor:      memory.Anchor{Kind: memory.Kind(r.AnchorKind), ID: r.AnchorID},
		ContentType: r.ContentType,
		Text:        r.Text,
		Revision:    uint64(r.Revision),
		ETag:        derefString(r.ETag),
		Provenance:  p,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

// loadRow fetches one row; reports exists/deleted. The anchor is part
// of the key: a row that exists under a different anchor is reported
// as not exists.
func (s *Store) loadRow(ctx context.Context, q rowQuerier, anchor memory.Anchor, id string) (memory.Entry, bool, bool, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT "+entryCols+" FROM entries WHERE id = ? AND anchor_kind = ? AND anchor_id = ?",
		id, string(anchor.Kind), anchor.ID)
	if err != nil {
		return memory.Entry{}, false, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return memory.Entry{}, false, false, err
		}
		return memory.Entry{}, false, false, nil
	}
	var r rowData
	if err := r.scan(rows.Scan); err != nil {
		return memory.Entry{}, false, false, err
	}
	entry, err := parseRow(&r)
	if err != nil {
		return memory.Entry{}, false, false, err
	}
	return entry, true, r.Deleted(), nil
}

func newProvenanceJSON(p memory.Provenance) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "{}", nil
	}
	return string(b), nil
}

// --- memory.Store ---------------------------------------------------

// Create implements memory.Store.
func (s *Store) Create(ctx context.Context, in memory.NewEntry) (memory.Entry, error) {
	if !in.Anchor.Valid() {
		return memory.Entry{}, memory.ErrInvalidAnchor
	}
	id := uuid.NewString()
	provJSON, err := newProvenanceJSON(in.Provenance)
	if err != nil {
		return memory.Entry{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	var entry memory.Entry
	for attempt := 0; ; attempt++ {
		entry, err = s.createOnce(ctx, in, id, provJSON, now)
		if err == nil || !errIsBusy(err) || attempt >= 20 {
			return entry, err
		}
	}
}

func (s *Store) createOnce(ctx context.Context, in memory.NewEntry, id, provJSON, now string) (memory.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Entry{}, err
	}
	defer tx.Rollback()

	if in.IdempotencyKey.IsSet() {
		var entryID, payloadHash string
		err := tx.QueryRowContext(ctx,
			"SELECT entry_id, payload_hash FROM idempotency WHERE key = ?",
			in.ScopedKey()).Scan(&entryID, &payloadHash)
		if err == nil {
			entry, exists, deleted, lerr := s.loadRow(ctx, tx, in.Anchor, entryID)
			if lerr != nil {
				return memory.Entry{}, lerr
			}
			switch {
			case !exists || deleted:
				return memory.Entry{}, memory.ErrNotFound
			case memory.PayloadHash(in) != payloadHash:
				return memory.Entry{}, &memory.ErrConflict{Current: entry}
			default:
				return entry, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return memory.Entry{}, err
		}
	}

	rev := uint64(1)
	etag := memory.ETagForRevision(rev)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entries (id, anchor_kind, anchor_id, content_type, text, revision, etag, provenance, created_at, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		id, string(in.Anchor.Kind), in.Anchor.ID, in.ContentType, in.Text, rev, etag, provJSON, now, now); err != nil {
		return memory.Entry{}, err
	}
	if in.IdempotencyKey.IsSet() {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency (key, entry_id, payload_hash, created_at) VALUES (?, ?, ?, ?)`,
			in.ScopedKey(), id, memory.PayloadHash(in), now); err != nil {
			// A concurrent create with the same key won the race;
			// tx rolls back, the winner's row is committed.
			return memory.Entry{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return memory.Entry{}, err
	}
	// Re-read after commit (the in-transaction row view is not valid
	// past Commit on this driver).
	entry, exists, deleted, err := s.loadRow(ctx, s.db, in.Anchor, id)
	if err != nil {
		return memory.Entry{}, err
	}
	if !exists || deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	return entry, nil
}

// Replace implements memory.Store. The anchor is bound inside the
// same read and CAS that touch the row: an id that exists under a
// different anchor yields ErrNotFound, never a conflict (there is no
// current to carry).
func (s *Store) Replace(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64, patch memory.Patch) (memory.Entry, error) {
	for attempt := 0; ; attempt++ {
		entry, err := s.replaceOnce(ctx, anchor, id, ifRevision, patch)
		if err == nil || !errIsBusy(err) || attempt >= 20 {
			return entry, err
		}
	}
}

func (s *Store) replaceOnce(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64, patch memory.Patch) (memory.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Entry{}, err
	}
	defer tx.Rollback()

	current, exists, deleted, err := s.loadRow(ctx, tx, anchor, id)
	if err != nil {
		return memory.Entry{}, err
	}
	if !exists || deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	if current.Revision != ifRevision {
		return memory.Entry{}, &memory.ErrConflict{Current: current}
	}

	updated := current
	if patch.ContentType != nil {
		updated.ContentType = *patch.ContentType
	}
	if patch.Text != nil {
		updated.Text = *patch.Text
	}
	if patch.Provenance != nil {
		updated.Provenance = *patch.Provenance
	}
	provJSON, err := newProvenanceJSON(updated.Provenance)
	if err != nil {
		return memory.Entry{}, err
	}
	newRev := ifRevision + 1
	now := time.Now().UTC().Format(time.RFC3339Nano)

	res, err := tx.ExecContext(ctx, `
		UPDATE entries SET content_type = ?, text = ?, provenance = ?, revision = ?, etag = ?, updated_at = ?
		WHERE id = ? AND anchor_kind = ? AND anchor_id = ? AND deleted_at IS NULL AND revision = ?`,
		updated.ContentType, updated.Text, provJSON, newRev, memory.ETagForRevision(newRev), now,
		id, string(anchor.Kind), anchor.ID, ifRevision)
	if err != nil {
		return memory.Entry{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Lost the CAS race inside the transaction; re-read and
		// report the winner.
		entry, exists, deleted, lerr := s.loadRow(ctx, tx, anchor, id)
		if lerr != nil {
			return memory.Entry{}, lerr
		}
		if !exists || deleted {
			return memory.Entry{}, memory.ErrNotFound
		}
		return memory.Entry{}, &memory.ErrConflict{Current: entry}
	}
	if err := tx.Commit(); err != nil {
		return memory.Entry{}, err
	}
	entry, exists, deleted, err := s.loadRow(ctx, s.db, anchor, id)
	if err != nil {
		return memory.Entry{}, err
	}
	if !exists || deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	return entry, nil
}

// Forget implements memory.Store. The anchor is bound inside the same
// read and CAS as Replace: a foreign anchor is ErrNotFound and the
// entry is left untouched.
func (s *Store) Forget(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) error {
	for attempt := 0; ; attempt++ {
		err := s.forgetOnce(ctx, anchor, id, ifRevision)
		if err == nil || !errIsBusy(err) || attempt >= 20 {
			return err
		}
	}
}

func (s *Store) forgetOnce(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	current, exists, deleted, err := s.loadRow(ctx, tx, anchor, id)
	if err != nil {
		return err
	}
	if !exists || deleted {
		return memory.ErrNotFound
	}
	if current.Revision != ifRevision {
		return &memory.ErrConflict{Current: current}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newRev := ifRevision + 1
	res, err := tx.ExecContext(ctx, `
		UPDATE entries SET revision = ?, etag = ?, updated_at = ?, deleted_at = ?
		WHERE id = ? AND anchor_kind = ? AND anchor_id = ? AND deleted_at IS NULL AND revision = ?`,
		newRev, memory.ETagForRevision(newRev), now, now,
		id, string(anchor.Kind), anchor.ID, ifRevision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		entry, exists, deleted, lerr := s.loadRow(ctx, tx, anchor, id)
		if lerr != nil {
			return lerr
		}
		if !exists || deleted {
			return memory.ErrNotFound
		}
		return &memory.ErrConflict{Current: entry}
	}
	return tx.Commit()
}

// Restore implements memory.Store. The anchor is bound inside the same
// read and CAS as Replace: a foreign anchor is ErrNotFound and the
// tombstone is left forgotten.
func (s *Store) Restore(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) (memory.Entry, error) {
	for attempt := 0; ; attempt++ {
		entry, err := s.restoreOnce(ctx, anchor, id, ifRevision)
		if err == nil || !errIsBusy(err) || attempt >= 20 {
			return entry, err
		}
	}
}

func (s *Store) restoreOnce(ctx context.Context, anchor memory.Anchor, id string, ifRevision uint64) (memory.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Entry{}, err
	}
	defer tx.Rollback()

	current, exists, deleted, err := s.loadRow(ctx, tx, anchor, id)
	if err != nil {
		return memory.Entry{}, err
	}
	if !exists || !deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	if current.Revision != ifRevision {
		return memory.Entry{}, &memory.ErrConflict{Current: current}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newRev := ifRevision + 1
	res, err := tx.ExecContext(ctx, `
		UPDATE entries SET revision = ?, etag = ?, updated_at = ?, deleted_at = NULL
		WHERE id = ? AND anchor_kind = ? AND anchor_id = ? AND deleted_at IS NOT NULL AND revision = ?`,
		newRev, memory.ETagForRevision(newRev), now,
		id, string(anchor.Kind), anchor.ID, ifRevision)
	if err != nil {
		return memory.Entry{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		entry, exists, deleted, lerr := s.loadRow(ctx, tx, anchor, id)
		if lerr != nil {
			return memory.Entry{}, lerr
		}
		if !exists || deleted {
			return memory.Entry{}, memory.ErrNotFound
		}
		return memory.Entry{}, &memory.ErrConflict{Current: entry}
	}
	if err := tx.Commit(); err != nil {
		return memory.Entry{}, err
	}
	entry, exists, deleted, err := s.loadRow(ctx, s.db, anchor, id)
	if err != nil {
		return memory.Entry{}, err
	}
	if !exists || deleted {
		return memory.Entry{}, memory.ErrNotFound
	}
	return entry, nil
}

// --- reads ----------------------------------------------------------

// anchorList returns live entries of anchor matching query ("" =
// all), ordered by creation time then id.
func (s *Store) anchorList(ctx context.Context, anchor memory.Anchor, query string) ([]memory.Entry, error) {
	if !anchor.Valid() {
		return nil, memory.ErrInvalidAnchor
	}
	q := `SELECT ` + entryCols + ` FROM entries
		WHERE anchor_kind = ? AND anchor_id = ? AND deleted_at IS NULL`
	args := []any{string(anchor.Kind), anchor.ID}
	if query != "" {
		q += ` AND lower(text) LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(query)+"%")
	}
	q += ` ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []memory.Entry{}
	for rows.Next() {
		var r rowData
		if err := r.scan(rows.Scan); err != nil {
			return nil, err
		}
		entry, err := parseRow(&r)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// escapeLike escapes LIKE wildcards for a quoted-ESCAPE pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search implements memory.Store.
func (s *Store) Search(ctx context.Context, q memory.SearchQuery) ([]memory.Entry, error) {
	return s.anchorList(ctx, q.Anchor, q.Query)
}

// Export implements memory.Store.
func (s *Store) Export(ctx context.Context, anchor memory.Anchor) ([]memory.Entry, error) {
	return s.anchorList(ctx, anchor, "")
}
