package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type matrixExecutionFixture struct {
	t       *testing.T
	h       *WorkerMatrixHandler
	storage *ossfake.Memory
	start   service.WorkerStart
	key     string
}

func newMatrixExecutionFixture(t *testing.T, work *service.WorkerMatrixWork) *matrixExecutionFixture {
	t.Helper()
	storage := ossfake.NewMemory()
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1beta1.WorkerSpec{WorkerName: "runtime-alice"}}
	deps := ServerDeps{Namespace: "default", OSS: storage, Client: fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(worker).Build()}
	f := &matrixExecutionFixture{t: t, storage: storage, start: service.WorkerStart{InstanceID: "actual/source/start", ProcessID: "source-process", Runtime: "hermes"}, key: service.WorkerMatrixEventKey("!room:test", "$input")}
	recovery := NewTaskRecoveryHandler(nil, deps)
	recovery.instance = func(context.Context, *authpkg.CallerIdentity) (string, string, error) {
		return f.start.InstanceID, f.start.Runtime, nil
	}
	f.h = NewWorkerMatrixHandler(deps, recovery)
	_, etag, err := f.h.store.Start(t.Context(), "runtime-alice", f.start, "")
	if err != nil {
		t.Fatal(err)
	}
	input := service.WorkerMatrixInput{RoomID: "!room:test", EventID: "$input", Intent: "message", Work: work,
		Input: service.WorkerMatrixContent{Sender: "@human:test", Type: "m.room.message", Content: map[string]any{"body": "complete the remaining work"}}}
	if _, _, err := f.h.store.Ingest(t.Context(), "runtime-alice", f.start, etag, service.WorkerMatrixConsumer{}, []service.WorkerMatrixInput{input}, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *matrixExecutionFixture) objectKey() string {
	return "agents/runtime-alice/runtime/matrix/events/" + f.key + ".json"
}

