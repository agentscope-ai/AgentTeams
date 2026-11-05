// Package server — per-runtime member config surface (P8, controller-side).
//
// The controller exposes each runtime's OWN native config surface. The
// surface description is a runtime-level static resource (released with the
// binary, strongly cached, instance-independent); the member config
// read/write is a thin file plane INSIDE the worker container (embedded
// docker mode only, same exec capability as coding-cli). The controller
// never translates fields between runtimes (v5.2): the surface is the
// runtime's own schema description, and OpenClaw's zod strict validation
// is the final arbiter for opaque object containers.
//
// Routes (fixed, per the P8 design §4):
//
//	GET  /api/runtimes/{runtime}/surface            static surface (immutable cache + ETag)
//	GET  /api/members/{name}/config                 current values + rev + reconcile state
//	PUT  /api/members/{name}/config?if-rev=...      merge declared keys (422 unknown_field / 409 stale)
//	POST /api/members/{name}/config/rollback?rev=... restore a pre-write snapshot
//
// Discipline (mirrors coding-cli / channels):
//   - embedded-mode gate (uniform 503 in kube mode — the exec capability is
//     docker-backend only; uniform so worker existence cannot be probed);
//   - team scope via the same findTeamMember predicate; hidden resources ->
//     404, never 403 (W8);
//   - team leaders are read-only (403 on writes);
//   - permissions come from the standing authorizer (ActionGet/ActionUpdate
//     on kind "worker") — no authorization logic invented here;
//   - every mutation is audit-logged.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig/openclaw"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// MemberConfigHandler serves the per-runtime member config surface.
type MemberConfigHandler struct {
	client    client.Client
	namespace string
	kubeMode  string
	audit     *audit.Client

	// execBackend resolves the worker's exec-capable backend (docker in
	// embedded mode). Injectable for tests; default reads from the
	// backend registry.
	execBackend func(ctx context.Context, name string) (openclaw.ExecBackend, error)

	store           openclaw.KeyStore
	reg             *runtimeconfig.Registry
	defaultRuntime  string
	openclawAdapter *openclaw.Adapter

	// nativeConfigPolicy is the model-access policy seam for native config
	// writes (wired by PR-B). nil = not wired yet (current behaviour).
	nativeConfigPolicy NativeConfigPolicy
}

// NewMemberConfigHandler creates the handler. runtimes is the registry of
// compiled-in runtime adapters (openclaw in P8a; hermes joins in P8b); the
// openclaw entry doubles as the concrete write-plane adapter for workers
// whose resolved runtime is openclaw.
func NewMemberConfigHandler(c client.Client, namespace, kubeMode string, ac *audit.Client, reg *backend.Registry, store openclaw.KeyStore, runtimes *runtimeconfig.Registry, defaultRuntime string) *MemberConfigHandler {
	h := &MemberConfigHandler{
		client:         c,
		namespace:      namespace,
		kubeMode:       kubeMode,
		audit:          ac,
		store:          store,
		reg:            runtimes,
		defaultRuntime: defaultRuntime,
	}
	if a, ok := runtimes.Lookup("openclaw"); ok {
		if ad, ok := a.(*openclaw.Adapter); ok {
			h.openclawAdapter = ad
		}
	}
	h.execBackend = func(ctx context.Context, _ string) (openclaw.ExecBackend, error) {
		if reg == nil {
			return nil, errors.New("no worker backend available")
		}
		b := reg.DetectWorkerBackend(ctx)
		if b == nil {
			return nil, errors.New("no worker backend available")
		}
		eb, ok := b.(openclaw.ExecBackend)
		if !ok {
			return nil, fmt.Errorf("worker backend %q does not support container exec", b.Name())
		}
		return eb, nil
	}
	return h
}

// --- scope ---------------------------------------------------------------

