package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LifecycleHandler handles imperative worker lifecycle operations.
type LifecycleHandler struct {
	k8s       client.Client
	registry  *backend.Registry
	namespace string

	readyMu sync.RWMutex
	ready   map[string]bool
}

func NewLifecycleHandler(k8s client.Client, registry *backend.Registry, namespace string) *LifecycleHandler {
	return &LifecycleHandler{
		k8s:       k8s,
		registry:  registry,
		namespace: namespace,
		ready:     make(map[string]bool),
	}
}

// Wake handles POST /api/v1/workers/{name}/wake
func (h *LifecycleHandler) Wake(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var worker v1beta1.Worker
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}

	// Set desired state in spec (declarative, triggers reconciler)
	running := "Running"
	worker.Spec.State = &running
	if err := h.k8s.Update(r.Context(), &worker); err != nil {
		writeK8sError(w, "update worker spec.state", err)
		return
	}

	// Directly operate on backend for immediate response
	b := h.registry.DetectWorkerBackend(r.Context())
	if b != nil {
		_ = b.Start(r.Context(), name)
	}

	h.setReady(name, false)

	// Refresh and update status
	_ = h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
	worker.Status.Phase = "Running"
	worker.Status.Message = ""
	_ = h.k8s.Status().Update(r.Context(), &worker)

	httputil.WriteJSON(w, http.StatusOK, WorkerLifecycleResponse{Name: name, Phase: "Running"})
}

// Sleep handles POST /api/v1/workers/{name}/sleep
func (h *LifecycleHandler) Sleep(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var worker v1beta1.Worker
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}

	// Set desired state in spec (declarative, triggers reconciler)
	sleeping := "Sleeping"
	worker.Spec.State = &sleeping
	if err := h.k8s.Update(r.Context(), &worker); err != nil {
		writeK8sError(w, "update worker spec.state", err)
		return
	}

	// Directly operate on backend for immediate response
	b := h.registry.DetectWorkerBackend(r.Context())
	if b != nil {
		_ = b.Stop(r.Context(), name)
	}

	h.setReady(name, false)

	// Refresh and update status
	_ = h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
	worker.Status.Phase = "Sleeping"
	worker.Status.Message = ""
	_ = h.k8s.Status().Update(r.Context(), &worker)

	httputil.WriteJSON(w, http.StatusOK, WorkerLifecycleResponse{Name: name, Phase: "Sleeping"})
}

// EnsureReady handles POST /api/v1/workers/{name}/ensure-ready
func (h *LifecycleHandler) EnsureReady(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var worker v1beta1.Worker
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}

	b := h.registry.DetectWorkerBackend(r.Context())
	needsStart := worker.Status.Phase == "Stopped" || worker.Status.Phase == "Sleeping"
	// backendRunning gates the "Ready" answer. The CR phase is written asynchronously
	// and the ready map only records the last self-report, so neither notices a
	// container that was stopped or removed afterwards: ask the backend. With no
	// backend detected there is nothing to ask, and the self-report stays the signal.
	backendRunning := b == nil
	if worker.Status.Phase == "Running" && b != nil {
		result, err := b.Status(r.Context(), name)
		switch {
		case err != nil || result == nil:
			// Fail closed: an undeterminable backend must not be reported Ready.
			log.Printf("[WARN] ensure-ready status worker %s: %v", name, err)
		case result.Status == backend.StatusRunning:
			backendRunning = true
		case result.Status == backend.StatusStopped || result.Status == backend.StatusNotFound:
			log.Printf("[INFO] ensure-ready worker %s: phase Running but backend reports %s, starting", name, result.Status)
			needsStart = true
		}
	}

	if needsStart {
		// Set desired state in spec (declarative)
		running := "Running"
		worker.Spec.State = &running
		if err := h.k8s.Update(r.Context(), &worker); err != nil {
			writeK8sError(w, "update worker spec.state", err)
			return
		}

		// Directly operate on backend for immediate response
		if b != nil {
			if err := b.Start(r.Context(), name); err != nil {
				// Start may fail if container/pod was removed (Stopped state on K8s).
				// The reconciler will handle recreation.
				log.Printf("[WARN] ensure-ready start worker %s: %v (reconciler will retry)", name, err)
			}
		}

		h.setReady(name, false)

		// Refresh and update status
		_ = h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
		worker.Status.Phase = "Running"
		worker.Status.Message = ""
		_ = h.k8s.Status().Update(r.Context(), &worker)
	}

	phase := worker.Status.Phase
	if phase == "Running" && backendRunning && h.isReady(name) {
		phase = "Ready"
	}

	httputil.WriteJSON(w, http.StatusOK, WorkerLifecycleResponse{Name: name, Phase: phase})
}

