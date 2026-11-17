package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// WorkerMatrixHandler exposes only the authenticated Worker's own consumption
// state. It grants no Project or cross-Worker listing/read capability.
type WorkerMatrixHandler struct {
	deps     ServerDeps
	recovery *TaskRecoveryHandler
	store    *service.WorkerMatrixStore
}

func NewWorkerMatrixHandler(deps ServerDeps, recovery *TaskRecoveryHandler) *WorkerMatrixHandler {
	return &WorkerMatrixHandler{deps: deps, recovery: recovery, store: &service.WorkerMatrixStore{Storage: deps.OSS}}
}

func (h *WorkerMatrixHandler) identity(w http.ResponseWriter, r *http.Request) (string, string, string, string, bool) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleWorker && caller.Role != authpkg.RoleTeamLeader) {
		writeError(w, 403, "Worker self-state identity required")
		return "", "", "", "", false
	}
	if caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.deps.Namespace {
		writeError(w, 403, "Worker belongs to another Controller namespace")
		return "", "", "", "", false
	}
	worker, err := NewCoordinationHandler(h.deps).worker(r.Context(), caller.Username)
	if err != nil {
		writeError(w, 403, "Worker identity unavailable")
		return "", "", "", "", false
	}
	instance, runtime, err := h.recovery.instance(r.Context(), caller)
	if err != nil {
		writeError(w, 503, "actual Worker start unavailable")
		return "", "", "", "", false
	}
	return worker.Spec.EffectiveWorkerName(worker.Name), instance, runtime, worker.Status.MatrixUserID, true
}

func (h *WorkerMatrixHandler) Get(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	state, etag, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	httputil.WriteJSON(w, 200, map[string]any{"state": state, "etag": etag, "instance_id": instance, "runtime": runtime})
}

func (h *WorkerMatrixHandler) GetInput(w http.ResponseWriter, r *http.Request) {
	name, _, _, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	input, err := h.store.Input(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	httputil.WriteJSON(w, 200, input)
}

func (h *WorkerMatrixHandler) Update(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, account, ok := h.identity(w, r)
	if !ok {
		return
	}
	var input struct {
		Action     string                              `json:"action"`
		InstanceID string                              `json:"instance_id"`
		ProcessID  string                              `json:"process_id"`
		Consumer   *service.WorkerMatrixConsumer       `json:"consumer,omitempty"`
		Inputs     []service.WorkerMatrixInput         `json:"inputs,omitempty"`
		Rooms      map[string]service.WorkerMatrixRoom `json:"rooms,omitempty"`
		Backfill   *service.WorkerMatrixBackfill       `json:"backfill,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ProcessID == "" || len(input.ProcessID) > 256 || input.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") {
		writeError(w, 400, "current process identity and valid Matrix request required")
		return
	}
	if input.InstanceID != instance {
		writeError(w, 409, "Worker instance changed")
		return
	}
	create := r.Header.Get("If-None-Match")
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" && create == "" {
		writeError(w, 428, "read the Worker state ETag first")
		return
	}
	decoded, decodeErr := hex.DecodeString(expected)
	if (create != "" && (create != "*" || expected != "")) || (create == "" && (decodeErr != nil || len(decoded) != 16)) {
		writeError(w, 400, "one exact ETag or create-if-absent condition required")
		return
	}
	start := service.WorkerStart{InstanceID: instance, ProcessID: input.ProcessID, Runtime: runtime}
	var state *service.WorkerMatrixState
	var etag string
	var err error
	switch input.Action {
	case "start":
		if input.Consumer != nil || len(input.Inputs) != 0 || len(input.Rooms) != 0 || input.Backfill != nil {
			writeError(w, 400, "start cannot commit a Matrix batch")
			return
		}
		state, etag, err = h.store.Start(r.Context(), name, start, expected)
	case "ingest":
		if input.Backfill != nil {
			writeError(w, 400, "use the explicit backfill action for history pages")
			return
		}
		if create != "" {
			writeError(w, 400, "ingest requires a registered start and native consumer")
			return
		}
		if input.Consumer != nil && account == "" {
			writeError(w, 503, "current Worker Matrix identity unavailable")
			return
		}
		if input.Consumer != nil && input.Consumer.Account != account {
			writeError(w, 403, "Matrix consumer must belong to this Worker identity")
			return
		}
		var consumer service.WorkerMatrixConsumer
		if input.Consumer != nil {
			consumer = *input.Consumer
		}
		state, etag, err = h.store.Ingest(r.Context(), name, start, expected, consumer, input.Inputs, input.Rooms)
	case "backfill":
		if create != "" || input.Backfill == nil || input.Consumer != nil || len(input.Inputs) != 0 || len(input.Rooms) != 0 {
			writeError(w, 400, "registered start and one explicit history page required")
			return
		}
		state, etag, err = h.store.Backfill(r.Context(), name, start, expected, *input.Backfill)
	default:
		writeError(w, 400, "unsupported Matrix state action")
		return
	}
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	httputil.WriteJSON(w, 200, map[string]any{"state": state, "etag": etag})
}

func (h *WorkerMatrixHandler) writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrWorkerStateConflict):
		writeError(w, 409, err.Error())
	case errors.Is(err, service.ErrWorkerStateInvalid):
		writeError(w, 400, err.Error())
	case errors.Is(err, os.ErrNotExist):
		writeError(w, 404, "Worker Matrix input not found")
	default:
		writeError(w, 503, "Worker Matrix state unavailable")
	}
}
