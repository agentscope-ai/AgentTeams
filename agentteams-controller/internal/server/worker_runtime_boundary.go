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

type handoffAcknowledgement struct {
	name           string
	start          service.WorkerStart
	record         *service.WorkerRuntimeState
	matrix         *service.WorkerMatrixState
	etag, expected string
}

func (h *WorkerMatrixHandler) readHandoffAcknowledgement(w http.ResponseWriter, r *http.Request) *handoffAcknowledgement {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return nil
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
		return nil
	}
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" {
		writeError(w, 428, "read the handoff ETag first")
		return nil
	}
	store := service.WorkerRuntimeStore{Storage: h.deps.OSS}
	state, etag, err := store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return nil
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: ack.ProcessID, Runtime: runtime}
	op := state.Handoff
	if ack.InstanceID != instance || op == nil || op.ID != ack.RequestID {
		writeError(w, 409, "registered start or handoff changed")
		return nil
	}
	matrix, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return nil
	}
	if matrix.ActiveStart != start {
		writeError(w, 409, "Matrix consumption owner changed")
		return nil
	}
	return &handoffAcknowledgement{name: name, start: start, record: state, matrix: matrix, etag: etag, expected: expected}
}

// ConfirmHandoffBoundary accepts only the registered source's quiescence
// acknowledgement. Checkpoint references are discovered by the Controller;
// the source cannot supply a partial list, versions, or a desired phase.
func (h *WorkerMatrixHandler) ConfirmHandoffBoundary(w http.ResponseWriter, r *http.Request) {
	if query := r.URL.Query(); query.Has("probe") && (len(query["probe"]) != 1 || query.Get("probe") != "true") {
		writeError(w, 400, "use probe=true for a read-only boundary check")
		return
	}
	ack := h.readHandoffAcknowledgement(w, r)
	if ack == nil {
		return
	}
	state, etag := ack.record, ack.etag
	op := state.Handoff
	if op.Source != ack.start {
		writeError(w, 409, "source start changed")
		return
	}
	if op.Phase != "checkpointed" {
		if op.Phase != "draining" || ack.expected != etag {
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
		for _, key := range ack.matrix.Pending {
			input, err := h.store.Input(r.Context(), ack.name, key)
			if err != nil {
				h.writeError(w, err)
				return
			}
			if input.Intent == "message" && (input.Work == nil || input.Work.Kind == "message") {
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
		if r.URL.Query().Get("probe") == "true" {
			w.Header().Set("ETag", `"`+etag+`"`)
			httputil.WriteJSON(w, 200, state)
			return
		}
		var err error
		state, etag, err = (service.WorkerRuntimeStore{Storage: h.deps.OSS}).ConfirmCheckpoint(r.Context(), ack.name, op.ID, ack.start, etag, refs)
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
		zero := 0
		ref.CompletedStepCount = &zero
		*refs = append(*refs, ref) // The target must still discover accepted work.
		return nil
	}
	var execution taskExecution
	if json.Unmarshal(raw, &execution) != nil || execution.Generation == 0 {
		return errors.New("invalid authoritative execution record")
	}
	if execution.State == "cancelled" || (execution.Checkpoint != nil && execution.Checkpoint.Delivery == "delivered") {
		return nil
	}
	cp := execution.Checkpoint
	if cp == nil || cp.Version == 0 || unresolvedOperation(cp) {
		return fmt.Errorf("%w: execution lacks a verified step boundary", service.ErrWorkerStateConflict)
	}
	// A saved result permits only replay of its stable reply transaction.
	// It can move even when the source stopped before recording release.
	deliveryOnly := cp.Delivery == "pending" && cp.Result != nil && cp.ReplyTxn != ""
	if !deliveryOnly && (execution.State != "released" || (execution.HandoffID != "" && execution.HandoffID != op.ID && execution.InstanceID == op.Source.InstanceID)) {
		return fmt.Errorf("%w: source execution has not released its current step", service.ErrWorkerStateConflict)
	}
	if len(cp.CompletedSteps)+len(cp.RemainingSteps) == 0 && cp.Result == nil && cp.Operation == nil {
		return fmt.Errorf("%w: checkpoint carries no progress", service.ErrWorkerStateConflict)
	}
	completed := len(cp.CompletedSteps)
	ref.CompletedStepCount = &completed
	if cp.Delivery == "pending" {
		ref.PendingReplyTxn = cp.ReplyTxn
	}
	if cp.Operation != nil && cp.Operation.State == "verified" {
		ref.VerifiedOperation = cp.Operation.ID
	}
	ref.Generation, ref.CheckpointVersion = execution.Generation, cp.Version
	*refs = append(*refs, ref)
	return nil
}
