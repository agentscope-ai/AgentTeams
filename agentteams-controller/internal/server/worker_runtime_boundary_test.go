package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func TestRuntimeHandoffConfirmationScansTaskAndMessageBeforeCheckpointing(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.h.recovery.project = newProjectTestHandler(t, f.storage, teamWithRoster("alpha-team"))
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", Team: "alpha-team"}
	f.expect("claim", nil, 200)
	store := service.WorkerRuntimeStore{Storage: f.storage}
	if _, _, err := store.Preflight(t.Context(), "runtime-alice", "switch", "codex", f.start, ""); err != nil {
		t.Fatal(err)
	}
	_, etag, err := store.BeginDrain(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	ack := func(proc string, want int) *service.WorkerRuntimeState {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"request_id": "switch", "instance_id": f.start.InstanceID, "process_id": proc})
		req := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/runtime/handoff/checkpoint", bytes.NewReader(data)), caller)
		req.Header.Set("X-AgentTeams-Process-ID", proc)
		req.Header.Set("If-Match", etag)
		w := httptest.NewRecorder()
		f.h.ConfirmHandoffBoundary(w, req)
		if w.Code != want {
			t.Fatalf("ack got %d want %d %s", w.Code, want, w.Body)
		}
		if want != 200 {
			return nil
		}
		var state service.WorkerRuntimeState
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return &state
	}
	// A previous shared checkpoint is not evidence that inference is stopped.
	taskExec := taskExecution{Generation: 4, InstanceID: f.start.InstanceID, ProcessID: f.start.ProcessID, Runtime: f.start.Runtime, State: "active", Checkpoint: &taskCheckpoint{Version: 2, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two"}}}
	saveTask := func() {
		putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alice", "status": "in_progress", "execution": taskExec}))
	}
	saveTask()
	ack("foreign-process", 409)
	ack(f.start.ProcessID, 409)
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"message-step"}}, 200)
	ack(f.start.ProcessID, 409)
	taskExec.State, taskExec.HandoffID = "released", "switch"
	taskExec.Checkpoint.Version = 3
	saveTask()
	state := ack(f.start.ProcessID, 200)
	refs := state.Handoff.CheckpointRefs
	if state.Handoff.Phase != "checkpointed" || len(refs) != 2 ||
		refs[0].Kind != "message" || refs[0].EventKey != f.key || refs[0].CheckpointVersion != 1 ||
		refs[1].Kind != "task" || refs[1].TeamID != "alpha-team" || refs[1].TaskID != "t1" || refs[1].Generation != 4 || refs[1].CheckpointVersion != 3 {
		t.Fatalf("incomplete handoff references %+v", state)
	}
	ack(f.start.ProcessID, 200) // Original drain ETag after a lost response.
	f.expect("claim", nil, 409)
}
