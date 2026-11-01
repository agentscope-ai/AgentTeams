package memory

import "context"

// Store is the backend-agnostic memory store contract.
//
// Every mutating method is compare-and-swap: ifRevision is the
// revision the caller last observed, and the write only proceeds when
// the entry's current revision equals it. A successful write advances
// the revision by exactly one — including Forget and Restore, which
// leave the entry at revision ifRevision+1 (deleted or live
// respectively). This is what makes a forgotten entry restorable with
// If-Match = the revision the forget returned.
//
// Anchoring: an entry is identified by (anchor, id), and every
// mutating method takes both. The anchor is part of the access
// boundary, not a filter: an id that exists under a different anchor
// is indistinguishable from a nonexistent id — the operation returns
// ErrNotFound and no content of the other anchor's entry is produced
// by any path, including CAS conflicts (a conflict's Current is always
// the requested anchor's entry). The binding must live inside the
// same query/CAS that touches the row (no read-then-check: that is a
// TOCTOU). Authorizing *who* may touch an anchor is a separate layer —
// the gateway's job (W1) — and never replaces this binding.
//
// Semantics the contract suite (internal/memory/contract) pins down:
//
//   - a CAS mismatch returns *ErrConflict carrying the Current entry
//     (the winner's state); the caller shows that to the user and
//     rewrites against it.
//   - operations on an unknown or forgotten entry return ErrNotFound,
//     as do operations whose anchor does not match the entry's.
//     Forgotten entries do not exist for Replace or Search; only
//     Restore can see them.
//   - Search and Export are scope-local: they see only live entries
//     of q.Anchor / anchor, never another anchor's entries, never
//     forgotten ones.
//   - Create honours NewEntry.IdempotencyKey, scoped to the anchor
//     (NewEntry.ScopedKey): the same key under a different anchor is
//     a fresh create, not a replay or a conflict. Under one anchor:
//     the same key with the same payload (content_type + text)
//     returns the first created entry and creates nothing further;
//     the same key with a different payload is a conflict; a key
//     whose entry was forgotten is unknown.
//   - Entry IDs are globally unique.
//
// Implementations: internal/memory/sqlite (file, unit level) and the
// PowerContext adapters in internal/memory/pc.
type Store interface {
	// Create stores a new entry under in.Anchor and returns it at
	// revision 1.
	Create(ctx context.Context, in NewEntry) (Entry, error)

	// Replace applies patch to the entry named by (anchor, id),
	// requiring ifRevision == current revision. Returns the updated
	// entry at revision ifRevision+1. An id that exists under a
	// different anchor is ErrNotFound.
	Replace(ctx context.Context, anchor Anchor, id string, ifRevision uint64, patch Patch) (Entry, error)

	// Forget forgets the entry named by (anchor, id), requiring
	// ifRevision == current revision. The entry becomes a restorable
	// tombstone at revision ifRevision+1. An id that exists under a
	// different anchor is ErrNotFound and the entry is left
	// untouched.
	Forget(ctx context.Context, anchor Anchor, id string, ifRevision uint64) error

	// Restore revives a forgotten entry named by (anchor, id),
	// requiring ifRevision == the tombstone revision (i.e. the
	// revision the forget left it at). Returns the live entry at
	// revision ifRevision+1. Restoring a live (not forgotten) entry,
	// or an id that exists only under a different anchor, is
	// ErrNotFound.
	Restore(ctx context.Context, anchor Anchor, id string, ifRevision uint64) (Entry, error)

	// Search returns the live entries of q.Anchor whose text
	// matches q.Query (case-insensitive substring; empty query
	// matches all), ordered by creation time then ID (ascending).
	Search(ctx context.Context, q SearchQuery) ([]Entry, error)

	// Export returns all live entries of anchor, ordered by
	// creation time then ID (ascending).
	Export(ctx context.Context, anchor Anchor) ([]Entry, error)
}
