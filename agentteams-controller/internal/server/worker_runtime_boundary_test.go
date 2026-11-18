package server

import (
	"bytes"
	"encoding/json"
	"errors"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"

	"context"
	"crypto/sha256"
	"encoding/hex"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/mocks"
)

func TestRuntimeHandoffConfirmationScansTaskAndMessageBeforeCheckpointing(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.h.recovery.project = newProjectTestHandler(t, f.storage, teamWithRoster("alpha-team"))
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", Team: "alpha-team"}
	f.expect("claim", nil, 200)
	store := service.WorkerRuntimeStore{Storage: f.storage}
	if _, _, err := store.Preflight(t.Context(), "runtime-alice", "switch", "codex", "codex:fixture", f.start, ""); err != nil {
		t.Fatal(err)
	}
	_, etag, err := store.BeginDrain(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	probe := false
	ack := func(proc string, want int) *service.WorkerRuntimeState {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"request_id": "switch", "instance_id": f.start.InstanceID, "process_id": proc})
		endpoint := "/api/v1/runtime/handoff/checkpoint"
		if probe {
			endpoint += "?probe=true"
		}
		req := withCaller(httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data)), caller)
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
	probe = true
	ack(f.start.ProcessID, 409)
	probe = false
	ack("foreign-process", 409)
	ack(f.start.ProcessID, 409)
	f.expect("checkpoint", &taskCheckpoint{Version: 1, CompletedSteps: []string{"message-step"}}, 200)
	ack(f.start.ProcessID, 409)
	taskExec.State, taskExec.HandoffID = "released", "switch"
	taskExec.Checkpoint.Version = 3
	saveTask()
	probe = true
	if state := ack(f.start.ProcessID, 200); state.Handoff.Phase != "draining" {
		t.Fatal("probe committed a checkpoint")
	}
	_, afterProbe, err := store.Load(t.Context(), "runtime-alice")
	if err != nil || afterProbe != etag {
		t.Fatal("probe mutated runtime state")
	}
	probe = false
	state := ack(f.start.ProcessID, 200)
	refs := state.Handoff.CheckpointRefs
	if state.Handoff.Phase != "checkpointed" || len(refs) != 2 ||
		refs[0].Kind != "message" || refs[0].EventKey != f.key || refs[0].CheckpointVersion != 1 ||
		refs[1].Kind != "task" || refs[1].TeamID != "alpha-team" || refs[1].TaskID != "t1" || refs[1].Generation != 4 || refs[1].CheckpointVersion != 3 {
		t.Fatalf("incomplete handoff references %+v", state)
	}
	ack(f.start.ProcessID, 200) // Original drain ETag after a lost response.
	f.expect("claim", nil, 409)
	_, rev, err := store.Load(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range [][2]string{{"checkpointed", "stopping"}, {"stopping", "stopped"}, {"stopped", "creating"}} {
		_, rev, err = store.AdvanceLifecycle(t.Context(), "runtime-alice", "switch", transition[0], transition[1], rev, "codex:fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, matrixRev, err := f.h.store.Load(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	f.start = service.WorkerStart{InstanceID: "actual/target/start", ProcessID: "target-process", Runtime: "codex"}
	if _, _, err = f.h.store.Start(t.Context(), "runtime-alice", f.start, matrixRev); err != nil {
		t.Fatal(err)
	}
	f.expect("claim", nil, 409)
	_, _, err = store.AdvanceLifecycle(t.Context(), "runtime-alice", "switch", "creating", "recovering", rev, "", &f.start)
	if err != nil {
		t.Fatal(err)
	}
	claimed := f.expect("claim", nil, 200)
	var targetExecution taskExecution
	if err := json.Unmarshal(claimed["execution"], &targetExecution); err != nil || targetExecution.Generation != 2 || targetExecution.State != "active" || targetExecution.HandoffID != "" || targetExecution.Checkpoint.Version != 1 {
		t.Fatalf("target claim did not preserve source checkpoint: %+v %v", targetExecution, err)
	}

	backendStatus := backend.WorkerResult{Status: backend.StatusRunning, Runtime: "codex", InstanceID: "target/start", HandoffLabel: state.Handoff.TargetLabel()}
	b := mocks.NewMockWorkerBackend()
	b.NameOverride = "actual"
	b.StatusFn = func(context.Context, string) (*backend.WorkerResult, error) { return &backendStatus, nil }
	f.h.deps.Backend = backend.NewRegistry([]backend.WorkerBackend{b})
	_, rev, err = store.Load(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	recovered := func(want int) *service.WorkerRuntimeState {
		t.Helper()
		return callTargetReady(t, f, caller, rev, want)
	}
	recovered(409) // Claim, no new step.
	f.expect("checkpoint", &taskCheckpoint{Version: 2, CompletedSteps: []string{"message-step"}}, 200)
	recovered(409) // A higher version alone is no progress.
	f.expect("checkpoint", &taskCheckpoint{Version: 3, CompletedSteps: []string{"message-step", "target-step"}}, 200)
	recovered(409) // One of the two references still belongs to the source.
	taskExec.Generation++
	taskExec.InstanceID, taskExec.ProcessID, taskExec.Runtime = f.start.InstanceID, f.start.ProcessID, f.start.Runtime
	taskExec.State, taskExec.HandoffID = "active", ""
	taskExec.Checkpoint.Version++
	taskExec.Checkpoint.CompletedSteps = append(taskExec.Checkpoint.CompletedSteps, "two")
	saveTask()
	final := recovered(200)
	if final.Handoff.Phase != "completed" || final.Handoff.SourceStopObservedAt == "" || final.Handoff.TargetRegisteredAt == "" || final.Handoff.CompletedAt == "" || len(final.Handoff.CheckpointRefs) != 2 {
		t.Fatalf("completion evidence %+v", final.Handoff)
	}
	for _, ref := range final.Handoff.CheckpointRefs {
		if ref.Outcome != "advanced" || ref.TargetGeneration <= ref.Generation || ref.TargetCheckpointVersion <= ref.CheckpointVersion {
			t.Fatalf("missing target evidence %+v", ref)
		}
	}
	recovered(200)
	_, doneETag, err := store.Load(t.Context(), "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Preflight(t.Context(), "runtime-alice", "return", "hermes", "hermes:fixture", f.start, doneETag)
	if err != nil {
		t.Fatalf("return switch after verified completion: %v", err)
	}

}

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

func TestRuntimeHandoffIdleTargetRequiresNativeReadyAndLiveBackend(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	f.expect("claim", nil, 200)
	f.expect("cancel", nil, 200)
	store := service.WorkerRuntimeStore{Storage: f.storage}
	ctx := t.Context()
	if _, _, err := store.Preflight(ctx, "runtime-alice", "switch", "qwenpaw", "qwenpaw:fixture", f.start, ""); err != nil {
		t.Fatal(err)
	}
	_, etag, err := store.BeginDrain(ctx, "runtime-alice")
	if err != nil {
		t.Fatal(err)
	}
	state, etag, err := store.ConfirmCheckpoint(ctx, "runtime-alice", "switch", f.start, etag, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range [][2]string{{"checkpointed", "stopping"}, {"stopping", "stopped"}, {"stopped", "creating"}} {
		_, etag, err = store.AdvanceLifecycle(ctx, "runtime-alice", "switch", transition[0], transition[1], etag, "qwenpaw:fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.replace()
	_, etag, err = store.AdvanceLifecycle(ctx, "runtime-alice", "switch", "creating", "recovering", etag, "", &f.start)
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
	if state.Handoff.Phase != "completed" || len(state.Handoff.CheckpointRefs) != 0 {
		t.Fatalf("idle readiness %+v", state.Handoff)
	}
}

func TestRuntimeHandoffDurableDeliverySurvivesAnotherTargetStart(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	ctx := t.Context()
	previous := service.WorkerStart{InstanceID: "actual/previous", ProcessID: "p2", Runtime: "codex"}
	current := service.WorkerStart{InstanceID: "actual/current", ProcessID: "p3", Runtime: "codex"}
	op := &service.WorkerHandoff{Target: &current, PriorTargets: []service.WorkerStart{previous}}
	exec := taskExecution{Generation: 2, Runtime: previous.Runtime, InstanceID: previous.InstanceID, ProcessID: previous.ProcessID, State: "active", Checkpoint: &taskCheckpoint{Version: 3, Result: &taskArtifact{Key: "result.md"}, ReplyTxn: "transaction", Delivery: "pending"}}
	ref := service.WorkerCheckpointRef{Kind: "task", TeamID: "alpha-team", TaskID: "t1", Generation: 1, CheckpointVersion: 2, PendingReplyTxn: "transaction"}
	observe := func(assigned, status string) error {
		putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"assigned_to": assigned, "status": status, "execution": exec}))
		return f.h.observeRecoveredWork(ctx, "runtime-alice", op, []string{"alice"}, &ref)
	}
	if err := observe("alice", "submitted"); err == nil {
		t.Fatal("a pending reply satisfied delivery")
	}
	exec.Checkpoint.Delivery, exec.Checkpoint.ReplyEvent = "delivered", "$one-event"
	exec.Checkpoint.ReplyTxn = "different"
	if err := observe("alice", "submitted"); err == nil {
		t.Fatal("another saved result satisfied delivery")
	}
	exec.Checkpoint.ReplyTxn = "transaction"
	if err := observe("alice", "submitted"); err != nil || ref.Outcome != "delivered" || ref.ReplyEvent != "$one-event" {
		t.Fatalf("durable receipt lost on next target start %+v %v", ref, err)
	}
	exec.ProcessID = "unregistered"
	if err := observe("alice", "submitted"); err == nil {
		t.Fatal("unregistered target receipt counted")
	}
	if err := observe("bob", "in_progress"); err != nil || ref.Outcome != "reassigned" {
		t.Fatalf("reassignment %+v %v", ref, err)
	}
	if err := observe("alice", "cancelled"); err != nil || ref.Outcome != "cancelled" {
		t.Fatalf("cancellation %+v %v", ref, err)
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
	refs := []service.WorkerCheckpointRef{}
	raw, _ := json.Marshal(execution)
	if err := collectBoundary(&refs, service.WorkerCheckpointRef{Kind: "task", TeamID: "alpha-team", TaskID: "t1"}, raw, &service.WorkerHandoff{ID: "switch", Source: f.start}, false); err != nil || len(refs) != 1 || refs[0].PendingReplyTxn != "fixed" {
		t.Fatalf("saved reply was not a safe handoff boundary: %+v %v", refs, err)
	}
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

func TestRuntimeHandoffLostSourceClassifiesOnlyItsOwnAuthoritativeWork(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	ctx := t.Context()
	tm := teamWithRoster("alpha-team")
	tm.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alice"}}
	if err := f.h.deps.Client.Create(ctx, tm); err != nil {
		t.Fatal(err)
	}
	var worker v1beta1.Worker
	if err := f.h.deps.Client.Get(ctx, client.ObjectKey{Name: "alice", Namespace: "default"}, &worker); err != nil {
		t.Fatal(err)
	}
	deps := f.h.deps
	deps.OSS = &mcLikeOSS{Memory: f.storage}
	inspector := NewLostHandoffInspector(deps)
	op := service.WorkerHandoff{ID: "switch", Phase: "source_lost", Source: f.start}
	// A verified old step remains uncertain if native execution had not released it.
	execution := taskExecution{Generation: 2, InstanceID: f.start.InstanceID, ProcessID: f.start.ProcessID, Runtime: f.start.Runtime, State: "active", Checkpoint: &taskCheckpoint{Version: 3, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two"}, Operation: &taskOperation{ID: "one", State: "verified", Evidence: "receipt"}}}
	putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"status": "in_progress", "assigned_to": "runtime-alice", "execution": execution}))
	refs, err := inspector(ctx, &worker, &op)
	if err != nil || len(refs) != 2 || refs[0].Kind != "message" || refs[0].RequiresVerification || refs[1].Kind != "task" || !refs[1].RequiresVerification || refs[1].Generation != 2 || refs[1].CheckpointVersion != 3 {
		t.Fatalf("orphan classification %+v %v", refs, err)
	}
	execution.State = "released"
	putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"status": "in_progress", "assigned_to": "runtime-alice", "execution": execution}))
	refs, err = inspector(ctx, &worker, &op)
	if err != nil || refs[1].RequiresVerification || refs[1].CompletedStepCount == nil || *refs[1].CompletedStepCount != 1 {
		t.Fatalf("explicit release lost %+v %v", refs, err)
	}
	putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"status": "in_progress", "assigned_to": "another-worker", "execution": execution}))
	refs, err = inspector(ctx, &worker, &op)
	if err != nil || len(refs) != 1 || refs[0].Kind != "message" {
		t.Fatalf("reassigned task remained in recovery scope %+v %v", refs, err)
	}
}

