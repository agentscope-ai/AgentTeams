package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

func messageDeliveryAckOnly(old, next *taskCheckpoint) bool {
	if next == nil || next.Delivery != "delivered" {
		return false
	}
	a, b := *old, *next
	b.Version, b.SavedAt, b.Delivery, b.ReplyEvent = a.Version, a.SavedAt, a.Delivery, a.ReplyEvent
	x, err := json.Marshal(a)
	y, otherErr := json.Marshal(b)
	return err == nil && otherErr == nil && bytes.Equal(x, y)
}

func matrixRecoveryMode(execution taskExecution) string {
	if execution.State == "cancelled" || (execution.Checkpoint != nil && execution.Checkpoint.Delivery == "delivered") {
		return "done"
	}
	if unresolvedOperation(execution.Checkpoint) {
		return "verify"
	}
	if execution.Checkpoint != nil && execution.Checkpoint.Delivery == "pending" {
		return "deliver"
	}
	return "resume"
}

func matrixRoomRecoveryMode(state *service.WorkerMatrixState, input *service.WorkerMatrixInput, execution taskExecution) string {
	mode := matrixRecoveryMode(execution)
	if mode == "resume" && len(state.Rooms[input.RoomID].Gaps) > 0 {
		return "backfill"
	}
	return mode
}

