package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// ConfirmHandoffBoundary accepts only the registered source's quiescence
// acknowledgement. Checkpoint references are discovered by the Controller;
// the source cannot supply a partial list, versions, or a desired phase.
func (h *WorkerMatrixHandler) ConfirmHandoffBoundary(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	var ack struct {
		RequestID  string `json:"request_id"`
		InstanceID string `json:"instance_id"`
		ProcessID  string `json:"process_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ack) != nil || decoder.Decode(new(any)) != io.EOF || ack.ProcessID == "" || ack.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
		writeError(w, 400, "registered process and handoff acknowledgement required")
		return
	}
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" {
		writeError(w, 428, "read the handoff ETag first")
		return
	}
	store := service.WorkerRuntimeStore{Storage: h.deps.OSS}
	state, etag, err := store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: ack.ProcessID, Runtime: runtime}
	op := state.Handoff
	if ack.InstanceID != instance || op == nil || op.ID != ack.RequestID || op.Source != start {
		writeError(w, 409, "source start or handoff changed")
		return
	}
	matrix, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if matrix.ActiveStart != start {
		writeError(w, 409, "Matrix consumption owner changed")
		return
	}
	if op.Phase != "checkpointed" {
		if op.Phase != "draining" || expected != etag {
			writeError(w, 409, "handoff phase or ETag changed")
			return
		}
		refs := []service.WorkerCheckpointRef{}
		caller := authpkg.CallerFromContext(r.Context())
		if caller.Team != "" {
			tasks, err := h.recovery.discoverRecoverableTasks(r.Context(), caller)
			if err != nil {
				writeError(w, 503, "owned tasks unavailable")
				return
			}
			for _, row := range tasks {
				var taskID string
				if json.Unmarshal(row.Task["task_id"], &taskID) != nil {
					writeError(w, 503, "task identity unavailable")
					return
				}
				ref := service.WorkerCheckpointRef{Kind: "task", TeamID: row.Team, TaskID: taskID}
				if err := collectBoundary(&refs, ref, row.Task["execution"], op); err != nil {
					h.writeError(w, err)
					return
				}
			}
		}
		for _, key := range matrix.Pending {
			input, err := h.store.Input(r.Context(), name, key)
			if err != nil {
				h.writeError(w, err)
				return
			}
			if input.Intent == "message" {
				ref := service.WorkerCheckpointRef{Kind: "message", EventKey: key}
				if err := collectBoundary(&refs, ref, input.Execution, op); err != nil {
					h.writeError(w, err)
					return
				}
			}
		}
		sort.Slice(refs, func(i, j int) bool {
			a, b := refs[i], refs[j]
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			if a.TeamID != b.TeamID {
				return a.TeamID < b.TeamID
			}
			if a.TaskID != b.TaskID {
				return a.TaskID < b.TaskID
			}
			return a.EventKey < b.EventKey
		})
		state, etag, err = store.ConfirmCheckpoint(r.Context(), name, op.ID, start, etag, refs)
		if err != nil {
			h.writeError(w, err)
			return
		}
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	httputil.WriteJSON(w, 200, state)
}

func collectBoundary(refs *[]service.WorkerCheckpointRef, ref service.WorkerCheckpointRef, raw json.RawMessage, op *service.WorkerHandoff) error {
	if len(raw) == 0 {
		return nil
	} // Accepted input, no native execution to drain.
	var execution taskExecution
	if json.Unmarshal(raw, &execution) != nil || execution.Generation == 0 {
		return errors.New("invalid authoritative execution record")
	}
	if execution.State == "cancelled" {
		return nil
	}
	cp := execution.Checkpoint
	if cp == nil || cp.Version == 0 || unresolvedOperation(cp) {
		return fmt.Errorf("%w: execution lacks a verified step boundary", service.ErrWorkerStateConflict)
	}
	if cp.Delivery != "delivered" && (execution.State != "released" || (execution.HandoffID != "" && execution.HandoffID != op.ID && execution.InstanceID == op.Source.InstanceID)) {
		return fmt.Errorf("%w: source execution has not released its current step", service.ErrWorkerStateConflict)
	}
	if len(cp.CompletedSteps)+len(cp.RemainingSteps) == 0 && cp.Result == nil && cp.Operation == nil {
		return fmt.Errorf("%w: checkpoint carries no progress", service.ErrWorkerStateConflict)
	}
	ref.Generation, ref.CheckpointVersion = execution.Generation, cp.Version
	*refs = append(*refs, ref)
	return nil
}
