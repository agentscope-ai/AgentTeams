// Package resources implements the R1 resources slice: the
// res.workers store, the three native worker handlers (create / list /
// get-by-name), the transparent PUT/DELETE proxy and the controller
// status writeback route.
//
// R1 is the minimal slice: res.workers plus the kernel ops
// tables (outbox / audit_events / idempotency_keys).
// Update / delete stay controller-authoritative until R2.
package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Store-level sentinel errors. Handlers map them to the frozen codes.
var (
	// ErrNotFound: no live row for the name (deleted_at IS NULL filter).
	ErrNotFound = errors.New("resources: worker not found")
	// ErrConflict: optimistic lock (version) did not match.
	ErrConflict = errors.New("resources: worker version conflict")
	// ErrAlreadyExists: duplicate primary key on create.
	ErrAlreadyExists = errors.New("resources: worker already exists")
	// ErrIdemMiss: no live idempotency record for the key.
	ErrIdemMiss = errors.New("resources: idempotency key not found")

	// ErrFieldTooLong means a stored column rejected the value (MySQL 1406).
	// It is a client error (400), not a 500: the request cannot ever succeed.
	ErrFieldTooLong = errors.New("resources: field value too long")
)

// Worker is one res.workers row. Nullable columns map to the
// zero value; timestamps are UTC.
type Worker struct {
	Name          string
	WorkerName    string
	TeamSlug      string
	Model         string
	ModelProvider string
	SubagentModel string

	LLMStreamFirstContentTimeout string
	LLMStreamIdleTimeout         string
	LLMMaxRetries                string
	LLMBackoffBase               string
	LLMBackoffCap                string
	LLMMaxConcurrent             string
	LLMMaxQPM                    string
	LLMRateLimitPause            string
	LLMRateLimitJitter           string
	LLMAcquireTimeout            string

	Runtime  string
	Image    string
	Identity string

	Soul   string
	Agents string

	Skills        []string
	RemoteSkills  json.RawMessage
	MCPServers    json.RawMessage
	PackageRef    string
	Expose        json.RawMessage
	ChannelPolicy json.RawMessage
	Resources     json.RawMessage
	Env           map[string]string

	ContainerManaged bool
	DesiredState     string

	ObservedPhase          string
	ObservedContainerState string
	MatrixUserID           string
	RoomID                 string
	LastActiveAt           *time.Time
	AgentStatus            string
	RunningTaskCount       *int
	LastRunAt              *time.Time
	LastFinishAt           *time.Time
	Message                string

	Generation int64
	SpecHash   string
	Version    int64

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Spec is the configuration subset that participates in spec_hash and in
// the generation counter (spec 变更 ⇔ generation+1). It excludes
// every observed_* column, the timestamps, deleted_at, version and
// generation itself.
type Spec struct {
	WorkerName    string
	TeamSlug      string
	Model         string
	ModelProvider string
	SubagentModel string

	LLMStreamFirstContentTimeout string
	LLMStreamIdleTimeout         string
	LLMMaxRetries                string
	LLMBackoffBase               string
	LLMBackoffCap                string
	LLMMaxConcurrent             string
	LLMMaxQPM                    string
	LLMRateLimitPause            string
	LLMRateLimitJitter           string
	LLMAcquireTimeout            string

	Runtime  string
	Image    string
	Identity string

	Soul   string
	Agents string

	Skills        []string
	RemoteSkills  json.RawMessage
	MCPServers    json.RawMessage
	PackageRef    string
	Expose        json.RawMessage
	ChannelPolicy json.RawMessage
	Resources     json.RawMessage
	Env           map[string]string

	ContainerManaged bool
	DesiredState     string
}

// Spec projects the worker's configuration fields.
func (w *Worker) Spec() Spec {
	return Spec{
		WorkerName:    w.WorkerName,
		TeamSlug:      w.TeamSlug,
		Model:         w.Model,
		ModelProvider: w.ModelProvider,
		SubagentModel: w.SubagentModel,

		LLMStreamFirstContentTimeout: w.LLMStreamFirstContentTimeout,
		LLMStreamIdleTimeout:         w.LLMStreamIdleTimeout,
		LLMMaxRetries:                w.LLMMaxRetries,
		LLMBackoffBase:               w.LLMBackoffBase,
		LLMBackoffCap:                w.LLMBackoffCap,
		LLMMaxConcurrent:             w.LLMMaxConcurrent,
		LLMMaxQPM:                    w.LLMMaxQPM,
		LLMRateLimitPause:            w.LLMRateLimitPause,
		LLMRateLimitJitter:           w.LLMRateLimitJitter,
		LLMAcquireTimeout:            w.LLMAcquireTimeout,

		Runtime:  w.Runtime,
		Image:    w.Image,
		Identity: w.Identity,
		Soul:     w.Soul,
		Agents:   w.Agents,

		Skills:        w.Skills,
		RemoteSkills:  w.RemoteSkills,
		MCPServers:    w.MCPServers,
		PackageRef:    w.PackageRef,
		Expose:        w.Expose,
		ChannelPolicy: w.ChannelPolicy,
		Resources:     w.Resources,
		Env:           w.Env,

		ContainerManaged: w.ContainerManaged,
		DesiredState:     w.DesiredState,
	}
}

// Observed is the controller-reported execution state written by
// POST /internal/v1/workers/status. It never touches generation, spec_hash
// or version.
type Observed struct {
	Phase            string
	ContainerState   string
	MatrixUserID     string
	RoomID           string
	Message          string
	LastActiveAt     *time.Time
	AgentStatus      string
	RunningTaskCount *int
	LastRunAt        *time.Time
	LastFinishAt     *time.Time
}

// ListFilter narrows List (both filters optional; no pagination in R1).
// Rows are returned in name ascending order.
type ListFilter struct {
	TeamSlug      string
	ObservedPhase string
}

// AuditMeta is the request context carried into the ops.audit_events row
// written in the same transaction as the business row (every
// mutation records an allow decision; denials are recorded by the handler).
type AuditMeta struct {
	RequestID       string
	ActorType       string
	ActorID         string
	OnBehalfOfType  string
	OnBehalfOfID    string
	TeamUID         string
	ProjectUID      string
	TaskID          string
	Action          string
	ResourceVersion *int64
	TokenJTI        string
	Client          string
}

// Store is the res.workers repository contract (2b1). Implementations own
// the single transaction that writes the worker row plus ops.outbox plus
// ops.audit_events.
type Store interface {
	Create(ctx context.Context, w *Worker, a *AuditMeta) (*Worker, error)
	Get(ctx context.Context, name string) (*Worker, error)
	List(ctx context.Context, f ListFilter) ([]*Worker, error)
	UpdateSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error)
	// SyncSpec applies the same optimistic-lock/generation logic as
	// UpdateSpec, but emits a generation-only "sync" outbox event instead of
	// an "update" carrying the spec. The transparent proxy uses it after a
	// controller PUT: the controller already applied the change, and
	// re-sending the service's possibly-partial view of the spec would revert
	// fields it cannot read back.
	SyncSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error)
	// UpdateStatus applies observed fields only when the row generation is
	// <= generation (out-of-order callbacks are discarded). It reports
	// whether the row was updated.
	UpdateStatus(ctx context.Context, name string, o Observed, generation int64) (bool, error)
	SoftDelete(ctx context.Context, name string, expectedVersion int64, a *AuditMeta) error
}

// IdemRecord is the stored first response for an Idempotency-Key
// (ops.idempotency_keys).
type IdemRecord struct {
	WorkerName  string
	RequestHash string
	Status      int
	Body        []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// IdemStore is the 24h idempotency-key store. Keys are scoped to the caller;
// a record also binds the normalized request hash so a different body with
// the same key conflicts instead of replaying the first response.
type IdemStore interface {
	Get(ctx context.Context, principal, key string) (*IdemRecord, error)
	Put(ctx context.Context, principal, key, requestHash, workerName string, status int, body []byte, ttl time.Duration) error
}
