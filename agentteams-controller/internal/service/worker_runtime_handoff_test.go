package service

import (
	"errors"
	"fmt"
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