func (f *matrixExecutionFixture) call(action string, cp *taskCheckpoint) *httptest.ResponseRecorder {
	f.t.Helper()
	input, etag, err := f.h.store.InputSnapshot(f.t.Context(), "runtime-alice", f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	var execution taskExecution
	if len(input.Execution) != 0 {
		if err := json.Unmarshal(input.Execution, &execution); err != nil {
			f.t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]any{"action": action, "instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "generation": execution.Generation, "checkpoint": cp})
	r := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/runtime/matrix/events/"+f.key+"/execution", strings.NewReader(string(body))), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"})
	r.SetPathValue("key", f.key)
	r.Header.Set("X-AgentTeams-Process-ID", f.start.ProcessID)
	r.Header.Set("If-Match", etag)
	w := httptest.NewRecorder()
	f.h.UpdateExecution(w, r)
	return w
}

func (f *matrixExecutionFixture) expect(action string, cp *taskCheckpoint, code int) map[string]json.RawMessage {
	f.t.Helper()
	r := f.call(action, cp)
	if r.Code != code {
		f.t.Fatalf("%s: got %d, want %d: %s", action, r.Code, code, r.Body)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *matrixExecutionFixture) replace() {
	f.t.Helper()
	_, etag, err := f.h.store.Load(f.t.Context(), "runtime-alice")
	if err != nil {
		f.t.Fatal(err)
	}
	f.start = service.WorkerStart{InstanceID: "actual/target/start", ProcessID: "target-process", Runtime: "qwenpaw"}
	if _, _, err := f.h.store.Start(f.t.Context(), "runtime-alice", f.start, etag); err != nil {
		f.t.Fatal(err)
	}
}

func (f *matrixExecutionFixture) artifact(name, content string) taskArtifact {
	f.t.Helper()
	data := []byte(content)
	sum := sha256.Sum256(data)
	artifact := taskArtifact{Key: "agents/runtime-alice/runtime/matrix/results/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	if err := f.storage.PutObject(f.t.Context(), artifact.Key, data); err != nil {
		f.t.Fatal(err)
	}
	return artifact
}

func TestMatrixMessageSavedResultResumesDeliveryInsteadOfInference(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	result := f.artifact("reply.txt", "saved final answer")
	cp := &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{}, Result: &result, Delivery: "pending", ReplyTxn: "stable-message-reply"}
	f.expect("checkpoint", cp, 200)
	f.replace()
	response := f.expect("claim", nil, 200)
	var execution taskExecution
	if err := json.Unmarshal(response["execution"], &execution); err != nil {
		t.Fatal(err)
	}
	if string(response["recovery"]) != `"deliver"` || string(response["saved_result"]) != `"saved final answer"` || execution.Generation != 2 || execution.Checkpoint.ReplyTxn != "stable-message-reply" {
		t.Fatalf("saved work was not retained across Harness replacement: %s", response)
	}
	cp.Version = 2
	cp.Delivery, cp.ReplyEvent = "delivered", "$one-canonical-reply"
	f.expect("checkpoint", cp, 200)
	if response := f.expect("claim", nil, 200); string(response["recovery"]) != `"done"` {
		t.Fatalf("delivered message restarted: %s", response)
	}
	state, _, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil || len(state.Pending) != 0 {
		t.Fatalf("delivered input remains actionable: %+v %v", state, err)
	}
}

func TestMatrixMessageUnknownEffectRequiresVerificationAfterReplacement(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	cp := &taskCheckpoint{Version: 1, RemainingSteps: []string{"one"}, Operation: &taskOperation{ID: "external-operation", State: "started"}}
	f.expect("checkpoint", cp, 200)
	f.replace()
	response := f.expect("claim", nil, 200)
	if string(response["recovery"]) != `"verify"` {
		t.Fatalf("unknown operation entered normal inference: %s", response)
	}
	cp.Version = 2
	cp.Operation = &taskOperation{ID: "blind-new-operation", State: "started"}
	f.expect("checkpoint", cp, 409)
	artifact := f.artifact("effect.txt", "external operation executed once")
	cp.Operation = &taskOperation{ID: "external-operation", State: "verified", Evidence: "external receipt and digest"}
	cp.Artifacts = []taskArtifact{artifact}
	cp.CompletedSteps, cp.RemainingSteps = []string{"one"}, []string{}
	if response := f.expect("checkpoint", cp, 200); string(response["recovery"]) != `"resume"` {
		t.Fatalf("verified work did not become resumable: %s", response)
	}
}

func TestMatrixMessageRejectsUnpersistedResultsAndTaskAuthorityCopies(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	result := taskArtifact{Key: "agents/runtime-alice/runtime/matrix/results/missing", SHA256: strings.Repeat("0", 64), Size: 10}
	f.expect("checkpoint", &taskCheckpoint{Version: 1, Result: &result, Delivery: "pending", ReplyTxn: "stable"}, 409)
	other := newMatrixExecutionFixture(t, &service.WorkerMatrixWork{Kind: "task", TeamID: "team", TaskID: "T"})
	other.expect("claim", nil, 409)
	input, err := other.h.store.Input(t.Context(), "runtime-alice", other.key)
	if err != nil || len(input.Execution) != 0 {
		t.Fatalf("Task execution was duplicated in the Worker ledger: %+v %v", input, err)
	}
}

func TestMatrixMessageCancellationCannotBeOverwrittenByLateReply(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	f.expect("cancel", nil, 200)
	result := f.artifact("late.txt", "late native final callback")
	f.expect("checkpoint", &taskCheckpoint{Version: 1, Result: &result, Delivery: "pending", ReplyTxn: "late"}, 409)
	if response := f.expect("claim", nil, 200); string(response["recovery"]) != `"done"` {
		t.Fatalf("cancelled message resumed: %s", response)
	}
}

func TestMatrixMessageRejectsOldProcessAndOldExecutionGeneration(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	old := f.start
	f.replace()
	f.expect("claim", nil, 200)
	for _, change := range []string{"old-process", "old-generation"} {
		_, etag, err := f.h.store.InputSnapshot(t.Context(), "runtime-alice", f.key)
		if err != nil {
			t.Fatal(err)
		}
		process := f.start.ProcessID
		if change == "old-process" {
			process = old.ProcessID
		}
		body, _ := json.Marshal(map[string]any{"action": "checkpoint", "instance_id": f.start.InstanceID, "process_id": process, "generation": 1, "checkpoint": &taskCheckpoint{Version: 1}})
		r := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/runtime/matrix/events/"+f.key+"/execution", strings.NewReader(string(body))), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"})
		r.SetPathValue("key", f.key)
		r.Header.Set("If-Match", etag)
		r.Header.Set("X-AgentTeams-Process-ID", process)
		w := httptest.NewRecorder()
		f.h.UpdateExecution(w, r)
		if w.Code != 409 {
			t.Fatalf("%s callback was admitted: %d %s", change, w.Code, w.Body)
		}
	}
}