func TestRuntimeHandoffDeliveredProgressStillRequiresTargetStep(t *testing.T) {
	f := newMatrixExecutionFixture(t, nil)
	ctx := t.Context()
	f.h.recovery.project = newProjectTestHandler(t, f.storage, teamWithRoster("alpha-team"))
	owner := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alice", WorkerName: "runtime-alice", Team: "alpha-team"}
	target := service.WorkerStart{Runtime: "codex", InstanceID: "actual/target", ProcessID: "target"}
	op := &service.WorkerHandoff{ID: "switch", Source: f.start, Target: &target}
	cp := &taskCheckpoint{Version: 5, CompletedSteps: []string{"one"}, RemainingSteps: []string{"two", "three"},
		Operation: &taskOperation{ID: "one", State: "verified", Evidence: "output/one"},
		Result:    &taskArtifact{Key: "progress.md"}, ReplyTxn: "progress-one", ReplyEvent: "$progress", Delivery: "delivered"}
	e := taskExecution{Generation: 1, InstanceID: f.start.InstanceID, ProcessID: f.start.ProcessID, Runtime: f.start.Runtime, State: "active", Checkpoint: cp, ReplyAcknowledgedGeneration: 1}
	put := func(status string) {
		putTask(f.storage, taskMetaKey, taskMeta(map[string]any{"status": status, "assigned_to": "runtime-alice", "execution": e}))
	}
	put("in_progress")
	refs, err := f.h.handoffRefs(ctx, owner, "runtime-alice", &service.WorkerMatrixState{}, op, false)
	if err != nil || len(refs) != 1 || refs[0].SourceProgressReplyTxn != "progress-one" || refs[0].CheckpointVersion != 5 {
		t.Fatalf("dropped source progress: %+v %v", refs, err)
	}
	ref := &refs[0]
	e.Generation++ // A replacement can inherit the checkpoint, but not the acknowledgement.
	put("in_progress")
	if _, err := f.h.handoffRefs(ctx, owner, "runtime-alice", &service.WorkerMatrixState{}, op, false); !errors.Is(err, service.ErrWorkerStateConflict) {
		t.Fatalf("active process inherited a release: %v", err)
	}
	e.Generation--
	put("in_progress")
	observe := func() error {
		return f.h.observeRecoveredWork(ctx, "runtime-alice", op, []string{"runtime-alice"}, ref)
	}
	if err := observe(); !errors.Is(err, service.ErrWorkerStateConflict) {
		t.Fatalf("old source delivery completed target: %v", err)
	}
	e.Generation, e.InstanceID, e.ProcessID, e.Runtime = 2, target.InstanceID, target.ProcessID, target.Runtime
	cp.Version++
	put("in_progress")
	if err := observe(); !errors.Is(err, service.ErrWorkerStateConflict) {
		t.Fatalf("reclaimed copy of the same step completed target: %v", err)
	}
	cp.CompletedSteps = append(cp.CompletedSteps, "two")
	cp.Version++
	put("in_progress")
	if err := observe(); err != nil || ref.Outcome != "advanced" || ref.TargetGeneration != 2 {
		t.Fatalf("new step not accepted: %+v %v", ref, err)
	}
	// A pending progress reply from an unfinished Task must first be delivered.
	// Its own delivery receipt cannot also prove that the target resumed work.
	cp.CompletedSteps = []string{"one"}
	cp.Delivery, cp.ReplyEvent = "pending", ""
	e.Generation, e.InstanceID, e.ProcessID, e.Runtime = 1, f.start.InstanceID, f.start.ProcessID, f.start.Runtime
	put("in_progress")
	refs, err = f.h.handoffRefs(ctx, owner, "runtime-alice", &service.WorkerMatrixState{}, op, false)
	if err != nil || len(refs) != 1 || refs[0].PendingReplyTxn != "progress-one" || refs[0].SourceProgressReplyTxn != "progress-one" {
		t.Fatalf("pending progress lost: %+v %v", refs, err)
	}
	ref = &refs[0]
	e.Generation, e.InstanceID, e.ProcessID, e.Runtime = 2, target.InstanceID, target.ProcessID, target.Runtime
	cp.Version++
	cp.Delivery, cp.ReplyEvent = "delivered", "$progress"
	put("in_progress")
	if err := observe(); !errors.Is(err, service.ErrWorkerStateConflict) {
		t.Fatalf("delivering a progress update completed unfinished work: %v", err)
	}
	cp.CompletedSteps = append(cp.CompletedSteps, "two")
	cp.Version++
	put("in_progress")
	if err := observe(); err != nil || ref.Outcome != "advanced" {
		t.Fatalf("pending progress blocked new completed step: %+v %v", ref, err)
	}

	put("submitted")
	refs, err = f.h.handoffRefs(ctx, owner, "runtime-alice", &service.WorkerMatrixState{}, op, false)
	if err != nil || len(refs) != 0 {
		t.Fatalf("submitted and delivered Task remained active: %+v %v", refs, err)
	}
}
