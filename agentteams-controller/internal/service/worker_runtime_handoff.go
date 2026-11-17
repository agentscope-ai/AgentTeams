package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// WorkerRuntimeHandoffAnnotation enrolls a Worker in reconciliation of its
// durable runtime record. The storage record alone owns operation identity.
const WorkerRuntimeHandoffAnnotation = "agentteams.io/runtime-handoff"

// WorkerRuntimeState belongs to the stable Worker storage identity. Native
// sessions, task checkpoints and Matrix consumption retain their own records.
type WorkerRuntimeState struct {
	SchemaVersion int            `json:"schema_version"`
	Kind          string         `json:"kind"`
	MemberPrefix  string         `json:"member_prefix"`
	ActiveStart   *WorkerStart   `json:"active_start,omitempty"`
	Handoff       *WorkerHandoff `json:"handoff,omitempty"`
}

type WorkerHandoff struct {
	ID                   string                `json:"id"`
	Phase                string                `json:"phase"`
	Source               WorkerStart           `json:"source"`
	TargetRuntime        string                `json:"target_runtime"`
	PriorTargets         []WorkerStart         `json:"prior_targets,omitempty"`
	Target               *WorkerStart          `json:"target,omitempty"`
	SourceStopObservedAt string                `json:"source_stop_observed_at,omitempty"`
	TargetRegisteredAt   string                `json:"target_registered_at,omitempty"`
	CompletedAt          string                `json:"completed_at,omitempty"`
	TargetImage          string                `json:"target_image,omitempty"`
	CheckpointRefs       []WorkerCheckpointRef `json:"checkpoint_refs,omitempty"`
}

type WorkerCheckpointRef struct {
	Kind                    string `json:"kind"`
	TeamID                  string `json:"team_id,omitempty"`
	TaskID                  string `json:"task_id,omitempty"`
	EventKey                string `json:"event_key,omitempty"`
	Generation              uint64 `json:"generation"`
	CheckpointVersion       uint64 `json:"checkpoint_version"`
	CompletedStepCount      *int   `json:"completed_step_count,omitempty"`
	PendingReplyTxn         string `json:"pending_reply_txn,omitempty"`
	VerifiedOperation       string `json:"verified_operation,omitempty"`
	Outcome                 string `json:"outcome,omitempty"`
	TargetGeneration        uint64 `json:"target_generation,omitempty"`
	TargetCheckpointVersion uint64 `json:"target_checkpoint_version,omitempty"`
	ReplyEvent              string `json:"reply_event,omitempty"`
}

type WorkerRuntimeStore struct{ Storage oss.StorageClient }

