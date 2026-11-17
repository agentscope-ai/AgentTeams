package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func matrixInput(id string) WorkerMatrixInput {
	return WorkerMatrixInput{RoomID: "!room:test", EventID: id, Intent: "message",
		Input: WorkerMatrixContent{Sender: "@human:test", Type: "m.room.message", Content: map[string]any{"msgtype": "m.text", "body": "continue"}}}
}

func matrixConsumer(runtime, token string, from ...string) WorkerMatrixConsumer {
	consumer := WorkerMatrixConsumer{Runtime: runtime, Account: "@worker:test", Device: "device", Format: "matrix-sync/v3", Token: token}
	if len(from) == 1 {
		consumer.FromToken = from[0]
	}
	return consumer
}

func startMatrixStore(t *testing.T, storage oss.StorageClient) (*WorkerMatrixStore, WorkerStart, string) {
	t.Helper()
	s := &WorkerMatrixStore{Storage: storage}
	start := WorkerStart{InstanceID: "docker/source/started-at", ProcessID: "actual-source-start", Runtime: "hermes"}
	_, etag, err := s.Start(t.Context(), "worker", start, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, start, etag
}

func TestWorkerMatrixStateSurvivesReplacementWithoutNativeDirectory(t *testing.T) {
	storage := ossfake.NewMemory()
	s, start, etag := startMatrixStore(t, storage)
	state, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "source-token"), []WorkerMatrixInput{matrixInput("$one")}, nil)
	if err != nil || len(state.Pending) != 1 {
		t.Fatalf("ingest: %+v %v", state, err)
	}
	// A new store has no local directory, native session or cursor cache.
	s = &WorkerMatrixStore{Storage: storage}
	target := WorkerStart{InstanceID: "docker/target/started-at", ProcessID: "actual-target-start", Runtime: "qwenpaw"}
	state, etag, err = s.Start(t.Context(), "worker", target, etag)
	if err != nil || len(state.Pending) != 1 || state.Consumers[WorkerMatrixConsumerKey(matrixConsumer("hermes", ""))].Token != "source-token" {
		t.Fatalf("replacement lost shared inputs/cursor: %+v %v", state, err)
	}
	state, _, err = s.Ingest(t.Context(), "worker", target, etag, matrixConsumer("qwenpaw", "target-token"), []WorkerMatrixInput{matrixInput("$one")}, nil)
	if err != nil || len(state.Pending) != 1 || len(state.Consumers) != 2 {
		t.Fatalf("replay did not deduplicate by event identity: %+v %v", state, err)
	}
	input, err := s.Input(t.Context(), "worker", WorkerMatrixEventKey("!room:test", "$one"))
	if err != nil || input.EventID != "$one" || input.MemberPrefix != "agents/worker" || input.InputState != "received" {
		t.Fatalf("durable input: %+v %v", input, err)
	}
}

type matrixFaultStore struct {
	*ossfake.Memory
	failKey    string
	afterInput func()
}

func (s *matrixFaultStore) PutObjectIfAbsent(ctx context.Context, key string, value []byte) error {
	if key == s.failKey {
		return fmt.Errorf("injected upload failure")
	}
	err := s.Memory.PutObjectIfAbsent(ctx, key, value)
	if s.afterInput != nil {
		callback := s.afterInput
		s.afterInput = nil
		callback()
	}
	return err
}

func TestWorkerMatrixCursorNeverAdvancesPastFailedInputUpload(t *testing.T) {
	storage := &matrixFaultStore{Memory: ossfake.NewMemory()}
	s, start, etag := startMatrixStore(t, storage)
	storage.failKey = "agents/worker/runtime/matrix/events/" + WorkerMatrixEventKey("!room:test", "$two") + ".json"
	inputs := []WorkerMatrixInput{matrixInput("$one"), matrixInput("$two")}
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next-token"), inputs, nil); err == nil {
		t.Fatal("failed upload advanced the consumer")
	}
	state, current, err := s.Load(t.Context(), "worker")
	if err != nil || current != etag || len(state.Pending) != 0 || len(state.Consumers) != 0 {
		t.Fatalf("failed batch changed the committed head: %+v %v", state, err)
	}
	storage.failKey = ""
	state, _, err = s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next-token"), inputs, nil)
	if err != nil || len(state.Pending) != 2 {
		t.Fatalf("retry did not retain both durable inputs: %+v %v", state, err)
	}
}