// Ready handles POST /api/v1/workers/{name}/ready — worker self-reports readiness.
func (h *LifecycleHandler) Ready(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	// Authorization (self-only for workers) is enforced by RequireAuthz middleware.
	h.setReady(name, true)
	h.updateWorkerRuntimeReport(name, readWorkerRuntimeReport(r))
	log.Printf("[READY] Worker %s reported ready", name)
	w.WriteHeader(http.StatusNoContent)
}

// workerRuntimeReport is the optional heartbeat payload sent by workers.
// Every field is optional so legacy workers (empty body) keep working.
type workerRuntimeReport struct {
	LastActiveAt     string `json:"lastActiveAt"`
	AgentStatus      string `json:"agentStatus"`
	RunningTaskCount *int   `json:"runningTaskCount"`
	LastRunAt        string `json:"lastRunAt"`
	LastFinishAt     string `json:"lastFinishAt"`
}

func readWorkerRuntimeReport(r *http.Request) workerRuntimeReport {
	var report workerRuntimeReport
	if r.Body != nil {
		// Best effort: a malformed or empty body must not fail the heartbeat.
		_ = json.NewDecoder(r.Body).Decode(&report)
	}
	return report
}

// updateWorkerRuntimeReport persists the runtime-reported fields onto the
// Worker status. Failures are non-fatal: the next heartbeat re-sends them.
func (h *LifecycleHandler) updateWorkerRuntimeReport(name string, report workerRuntimeReport) {
	if report.LastActiveAt == "" && report.AgentStatus == "" &&
		report.RunningTaskCount == nil && report.LastRunAt == "" && report.LastFinishAt == "" {
		return
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var worker v1beta1.Worker
		if err := h.k8s.Get(context.Background(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
			return client.IgnoreNotFound(err)
		}
		changed := false
		if report.LastActiveAt != "" && isLastActiveNewer(report.LastActiveAt, worker.Status.LastActiveAt) {
			worker.Status.LastActiveAt = report.LastActiveAt
			changed = true
		}
		if report.AgentStatus != "" && report.AgentStatus != worker.Status.AgentStatus {
			worker.Status.AgentStatus = report.AgentStatus
			changed = true
		}
		if report.RunningTaskCount != nil &&
			(worker.Status.RunningTaskCount == nil || *report.RunningTaskCount != *worker.Status.RunningTaskCount) {
			count := *report.RunningTaskCount
			worker.Status.RunningTaskCount = &count
			changed = true
		}
		if report.LastRunAt != "" && report.LastRunAt != worker.Status.LastRunAt {
			worker.Status.LastRunAt = report.LastRunAt
			changed = true
		}
		if report.LastFinishAt != "" && report.LastFinishAt != worker.Status.LastFinishAt {
			worker.Status.LastFinishAt = report.LastFinishAt
			changed = true
		}
		if !changed {
			return nil
		}
		return h.k8s.Status().Update(context.Background(), &worker)
	})
	if err != nil {
		log.Printf("[READY] worker %s runtime report persist failed (non-fatal): %v", name, err)
	}
}

// GetWorkerRuntimeStatus handles GET /api/v1/workers/{name}/status — aggregates CR + backend state.
func (h *LifecycleHandler) GetWorkerRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	var worker v1beta1.Worker
	if err := h.k8s.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		writeK8sError(w, "get worker", err)
		return
	}

	resp := workerToResponse(&worker)

	if team, member, ok, terr := findTeamMember(r.Context(), h.k8s, h.namespace, name); terr != nil {
		writeK8sError(w, "get worker team membership", terr)
		return
	} else if ok {
		applyTeamMember(&resp, team, member)
	}

	b := h.registry.DetectWorkerBackend(r.Context())
	if b != nil {
		result, err := b.Status(r.Context(), name)
		if err == nil && result != nil {
			resp.Message = "backend=" + result.Backend + " status=" + string(result.Status)
			if result.Message != "" {
				resp.Message += " message=" + result.Message
			}
			resp.ContainerState = string(result.Status)
			if result.Status == backend.StatusRunning && h.isReady(name) {
				resp.Phase = "Ready"
			}
		}
	}

	httputil.WriteJSON(w, http.StatusOK, resp)
}

// --- readiness helpers ---

func (h *LifecycleHandler) setReady(name string, ready bool) {
	h.readyMu.Lock()
	defer h.readyMu.Unlock()
	if ready {
		h.ready[name] = true
	} else {
		delete(h.ready, name)
	}
}

func (h *LifecycleHandler) isReady(name string) bool {
	h.readyMu.RLock()
	defer h.readyMu.RUnlock()
	return h.ready[name]
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backend.ErrNotFound):
		httputil.WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, backend.ErrConflict):
		httputil.WriteError(w, http.StatusConflict, err.Error())
	default:
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
	}
}
