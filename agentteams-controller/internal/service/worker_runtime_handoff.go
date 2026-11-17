package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
	ID             string                `json:"id"`
	Phase          string                `json:"phase"`
	Source         WorkerStart           `json:"source"`
	TargetRuntime  string                `json:"target_runtime"`
	CheckpointRefs []WorkerCheckpointRef `json:"checkpoint_refs,omitempty"`
}

type WorkerCheckpointRef struct {
	Kind              string `json:"kind"`
	TeamID            string `json:"team_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	EventKey          string `json:"event_key,omitempty"`
	Generation        uint64 `json:"generation"`
	CheckpointVersion uint64 `json:"checkpoint_version"`
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
	if h := state.Handoff; h != nil && ((h.Phase != "preflight" && h.Phase != "draining" && h.Phase != "checkpointed") || !validHandoffID(h.ID) || h.Source != *state.ActiveStart || !validHandoffTarget(h.TargetRuntime)) {
		return nil, "", fmt.Errorf("%w: unsupported handoff record", ErrWorkerStateInvalid)
	}
	return &state, workerMatrixETag(data), nil
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
func (s WorkerRuntimeStore) Preflight(ctx context.Context, name, id, target string, source WorkerStart, expected string) (*WorkerRuntimeState, string, error) {
	if !validRuntimeStart(source) || !validHandoffTarget(target) || !validHandoffID(id) {
		return nil, "", fmt.Errorf("%w: a registered source start, request ID and supported target Harness are required", ErrWorkerStateInvalid)
	}
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	handoff := WorkerHandoff{ID: id, Phase: "preflight", Source: source, TargetRuntime: target}
	if h := state.Handoff; h != nil {
		if h.ID == id && h.Source == source && h.TargetRuntime == target {
			return state, etag, nil
		}
		return nil, "", fmt.Errorf("%w: another handoff is already in progress", ErrWorkerStateConflict)
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