func TestWorkerMatrixReplacementFencesAnAlreadyRunningOldCommit(t *testing.T) {
	storage := &matrixFaultStore{Memory: ossfake.NewMemory()}
	s, old, etag := startMatrixStore(t, storage)
	target := WorkerStart{InstanceID: "docker/target/started-at", ProcessID: "target-start", Runtime: "qwenpaw"}
	storage.afterInput = func() {
		if _, _, err := s.Start(t.Context(), "worker", target, etag); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Ingest(t.Context(), "worker", old, etag, matrixConsumer("hermes", "late-old-token"), []WorkerMatrixInput{matrixInput("$one")}, nil); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("old in-flight commit was not fenced: %v", err)
	}
	state, current, err := s.Load(t.Context(), "worker")
	if err != nil || state.ActiveStart != target || len(state.Consumers) != 0 {
		t.Fatalf("old process overwrote replacement: %+v %v", state, err)
	}
	// The prior head did not advance its cursor; a replay can adopt the input.
	state, _, err = s.Ingest(t.Context(), "worker", target, current, matrixConsumer("qwenpaw", "target-token"), []WorkerMatrixInput{matrixInput("$one")}, nil)
	if err != nil || len(state.Pending) != 1 {
		t.Fatalf("orphan input could not be adopted: %+v %v", state, err)
	}
}

func TestWorkerMatrixRejectsUnknownVersionScopeAndUnverifiedProcessTakeover(t *testing.T) {
	storage := ossfake.NewMemory()
	s, start, etag := startMatrixStore(t, storage)
	other := start
	other.ProcessID = "competing-process"
	if _, _, err := s.Start(t.Context(), "worker", other, etag); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("same-instance takeover was not rejected: %v", err)
	}
	if _, _, err := s.Load(t.Context(), "../other"); !errors.Is(err, ErrWorkerStateInvalid) {
		t.Fatalf("invalid scope: %v", err)
	}
	for _, body := range []string{
		`{"schema_version":2,"kind":"WorkerMatrixState","member_prefix":"agents/worker"}`,
		`{"schema_version":1,"kind":"WorkerMatrixState","member_prefix":"agents/other"}`,
	} {
		if err := storage.PutObject(t.Context(), "agents/worker/runtime/matrix/state.json", []byte(body)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Load(t.Context(), "worker"); !errors.Is(err, ErrWorkerStateInvalid) {
			t.Fatalf("unknown or foreign record was silently reset: %v", err)
		}
	}
}

func TestWorkerMatrixDoesNotRewriteInputOrAcceptForeignCursor(t *testing.T) {
	storage := ossfake.NewMemory()
	s, start, etag := startMatrixStore(t, storage)
	input := matrixInput("$one")
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "one"), []WorkerMatrixInput{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.Input.Content["body"] = "rewritten"
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "two", "one"), []WorkerMatrixInput{input}, nil); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("same event acquired different input: %v", err)
	}
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("qwenpaw", "foreign"), nil, nil); !errors.Is(err, ErrWorkerStateInvalid) {
		t.Fatalf("cursor from another native consumer accepted: %v", err)
	}
}

func TestWorkerMatrixCoverageRequiresRetainedAnchorAndKeepsGap(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	room := WorkerMatrixRoom{Through: "$missing", Gaps: []string{"backfill-token"}}
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next"), nil, map[string]WorkerMatrixRoom{"!room:test": room}); !errors.Is(err, ErrWorkerStateInvalid) {
		t.Fatalf("unretained room anchor was accepted: %v", err)
	}
	room.Through = "$one"
	state, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next"), []WorkerMatrixInput{matrixInput("$one")}, map[string]WorkerMatrixRoom{"!room:test": room})
	if err != nil || len(state.Rooms["!room:test"].Gaps) != 1 {
		t.Fatalf("limited-sync gap was not retained: %+v %v", state, err)
	}
}

func TestWorkerMatrixIdentityMatchesSharedUTF8Format(t *testing.T) {
	for room, want := range map[string]string{
		"!房间<&:test":           "09c2619cdff40ecf98629d3d2064f5448c47d8b6461feaac31e28c5612234d78",
		"!line\u2028room:test": "ed24f0e5be3b9d5c41e6fed32f3f66f99faeb62d4a70c106edccd9fb90547c03",
	} {
		if got := WorkerMatrixEventKey(room, "$event"); got != want {
			t.Errorf("UTF-8 identity for %q: got %s, want %s", room, got, want)
		}
	}
}

func TestWorkerMatrixStrictStorageFailureDoesNotAdvanceCursor(t *testing.T) {
	storage := ossfake.NewMemory()
	s, start, etag := startMatrixStore(t, storage)
	storage.StrictUnsupported.Store(true)
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "next"), []WorkerMatrixInput{matrixInput("$one")}, nil); !errors.Is(err, oss.ErrConditionalUnsupported) {
		t.Fatalf("unsupported CAS silently degraded: %v", err)
	}
	state, after, err := s.Load(t.Context(), "worker")
	if err != nil || after != etag || len(state.Consumers) != 0 {
		t.Fatalf("unsupported CAS changed the cursor: %+v %v", state, err)
	}
}

