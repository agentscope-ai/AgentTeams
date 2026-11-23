package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"context"
	"crypto/sha256"
	"encoding/hex"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

func callTargetReady(t *testing.T, f *matrixExecutionFixture, caller *authpkg.CallerIdentity, etag string, want int) *service.WorkerRuntimeState {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"request_id": "switch", "instance_id": f.start.InstanceID, "process_id": f.start.ProcessID})
	r := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/runtime/handoff/recovered", bytes.NewReader(body)), caller)
	r.Header.Set("If-Match", etag)
	r.Header.Set("X-AgentTeams-Process-ID", f.start.ProcessID)
	w := httptest.NewRecorder()
	f.h.ConfirmHandoffRecovery(w, r)
	if w.Code != want {
		t.Fatalf("target ready got %d want %d %s", w.Code, want, w.Body)
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

func TestRuntimeHandoffReadyWithUnfinishedTaskAndMessage(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	// No new progress was saved before the old process stopped.
	f.h.recovery.project = newProjectTestHandler(t, f.storage, teamWithRoster("alpha-team"))
	putTask(f.storage, taskMetaKey, taskMeta(map[string]any{
		"assigned_to": "alice", "status": "in_progress",
		"execution": taskExecution{Generation: 1, InstanceID: f.start.InstanceID, ProcessID: f.start.ProcessID, Runtime: f.start.Runtime, State: "needs_verification",
			Checkpoint: &taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two"},
				Operation: &taskOperation{ID: "native-turn:" + f.start.ProcessID, State: "unknown"}}},
	}))
	store := service.WorkerRuntimeStore{Storage: f.storage}
	ctx := t.Context()
	state, etag, err := store.Preflight(ctx, "runtime-alice", "switch", "qwenpaw", "qwenpaw:fixture", f.start, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range [][2]string{{"stopping", "stopped"}, {"stopped", "creating"}} {
		_, etag, err = store.AdvanceLifecycle(ctx, "runtime-alice", "switch", transition[0], transition[1], etag, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.replace()
	_, etag, err = store.AdvanceLifecycle(ctx, "runtime-alice", "switch", "creating", "recovering", etag, &f.start)
	if err != nil {
		t.Fatal(err)
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice"}
	b := mocks.NewMockWorkerBackend()
	b.NameOverride = "actual"
	actual := backend.WorkerResult{Status: backend.StatusStarting, Runtime: "qwenpaw", InstanceID: "target/start", HandoffLabel: state.Handoff.TargetLabel()}
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) { return &actual, nil }
	f.h.deps.Backend = backend.NewRegistry([]backend.WorkerBackend{b})
	callTargetReady(t, f, caller, etag, 409)
	actual.Status = backend.StatusReady
	state = callTargetReady(t, f, caller, etag, 200)
	if state.Handoff.Phase != "completed" {
		t.Fatalf("unfinished work blocked readiness: %+v", state.Handoff)
	}
	claimed := f.expect("claim", nil, 200)
	var execution taskExecution
	if err := json.Unmarshal(claimed["execution"], &execution); err != nil || execution.Generation != 2 || execution.ProcessID != f.start.ProcessID || execution.Checkpoint != nil {
		t.Fatalf("target did not resume uncheckpointed work: %+v %v", execution, err)
	}

	body, _ := json.Marshal(map[string]string{"action": "claim", "instance_id": f.start.InstanceID, "process_id": f.start.ProcessID})
	caller.Team = "alpha-team"
	req := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/execution?team=alpha-team", bytes.NewReader(body)), caller)
	req.SetPathValue("id", "t1")
	req.Header.Set("If-Match", taskMetaETag(t, f.storage, taskMetaKey))
	resp := httptest.NewRecorder()
	f.h.recovery.UpdateExecution(resp, req)
	var resumed struct {
		Execution taskExecution
	}
	if resp.Code != 200 || json.Unmarshal(resp.Body.Bytes(), &resumed) != nil {
		t.Fatalf("target could not resume interrupted task: %d %s", resp.Code, resp.Body)
	}
	cp := resumed.Execution.Checkpoint
	if resumed.Execution.State != "active" || resumed.Execution.Generation != 2 || cp == nil ||
		cp.Operation != nil || !slices.Equal(cp.CompletedSteps, []string{"one"}) || !slices.Equal(cp.RemainingSteps, []string{"two"}) {
		t.Fatalf("latest saved progress did not survive interruption: %+v", resumed.Execution)
	}
}

func TestRuntimeHandoffAcceptedTaskWithSavedReplyPermitsDeliveryOnly(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	store := f.storage
	const key = "teams/alpha-team/shared/tasks/t1/checkpoints/reply.txt"
	data := []byte("saved reply")
	digest := sha256.Sum256(data)
	result := taskArtifact{Key: key, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
	if err := store.PutObject(t.Context(), key, data); err != nil {
		t.Fatal(err)
	}
	cp := taskCheckpoint{Version: 1, CompletedSteps: []string{"one"}, Result: &result, Delivery: "pending", ReplyTxn: "fixed"}
	execution := taskExecution{Generation: 1, InstanceID: f.start.InstanceID, ProcessID: f.start.ProcessID, Runtime: f.start.Runtime, State: "active", Checkpoint: &cp}
	putTask(store, taskMetaKey, taskMeta(map[string]any{"assigned_to": "alice", "status": "completed", "execution": execution}))
	project := newProjectTestHandler(t, store, teamWithRoster("alpha-team"))
	f.h.recovery.project = project
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", Team: "alpha-team"}
	f.replace()
	call := func(action string, next *taskCheckpoint, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": action, "instance_id": f.start.InstanceID, "process_id": f.start.ProcessID, "checkpoint": next})
		req := withCaller(httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/execution?team=alpha-team", bytes.NewReader(body)), caller)
		req.SetPathValue("id", "t1")
		req.Header.Set("If-Match", taskMetaETag(t, store, taskMetaKey))
		w := httptest.NewRecorder()
		f.h.recovery.UpdateExecution(w, req)
		if w.Code != want {
			t.Fatalf("%s got %d want %d %s", action, w.Code, want, w.Body)
		}
	}
	call("claim", nil, 200)
	cp.Version++
	cp.Delivery, cp.ReplyEvent = "delivered", "$idempotent-event"
	cp.CompletedSteps = []string{"one", "unexpected"}
	call("checkpoint", &cp, 409)
	cp.CompletedSteps = []string{"one"}
	call("checkpoint", &cp, 200)
	call("claim", nil, 409)
}

func TestRuntimeHandoffRegisteredWorkerRejectsForgedTaskProcess(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	ctx := t.Context()
	store := service.WorkerRuntimeStore{Storage: f.storage}
	if _, _, err := store.SyncRegisteredStart(ctx, "runtime-alice", f.start.InstanceID, f.start.Runtime); err != nil {
		t.Fatal(err)
	}
	forged := f.start
	forged.ProcessID = "another-process"
	if _, err := executionHandoff(ctx, f.storage, "runtime-alice", forged); !errors.Is(err, service.ErrWorkerStateConflict) {
		t.Fatalf("forged task process admitted: %v", err)
	}
	if op, err := executionHandoff(ctx, f.storage, "runtime-alice", f.start); err != nil || op != nil {
		t.Fatalf("registered process refused: %+v %v", op, err)
	}
}
