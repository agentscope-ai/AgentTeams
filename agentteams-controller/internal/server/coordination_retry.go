package server

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// retryAfterNativeFailure is called only after the framework has ended a turn
// with a retryable Task-tool error. Polling an unfinished task is not this fact.
// The caller must persist the changed delivery with its original CAS condition.
func (h *CoordinationHandler) retryAfterNativeFailure(ctx context.Context, d *coordinationDelivery, nativeID string) (bool, error) {
	if d.Event.Protocol != "task/v1" || d.Transport != "matrix" ||
		(d.Event.Kind != "handoff" && d.Event.Kind != "task_submitted" && d.Event.Kind != "main_task_ready") {
		return false, fmt.Errorf("event does not support coordination-phase recovery")
	}
	if slices.Contains(d.PreviousNativeIDs, nativeID) {
		return true, nil // A lost response must not restart a newer attempt.
	}
	if nativeID == "" || nativeID != d.NativeID {
		return false, fmt.Errorf("native delivery attempt changed")
	}
	pending, err := h.coordinationPhasePending(ctx, d.Event)
	if err != nil || !pending {
		return false, err
	}
	d.PreviousNativeIDs = append(d.PreviousNativeIDs, nativeID)
	d.NativeID, d.MatrixEventID, d.Error = "", "", ""
	d.NativeStart = nil
	d.Status = "queued"
	d.Replays++
	d.NextAttempt = time.Now().UTC().Add(2 * time.Second)
	return true, nil
}

func (h *CoordinationHandler) coordinationPhasePending(ctx context.Context, event coordinationEvent) (bool, error) {
	e, err := h.resolve(ctx, event, &authpkg.CallerIdentity{
		Role: event.ActorRole, Username: event.ActorName, Team: event.Team,
	})
	if errors.Is(err, errCoordinationRejected) {
		return false, nil // Cancellation/reassignment invalidates the old work.
	}
	if err != nil {
		return false, err
	}
	if e.Recipient != event.Recipient {
		return false, nil
	}
	main, task, err := h.state(ctx, e)
	if err != nil {
		return false, err
	}
	pending := false
	switch e.Kind {
	case "handoff":
		pending, err = h.handoffNeedsPlan(ctx, e)
		if err != nil {
			return false, err
		}
	case "task_submitted":
		state := coordinationString(main, "status")
		pending = state != "cancelled" && state != "completed" && state != "blocked" &&
			coordinationString(task, "status") == "submitted" && coordinationString(task, "submission_id") == e.SubmissionID
	case "main_task_ready":
		state := coordinationString(main, "status")
		pending = (state == "in_progress" || state == "submitted") && coordinationReady(main)
	}
	return pending, nil
}

func (h *CoordinationHandler) handoffNeedsPlan(ctx context.Context, e coordinationEvent) (bool, error) {
	ids, err := h.handoffPendingTaskIDs(ctx, e)
	return len(ids) > 0, err
}

func (h *CoordinationHandler) handoffPendingTaskIDs(ctx context.Context, e coordinationEvent) ([]string, error) {
	var pending []string
	for _, request := range e.Tasks {
		current, err := h.read(ctx, path.Join("teams", e.StorageTeam, "shared/tasks", request.TaskID, "meta.json"))
		if err != nil {
			return nil, err
		}
		state := coordinationString(current, "status")
		if current["task_plan"] == nil && state != "cancelled" && state != "completed" && state != "blocked" {
			pending = append(pending, request.TaskID)
		}
	}
	return pending, nil
}

func (d coordinationDelivery) matrixTransactionID() string {
	if d.Replays > 0 {
		return fmt.Sprintf("%s-retry-%d", d.Event.ID, d.Replays)
	}
	return d.Event.ID
}

func (h *CoordinationHandler) recoverChangedNativeStart(ctx context.Context, d *coordinationDelivery) (bool, error) {
	if d.NativeStart == nil || d.NativeID == "" {
		return false, nil
	}
	worker, err := h.worker(ctx, d.Event.RecipientName)
	if err != nil {
		return false, err
	}
	store := service.WorkerMatrixStore{Storage: h.deps.OSS}
	state, _, err := store.Load(ctx, worker.Spec.EffectiveWorkerName(worker.Name))
	if err != nil {
		return false, err
	}
	if state.ActiveStart.InstanceID == "" || state.ActiveStart.ProcessID == "" || state.ActiveStart == *d.NativeStart {
		return false, nil
	}
	return h.retryAfterNativeFailure(ctx, d, d.NativeID)
}