func (s WorkerRuntimeStore) Load(ctx context.Context, name string) (*WorkerRuntimeState, string, error) {
	if s.Storage == nil {
		return nil, "", errors.New("Worker runtime storage unavailable")
	}
	prefix, err := workerMatrixPrefix(name)
	if err != nil {
		return nil, "", err
	}
	data, err := s.Storage.GetObject(ctx, prefix+"/runtime/state.json")
	if errors.Is(err, os.ErrNotExist) {
		return &WorkerRuntimeState{SchemaVersion: 1, Kind: "WorkerRuntimeState", MemberPrefix: prefix}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var state WorkerRuntimeState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if !json.Valid(data) || dec.Decode(&state) != nil || state.SchemaVersion != 1 || state.Kind != "WorkerRuntimeState" || state.MemberPrefix != prefix || state.ActiveStart == nil || !validRuntimeStart(*state.ActiveStart) {
		return nil, "", fmt.Errorf("%w: unsupported or foreign runtime state", ErrWorkerStateInvalid)
	}
	if op := state.Handoff; op != nil {
		valid := validHandoffID(op.ID) && validRuntimeStart(op.Source) && validHandoffTarget(op.TargetRuntime)
		switch op.Phase {
		case "preflight", "draining", "checkpointed", "stopping", "stopped", "creating":
			valid = valid && op.Source == *state.ActiveStart && op.Target == nil
		case "recovering", "completed":
			valid = valid && op.Target != nil && validRuntimeStart(*op.Target) && (op.Phase == "completed" || *op.Target == *state.ActiveStart) && op.Target.Runtime == op.TargetRuntime && op.Target.InstanceID != op.Source.InstanceID
		default:
			valid = false
		}
		if !valid {
			return nil, "", fmt.Errorf("%w: unsupported handoff record", ErrWorkerStateInvalid)
		}
	}
	return &state, workerMatrixETag(data), nil
}

// SyncRegisteredStart publishes every actual Worker start, including initial
// boot and ordinary restarts with no operator handoff. The backend identity is
// supplied by the Controller; Matrix registration supplies its process ID.
func (s WorkerRuntimeStore) SyncRegisteredStart(ctx context.Context, name, instance, runtime string) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if op := state.Handoff; op != nil && op.Phase != "completed" {
		return state, etag, nil
	}
	if current := state.ActiveStart; current != nil && current.InstanceID == instance && current.Runtime == runtime {
		return state, etag, nil
	}
	matrix := WorkerMatrixStore{Storage: s.Storage}
	observed, _, err := matrix.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	start := observed.ActiveStart
	if start.InstanceID != instance || start.Runtime != runtime || !validRuntimeStart(start) {
		return state, etag, nil
	}
	state.ActiveStart = &start
	state, etag, err = s.save(ctx, state, etag)
	if errors.Is(err, ErrWorkerStateConflict) {
		return s.Load(ctx, name)
	}
	return state, etag, err
}

// HandoffTargetImage is shared by declarative and operator-initiated switches.
func HandoffTargetImage(w *v1beta1.Worker, wb backend.WorkerBackend, actualRuntime, target string) (string, error) {
	fenced, ok := wb.(backend.RuntimeHandoffSource)
	if !ok {
		return "", fmt.Errorf("%w: backend does not support verified runtime replacement", ErrWorkerStateConflict)
	}
	if w.Spec.Image != "" && (w.Spec.Runtime == target || (w.Spec.Runtime == "" && actualRuntime == target)) {
		return w.Spec.Image, nil
	}
	image := fenced.HandoffTargetImage(target)
	if image == "" {
		return "", fmt.Errorf("%w: no Worker image configured for target Harness %s", ErrWorkerStateConflict, target)
	}
	return image, nil
}

func ValidWorkerHandoffRequest(id, target string) bool {
	return validHandoffID(id) && validHandoffTarget(target)
}

func validRuntimeStart(start WorkerStart) bool {
	return start.InstanceID != "" && start.ProcessID != "" && start.Runtime != "" && backend.ValidRuntime(start.Runtime)
}
func validHandoffID(id string) bool { return len(id) <= 128 && workerStateName.MatchString(id) }
func validHandoffTarget(runtime string) bool {
	return runtime != "" && runtime != backend.RuntimeCopaw && backend.ValidRuntime(runtime)
}

// Preflight records a planned switch before either instance is modified. An
// exact retry after a lost response returns the current phase of that operation.
func (s WorkerRuntimeStore) Preflight(ctx context.Context, name, id, target, image string, source WorkerStart, expected string) (*WorkerRuntimeState, string, error) {
	if !validRuntimeStart(source) || !validHandoffTarget(target) || !validHandoffID(id) || image == "" {
		return nil, "", fmt.Errorf("%w: a registered source start, request ID and supported target Harness are required", ErrWorkerStateInvalid)
	}
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	handoff := WorkerHandoff{ID: id, Phase: "preflight", Source: source, TargetRuntime: target, TargetImage: image}
	if h := state.Handoff; h != nil {
		if h.ID == id && h.Source == source && h.TargetRuntime == target {
			return state, etag, nil
		}
		if h.Phase != "completed" || h.ID == id {
			return nil, "", fmt.Errorf("%w: another handoff is already in progress", ErrWorkerStateConflict)
		}
	}
	if expected != etag {
		return nil, "", ErrWorkerStateConflict
	}
	state.ActiveStart = &source
	state.Handoff = &handoff
	return s.save(ctx, state, etag)
}

