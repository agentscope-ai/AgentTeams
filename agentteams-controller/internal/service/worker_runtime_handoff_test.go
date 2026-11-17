package service

import (
	"errors"
	"fmt"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestHandoffPreflightConcurrentRequestsKeepOneOperation(t *testing.T) {
	source := WorkerStart{InstanceID: "docker/boot", ProcessID: "registered-start", Runtime: "hermes"}
	store := WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	etag := ""
	var winners int
	results := make(chan error, 8)
	for i := range cap(results) {
		go func() {
			_, _, err := store.Preflight(t.Context(), "agent", fmt.Sprintf("op-%d", i), "codex", "codex:fixture", source, etag)
			results <- err
		}()
	}
	for range cap(results) {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrWorkerStateConflict) {
			t.Fatal(err)
		}
	}
	after, newETag, err := store.Load(t.Context(), "agent")
	if err != nil || winners != 1 || after.Handoff.TargetRuntime != "codex" || newETag == etag {
		t.Fatalf("winners=%d after=%+v err=%v", winners, after, err)
	}
	if _, _, err = store.Preflight(t.Context(), "agent", "different", "qwenpaw", "qwenpaw:fixture", source, newETag); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("fresh ETag replaced an in-flight operation: %v", err)
	}
	draining, drainETag, err := store.BeginDrain(t.Context(), "agent")
	if err != nil || draining.Handoff.Phase != "draining" || drainETag == newETag {
		t.Fatalf("begin drain %v %v", draining, err)
	}
	retried, retryETag, err := store.Preflight(t.Context(), "agent", after.Handoff.ID, "codex", "codex:fixture", source, "")
	if err != nil || retried.Handoff.Phase != "draining" || retryETag != drainETag {
		t.Fatalf("lost-response retry %v %v", retried, err)
	}

}

func TestHandoffPreflightPreservesUnreadableFutureOrForeignState(t *testing.T) {
	storage := ossfake.NewMemory()
	store := WorkerRuntimeStore{Storage: storage}
	source := WorkerStart{InstanceID: "docker/boot", ProcessID: "registered-start", Runtime: "hermes"}
	for _, data := range []string{
		`{"schema_version":2,"kind":"WorkerRuntimeState","member_prefix":"agents/agent"}`,
		`{"schema_version":1,"kind":"WorkerRuntimeState","member_prefix":"agents/foreign","active_start":{"instance_id":"x","process_id":"p","runtime":"hermes"}}`,
		`{"schema_version":1,"kind":"WorkerRuntimeState","member_prefix":"agents/agent","active_start":{"instance_id":"x","process_id":"p","runtime":"hermes"},"new_field":42}`,
	} {
		key := "agents/agent/runtime/state.json"
		if err := storage.PutObject(t.Context(), key, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Preflight(t.Context(), "agent", "op", "codex", "codex:fixture", source, workerMatrixETag([]byte(data))); !errors.Is(err, ErrWorkerStateInvalid) {
			t.Fatalf("unexpected result %v", err)
		}
		saved, err := storage.GetObject(t.Context(), key)
		if err != nil || string(saved) != data {
			t.Fatalf("original record changed %q %v", saved, err)
		}
	}
}

func TestRuntimeHandoffPublishesRegisteredBootAndRepairsAfterWriterRestart(t *testing.T) {
	storage := ossfake.NewMemory()
	store := WorkerRuntimeStore{Storage: storage}
	matrix := WorkerMatrixStore{Storage: storage}
	ctx := t.Context()
	start := WorkerStart{InstanceID: "docker/first", ProcessID: "process1", Runtime: "hermes"}
	if state, _, err := store.SyncRegisteredStart(ctx, "alice", start.InstanceID, start.Runtime); err != nil || state.ActiveStart != nil {
		t.Fatalf("unregistered process published: %+v %v", state, err)
	}
	_, matrixETag, err := matrix.Start(ctx, "alice", start, "")
	if err != nil {
		t.Fatal(err)
	}
	state, etag, err := store.SyncRegisteredStart(ctx, "alice", start.InstanceID, start.Runtime)
	if err != nil || state.Handoff != nil || *state.ActiveStart != start || etag == "" {
		t.Fatalf("initial boot: %+v %v", state, err)
	}
	next := WorkerStart{InstanceID: "docker/second", ProcessID: "process2", Runtime: "hermes"}
	if _, _, err := matrix.Start(ctx, "alice", next, matrixETag); err != nil {
		t.Fatal(err)
	}
	fresh := WorkerRuntimeStore{Storage: storage}
	state, _, err = fresh.SyncRegisteredStart(ctx, "alice", next.InstanceID, next.Runtime)
	if err != nil || *state.ActiveStart != next {
		t.Fatalf("unplanned restart: %+v %v", state, err)
	}
	state, _, err = fresh.SyncRegisteredStart(ctx, "alice", start.InstanceID, start.Runtime)
	if err != nil || *state.ActiveStart != next {
		t.Fatalf("late source replaced current registration: %+v %v", state, err)
	}
}

func TestRuntimeHandoffRebindsOnlyProvenRegisteredSourceSuccessor(t *testing.T) {
	ctx := t.Context()
	store := WorkerRuntimeStore{Storage: ossfake.NewMemory()}
	original := WorkerStart{InstanceID: "docker/id/first", Runtime: "hermes", ProcessID: "old"}
	next := WorkerStart{InstanceID: "docker/id/second", Runtime: "hermes", ProcessID: "new"}
	if _, _, err := store.Preflight(ctx, "agent", "op", "codex", "codex:fixture", original, ""); err != nil {
		t.Fatal(err)
	}
	_, etag, err := store.BeginDrain(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	exit := backend.RuntimeExitObservation{Exited: true, Successor: "id/second"}
	state, etag, err := store.RecordSourceLoss(ctx, "agent", "op", etag, exit, nil, nil)
	if err != nil || state.Handoff.SourceLossReason != "successor_registration_pending" || state.Handoff.Source != original {
		t.Fatalf("unregistered source took over %+v %v", state, err)
	}
	forged := next
	forged.InstanceID = "docker/other"
	if _, _, err := store.RecordSourceLoss(ctx, "agent", "op", etag, exit, &forged, nil); !errors.Is(err, ErrWorkerStateInvalid) {
		t.Fatal("admitted unrelated source")
	}
	state, _, err = store.RecordSourceLoss(ctx, "agent", "op", etag, exit, &next, nil)
	if err != nil || state.Handoff.Phase != "draining" || len(state.Handoff.LostSources) != 1 || state.Handoff.LostSources[0] != original || state.Handoff.Source != next || *state.ActiveStart != next {
		t.Fatalf("lost original process or gate %+v %v", state, err)
	}
}
