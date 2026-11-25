package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestClaudeCodeHandoffRecordsEitherDirection(t *testing.T) {
	for _, pair := range [][2]string{{"claudecode", "qwenpaw"}, {"qwenpaw", "claudecode"}} {
		store := WorkerRuntimeStore{Storage: ossfake.NewMemory()}
		source := WorkerStart{InstanceID: "docker/boot", ProcessID: "registered-start", Runtime: pair[0]}
		got, _, err := store.Preflight(t.Context(), "agent", "switch", pair[1], "image:dev", source, "")
		if err != nil || got.Handoff.Source != source || got.Handoff.TargetRuntime != pair[1] || got.Handoff.Phase != "stopping" {
			t.Fatalf("%s to %s: got %+v, %v", pair[0], pair[1], got, err)
		}
	}
}

func TestClaudeCodeHandoffRejectsEncryptionBeforeRoomCheck(t *testing.T) {
	w := &v1beta1.Worker{}
	calls := 0
	m := WorkerHandoffMatrix{CheckPlaintextRooms: func(context.Context, string) error { calls++; return nil }}
	for _, value := range []string{"1", "true", "TRUE"} {
		w.Spec.Env = map[string]string{"AGENTTEAMS_MATRIX_E2EE": value}
		if err := m.Validate(t.Context(), w, backend.RuntimeClaudeCode); !errors.Is(err, ErrWorkerStateConflict) {
			t.Fatalf("encrypted ClaudeCode handoff accepted: %v", err)
		}
	}
	if calls != 0 {
		t.Fatal("encrypted handoff reached room checks")
	}
	w.Spec.Env = nil
	if err := m.Validate(t.Context(), w, backend.RuntimeClaudeCode); err != nil || calls != 1 {
		t.Fatalf("plaintext handoff did not check rooms: %v, calls=%d", err, calls)
	}
}

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
	retried, retryETag, err := store.Preflight(t.Context(), "agent", after.Handoff.ID, "codex", "codex:fixture", source, "")
	if err != nil || retried.Handoff.Phase != "stopping" || retryETag != newETag {
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
	_, etag, err := store.Preflight(ctx, "agent", "op", "codex", "codex:fixture", original, "")
	if err != nil {
		t.Fatal(err)
	}
	forged := next
	forged.InstanceID = "docker/other"
	if _, _, err := store.RebindSource(ctx, "agent", "op", etag, original, forged); !errors.Is(err, ErrWorkerStateInvalid) {
		t.Fatal("admitted unrelated source")
	}
	state, _, err := store.RebindSource(ctx, "agent", "op", etag, original, next)
	if err != nil || state.Handoff.Phase != "stopping" || len(state.Handoff.LostSources) != 1 || state.Handoff.LostSources[0] != original || state.Handoff.Source != next || *state.ActiveStart != next {
		t.Fatalf("lost original process or gate %+v %v", state, err)
	}
}

func TestRuntimeHandoffPlaintextOnlyTargetChecksJoinedRoomsAsWorker(t *testing.T) {
	status := http.StatusNotFound
	absent := true
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer existing-worker-session" {
			t.Errorf("preflight must only read using the existing Worker session")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			fmt.Fprint(w, `{"joined_rooms":["!room:example"]}`)
		case strings.HasSuffix(r.URL.Path, "/rooms/!room:example/state/m.room.encryption/"):
			w.WriteHeader(status)
			if status == http.StatusNotFound && absent {
				fmt.Fprint(w, `{"errcode":"M_NOT_FOUND"}`)
			}
			if status == http.StatusOK {
				fmt.Fprint(w, `{"algorithm":"m.megolm.v1.aes-sha2"}`)
			}
		default:
			t.Errorf("unexpected Matrix request %q", r.URL.Path)
		}
	}))
	defer server.Close()
	p := Provisioner{matrix: matrix.NewTuwunelClient(matrix.Config{ServerURL: server.URL}, server.Client()), creds: fakeCredentialStore{"member-cr": {MatrixToken: "existing-worker-session"}}}
	w := &v1beta1.Worker{}
	w.Name = "member-cr"
	w.Spec.WorkerName = "stable-name"
	m := p.HandoffMatrix()
	if err := m.Validate(t.Context(), w, "codex"); err != nil || calls != 2 {
		t.Fatalf("plaintext room not verified: %v, calls=%d", err, calls)
	}
	absent = false
	if err := m.Validate(t.Context(), w, "codex"); err == nil {
		t.Fatal("treated a proxy 404 as a plaintext room")
	}
	status = http.StatusOK
	if err := m.Validate(t.Context(), w, "deepseek-harness"); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("accepted encrypted room: %v", err)
	}
	status = http.StatusForbidden
	if err := m.Validate(t.Context(), w, "codex"); err == nil || errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("treated an unreadable room as known: %v", err)
	}
	lastCalls := calls
	w.Spec.Env = map[string]string{"AGENTTEAMS_MATRIX_E2EE": "TRUE"}
	if err := m.Validate(t.Context(), w, "codex"); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("accepted worker E2EE environment: %v", err)
	}
	w.Spec.Env["AGENTTEAMS_MATRIX_E2EE"] = "0"
	m.E2EEEnabled = true
	if err := m.Validate(t.Context(), w, "deepseek-harness"); !errors.Is(err, ErrWorkerStateConflict) {
		t.Fatalf("worker override bypassed cluster E2EE: %v", err)
	}
	if err := m.Validate(t.Context(), w, "hermes"); err != nil || calls != lastCalls {
		t.Fatalf("unnecessary Matrix request, error=%v, calls=%d", err, calls)
	}
}
