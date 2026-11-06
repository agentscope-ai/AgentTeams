package runtimeconfig

import (
	"context"
	"time"
)

// Applied-state vocabulary (design §4.3) shared by every runtime's config
// manager. Reconcile is fail-closed: a write is "applied" only when the
// runtime proves it picked the config up, and a reconcile that cannot be
// completed is "pending" — never "applied". Runtimes without an applied
// hash (hermes) use the weak read-back form: on-disk bytes match what was
// written -> "applied", otherwise "pending".
const (
	StateApplied            = "applied"
	StatePending            = "pending"
	StateUnapplied          = "unapplied"
	StateAppliedButInactive = "applied-but-inactive"
)

// UnsupportedKey is a submitted key the write skipped, with the reason.
type UnsupportedKey struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ConfigState is the GET /config projection.
type ConfigState struct {
	Runtime string         `json:"runtime"`
	Rev     string         `json:"rev"` // sha256 hex of the current on-disk bytes (optimistic-lock token)
	Values  map[string]any `json:"values"`
	// Applied is the reconcile verdict: applied | pending | unapplied |
	// applied-but-inactive (State* constants).
	Applied string `json:"applied"`
	// PendingRestart lists the applies=restart keys of the last
	// controller write. The frontend shows the "restart required" banner
	// from it; the controller never restarts the worker itself.
	PendingRestart []string `json:"pendingRestart"`
	// Inactive lists written channel ids whose channel plugin is not
	// loaded (or has no plugin at all): the config is applied to the file
	// but inert at runtime — the "applied-but-inactive" state (§8.4).
	// Runtimes without a plugin model (hermes) never set it.
	Inactive []string `json:"inactive,omitempty"`
}

// WriteResult is the PUT/rollback response.
type WriteResult struct {
	Runtime string `json:"runtime"`
	Rev     string `json:"rev"` // sha256 hex of the new on-disk bytes
	// Applied is the post-write reconcile verdict (State* constants).
	Applied        string           `json:"applied"`
	PendingRestart []string         `json:"pendingRestart"`
	Inactive       []string         `json:"inactive,omitempty"`
	Unsupported    []UnsupportedKey `json:"unsupported,omitempty"`
	// Snapshot is the storage key of the pre-write snapshot:
	// rollback = POST .../config/rollback?rev=<pre-write rev>.
	Snapshot string `json:"snapshot"`
}

// ExecBackend is the container exec capability the write path needs
// (backend.WorkerExecBackend; embedded docker mode only). It lives here —
// not in any runtime package — because every runtime's config manager
// talks to the worker through the same capability.
type ExecBackend interface {
	Exec(ctx context.Context, name string, command []string, timeout time.Duration) (stdout, stderr string, exitCode int, err error)
	WriteFile(ctx context.Context, name, path, content string) error
}

// KeyStore is the object-storage capability for desired state + snapshots
// (oss.StorageClient satisfies it). Keys are relative to the storage
// prefix, same convention as the deployer's agents/{worker}/openclaw.json.
type KeyStore interface {
	PutObject(ctx context.Context, key string, data []byte) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	Stat(ctx context.Context, key string) error
}

// Manager is the per-runtime native member-config read/write plane. A
// runtime's config manager reads and writes the worker's own native config
// file (embedded docker mode only) with optimistic locking by on-disk
// sha256 revision, pre-write snapshots in object storage, and per-runtime
// reconcile semantics. The server layer never translates fields between
// runtimes; it only routes a worker to the manager of its resolved runtime.
type Manager interface {
	// Runtime is the runtime id this manager reads and writes.
	Runtime() string
	// Read returns the current config state (GET /config projection).
	Read(ctx context.Context, worker string) (ConfigState, error)
	// Write merges values (a partial document of declared keys) into the
	// worker's native config file, guarded by ifRev ("" = unchecked).
	Write(ctx context.Context, worker string, values map[string]any, ifRev string) (WriteResult, error)
	// Rollback restores the pre-write snapshot taken when the state
	// identified by rev was current.
	Rollback(ctx context.Context, worker, rev string) (WriteResult, error)
}
