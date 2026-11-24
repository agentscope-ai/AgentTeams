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
	"strings"
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

// WorkerRuntimeStatus adds the observed target diagnostic to operator readback.
type WorkerRuntimeStatus struct {
	WorkerRuntimeState
	TargetFailure string `json:"target_failure,omitempty"`
}

type WorkerHandoff struct {
	ID                   string          `json:"id"`
	Phase                string          `json:"phase"`
	Source               WorkerStart     `json:"source"`
	TargetRuntime        string          `json:"target_runtime"`
	PriorTargets         []WorkerStart   `json:"prior_targets,omitempty"`
	Target               *WorkerStart    `json:"target,omitempty"`
	LostSources          []WorkerStart   `json:"lost_sources,omitempty"`
	SourceLossReason     string          `json:"source_loss_reason,omitempty"`
	SourceStopObservedAt string          `json:"source_stop_observed_at,omitempty"`
	TargetRegisteredAt   string          `json:"target_registered_at,omitempty"`
	CompletedAt          string          `json:"completed_at,omitempty"`
	TargetImage          string          `json:"target_image,omitempty"`
	LegacyCheckpointRefs json.RawMessage `json:"checkpoint_refs,omitempty"`
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
		case "preflight", "draining", "source_lost", "checkpointed", "stopping", "stopped", "creating":
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

// WorkerHandoffMatrix checks the deployment policy, the target environment, and
// the actual rooms joined by this Worker before a plaintext-only target starts.
type WorkerHandoffMatrix struct {
	E2EEEnabled         bool
	CheckPlaintextRooms func(context.Context, string) error
}

func (m WorkerHandoffMatrix) Validate(ctx context.Context, w *v1beta1.Worker, target string) error {
	if target != backend.RuntimeCodex && target != backend.RuntimeDeepSeekHarness && target != backend.RuntimeOpenCode {
		return nil
	}
	value := w.Spec.Env["AGENTTEAMS_MATRIX_E2EE"]
	if m.E2EEEnabled || value == "1" || strings.EqualFold(value, "true") {
		return fmt.Errorf("%w: target Harness %s does not support Matrix E2EE", ErrWorkerStateConflict, target)
	}
	if m.CheckPlaintextRooms == nil {
		return errors.New("cannot verify Worker Matrix room encryption")
	}
	// Credentials are keyed by the Worker CR, even when spec.workerName differs.
	return m.CheckPlaintextRooms(ctx, w.Name)
}

func (p *Provisioner) HandoffMatrix() WorkerHandoffMatrix {
	return WorkerHandoffMatrix{E2EEEnabled: p.matrixConfig.E2EEEnabled, CheckPlaintextRooms: p.checkPlaintextWorkerRooms}
}

func (p *Provisioner) checkPlaintextWorkerRooms(ctx context.Context, credentialName string) error {
	creds, err := p.loadWorkerCredentials(ctx, credentialName)
	if err != nil {
		return err
	}
	if creds == nil || creds.MatrixToken == "" {
		return errors.New("Worker Matrix token unavailable for handoff preflight")
	}
	rooms, err := p.matrix.ListJoinedRooms(ctx, creds.MatrixToken)
	if err != nil {
		return err
	}
	if rooms == nil {
		return errors.New("missing Worker Matrix room membership list")
	}
	for _, room := range rooms {
		state, err := p.matrix.GetRoomState(ctx, room, "m.room.encryption", "", creds.MatrixToken)
		if err != nil {
			return err
		}
		if state != nil {
			return fmt.Errorf("%w: joined room %s requires Matrix E2EE", ErrWorkerStateConflict, room)
		}
	}
	return nil
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
	handoff := WorkerHandoff{ID: id, Phase: "stopping", Source: source, TargetRuntime: target, TargetImage: image}
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

// ResumeStop upgrades unfinished records from the earlier draft protocol.
func (s WorkerRuntimeStore) ResumeStop(ctx context.Context, name string) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil || state.Handoff == nil {
		return state, etag, err
	}
	switch state.Handoff.Phase {
	case "preflight", "draining", "checkpointed", "source_lost":
		state.Handoff.Phase = "stopping"
		state.Handoff.SourceLossReason = ""
		state.Handoff.LegacyCheckpointRefs = nil
		return s.save(ctx, state, etag)
	default:
		return state, etag, nil
	}
}

// TargetLabel identifies this operation on the actual replacement resource.
func (h *WorkerHandoff) TargetLabel() string {
	key, _ := json.Marshal([]string{h.ID, h.Source.InstanceID, h.Source.ProcessID, h.TargetRuntime})
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:16])
}

// AdvanceLifecycle persists intent before any destructive backend action.
// Only a backend-matched and registered target may become the active process.
func (s WorkerRuntimeStore) AdvanceLifecycle(ctx context.Context, name, id, from, to, expected string, target *WorkerStart) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	op := state.Handoff
	if op == nil || op.ID != id || op.Phase != from || etag != expected {
		return nil, "", ErrWorkerStateConflict
	}
	switch {
	case from == "stopping" && to == "stopped":
		op.SourceStopObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	case from == "stopped" && to == "creating":
	case (from == "creating" || from == "recovering") && to == "recovering" && target != nil && validRuntimeStart(*target) && target.Runtime == op.TargetRuntime && target.InstanceID != op.Source.InstanceID:
		op.TargetRegisteredAt = time.Now().UTC().Format(time.RFC3339Nano)
		op.Target = target
		state.ActiveStart = target
	default:
		return nil, "", ErrWorkerStateInvalid
	}
	op.Phase = to
	return s.save(ctx, state, etag)
}

// RebindSource tracks a backend-proven restart of the same source resource.
func (s WorkerRuntimeStore) RebindSource(ctx context.Context, name, id, expected string, previous, successor WorkerStart) (*WorkerRuntimeState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	op := state.Handoff
	if op == nil || op.ID != id || op.Source != previous || etag != expected || (op.Phase != "stopping" && op.Phase != "stopped") {
		return nil, "", ErrWorkerStateConflict
	}
	oldBackend, oldInstance, _ := strings.Cut(previous.InstanceID, "/")
	newBackend, newInstance, _ := strings.Cut(successor.InstanceID, "/")
	oldResource, _, _ := strings.Cut(oldInstance, "/")
	newResource, _, _ := strings.Cut(newInstance, "/")
	if !validRuntimeStart(successor) || successor.Runtime != previous.Runtime || oldBackend != newBackend || oldResource != newResource || successor.InstanceID == previous.InstanceID {
		return nil, "", ErrWorkerStateInvalid
	}
	op.LostSources = append(op.LostSources, previous)
	op.Source, state.ActiveStart = successor, &successor
	op.SourceStopObservedAt, op.Phase = "", "stopping"
	return s.save(ctx, state, etag)
}

// Complete marks the registered target ready for normal Worker operation.
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
