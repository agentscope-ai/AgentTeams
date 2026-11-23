package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// NativePhase is a framework lifecycle boundary, not an Agent tool or a
// Harness-specific endpoint. Only the recipient's registered start may use it.
func (h *CoordinationHandler) NativePhase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		writeError(w, 404, "coordination receipt not found")
		return
	}
	var input struct {
		Action        string `json:"action"`
		NativeEventID string `json:"native_event_id"`
		ProcessID     string `json:"process_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF ||
		(input.Action != "begin" && input.Action != "retry") || !strings.HasPrefix(input.NativeEventID, "$") ||
		len(input.NativeEventID) > 4096 || input.ProcessID == "" || len(input.ProcessID) > 256 ||
		input.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
		writeError(w, 400, "native attempt and current process required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleTeamLeader {
		writeError(w, 403, "coordination recipient identity required")
		return
	}
	strict, ok := h.deps.OSS.(conditionalWriter)
	if !ok {
		writeError(w, 503, "strict coordination storage required")
		return
	}
	for range 8 {
		key := coordinationPending + id + ".json"
		data, err := h.deps.OSS.GetObject(r.Context(), key)
		archived := errors.Is(err, os.ErrNotExist)
		if archived {
			data, err = h.deps.OSS.GetObject(r.Context(), coordinationDone+id+".json")
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, 404, "coordination receipt not found")
			} else {
				writeError(w, 503, "coordination receipt unavailable")
			}
			return
		}
		var delivery coordinationDelivery
		if json.Unmarshal(data, &delivery) != nil || delivery.Event.ID != id {
			writeError(w, 503, "invalid coordination receipt")
			return
		}
		event := delivery.Event
		if event.Protocol != "task/v1" || delivery.Transport != "matrix" ||
			(event.Kind != "handoff" && event.Kind != "task_submitted" && event.Kind != "main_task_ready") {
			writeError(w, 400, "event does not support coordination-phase recovery")
			return
		}
		worker, err := h.worker(r.Context(), caller.Username)
		if err != nil || event.RecipientKind != "worker" || worker.Name != event.RecipientName ||
			worker.Status.MatrixUserID != event.Recipient {
			writeError(w, 403, "coordination receipt belongs to another recipient")
			return
		}
		store := service.WorkerMatrixStore{Storage: h.deps.OSS}
		state, _, err := store.Load(r.Context(), worker.Spec.EffectiveWorkerName(worker.Name))
		if err != nil {
			writeError(w, 503, "registered native start unavailable")
			return
		}
		if state.ActiveStart.InstanceID == "" || state.ActiveStart.ProcessID != input.ProcessID {
			writeError(w, 409, "native process was replaced")
			return
		}
		nativeID := "matrix:" + input.NativeEventID
		if slices.Contains(delivery.PreviousNativeIDs, nativeID) {
			httputil.WriteJSON(w, 200, map[string]any{"active": false, "reused": true})
			return
		}
		if delivery.NativeID != nativeID {
			if delivery.NativeID == "" && !archived {
				writeError(w, 503, "native delivery receipt is not committed yet")
			} else {
				writeError(w, 409, "native delivery attempt changed")
			}
			return
		}
		pending, err := h.coordinationPhasePending(r.Context(), event)
		if err != nil {
			writeError(w, 503, "coordination task state unavailable")
			return
		}
		if !pending {
			httputil.WriteJSON(w, 200, map[string]any{"active": false, "settled": true})
			return
		}
		if delivery.Status == "failed" {
			httputil.WriteJSON(w, 200, map[string]any{"active": false, "failed": true})
			return
		}
		if archived {
			writeError(w, 409, "legacy phase was archived before its durable outcome")
			return
		}
		response := map[string]any{"active": true}
		if input.Action == "begin" {
			if delivery.NativeStart != nil && *delivery.NativeStart != state.ActiveStart {
				h.wakeRunner()
				httputil.WriteJSON(w, 200, map[string]any{"active": false, "replaced": true})
				return
			}
			delivery.NativeStart = &state.ActiveStart
			if event.Kind == "handoff" {
				ids, err := h.handoffPendingTaskIDs(r.Context(), event)
				if err != nil {
					writeError(w, 503, "handoff task state unavailable")
					return
				}
				response["pending_task_ids"] = ids
			}
		} else {
			queued, err := h.retryAfterNativeFailure(r.Context(), &delivery, nativeID)
			if err != nil {
				writeError(w, 503, "coordination retry state unavailable")
				return
			}
			response = map[string]any{"active": false, "retry_queued": queued}
			delivery.LeaseUntil = time.Time{}
		}
		next, _ := json.Marshal(delivery)
		if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(data)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			writeError(w, 503, "coordination phase commit unavailable")
			return
		}
		h.wakeRunner()
		httputil.WriteJSON(w, 200, response)
		return
	}
	writeError(w, 503, "coordination phase raced; retry the same request")
}