// BeginDrain can be called by the HTTP request and by the reconciler after a
// crash. A pending request already closes claim admission, even between writes.
func (s WorkerRuntimeStore) BeginDrain(ctx context.Context, name string) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil || state.Handoff == nil || state.Handoff.Phase != "preflight" {
		return state, etag, err
	}
	state.Handoff.Phase = "draining"
	state, etag, err = s.save(ctx, state, etag)
	if errors.Is(err, ErrWorkerStateConflict) {
		return s.Load(ctx, name)
	}
	return state, etag, err
}

// ConfirmCheckpoint commits the server-discovered references in the same CAS
// as the phase. A stopped container is verified independently by its backend.
func (s WorkerRuntimeStore) ConfirmCheckpoint(ctx context.Context, name, id string, source WorkerStart, expected string, refs []WorkerCheckpointRef) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	op := state.Handoff
	if op == nil || op.ID != id || op.Source != source {
		return nil, "", ErrWorkerStateConflict
	}
	if op.Phase == "checkpointed" {
		return state, etag, nil
	}
	if op.Phase != "draining" || etag != expected {
		return nil, "", ErrWorkerStateConflict
	}
	op.CheckpointRefs, op.Phase = refs, "checkpointed"
	return s.save(ctx, state, etag)
}

// TargetLabel identifies this operation on the actual replacement resource.
func (h *WorkerHandoff) TargetLabel() string {
	key, _ := json.Marshal([]string{h.ID, h.Source.InstanceID, h.Source.ProcessID, h.TargetRuntime})
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:16])
}

// AdvanceLifecycle persists intent before any destructive backend action.
// Only a backend-matched and registered target may become the active process.
func (s WorkerRuntimeStore) AdvanceLifecycle(ctx context.Context, name, id, from, to, expected, image string, target *WorkerStart) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	op := state.Handoff
	if op == nil || op.ID != id || op.Phase != from || etag != expected {
		return nil, "", ErrWorkerStateConflict
	}
	switch {
	case from == "checkpointed" && to == "stopping" && image != "":
		op.TargetImage = image
	case from == "stopping" && to == "stopped":
		op.SourceStopObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	case from == "stopped" && to == "creating":
	case (from == "creating" || from == "recovering") && to == "recovering" && target != nil && validRuntimeStart(*target) && target.Runtime == op.TargetRuntime && target.InstanceID != op.Source.InstanceID:
		if op.Target != nil && *op.Target != *target {
			op.PriorTargets = append(op.PriorTargets, *op.Target)
		}
		op.TargetRegisteredAt = time.Now().UTC().Format(time.RFC3339Nano)
		op.Target = target
		state.ActiveStart = target
	default:
		return nil, "", ErrWorkerStateInvalid
	}
	op.Phase = to
	return s.save(ctx, state, etag)
}

// Complete records the observed target evidence alongside the source references.
func (s WorkerRuntimeStore) Complete(ctx context.Context, state *WorkerRuntimeState, expected string) (*WorkerRuntimeState, string, error) {
	if state.Handoff == nil || state.Handoff.Phase != "recovering" || state.Handoff.Target == nil {
		return nil, "", ErrWorkerStateConflict
	}
	state.Handoff.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.Handoff.Phase = "completed"
	return s.save(ctx, state, expected)
}

func (s WorkerRuntimeStore) save(ctx context.Context, state *WorkerRuntimeState, etag string) (*WorkerRuntimeState, string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, "", err
	}
	key := state.MemberPrefix + "/runtime/state.json"
	if err := putWorkerRuntimeJSON(ctx, s.Storage, key, data, etag); err != nil {
		return nil, "", err
	}
	return state, workerMatrixETag(data), nil
}