// memberScope validates the worker name, enforces embedded mode and team
// scope, and resolves the exec-capable backend. Mirrors codingCliScope:
// hidden resources are 404, never 403 (W8).
func (h *MemberConfigHandler) memberScope(w http.ResponseWriter, r *http.Request, name string) (v1beta1.Worker, openclaw.ExecBackend, bool) {
	var worker v1beta1.Worker
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return worker, nil, false
	}
	// Uniform 503 in non-embedded mode: the exec capability is
	// docker-backend only; a uniform answer keeps worker existence
	// unprobed.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker config management requires embedded mode")
		return worker, nil, false
	}
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return worker, nil, false
		}
		writeK8sError(w, "get worker config", err)
		return worker, nil, false
	}
	// Standalone workers (no team) resolve to "" which TeamMatches rejects,
	// hiding them from team-scoped callers as 404.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker config", err)
		return worker, nil, false
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) {
		allowed := caller.TeamMatches(teamName)
		if !allowed && r.Method == http.MethodGet {
			allowed = caller.WorkerReadable(teamName, name)
		}
		if !allowed {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return worker, nil, false
		}
	}
	eb, err := h.execBackend(r.Context(), name)
	if err != nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker exec backend unavailable: "+err.Error())
		return worker, nil, false
	}
	return worker, eb, true
}

// managerFor resolves the worker's runtime through the controller's
// standard runtime resolution (backend.ResolveRuntime: an empty
// spec.runtime means the install default, normally openclaw — the same
// helper worker_controller uses, so a legacy worker whose container
// actually runs openclaw stays configurable) and returns the config
// manager for the registered adapter. A resolved runtime with no
// registered surface is 409 runtime_unknown with the supported list —
// never a fallback adapter (writing to the wrong runtime is worse than
// refusing).
func (h *MemberConfigHandler) managerFor(w http.ResponseWriter, runtime string, eb openclaw.ExecBackend) *openclaw.ConfigManager {
	openclawRuntime := ""
	if h.openclawAdapter != nil {
		openclawRuntime = h.openclawAdapter.Surface().Runtime
	}
	if runtime != openclawRuntime {
		httputil.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":     "runtime_unknown",
			"runtime":   runtime,
			"supported": h.reg.Runtimes(),
		})
		return nil
	}
	return openclaw.NewConfigManager(h.openclawAdapter, eb, h.store)
}

// --- surface ---------------------------------------------------------------

// getSurface serves the runtime's static surface description. It is a
// runtime-level resource with no instance data: login only (no team
// scope), strongly cached with an ETag. The optional ?version= query is
// informational only — the surface ships with the binary, so the content
// is always this binary's.
func (h *MemberConfigHandler) getSurface(w http.ResponseWriter, r *http.Request) {
	runtime := r.PathValue("runtime")
	adapter, ok := h.reg.Lookup(runtime)
	if !ok {
		httputil.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error":     "unknown runtime",
			"runtime":   runtime,
			"supported": h.reg.Runtimes(),
		})
		return
	}
	surface := adapter.Surface()
	etag := "\"" + surface.Runtime + "-" + surface.RuntimeVersion + "-" + surface.SurfaceVersion + "\""
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, surface)
}

// etagMatches implements the subset of RFC 9110 If-None-Match the
// controller needs: a comma-separated list compared against one ETag,
// plus the weak-comparison wildcard "*".
func etagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		c := strings.TrimSpace(candidate)
		if c == "*" || c == etag {
			return true
		}
	}
	return false
}

// --- read / write / rollback -----------------------------------------------

