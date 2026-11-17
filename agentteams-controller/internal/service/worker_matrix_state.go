package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

var (
	ErrWorkerStateConflict = errors.New("worker runtime state changed")
	ErrWorkerStateInvalid  = errors.New("invalid worker runtime state")
	workerStateName        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// WorkerStart is supplied by the Controller after verifying the actual backend
// instance. A process claim alone is not evidence that an old process stopped.
type WorkerStart struct {
	InstanceID string `json:"instance_id"`
	ProcessID  string `json:"process_id"`
	Runtime    string `json:"runtime"`
}

type WorkerMatrixConsumer struct {
	Runtime   string `json:"runtime"`
	Account   string `json:"account"`
	Device    string `json:"device,omitempty"`
	Format    string `json:"format"`
	Token     string `json:"token"`
	FromToken string `json:"from_token,omitempty"`
}

type WorkerMatrixRoom struct {
	Through  string                     `json:"through"`
	Gaps     []string                   `json:"gaps,omitempty"`
	Backfill map[string]WorkerMatrixGap `json:"backfill,omitempty"`
}

type WorkerMatrixGap struct {
	FromToken string `json:"from_token"`
	Until     string `json:"until"`
	Before    string `json:"before"`
	Boundary  bool   `json:"boundary,omitempty"`
}

// Inputs are the decoded /messages dir=b chunk, in server return order.
type WorkerMatrixBackfill struct {
	RoomID    string              `json:"room_id"`
	GapID     string              `json:"gap_id"`
	FromToken string              `json:"from_token"`
	NextToken string              `json:"next_token"`
	Inputs    []WorkerMatrixInput `json:"inputs"`
}

type WorkerMatrixState struct {
	SchemaVersion int                             `json:"schema_version"`
	Kind          string                          `json:"kind"`
	MemberPrefix  string                          `json:"member_prefix"`
	ActiveStart   WorkerStart                     `json:"active_start"`
	Pending       []string                        `json:"pending"`
	Consumers     map[string]WorkerMatrixConsumer `json:"consumers"`
	Rooms         map[string]WorkerMatrixRoom     `json:"rooms"`
}

type WorkerMatrixContent struct {
	Sender         string         `json:"sender"`
	Type           string         `json:"type"`
	OriginServerTS int64          `json:"origin_server_ts"`
	Content        map[string]any `json:"content"`
}

type WorkerMatrixWork struct {
	Kind   string `json:"kind"`
	TeamID string `json:"team_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type WorkerMatrixNativeCommit struct {
	Runtime      string      `json:"runtime"`
	Start        WorkerStart `json:"start"`
	NativeRef    string      `json:"native_ref"`
	NativeDigest string      `json:"native_digest"`
	SavedAt      string      `json:"saved_at"`
}

// WorkerMatrixInput is immutable accepted input, not execution completion or a
// native read receipt. Task work references authority instead of copying it.
type WorkerMatrixInput struct {
	SchemaVersion int                       `json:"schema_version"`
	Kind          string                    `json:"kind"`
	MemberPrefix  string                    `json:"member_prefix"`
	RoomID        string                    `json:"room_id"`
	EventID       string                    `json:"event_id"`
	Input         WorkerMatrixContent       `json:"input"`
	InputState    string                    `json:"input_state"`
	Intent        string                    `json:"intent"`
	Work          *WorkerMatrixWork         `json:"work,omitempty"`
	NativeCommit  *WorkerMatrixNativeCommit `json:"native_commit,omitempty"`
	Execution     json.RawMessage           `json:"execution,omitempty"`
}

// WorkerMatrixStore commits the cursor and pending index only after all input
// objects are durable. HTTP ownership/policy validation belongs to the caller;
// this store never accepts a caller-selected object-storage prefix.
type WorkerMatrixStore struct {
	Storage oss.StorageClient
}

func workerMatrixPrefix(name string) (string, error) {
	if !workerStateName.MatchString(name) {
		return "", fmt.Errorf("%w: Worker storage identity", ErrWorkerStateInvalid)
	}
	return "agents/" + name, nil
}

// Hashes follow the shared UTF-8 compact JSON-array identity contract. Keeping
// this in the common layer avoids native SDK filename/escaping conventions.
func workerMatrixIdentity(parts ...string) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, part := range parts {
		if i != 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range part {
			switch r {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '\b', '\f', '\n', '\r', '\t':
				b.WriteByte('\\')
				b.WriteByte(map[rune]byte{'\b': 'b', '\f': 'f', '\n': 'n', '\r': 'r', '\t': 't'}[r])
			default:
				if r < 0x20 {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

func WorkerMatrixEventKey(room, event string) string {
	return workerMatrixIdentity(room, event)
}

func WorkerTaskSessionKey(memberPrefix, team, task string) string {
	return workerMatrixIdentity(memberPrefix, "task", team, task)
}

func WorkerMessageSessionKey(memberPrefix, room, event string) string {
	return workerMatrixIdentity(memberPrefix, "message", room, event)
}

func WorkerMatrixConsumerKey(consumer WorkerMatrixConsumer) string {
	return workerMatrixIdentity(consumer.Runtime, consumer.Account, consumer.Device, consumer.Format)
}

func workerMatrixETag(data []byte) string {
	// Same single-part JSON-object ETag used by existing Task conditional commits.
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func (s *WorkerMatrixStore) Load(ctx context.Context, name string) (*WorkerMatrixState, string, error) {
	prefix, err := workerMatrixPrefix(name)
	if err != nil {
		return nil, "", err
	}
	state := &WorkerMatrixState{SchemaVersion: 1, Kind: "WorkerMatrixState", MemberPrefix: prefix,
		Pending: []string{}, Consumers: map[string]WorkerMatrixConsumer{}, Rooms: map[string]WorkerMatrixRoom{}}
	data, err := s.Storage.GetObject(ctx, prefix+"/runtime/matrix/state.json")
	if errors.Is(err, os.ErrNotExist) {
		return state, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if !json.Valid(data) || decoder.Decode(state) != nil || state.SchemaVersion != 1 || state.Kind != "WorkerMatrixState" || state.MemberPrefix != prefix || state.ActiveStart.InstanceID == "" || state.ActiveStart.ProcessID == "" || !backend.ValidRuntime(state.ActiveStart.Runtime) || state.Pending == nil || state.Consumers == nil || state.Rooms == nil {
		return nil, "", fmt.Errorf("%w: unsupported or foreign Matrix record", ErrWorkerStateInvalid)
	}
	return state, workerMatrixETag(data), nil
}

func (s *WorkerMatrixStore) commit(ctx context.Context, state *WorkerMatrixState, etag string) (*WorkerMatrixState, string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, "", err
	}
	key := state.MemberPrefix + "/runtime/matrix/state.json"
	if etag == "" {
		err = s.Storage.PutObjectIfAbsent(ctx, key, data)
	} else if strict, ok := s.Storage.(interface {
		PutObjectIfMatchStrict(context.Context, string, []byte, string) error
	}); ok {
		err = strict.PutObjectIfMatchStrict(ctx, key, data, etag)
	} else {
		err = oss.ErrConditionalUnsupported
	}
	if errors.Is(err, oss.ErrPreconditionFailed) {
		return nil, "", ErrWorkerStateConflict
	}
	if err != nil {
		return nil, "", err
	}
	return state, workerMatrixETag(data), nil
}

func (s *WorkerMatrixStore) Start(ctx context.Context, name string, start WorkerStart, expected string) (*WorkerMatrixState, string, error) {
	if start.InstanceID == "" || start.ProcessID == "" || !backend.ValidRuntime(start.Runtime) {
		return nil, "", fmt.Errorf("%w: verified startup identity required", ErrWorkerStateInvalid)
	}
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if etag != expected || (state.ActiveStart.InstanceID == start.InstanceID && state.ActiveStart != start) {
		return nil, "", ErrWorkerStateConflict
	}
	state.ActiveStart = start
	return s.commit(ctx, state, etag)
}

func (s *WorkerMatrixStore) Input(ctx context.Context, name, eventKey string) (*WorkerMatrixInput, error) {
	input, _, err := s.InputSnapshot(ctx, name, eventKey)
	return input, err
}

func (s *WorkerMatrixStore) InputSnapshot(ctx context.Context, name, eventKey string) (*WorkerMatrixInput, string, error) {
	prefix, err := workerMatrixPrefix(name)
	if err != nil {
		return nil, "", err
	}
	keyBytes, err := hex.DecodeString(eventKey)
	if err != nil || len(keyBytes) != sha256.Size || eventKey != strings.ToLower(eventKey) {
		return nil, "", fmt.Errorf("%w: event identity", ErrWorkerStateInvalid)
	}
	data, err := s.Storage.GetObject(ctx, prefix+"/runtime/matrix/events/"+eventKey+".json")
	if err != nil {
		return nil, "", err
	}
	var input WorkerMatrixInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !json.Valid(data) || decoder.Decode(&input) != nil || input.SchemaVersion != 1 || input.Kind != "WorkerMatrixEvent" || input.MemberPrefix != prefix || WorkerMatrixEventKey(input.RoomID, input.EventID) != eventKey {
		return nil, "", fmt.Errorf("%w: unsupported or foreign Matrix input", ErrWorkerStateInvalid)
	}
	return &input, workerMatrixETag(data), nil
}

func sameWorkerMatrixInput(a, b WorkerMatrixInput) bool {
	// Redelivery must retain newer execution and native-commit evidence. Only
	// immutable accepted input and its work identity participate in comparison.
	a.InputState, b.InputState = "received", "received"
	a.NativeCommit, b.NativeCommit = nil, nil
	a.Execution, b.Execution = nil, nil
	x, err := json.Marshal(a)
	y, otherErr := json.Marshal(b)
	return err == nil && otherErr == nil && bytes.Equal(x, y)
}

// persistInputs is shared by incremental sync and backwards pagination. Both
// publish their index/cursor only after immutable inputs are durable and readable.
func (s *WorkerMatrixStore) persistInputs(ctx context.Context, name string, state *WorkerMatrixState, inputs []WorkerMatrixInput) ([]string, error) {
	keys := []string{}
	for _, input := range inputs {
		if !strings.HasPrefix(input.RoomID, "!") || !strings.HasPrefix(input.EventID, "$") || !strings.HasPrefix(input.Input.Sender, "@") || input.Input.Type == "" || input.Input.Content == nil || !slices.Contains([]string{"context", "message", "task_notification", "control"}, input.Intent) {
			return nil, fmt.Errorf("%w: complete accepted Matrix input required", ErrWorkerStateInvalid)
		}
		if (input.MemberPrefix != "" && input.MemberPrefix != state.MemberPrefix) || (input.SchemaVersion != 0 && input.SchemaVersion != 1) || (input.InputState != "" && input.InputState != "received") || input.NativeCommit != nil || input.Execution != nil {
			return nil, fmt.Errorf("%w: input scope/version or forged native receipt", ErrWorkerStateInvalid)
		}
		if input.Work != nil && ((input.Work.Kind != "task" && input.Work.Kind != "message") || (input.Work.Kind == "task" && (!workerStateName.MatchString(input.Work.TeamID) || !workerStateName.MatchString(input.Work.TaskID)))) {
			return nil, fmt.Errorf("%w: work identity", ErrWorkerStateInvalid)
		}
		input.SchemaVersion, input.Kind, input.MemberPrefix, input.InputState = 1, "WorkerMatrixEvent", state.MemberPrefix, "received"
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		eventKey := WorkerMatrixEventKey(input.RoomID, input.EventID)
		key := state.MemberPrefix + "/runtime/matrix/events/" + eventKey + ".json"
		if err = s.Storage.PutObjectIfAbsent(ctx, key, data); err != nil && !errors.Is(err, oss.ErrPreconditionFailed) {
			return nil, err
		}
		// Verify persistence before publishing a cursor that covers this input.
		stored, err := s.Input(ctx, name, eventKey)
		if err != nil {
			return nil, fmt.Errorf("input persistence could not be verified: %w", errors.Join(err, oss.ErrStorageUnavailable))
		}
		if !sameWorkerMatrixInput(*stored, input) {
			return nil, fmt.Errorf("%w: immutable Matrix input changed", ErrWorkerStateConflict)
		}
		keys = append(keys, eventKey)
	}
	return keys, nil
}

func (s *WorkerMatrixStore) Ingest(ctx context.Context, name string, start WorkerStart, expected string, consumer WorkerMatrixConsumer, inputs []WorkerMatrixInput, rooms map[string]WorkerMatrixRoom) (*WorkerMatrixState, string, error) {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if expected == "" || etag != expected || state.ActiveStart != start {
		return nil, "", ErrWorkerStateConflict
	}
	hasCursor := consumer != (WorkerMatrixConsumer{})
	if hasCursor && (consumer.Runtime != start.Runtime || !strings.HasPrefix(consumer.Account, "@") || consumer.Format == "" || consumer.Token == "") {
		return nil, "", fmt.Errorf("%w: native consumer identity required", ErrWorkerStateInvalid)
	}
	consumerKey := WorkerMatrixConsumerKey(consumer)
	if previous, exists := state.Consumers[consumerKey]; hasCursor && exists && consumer.FromToken != previous.Token {
		return nil, "", fmt.Errorf("%w: native sync started from an older cursor", ErrWorkerStateConflict)
	}
	consumer.FromToken = "" // Request precondition, not another persisted cursor.
	keys, err := s.persistInputs(ctx, name, state, inputs)
	if err != nil {
		return nil, "", err
	}
	for _, key := range keys {
		if !slices.Contains(state.Pending, key) {
			state.Pending = append(state.Pending, key)
		}
	}
	for roomID, room := range rooms {
		if len(room.Backfill) != 0 {
			return nil, "", fmt.Errorf("%w: backfill bounds are Controller-owned", ErrWorkerStateInvalid)
		}
		previous := state.Rooms[roomID]
		room.Backfill = maps.Clone(previous.Backfill)
		if room.Backfill == nil {
			room.Backfill = map[string]WorkerMatrixGap{}
		}
		room.Gaps = slices.Clone(room.Gaps)
		input, err := s.Input(ctx, name, WorkerMatrixEventKey(roomID, room.Through))
		if err != nil {
			return nil, "", fmt.Errorf("%w: room anchor must reference retained input: %v", ErrWorkerStateInvalid, err)
		}
		if input.RoomID != roomID {
			return nil, "", fmt.Errorf("%w: room anchor scope", ErrWorkerStateInvalid)
		}
		for _, id := range room.Gaps {
			if id == "" {
				return nil, "", fmt.Errorf("%w: gap pagination token required", ErrWorkerStateInvalid)
			}
			if slices.Contains(previous.Gaps, id) {
				continue // Preserve legacy holes without inventing missing bounds.
			}
			overlaps := slices.ContainsFunc(inputs, func(input WorkerMatrixInput) bool {
				return input.RoomID == roomID && previous.Through != "" && input.EventID == previous.Through
			})
			if overlaps {
				room.Gaps = slices.DeleteFunc(slices.Clone(room.Gaps), func(gap string) bool { return gap == id })
				continue // This actual timeline already reconnects the old anchor.
			}
			before := ""
			for _, input := range inputs {
				if input.RoomID == roomID {
					before = WorkerMatrixEventKey(roomID, input.EventID)
					break
				}
			}
			if before == "" {
				return nil, "", fmt.Errorf("%w: limited timeline inputs required", ErrWorkerStateInvalid)
			}
			room.Backfill[id] = WorkerMatrixGap{FromToken: id, Until: previous.Through, Before: before}
		}
		// An ordinary incremental batch cannot prove that an earlier limited
		// timeline was backfilled. Keep holes until explicit backfill completion.
		for _, gap := range state.Rooms[roomID].Gaps {
			if !slices.Contains(room.Gaps, gap) {
				room.Gaps = append(room.Gaps, gap)
			}
		}
		state.Rooms[roomID] = room
	}
	// Preserve the native batch's accepted order. Event hashes are identities,
	// not a conversation ordering; redelivery must not reshuffle queued work.
	if hasCursor {
		state.Consumers[consumerKey] = consumer
	}
	// Start() changes this same object, fencing commits already in flight from
	// an old start. No cross-object check-then-put is used for the cursor.
	return s.commit(ctx, state, etag)
}

// Retire is called by the Controller only after authoritative execution is
// terminal. A failed index cleanup cannot undo a persisted result/receipt.
func (s *WorkerMatrixStore) Retire(ctx context.Context, name string, start WorkerStart, eventKey string) error {
	state, etag, err := s.Load(ctx, name)
	if err != nil {
		return err
	}
	if state.ActiveStart != start {
		return ErrWorkerStateConflict
	}
	if !slices.Contains(state.Pending, eventKey) {
		return nil
	}
	for _, room := range state.Rooms {
		for _, gap := range room.Backfill {
			if gap.Before == eventKey {
				return ErrWorkerStateConflict // Keep the queue insertion anchor until covered.
			}
		}
	}
	state.Pending = slices.DeleteFunc(state.Pending, func(key string) bool { return key == eventKey })
	_, _, err = s.commit(ctx, state, etag)
	return err
}