func TestWorkerMatrixRedeliveryCannotOverwriteNewerMessageProgress(t *testing.T) {
	storage := ossfake.NewMemory()
	s, start, etag := startMatrixStore(t, storage)
	input := matrixInput("$one")
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "one"), []WorkerMatrixInput{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := WorkerMatrixEventKey(input.RoomID, input.EventID)
	stored, err := s.Input(t.Context(), "worker", key)
	if err != nil {
		t.Fatal(err)
	}
	stored.InputState = "native_committed"
	stored.NativeCommit = &WorkerMatrixNativeCommit{Runtime: "hermes", NativeRef: "native-session", NativeDigest: "persisted-digest"}
	stored.Execution = json.RawMessage(`{"generation":1,"checkpoint":{"version":4,"completed_steps":["one"]}}`)
	data, _ := json.Marshal(stored)
	if err := storage.PutObject(t.Context(), "agents/worker/runtime/matrix/events/"+key+".json", data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "two", "one"), []WorkerMatrixInput{input}, nil); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Input(t.Context(), "worker", key)
	if err != nil || retained.NativeCommit == nil || string(retained.Execution) != string(stored.Execution) {
		t.Fatalf("replay replaced newer native/execution facts: %+v %v", retained, err)
	}
}

func TestWorkerMatrixNativeInputCanPersistBeforeItsBatchCursor(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	state, _, err := s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, []WorkerMatrixInput{matrixInput("$one")}, nil)
	if err != nil || len(state.Pending) != 1 || len(state.Consumers) != 0 {
		t.Fatalf("native callback must not invent a sync token: %+v %v", state, err)
	}
}

