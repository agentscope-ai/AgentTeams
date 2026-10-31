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
// Semantics the contract suite (internal/memory/contract) pins down:
//
//   - a CAS mismatch returns *ErrConflict carrying the Current entry
//     (the winner's state); the caller shows that to the user and
//     rewrites against it.
//   - operations on an unknown or forgotten entry return ErrNotFound.
//     Forgotten entries do not exist for Replace or Search; only
//     Restore can see them.
//   - Search and Export are scope-local: they see only live entries
//     of q.Anchor / anchor, never another anchor's entries, never
//     forgotten ones.
//   - Create honours NewEntry.IdempotencyKey: the same key with the
//     same payload (anchor + content_type + text) returns the first
//     created entry and creates nothing further; the same key with a
//     different payload is a conflict; a key whose entry was forgotten
//     is unknown.
//   - Entry IDs are globally unique; scope isolation on mutations is
//     not a store concern (the HTTP layer keys by the ID in the URL),
//     while Search/Export isolation is, by construction.
//
// Implementations: internal/memory/sqlite (file, unit level) and the
// PowerContext adapters in internal/memory/pc. Authorization of
// *who* may touch an anchor is the gateway's job (W1), not the
// store's.
type Store interface {
	// Create stores a new entry under in.Anchor and returns it at
	// revision 1.
	Create(ctx context.Context, in NewEntry) (Entry, error)

	// Replace applies patch to the entry with id, requiring
	// ifRevision == current revision. Returns the updated entry at
	// revision ifRevision+1.
	Replace(ctx context.Context, id string, ifRevision uint64, patch Patch) (Entry, error)

	// Forget forgets the entry with id, requiring ifRevision ==
	// current revision. The entry becomes a restorable tombstone at
	// revision ifRevision+1.
	Forget(ctx context.Context, id string, ifRevision uint64) error

	// Restore revives a forgotten entry, requiring ifRevision == the
	// tombstone revision (i.e. the revision the forget left it at).
	// Returns the live entry at revision ifRevision+1. Restoring a
	// live (not forgotten) entry is ErrNotFound.
	Restore(ctx context.Context, id string, ifRevision uint64) (Entry, error)

	// Search returns the live entries of q.Anchor whose text
	// matches q.Query (case-insensitive substring; empty query
	// matches all), ordered by creation time then ID (ascending).
	Search(ctx context.Context, q SearchQuery) ([]Entry, error)

	// Export returns all live entries of anchor, ordered by
	// creation time then ID (ascending).
	Export(ctx context.Context, anchor Anchor) ([]Entry, error)
}
