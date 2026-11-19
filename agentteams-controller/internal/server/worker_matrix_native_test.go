package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func (f *matrixExecutionFixture) native(method string, body any, etag, process string) *httptest.ResponseRecorder {
	f.t.Helper()
	data, _ := json.Marshal(body)
	r := withCaller(httptest.NewRequest(method, "/api/v1/runtime/matrix/events/"+f.key+"/native", strings.NewReader(string(data))),
		&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"})
	r.SetPathValue("key", f.key)
	r.Header.Set("If-Match", etag)
	r.Header.Set("X-AgentTeams-Process-ID", process)
	w := httptest.NewRecorder()
	if method == "GET" {
		f.h.GetNativeInput(w, r)
	} else {
		f.h.CommitNativeInput(w, r)
	}
	return w
}

func TestWorkerMatrixNativeInputIsPersistenceEvidenceNotTaskExecution(t *testing.T) {
	f := newMatrixExecutionFixture(t, &service.WorkerMatrixWork{Kind: "task", TeamID: "team", TaskID: "T"})
	view := sessionBody(t, f.native("GET", nil, "", ""))
	body := map[string]any{"instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "native_ref": "native-session", "native_digest": strings.Repeat("a", 64)}
	saved := sessionBody(t, f.native("POST", body, view["etag"].(string), f.start.ProcessID))
	if saved["current"] != true {
		t.Fatal("matching current native persistence facts were not retained")
	}
	input, _, err := f.h.store.InputSnapshot(t.Context(), "runtime-alice", f.key)
	if err != nil || input.InputState != "native_committed" || input.NativeCommit == nil || input.NativeCommit.Start != f.start || len(input.Execution) != 0 {
		t.Fatalf("native read changed Task authority or lost start binding: %+v %v", input, err)
	}
	state, _, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil || len(state.Pending) != 1 {
		t.Fatal("Task notification native input was mistaken for completed work")
	}
	f.replace()
	if view := sessionBody(t, f.native("GET", nil, "", "")); view["current"] != false {
		t.Fatal("source Harness native receipt proved target persistence")
	}
	if r := f.native("POST", body, saved["etag"].(string), "source-process"); r.Code != 409 {
		t.Fatalf("old native callback accepted after replacement: %d %s", r.Code, r.Body)
	}
}

func TestWorkerMatrixNativeCommitPreservesNewCheckpointAndRejectsOldETag(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	view := sessionBody(t, f.native("GET", nil, "", ""))
	f.expect("claim", nil, 200)
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}}, 200)
	body := map[string]any{"instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "native_ref": "native-session", "native_digest": strings.Repeat("a", 64)}
	if r := f.native("POST", body, view["etag"].(string), f.start.ProcessID); r.Code != 409 {
		t.Fatalf("native callback overwrote newer checkpoint: %d %s", r.Code, r.Body)
	}
	view = sessionBody(t, f.native("GET", nil, "", ""))
	saved := sessionBody(t, f.native("POST", body, view["etag"].(string), f.start.ProcessID))
	input, _, _ := f.h.store.InputSnapshot(t.Context(), "runtime-alice", f.key)
	var execution taskExecution
	_ = json.Unmarshal(input.Execution, &execution)
	if execution.Checkpoint.Version != 1 || len(execution.Checkpoint.CompletedSteps) != 1 {
		t.Fatal("native receipt erased authoritative progress")
	}
	before, _ := f.storage.GetObject(t.Context(), f.objectKey())
	again := sessionBody(t, f.native("POST", body, saved["etag"].(string), f.start.ProcessID))
	after, _ := f.storage.GetObject(t.Context(), f.objectKey())
	if string(before) != string(after) || again["etag"] != saved["etag"] {
		t.Fatal("duplicate native acknowledgement rewrote execution evidence")
	}
}

func TestWorkerMatrixInFlightNativeFactRemainsSourceBoundAfterReplacement(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	view := sessionBody(t, f.native("GET", nil, "", ""))
	storage := &matrixExecutionCASStore{Memory: f.storage, eventKey: f.objectKey()}
	f.h.deps.OSS = storage
	storage.before = f.replace
	body := map[string]any{"instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "native_ref": "source/session/row", "native_digest": strings.Repeat("a", 64)}
	response := sessionBody(t, f.native("POST", body, view["etag"].(string), f.start.ProcessID))
	if response["current"] != false {
		t.Fatal("in-flight source persistence evidence was promoted to target state")
	}
	input, _, _ := f.h.store.InputSnapshot(t.Context(), "runtime-alice", f.key)
	if input.NativeCommit.Start.Runtime != "hermes" || len(input.Execution) != 0 {
		t.Fatal("old native input fact changed target execution authority")
	}
}

func TestWorkerMatrixNativeContextCanRetireWithoutCompletingMessageWork(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	messageKey := f.key
	_, etag, _ := f.h.store.Load(t.Context(), "runtime-alice")
	input := service.WorkerMatrixInput{RoomID: "!room:test", EventID: "$context", Intent: "context",
		Input: service.WorkerMatrixContent{Sender: "@human:test", Type: "m.room.message", Content: map[string]any{"body": "context only"}}}
	if _, _, err := f.h.store.Ingest(t.Context(), "runtime-alice", f.start, etag, service.WorkerMatrixConsumer{}, []service.WorkerMatrixInput{input}, nil); err != nil {
		t.Fatal(err)
	}
	f.key = service.WorkerMatrixEventKey(input.RoomID, input.EventID)
	view := sessionBody(t, f.native("GET", nil, "", ""))
	body := map[string]any{"instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "native_ref": "session/context-row", "native_digest": strings.Repeat("a", 64)}
	sessionBody(t, f.native("POST", body, view["etag"].(string), f.start.ProcessID))
	state, _, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil || len(state.Pending) != 1 || state.Pending[0] != messageKey {
		t.Fatalf("native context retired actual message execution: %+v %v", state, err)
	}
	if _, err := f.h.store.Input(t.Context(), "runtime-alice", f.key); err != nil {
		t.Fatal("retiring pending context deleted retained history")
	}
}