func TestWorkerMatrixOverlapRespectsDurableWorkState(t *testing.T) {
	cases := []struct {
		event     string
		intent    string
		work      *WorkerMatrixWork
		native    bool
		execution string
		pending   bool
	}{
		{"$delivered", "message", nil, true, `{"state":"active","checkpoint":{"delivery":"delivered"}}`, false},
		{"$native-only", "message", nil, true, "", true},
		{"$cancelled", "message", nil, false, `{"state":"cancelled"}`, false},
		{"$verify", "message", nil, true, `{"state":"needs_verification"}`, true},
		{"$reply", "message", nil, true, `{"checkpoint":{"delivery":"pending"}}`, true},
		{"$context", "context", nil, false, "", true},
		{"$control", "control", nil, true, "", true},
		{"$task", "task_notification", &WorkerMatrixWork{Kind: "task", TeamID: "team", TaskID: "T"}, true, "", true},
		{"$handled-context", "context", nil, true, "", false},
	}
	for _, mode := range []string{"sync", "backfill"} {
		t.Run(mode, func(t *testing.T) {
			storage := ossfake.NewMemory()
			s, source, etag := startMatrixStore(t, storage)
			const room = "!room:test"
			var inputs []WorkerMatrixInput
			var want []string
			for _, tc := range cases {
				input := matrixInput(tc.event)
				input.Intent, input.Work = tc.intent, tc.work
				inputs = append(inputs, input)
				key := WorkerMatrixEventKey(room, tc.event)
				if tc.pending {
					want = append(want, key)
				}
				// Seed durable inputs without a pending index. A previously processed
				// event and an unpublished input can both be absent from that index.
				input.SchemaVersion, input.Kind, input.MemberPrefix, input.InputState = 1, "WorkerMatrixEvent", "agents/worker", "received"
				if tc.native {
					input.InputState = "native_committed"
					input.NativeCommit = &WorkerMatrixNativeCommit{Runtime: source.Runtime, Start: source, NativeRef: "session", NativeDigest: "row-digest"}
				}
				input.Execution = json.RawMessage(tc.execution)
				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				if err := storage.PutObject(t.Context(), "agents/worker/runtime/matrix/events/"+key+".json", data); err != nil {
					t.Fatal(err)
				}
			}
			target := WorkerStart{InstanceID: "replacement", ProcessID: "target", Runtime: "qwenpaw"}
			_, etag, err := s.Start(t.Context(), "worker", target, etag)
			if err != nil {
				t.Fatal(err)
			}
			var state *WorkerMatrixState
			if mode == "sync" {
				state, _, err = s.Ingest(t.Context(), "worker", target, etag, matrixConsumer(target.Runtime, "next"), inputs,
					map[string]WorkerMatrixRoom{room: {Through: cases[len(cases)-1].event, Gaps: []string{"gap"}}})
				if err == nil && state.Rooms[room].Backfill["gap"].Before != want[0] {
					t.Errorf("gap anchored to retired input: %+v", state.Rooms[room].Backfill["gap"])
				}
			} else {
				_, etag, err = s.Ingest(t.Context(), "worker", target, etag, matrixConsumer(target.Runtime, "next"), nil,
					map[string]WorkerMatrixRoom{room: {Gaps: []string{"gap"}}})
				if err != nil {
					t.Fatal(err)
				}
				slices.Reverse(inputs)
				// The newest history page has only a previously handled context.
				state, etag, err = s.Backfill(t.Context(), "worker", target, etag, WorkerMatrixBackfill{RoomID: room,
					GapID: "gap", FromToken: "gap", NextToken: "more", Inputs: inputs[:1]})
				if err != nil {
					t.Fatal(err)
				}
				if state.Rooms[room].Through != inputs[0].EventID || len(state.Pending) != 0 {
					t.Errorf("retired history must advance coverage without creating work: %+v", state)
				}
				state, _, err = s.Backfill(t.Context(), "worker", target, etag, WorkerMatrixBackfill{RoomID: room,
					GapID: "gap", FromToken: "more", Inputs: inputs[1:]})
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(state.Pending, want) {
				t.Errorf("pending order got %v, want %v", state.Pending, want)
			}
		})
	}
}

func TestWorkerMatrixIncrementalSyncCannotForgetAnOlderGap(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	_, etag, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "one"), []WorkerMatrixInput{matrixInput("$one")}, map[string]WorkerMatrixRoom{"!room:test": {Through: "$one", Gaps: []string{"old-gap"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Ingest(t.Context(), "worker", start, etag, matrixConsumer("hermes", "two", "one"), []WorkerMatrixInput{matrixInput("$two")}, map[string]WorkerMatrixRoom{"!room:test": {Through: "$two"}})
	if err != nil || len(state.Rooms["!room:test"].Gaps) != 1 || state.Rooms["!room:test"].Gaps[0] != "old-gap" {
		t.Fatalf("incremental sync erased outstanding backlog: %+v %v", state, err)
	}
}

func TestWorkerMatrixReturnToOldHarnessCannotRestoreItsOlderCursor(t *testing.T) {
	s, source, etag := startMatrixStore(t, ossfake.NewMemory())
	_, etag, err := s.Ingest(t.Context(), "worker", source, etag, matrixConsumer("hermes", "newest-shared-token"), []WorkerMatrixInput{matrixInput("$one")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := WorkerStart{InstanceID: "docker/qwenpaw/start", ProcessID: "target-process", Runtime: "qwenpaw"}
	_, etag, err = s.Start(t.Context(), "worker", target, etag)
	if err != nil {
		t.Fatal(err)
	}
	returned := WorkerStart{InstanceID: "docker/hermes/returned", ProcessID: "returned-process", Runtime: "hermes"}
	_, etag, err = s.Start(t.Context(), "worker", returned, etag)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Ingest(t.Context(), "worker", returned, etag, matrixConsumer("hermes", "derived-from-old-cache", "older-native-token"), nil, nil); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("old native cursor replaced shared progress: %v", err)
	}
	state, _, err := s.Load(t.Context(), "worker")
	if err != nil || state.Consumers[WorkerMatrixConsumerKey(matrixConsumer("hermes", ""))].Token != "newest-shared-token" {
		t.Fatalf("authoritative cursor changed: %+v %v", state, err)
	}
}

func TestWorkerMatrixConcurrentBatchesCannotLoseCommittedInputs(t *testing.T) {
	s, start, etag := startMatrixStore(t, ossfake.NewMemory())
	const writers = 12
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			_, _, err := s.Ingest(t.Context(), "worker", start, etag, WorkerMatrixConsumer{}, []WorkerMatrixInput{matrixInput(fmt.Sprintf("$event-%d", i))}, nil)
			results <- err
		}()
	}
	winners := 0
	for range writers {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, ErrWorkerStateConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("same-version batch winners: got %d, want 1", winners)
	}
	for i := range writers {
		_, current, err := s.Load(t.Context(), "worker")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Ingest(t.Context(), "worker", start, current, WorkerMatrixConsumer{}, []WorkerMatrixInput{matrixInput(fmt.Sprintf("$event-%d", i))}, nil); err != nil {
			t.Fatal(err)
		}
	}
	state, _, err := s.Load(t.Context(), "worker")
	if err != nil || len(state.Pending) != writers {
		t.Fatalf("lost input after CAS retry: %+v %v", state, err)
	}
}
