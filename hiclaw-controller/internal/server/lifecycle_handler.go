package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LifecycleHandler handles imperative worker lifecycle operations.
type LifecycleHandler struct {
	k8s                   client.Client
	registry              *backend.Registry
	namespace             string
	defaultBackendRuntime string

	readyMu sync.RWMutex
	ready   map[string]bool
}

func NewLifecycleHandler(k8s client.Client, registry *backend.Registry, namespace, defaultBackendRuntime string) *LifecycleHandler {
	return &LifecycleHandler{
		k8s:                   k8s,
		registry:              registry,
		namespace:             namespace,
		defaultBackendRuntime: defaultBackendRuntime,
		ready:                 make(map[string]bool),
	}
}

// resolveBackendRuntime returns the worker's explicit backendRuntime first,
// then the cluster default.
func (h *LifecycleHandler) resolveBackendRuntime(ctx context.Context, worker *v1beta1.Worker) string {
	br := worker.Spec.GetBackendRuntime()
	if br == "" {
		br = h.defaultBackendRuntime
	}
	return br
}

func (h *LifecycleHandler) resolveWorkerBackend(ctx context.Context, worker *v1beta1.Worker) backend.WorkerBackend {
	backendRuntime := h.resolveBackendRuntime(ctx, worker)
	b, err := h.registry.GetBackendForType(ctx, backendRuntime)
	if err != nil {
		return h.registry.DetectWorkerBackend(ctx)
	}
	return b
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
	b := h.resolveWorkerBackend(r.Context(), &worker)
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
	b := h.resolveWorkerBackend(r.Context(), &worker)
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

	if worker.Status.Phase == "Stopped" || worker.Status.Phase == "Sleeping" {
		// Set desired state in spec (declarative)
		running := "Running"
		worker.Spec.State = &running
		if err := h.k8s.Update(r.Context(), &worker); err != nil {
			writeK8sError(w, "update worker spec.state", err)
			return
		}

		// Directly operate on backend for immediate response
		b := h.resolveWorkerBackend(r.Context(), &worker)
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
	if phase == "Running" && h.isReady(name) {
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

	lastActiveAt, ok := parseReadyLastActiveAt(w, r)
	if !ok {
		return
	}

	// Authorization (self-only for workers) is enforced by RequireAuthz middleware.
	h.setReady(name, true)
	if lastActiveAt != "" {
		if err := h.updateLastActiveAt(r.Context(), name, lastActiveAt); err != nil {
			if errors.Is(err, errForbidden) {
				httputil.WriteError(w, http.StatusForbidden, err.Error())
				return
			}
			writeK8sError(w, "update worker lastActiveAt", err)
			return
		}
	}
	log.Printf("[READY] Worker %s reported ready", name)
	w.WriteHeader(http.StatusNoContent)
}

// Heartbeat handles POST /api/v1/workers/{name}/heartbeat — periodic heartbeat from worker.
func (h *LifecycleHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required")
		return
	}

	lastActiveAt, ok := parseReadyLastActiveAt(w, r)
	if !ok {
		return
	}

	if err := h.updateLastHeartbeat(r.Context(), name); err != nil {
		if errors.Is(err, errForbidden) {
			httputil.WriteError(w, http.StatusForbidden, err.Error())
			return
		}
		writeK8sError(w, "update worker lastHeartbeat", err)
		return
	}
	if lastActiveAt != "" {
		if err := h.updateLastActiveAt(r.Context(), name, lastActiveAt); err != nil {
			if errors.Is(err, errForbidden) {
				httputil.WriteError(w, http.StatusForbidden, err.Error())
				return
			}
			writeK8sError(w, "update worker lastActiveAt", err)
			return
		}
	}
	log.Printf("[HEARTBEAT] Worker %s heartbeat", name)
	w.WriteHeader(http.StatusNoContent)
}

func (h *LifecycleHandler) updateLastHeartbeat(ctx context.Context, name string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var worker v1beta1.Worker
		err := h.k8s.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
		if err == nil {
			worker.Status.LastHeartbeat = now
			worker.Status.Phase = "Running"
			return h.k8s.Status().Update(ctx, &worker)
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		return apierrors.NewNotFound(v1beta1.Resource("workers"), name)
	})
}

type readyRequest struct {
	LastActiveAt string `json:"lastActiveAt,omitempty"`
}

func parseReadyLastActiveAt(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", true
	}
	defer r.Body.Close()

	var req readyRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return "", false
	}
	if req.LastActiveAt == "" {
		return "", true
	}
	t, err := time.Parse(time.RFC3339, req.LastActiveAt)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "lastActiveAt must be RFC3339")
		return "", false
	}
	if t.After(time.Now().Add(5 * time.Minute)) {
		httputil.WriteError(w, http.StatusBadRequest, "lastActiveAt is too far in the future")
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}

func (h *LifecycleHandler) updateLastActiveAt(ctx context.Context, name, lastActiveAt string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var worker v1beta1.Worker
		err := h.k8s.Get(ctx, client.ObjectKey{Name: name, Namespace: h.namespace}, &worker)
		if err == nil {
			if !isNewerRFC3339(lastActiveAt, worker.Status.LastActiveAt) {
				return nil
			}
			worker.Status.LastActiveAt = lastActiveAt
			return h.k8s.Status().Update(ctx, &worker)
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		return apierrors.NewNotFound(v1beta1.Resource("workers"), name)
	})
}

// errForbidden is a sentinel used by requireCallerSameTeam so that callers
// can distinguish authorization failures from transient K8s errors.
var errForbidden = errors.New("forbidden")

// requireCallerSameTeam checks that the authenticated caller belongs to the
// same team as the target resource. This prevents cross-team writes to
// LastHeartbeat/LastActiveAt when the target has no standalone Worker CR
// (i.e. ResourceTeam was empty at the middleware level).
func requireCallerSameTeam(ctx context.Context, targetTeam string) error {
	caller := auth.CallerFromContext(ctx)
	if caller == nil {
		// Unauthenticated (shouldn't happen behind auth middleware); deny.
		return fmt.Errorf("%w: unauthenticated caller", errForbidden)
	}
	// Admin and Manager are trusted; standalone workers already pass
	// requireSelf in middleware so they won't reach this path.
	if caller.Role == auth.RoleAdmin || caller.Role == auth.RoleManager {
		return nil
	}
	if caller.Team != "" && caller.Team != targetTeam {
		return fmt.Errorf("%w: caller team %q does not match target team %q", errForbidden, caller.Team, targetTeam)
	}
	return nil
}

func isNewerRFC3339(next, current string) bool {
	if next == "" {
		return false
	}
	if current == "" {
		return true
	}
	nextTime, err := time.Parse(time.RFC3339, next)
	if err != nil {
		return false
	}
	currentTime, err := time.Parse(time.RFC3339, current)
	if err != nil {
		return true
	}
	return nextTime.After(currentTime)
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

	resp := workerToResponse(r.Context(), h.k8s, h.namespace, &worker)

	b := h.resolveWorkerBackend(r.Context(), &worker)
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
