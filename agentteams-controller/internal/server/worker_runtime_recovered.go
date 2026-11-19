package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

type handoffAcknowledgement struct {
	name           string
	start          service.WorkerStart
	record         *service.WorkerRuntimeState
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
	return &handoffAcknowledgement{name: name, start: start, record: state, etag: etag, expected: expected}
}

// ConfirmHandoffRecovery records a live replacement's startup readiness.
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
		state, etag, err = (service.WorkerRuntimeStore{Storage: h.deps.OSS}).Complete(r.Context(), state, etag)
		if err != nil {
			h.writeError(w, err)
			return
		}
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	httputil.WriteJSON(w, 200, state)
}