func (h *MemberConfigHandler) getConfig(w http.ResponseWriter, r *http.Request) {
	worker, eb, ok := h.memberScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	runtime := backend.ResolveRuntime(worker.Spec.Runtime, h.defaultRuntime)
	mgr := h.managerFor(w, runtime, eb)
	if mgr == nil {
		return
	}
	st, err := mgr.Read(r.Context(), worker.Name)
	if err != nil {
		writeConfigStoreError(w, "read worker config", err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, st)
}

func (h *MemberConfigHandler) putConfig(w http.ResponseWriter, r *http.Request) {
	worker, eb, ok := h.memberScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker config")
		return
	}
	runtime := backend.ResolveRuntime(worker.Spec.Runtime, h.defaultRuntime)
	mgr := h.managerFor(w, runtime, eb)
	if mgr == nil {
		return
	}
	body, ok := readCodingCliBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Patch map[string]any `json:"patch"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be an object with a \"patch\" field: "+err.Error())
		return
	}
	if req.Patch == nil {
		httputil.WriteError(w, http.StatusBadRequest, `the "patch" object is required`)
		return
	}
	if h.nativeConfigPolicy != nil {
		read := func(ctx context.Context) (map[string]any, error) {
			st, err := mgr.Read(ctx, worker.Name)
			if err != nil {
				return nil, err
			}
			return st.Values, nil
		}
		if err := h.nativeConfigPolicy.CheckNativeWrite(r.Context(), caller, worker.Name, runtime, req.Patch, read); err != nil {
			var pe *NativeConfigPolicyError
			if errors.As(err, &pe) {
				httputil.WriteJSON(w, http.StatusForbidden, map[string]any{
					"error": pe.Code, "message": pe.Message,
				})
				return
			}
			httputil.WriteError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	res, err := mgr.Write(r.Context(), worker.Name, req.Patch, r.URL.Query().Get("if-rev"))
	if err != nil {
		writeConfigStoreError(w, "write worker config", err)
		return
	}
	h.auditConfigWrite(r.Context(), worker.Name, "member_config_write", caller)
	log.FromContext(r.Context()).Info("worker member config updated",
		"worker", worker.Name, "rev", res.Rev, "actor", authzActor(caller))
	httputil.WriteJSON(w, http.StatusOK, res)
}

func (h *MemberConfigHandler) rollbackConfig(w http.ResponseWriter, r *http.Request) {
	worker, eb, ok := h.memberScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker config")
		return
	}
	rev := r.URL.Query().Get("rev")
	if !openclaw.ValidRev(rev) {
		httputil.WriteError(w, http.StatusBadRequest, "rev must be a 64-char sha256 hex string")
		return
	}
	runtime := backend.ResolveRuntime(worker.Spec.Runtime, h.defaultRuntime)
	mgr := h.managerFor(w, runtime, eb)
	if mgr == nil {
		return
	}
	res, err := mgr.Rollback(r.Context(), worker.Name, rev)
	if err != nil {
		writeConfigStoreError(w, "rollback worker config", err)
		return
	}
	h.auditConfigWrite(r.Context(), worker.Name, "member_config_rollback", caller)
	log.FromContext(r.Context()).Info("worker member config rolled back",
		"worker", worker.Name, "rev", res.Rev, "actor", authzActor(caller))
	httputil.WriteJSON(w, http.StatusOK, res)
}

// --- native config policy seam ------------------------------------------------

// NativeConfigPolicy decides whether a caller may write a runtime's native
// configuration. Implementations live in the model-access layer; nil means
// "not wired yet" (current behaviour).
type NativeConfigPolicy interface {
	CheckNativeWrite(ctx context.Context, caller *authpkg.CallerIdentity, worker, runtime string,
		patch map[string]any, readCurrent func(context.Context) (map[string]any, error)) error
}

// NativeConfigPolicyError carries a machine-readable code for 403 responses.
type NativeConfigPolicyError struct {
	Code    string
	Message string
}

func (e *NativeConfigPolicyError) Error() string { return e.Code + ": " + e.Message }

// --- errors / audit ---------------------------------------------------------

// writeConfigStoreError maps config-store errors to HTTP statuses
// (fail-closed: anything unexpected is an opaque 502, never a guessed
// success).
func writeConfigStoreError(w http.ResponseWriter, op string, err error) {
	var (
		missing *openclaw.ConfigFileMissingError
		stale   *openclaw.StaleRevisionError
		unknown *openclaw.UnknownFieldsError
		nokeys  *openclaw.NoWritableKeysError
		runtime *openclaw.UnknownRuntimeError
		verr    *openclaw.ValidationError
	)
	switch {
	case errors.As(err, &missing):
		httputil.WriteError(w, http.StatusBadGateway, op+": "+err.Error())
	case errors.As(err, &stale):
		httputil.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":    "stale_rev",
			"current":  stale.Current,
			"expected": stale.Expected,
		})
	case errors.As(err, &unknown):
		httputil.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "unknown_field",
			"fields": unknown.Fields,
		})
	case errors.As(err, &nokeys):
		httputil.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       "no_writable_keys",
			"unsupported": nokeys.Unsupported,
		})
	case errors.As(err, &verr):
		httputil.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "invalid_value",
			"key":    verr.Key,
			"detail": verr.Err.Error(),
		})
	case errors.As(err, &runtime):
		httputil.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":     "runtime_unknown",
			"runtime":   runtime.Runtime,
			"supported": runtime.Supported,
		})
	default:
		httputil.WriteError(w, http.StatusBadGateway, op+": "+err.Error())
	}
}

func (h *MemberConfigHandler) auditConfigWrite(ctx context.Context, worker, action string, caller *authpkg.CallerIdentity) {
	if h.audit == nil || caller == nil {
		return
	}
	h.audit.Record(ctx, audit.Event{
		Who:    caller.Username,
		Role:   caller.Role,
		Target: worker + "/config",
		Action: action,
	})
}