type matrixExecutionCASStore struct {
	*ossfake.Memory
	eventKey string
	before   func()
}

func (s *matrixExecutionCASStore) PutObjectIfMatchStrict(ctx context.Context, key string, data []byte, etag string) error {
	if key == s.eventKey && s.before != nil {
		callback := s.before
		s.before = nil
		callback()
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, data, etag)
}

func TestMatrixMessageInFlightCheckpointCannotUndoCancellation(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	storage := &matrixExecutionCASStore{Memory: f.storage, eventKey: f.objectKey()}
	f.h.deps.OSS = storage
	storage.before = func() { f.expect("cancel", nil, 200) }
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"late"}}, 409)
	input, err := f.h.store.Input(t.Context(), "runtime-alice", f.key)
	if err != nil {
		t.Fatal(err)
	}
	var execution taskExecution
	if err := json.Unmarshal(input.Execution, &execution); err != nil {
		t.Fatal(err)
	}
	if execution.State != "cancelled" || execution.Checkpoint != nil {
		t.Fatalf("late checkpoint overwrote cancellation: %+v", execution)
	}
}

func TestMatrixMessageSavedResultCannotHideNewWorkInDeliveryAck(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	result := f.artifact("final.txt", "saved answer")
	cp := &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, Result: &result, Delivery: "pending", ReplyTxn: "stable"}
	f.expect("checkpoint", cp, 200)
	cp.Version, cp.Delivery, cp.ReplyEvent = 2, "delivered", "$reply"
	cp.CompletedSteps = []string{"one", "unexpected-new-work"}
	f.expect("checkpoint", cp, 409)
	cp.CompletedSteps = []string{"one"}
	f.expect("checkpoint", cp, 200)
}

func TestMatrixMessageGapBlocksInferenceButAllowsSavedResultDelivery(t *testing.T) {
	for _, saved := range []bool{false, true} {
		f := newMatrixExecutionFixture(t, nil)
		if saved {
			f.expect("claim", nil, 200)
			result := f.artifact("reply.txt", "already executed answer")
			f.expect("checkpoint", &taskCheckpoint{Version: 1, Result: &result, Delivery: "pending", ReplyTxn: "same-txn"}, 200)
		}
		_, etag, err := f.h.store.Load(t.Context(), "runtime-alice")
		if err != nil {
			t.Fatal(err)
		}
		_, etag, err = f.h.store.Ingest(t.Context(), "runtime-alice", f.start, etag, service.WorkerMatrixConsumer{}, nil,
			map[string]service.WorkerMatrixRoom{"!room:test": {Through: "$input"}})
		if err != nil {
			t.Fatal(err)
		}
		newInput := service.WorkerMatrixInput{RoomID: "!room:test", EventID: "$new", Intent: "context",
			Input: service.WorkerMatrixContent{Sender: "@human:test", Type: "m.room.message", Content: map[string]any{"body": "later context"}}}
		_, _, err = f.h.store.Ingest(t.Context(), "runtime-alice", f.start, etag, service.WorkerMatrixConsumer{}, []service.WorkerMatrixInput{newInput},
			map[string]service.WorkerMatrixRoom{"!room:test": {Through: "$new", Gaps: []string{"limited-gap"}}})
		if err != nil {
			t.Fatal(err)
		}
		if !saved {
			f.expect("claim", nil, 409)
		} else if response := f.expect("claim", nil, 200); string(response["recovery"]) != `"deliver"` {
			t.Fatal("history gap forced already saved work into normal inference")
		}
		r := withCaller(httptest.NewRequest("GET", "/unused", nil), &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"})
		r.SetPathValue("key", f.key)
		w := httptest.NewRecorder()
		f.h.GetExecution(w, r)
		var response struct {
			Recovery string `json:"recovery"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || (!saved && response.Recovery != "backfill") || (saved && response.Recovery != "deliver") {
			t.Fatalf("room-aware admission: %d %s", w.Code, w.Body)
		}
	}
}