func (h *WorkerMatrixHandler) GetExecution(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	input, etag, err := h.store.InputSnapshot(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	var execution taskExecution
	if len(input.Execution) != 0 && json.Unmarshal(input.Execution, &execution) != nil {
		writeError(w, 503, "message execution unavailable")
		return
	}
	state, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	httputil.WriteJSON(w, 200, map[string]any{"input": input, "execution": execution, "etag": etag, "instance_id": instance, "runtime": runtime, "recovery": matrixRoomRecoveryMode(state, input, execution)})
}

func (h *WorkerMatrixHandler) UpdateExecution(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	var request struct {
		Action     string          `json:"action"`
		InstanceID string          `json:"instance_id"`
		ProcessID  string          `json:"process_id"`
		Generation uint64          `json:"generation"`
		Checkpoint *taskCheckpoint `json:"checkpoint"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.ProcessID == "" || len(request.ProcessID) > 256 || request.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
		writeError(w, 400, "current process identity and valid execution request required")
		return
	}
	if request.InstanceID != instance {
		writeError(w, 409, "Worker instance changed")
		return
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: request.ProcessID, Runtime: runtime}
	state, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if state.ActiveStart != start {
		writeError(w, 409, "register the current Worker start before executing messages")
		return
	}
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" {
		writeError(w, 428, "read the message execution ETag first")
		return
	}
	input, etag, err := h.store.InputSnapshot(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	if expected != etag {
		writeError(w, 409, "message execution changed")
		return
	}
	if input.Intent != "message" || (input.Work != nil && input.Work.Kind != "message") {
		writeError(w, 409, "Task and control inputs retain their own authoritative execution path")
		return
	}
	var execution taskExecution
	if len(input.Execution) != 0 && json.Unmarshal(input.Execution, &execution) != nil {
		writeError(w, 503, "message execution unavailable")
		return
	}
	sameProcess := execution.InstanceID == instance && execution.ProcessID == request.ProcessID
	if matrixRecoveryMode(execution) == "done" {
		if request.Action != "claim" {
			writeError(w, 409, "message execution is terminal")
			return
		}
		_ = h.store.Retire(r.Context(), name, start, r.PathValue("key"))
		httputil.WriteJSON(w, 200, map[string]any{"input": input, "execution": execution, "etag": etag, "recovery": "done"})
		return
	}
	op, err := executionHandoff(r.Context(), h.deps.OSS, name)
	if err != nil {
		writeError(w, 503, "Worker handoff state unavailable")
		return
	}
	if err := checkHandoffExecution(op, start, request.Action, execution.Checkpoint, request.Checkpoint); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	switch request.Action {
	case "claim":
		if matrixRoomRecoveryMode(state, input, execution) == "backfill" {
			writeError(w, 409, "backfill or verify the room's missing history before new inference")
			return
		}
		if !sameProcess {
			if execution.InstanceID == instance {
				writeError(w, 409, "another process owns this message execution")
				return
			}
			execution.HandoffID = ""
			execution.Generation++
			execution.InstanceID, execution.ProcessID, execution.Runtime = instance, request.ProcessID, runtime
		}
		execution.State = "active"
		if unresolvedOperation(execution.Checkpoint) {
			execution.State = "needs_verification"
		}
	case "checkpoint", "release", "cancel":
		if !sameProcess || execution.Generation == 0 || request.Generation != execution.Generation || execution.State == "released" {
			writeError(w, 409, "message execution ownership changed")
			return
		}
		if request.Action == "cancel" {
			execution.State = "cancelled"
			break
		}
		if matrixRecoveryMode(execution) == "deliver" && (request.Action != "checkpoint" || !messageDeliveryAckOnly(execution.Checkpoint, request.Checkpoint)) {
			writeError(w, 409, "a saved message result permits only delivery acknowledgement or cancellation")
			return
		}
		key := input.MemberPrefix + "/runtime/matrix/events/" + r.PathValue("key") + ".json"
		if err := validateWorkCheckpoint(r.Context(), h.deps.OSS, key, input.MemberPrefix+"/runtime/matrix/results/", execution.Checkpoint, request.Checkpoint); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		execution.Checkpoint = request.Checkpoint
		execution.State = "active"
		if unresolvedOperation(execution.Checkpoint) {
			execution.State = "needs_verification"
		}
		if request.Action == "release" {
			if unresolvedOperation(execution.Checkpoint) {
				writeError(w, 409, "cannot release an unverified operation")
				return
			}
			execution.State = "released"
		}
	default:
		writeError(w, 400, "unsupported message execution action")
		return
	}
	var savedResult string
	if matrixRecoveryMode(execution) == "deliver" {
		artifact := execution.Checkpoint.Result
		if artifact == nil {
			writeError(w, 409, "saved message result reference unavailable; verification required")
			return
		}
		data, err := h.deps.OSS.GetObject(r.Context(), artifact.Key)
		digest := sha256.Sum256(data)
		if err != nil || int64(len(data)) != artifact.Size || hex.EncodeToString(digest[:]) != artifact.SHA256 {
			writeError(w, 409, "saved message result unavailable or changed; verification required")
			return
		}
		savedResult = string(data)
	}
	if op != nil && request.Action != "cancel" && execution.Checkpoint != nil && !unresolvedOperation(execution.Checkpoint) {
		execution.State, execution.HandoffID = "released", op.ID
	}
	input.Execution, _ = json.Marshal(execution)
	body, err := json.Marshal(input)
	if err != nil {
		writeError(w, 503, "message execution encoding failed")
		return
	}
	strict, ok := h.deps.OSS.(conditionalWriter)
	if !ok {
		writeError(w, 503, "strict conditional storage required")
		return
	}
	key := input.MemberPrefix + "/runtime/matrix/events/" + r.PathValue("key") + ".json"
	if err := strict.PutObjectIfMatchStrict(r.Context(), key, body, etag); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			writeError(w, 409, "message execution changed")
		} else {
			writeError(w, 503, "message execution commit failed")
		}
		return
	}
	if request.Action == "claim" && !handoffClaimStillOpen(w, r, h.deps.OSS, name) {
		return
	}
	if matrixRecoveryMode(execution) == "done" {
		_ = h.store.Retire(r.Context(), name, start, r.PathValue("key"))
	}
	w.Header().Set("ETag", contentETag(body))
	httputil.WriteJSON(w, 200, map[string]any{"input": input, "execution": execution, "etag": contentETag(body), "recovery": matrixRoomRecoveryMode(state, input, execution), "saved_result": savedResult})
}
