package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// Task notifications reconstruct their context from authoritative Task records.
// Missing ordinary chat history must not delay that independent lifecycle.
func (h *WorkerMatrixHandler) messageRecoveryMode(ctx context.Context, account string, state *service.WorkerMatrixState, input *service.WorkerMatrixInput, execution taskExecution) (string, error) {
	mode := matrixRoomRecoveryMode(state, input, execution)
	if mode != "backfill" {
		return mode, nil
	}
	wire, _ := input.Input.Content["org.agentteams.coordination"].(map[string]any)
	meta, _ := wire["channel_meta"].(map[string]any)
	event, _ := meta["teamharness_coordination"].(map[string]any)
	id, _ := event["id"].(string)
	if wire == nil {
		binding, err := h.recovery.roomTaskBinding(ctx, input.RoomID)
		if err != nil {
			return "", err
		}
		if binding != nil {
			return "resume", nil
		}
		return mode, nil
	}
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return mode, nil
	}
	for _, prefix := range []string{coordinationPending, coordinationDone} {
		body, err := h.deps.OSS.GetObject(ctx, prefix+id+".json")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var receipt coordinationDelivery
		if err := json.Unmarshal(body, &receipt); err != nil {
			return "", err
		}
		e := receipt.Event
		if e.ID != id || e.Protocol != "task/v1" || receipt.Transport != "matrix" ||
			receipt.NativeID != "matrix:"+input.EventID || e.Recipient != account ||
			e.RoomID != input.RoomID || e.Actor != input.Input.Sender {
			return mode, nil
		}
		switch e.Kind {
		case "handoff", "task_assigned", "task_submitted", "main_task_ready":
			switch receipt.Status {
			case "queued", "dispatching", "running", "completed":
				return "resume", nil
			}
		}
		return mode, nil
	}
	return mode, nil
}
