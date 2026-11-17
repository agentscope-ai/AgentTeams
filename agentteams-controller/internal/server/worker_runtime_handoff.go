package server

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RuntimeHandoff records and reads the preflight shared by operator clients.
// Readback works when the source is offline, including after Controller restart.
func (h *WorkerMatrixHandler) RuntimeHandoff(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager) {
		writeError(w, 403, "Worker runtime changes require Manager or platform Admin")
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
		httputil.WriteJSON(w, http.StatusOK, state)
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
	respond(store.Preflight(r.Context(), name, input.RequestID, input.Target, start, expected))
}
