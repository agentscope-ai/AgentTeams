package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RuntimeHandoff records and reads the preflight shared by operator clients.
// Readback works when the source is offline, including after Controller restart.
func (h *WorkerMatrixHandler) RuntimeHandoff(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager) {
		writeError(w, 403, "Worker runtime changes require Manager or platform Admin")
		return
	}
	if caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.deps.Namespace {
		writeError(w, 403, "operator belongs to another Controller namespace")
		return
	}
	reader := h.deps.APIReader
	if reader == nil {
		reader = h.deps.Client
	}
	var worker v1beta1.Worker
	if err := reader.Get(r.Context(), client.ObjectKey{Namespace: h.deps.Namespace, Name: r.PathValue("name")}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}
	if h.deps.OSS == nil {
		writeError(w, 503, "Worker state storage unavailable")
		return
	}
	name := worker.Spec.EffectiveWorkerName(worker.Name)
	store := service.WorkerRuntimeStore{Storage: h.deps.OSS}
	respond := func(state *service.WorkerRuntimeState, etag string, err error) {
		if err != nil {
			h.writeError(w, err)
			return
		}
		if etag != "" {
			w.Header().Set("ETag", `"`+etag+`"`)
		}
		status := service.WorkerRuntimeStatus{WorkerRuntimeState: *state}
		if r.Method == http.MethodGet {
			status.TargetFailure = h.handoffTargetFailure(r.Context(), worker.Name, state.Handoff)
		}
		httputil.WriteJSON(w, http.StatusOK, status)
	}
	if r.Method == http.MethodGet {
		respond(store.Load(r.Context(), name))
		return
	}
	var input struct {
		RequestID string `json:"request_id"`
		Target    string `json:"target_runtime"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "one handoff request with request_id and target_runtime required")
		return
	}
	create, match := r.Header.Get("If-None-Match"), r.Header.Get("If-Match")
	if create == "" && match == "" {
		writeError(w, 428, "read the current handoff before starting preflight")
		return
	}
	expected := strings.Trim(match, `"`)
	condition, err := hex.DecodeString(expected)
	if (create != "" && (create != "*" || match != "")) || (create == "" && (err != nil || len(condition) != 16)) {
		writeError(w, 400, "use one exact ETag or create-if-absent condition")
		return
	}
	if !service.ValidWorkerHandoffRequest(input.RequestID, input.Target) {
		writeError(w, 400, "valid request ID and supported target Harness required")
		return
	}
	setAnnotation := func(value string) error {
		if worker.Annotations[service.WorkerRuntimeHandoffAnnotation] == value {
			return nil
		}
		before := worker.DeepCopy()
		if worker.Annotations == nil {
			worker.Annotations = make(map[string]string)
		}
		worker.Annotations[service.WorkerRuntimeHandoffAnnotation] = value
		return h.deps.Client.Patch(r.Context(), &worker, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	enroll := func() bool {
		if worker.Annotations[service.WorkerRuntimeHandoffAnnotation] != "" {
			return true
		}
		if err := setAnnotation("true"); err != nil {
			writeK8sError(w, "enable Worker handoff reconciliation", err)
			return false
		}
		return true
	}
	respondToOperation := func(state *service.WorkerRuntimeState, etag string, err error) {
		if err == nil && state.Handoff != nil && state.Handoff.Phase != "completed" {
			// The enrollment watch may run before this storage commit. Signal
			// the durable operation so an idle Worker need not wait to poll.
			if signalErr := setAnnotation("request:" + state.Handoff.ID); signalErr != nil {
				log.FromContext(r.Context()).Error(signalErr, "could not notify Worker reconciler of durable handoff", "worker", worker.Name)
			}
		}
		respond(state, etag, err)
	}
	current, _, err := store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if op := current.Handoff; op != nil && (op.Phase != "completed" || op.ID == input.RequestID) {
		if op.ID != input.RequestID || op.TargetRuntime != input.Target {
			writeError(w, 409, "another handoff is already in progress")
			return
		}
		if !enroll() {
			return
		}
		// A lost response may be retried after the source went offline.
		respondToOperation(store.ResumeStop(r.Context(), name))
		return
	}
	if !worker.Spec.DesiredContainerMan() || worker.Spec.DesiredState() != "Running" || (worker.Spec.DeployMode != nil && *worker.Spec.DeployMode == v1beta1.DeployModeEdge) {
		writeError(w, 409, "preflight requires a managed Worker with desired state Running")
		return
	}
	matrix, _, err := h.store.Load(r.Context(), name)
	if err != nil {
		h.writeError(w, err)
		return
	}
	instance, runtime, err := h.recovery.runningWorkerInstance(r.Context(), &worker)
	if err != nil {
		writeError(w, 503, "current Worker start unavailable")
		return
	}
	start := matrix.ActiveStart
	if start.InstanceID != instance || start.Runtime != runtime || start.ProcessID == "" {
		writeError(w, 409, "the running Worker must register its current source start")
		return
	}
	backendName, _, _ := strings.Cut(instance, "/")
	wb, err := h.deps.Backend.GetWorkerBackend(r.Context(), backendName)
	if err != nil {
		writeError(w, 503, "source backend unavailable")
		return
	}
	if err := h.deps.HandoffMatrix.Validate(r.Context(), &worker, input.Target); err != nil {
		h.writeError(w, err)
		return
	}
	image, err := service.HandoffTargetImage(&worker, wb, runtime, input.Target)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if !enroll() {
		return
	}
	respondToOperation(store.Preflight(r.Context(), name, input.RequestID, input.Target, image, start, expected))
}

func (h *WorkerMatrixHandler) handoffTargetFailure(ctx context.Context, name string, op *service.WorkerHandoff) string {
	if op == nil || (op.Phase != "creating" && op.Phase != "recovering") || h.deps.Backend == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	backendName, _, _ := strings.Cut(op.Source.InstanceID, "/")
	wb, err := h.deps.Backend.GetWorkerBackend(ctx, backendName)
	if err != nil {
		return ""
	}
	status, err := wb.Status(ctx, name)
	if err != nil || status == nil || status.HandoffLabel != op.TargetLabel() || status.Runtime != op.TargetRuntime {
		return ""
	}
	switch status.Status {
	case backend.StatusStopped, backend.StatusFailed:
		if status.Message != "" {
			return status.Message
		}
		return "Target Worker is " + string(status.Status) + "."
	}
	return ""
}

// SelfHandoff exposes the registered target to its startup supervisor.
func (h *WorkerMatrixHandler) SelfHandoff(w http.ResponseWriter, r *http.Request) {
	name, instance, runtime, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	state, etag, err := (service.WorkerRuntimeStore{Storage: h.deps.OSS}).SyncRegisteredStart(r.Context(), name, instance, runtime)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
	httputil.WriteJSON(w, http.StatusOK, state)
}

func executionHandoff(ctx context.Context, store oss.StorageClient, name string, start service.WorkerStart) (*service.WorkerHandoff, error) {
	state, _, err := (service.WorkerRuntimeStore{Storage: store}).Load(ctx, name)
	if err != nil {
		return nil, err
	}
	if state.Handoff == nil || state.Handoff.Phase == "completed" {
		if state.ActiveStart != nil && *state.ActiveStart != start {
			return nil, fmt.Errorf("%w: register this actual Worker start before execution", service.ErrWorkerStateConflict)
		}
		return nil, nil
	}
	if op := state.Handoff; op != nil && op.Phase == "recovering" && op.Target != nil && *op.Target == start {
		// The backend and native-consumption registration both confirmed this
		// replacement. It uses the normal task and message claim path.
		return nil, nil
	}
	return state.Handoff, nil
}

func checkHandoffExecution(op *service.WorkerHandoff, start service.WorkerStart, action string) error {
	if op == nil || (op.Source == start && op.Phase == "stopping" && action != "claim") {
		return nil
	}
	return fmt.Errorf("Worker handoff %s is in progress; execution admission is closed", op.ID)
}

func writeHandoffAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrWorkerStateConflict) {
		writeError(w, 409, err.Error())
		return
	}
	writeError(w, 503, "Worker handoff state unavailable")
}

// Recheck after the per-execution CAS. A claim whose write straddled preflight
// must never return permission to start a native turn.
func handoffClaimStillOpen(w http.ResponseWriter, r *http.Request, store oss.StorageClient, name string, start service.WorkerStart) bool {
	op, err := executionHandoff(r.Context(), store, name, start)
	if err != nil {
		writeHandoffAdmissionError(w, err)
		return false
	}
	if op != nil {
		writeError(w, 409, "Worker handoff is in progress; new execution is closed")
		return false
	}
	return true
}
