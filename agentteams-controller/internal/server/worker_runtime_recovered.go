package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// ConfirmHandoffRecovery is the target supervisor's startup-ready callback.
// Registration and native readiness suffice only when the source handed over
// no work. Otherwise observe durable target execution for every reference.
func (h *WorkerMatrixHandler) ConfirmHandoffRecovery(w http.ResponseWriter, r *http.Request) {
	ack := h.readHandoffAcknowledgement(w, r)
	if ack == nil {
		return
	}
	state, etag := ack.record, ack.etag
	op := state.Handoff
	if op.Target == nil || *op.Target != ack.start || (op.Phase != "completed" && op.Phase != "recovering") {
		writeError(w, 409, "target registration or handoff changed")
		return
	}
	if op.Phase == "recovering" {
		if etag != ack.expected {
			writeError(w, 409, "handoff revision changed")
			return
		}
		caller := authpkg.CallerFromContext(r.Context())
		worker, err := NewCoordinationHandler(h.deps).worker(r.Context(), caller.Username)
		if err != nil || h.deps.Backend == nil {
			writeError(w, 503, "target Worker backend unavailable")
			return
		}
		kind, instance, _ := strings.Cut(ack.start.InstanceID, "/")
		b, err := h.deps.Backend.GetWorkerBackend(r.Context(), kind)
		if err != nil {
			writeError(w, 503, "target backend unavailable")
			return
		}
		actual, err := b.Status(r.Context(), worker.Name)
		if err != nil {
			writeError(w, 503, "target readiness unavailable")
			return
		}
		if actual == nil || actual.InstanceID != instance || actual.Runtime != op.TargetRuntime || actual.HandoffLabel != op.TargetLabel() || (actual.Status != backend.StatusReady && actual.Status != backend.StatusRunning) {
			writeError(w, 409, "target Worker has not reached readiness")
			return
		}
		owners := []string{caller.Username, caller.WorkerName, caller.MatrixUserID, worker.Name, worker.Spec.EffectiveWorkerName(worker.Name), worker.Status.MatrixUserID}
		for i := range op.CheckpointRefs {
			if err := h.observeRecoveredWork(r.Context(), ack.name, op, owners, &op.CheckpointRefs[i]); err != nil {
				h.writeError(w, err)
				return
			}
		}
		state, etag, err = (service.WorkerRuntimeStore{Storage: h.deps.OSS}).Complete(r.Context(), state, etag)
		if err != nil {
			h.writeError(w, err)
			return
		}
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	httputil.WriteJSON(w, 200, state)
}

func (h *WorkerMatrixHandler) observeRecoveredWork(ctx context.Context, name string, op *service.WorkerHandoff, owners []string, ref *service.WorkerCheckpointRef) error {
	var raw json.RawMessage
	var taskStatus string
	switch ref.Kind {
	case "task":
		if !coordinationSafeID.MatchString(ref.TeamID) || !coordinationSafeID.MatchString(ref.TaskID) {
			return errors.New("invalid persisted task reference")
		}
		body, err := h.deps.OSS.GetObject(ctx, "teams/"+ref.TeamID+"/shared/tasks/"+ref.TaskID+"/meta.json")
		if err != nil {
			return fmt.Errorf("recovery task snapshot unavailable: %w", err)
		}
		var task map[string]json.RawMessage
		var taskID, assigned string
		if json.Unmarshal(body, &task) != nil || json.Unmarshal(task["task_id"], &taskID) != nil || taskID != ref.TaskID || json.Unmarshal(task["status"], &taskStatus) != nil {
			return errors.New("invalid recovery task snapshot")
		}
		if taskStatus == "cancelled" {
			ref.Outcome = "cancelled"
			return nil
		}
		if json.Unmarshal(task["assigned_to"], &assigned) != nil || assigned == "" {
			return errors.New("recovery task owner unavailable")
		}
		if !taskAssignedTo(task, owners...) {
			ref.Outcome = "reassigned"
			return nil
		}
		raw = task["execution"]
	case "message":
		input, err := h.store.Input(ctx, name, ref.EventKey)
		if err != nil {
			return err
		}
		raw = input.Execution
	default:
		return errors.New("unknown work reference in handoff")
	}
	var execution taskExecution
	if len(raw) == 0 {
		return fmt.Errorf("%w: target has not claimed handed-over work", service.ErrWorkerStateConflict)
	}
	if json.Unmarshal(raw, &execution) != nil {
		return errors.New("invalid authoritative recovery execution")
	}
	if execution.State == "cancelled" {
		ref.Outcome = "cancelled"
		return nil
	}
	cp := execution.Checkpoint
	terminalWork := ref.Kind == "message" || taskStatus == "submitted" || taskStatus == "completed"
	deliverySatisfiesWork := func() bool {
		if cp == nil || cp.Delivery != "delivered" || cp.Result == nil || cp.ReplyEvent == "" {
			return false
		}
		if ref.SourceProgressReplyTxn != "" {
			// The Task continued beyond this source reply, even if the target
			// had to deliver that reply before starting its own next step.
			return terminalWork && cp.ReplyTxn != ref.SourceProgressReplyTxn
		}
		if ref.PendingReplyTxn != "" {
			return cp.ReplyTxn == ref.PendingReplyTxn
		}
		return terminalWork
	}
	// Older handoff records can include work already delivered by the source.
	if deliverySatisfiesWork() && execution.Generation == ref.Generation && cp.Version == ref.CheckpointVersion {
		ref.Outcome, ref.ReplyEvent = "already_delivered", cp.ReplyEvent
		return nil
	}
	observed := service.WorkerStart{InstanceID: execution.InstanceID, ProcessID: execution.ProcessID, Runtime: execution.Runtime}
	admitted := op.Target != nil && *op.Target == observed
	if cp != nil && cp.Delivery == "delivered" && slices.Contains(op.PriorTargets, observed) {
		// Previously delivered work is retired. Its target process may
		// restart while another item from the same handoff is outstanding.
		admitted = true
	}
	if execution.Generation <= ref.Generation || !admitted || cp == nil || cp.Version <= ref.CheckpointVersion || unresolvedOperation(cp) {
		return fmt.Errorf("%w: target has not checkpointed its recovery execution", service.ErrWorkerStateConflict)
	}
	switch {
	case deliverySatisfiesWork():
		ref.Outcome, ref.ReplyEvent = "delivered", cp.ReplyEvent
	case (ref.PendingReplyTxn != "" && ref.SourceProgressReplyTxn == "") || cp.Delivery == "pending":
		return fmt.Errorf("%w: saved reply still awaits delivery", service.ErrWorkerStateConflict)
	case ref.CompletedStepCount != nil && len(cp.CompletedSteps) > *ref.CompletedStepCount:
		ref.Outcome = "advanced"
	case ref.CompletedStepCount != nil && cp.Operation != nil && cp.Operation.State == "verified" && cp.Operation.Evidence != "" && cp.Operation.ID != ref.VerifiedOperation:
		ref.Outcome = "verified"
	default:
		return fmt.Errorf("%w: checkpoint has no new completed step or verified effect", service.ErrWorkerStateConflict)
	}
	ref.TargetGeneration, ref.TargetCheckpointVersion = execution.Generation, cp.Version
	return nil
}
