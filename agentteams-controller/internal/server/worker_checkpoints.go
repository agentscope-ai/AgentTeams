package server

// Worker checkpoint inspection (GET /api/v1/workers/{name}/checkpoints/...).
//
// QwenPaw 2.1 ships a workspace checkpoint system — a framework-level hook
// auto-snapshots after every response round (plus manual /checkpoint
// snapshot), stored in {workspace}/checkpoints/shadow.git — and exposes it on
// each worker's qwenpaw app at :8088 (0.0.0.0 listen; no auth in worker
// context because no console user is registered). The Controller proxies two
// read-only subpaths so L2 humans and the workbench plugin can inspect every
// worker's execution timeline without reaching into the docker network
// directly.
//
// Embedded mode only: the worker app is reachable by container name inside
// the shared docker network. The effective container name prefix comes from
// configuration (AGENTTEAMS_PROXY_CONTAINER_PREFIX, or derived from
// AGENTTEAMS_RESOURCE_PREFIX when auto-prefixing is enabled; empty when
// auto-prefixing is disabled), and the port is the effective console port
// resolved through the same system-wins env chain used at container
// creation (service.EffectiveWorkerConsolePort — a conflicting spec.env
// value is discarded, so the container always listens on 8088). In kube
// mode there is no stable in-cluster DNS name for the worker pod, so the
// endpoints return 503.
//
// Fixed-path forwarding only (graph / status, plus the whitelisted limit
// query on graph) — never a generic reverse proxy, so the attack surface is
// limited to two read-only QwenPaw endpoints.

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// checkpointProxyTimeout bounds each upstream call.
	checkpointProxyTimeout = 5 * time.Second
)

// checkpointSubpaths is the fixed whitelist of forwardable QwenPaw endpoints.
var checkpointSubpaths = map[string]bool{
	"graph":  true,
	"status": true,
}

// workerNamePattern matches Kubernetes DNS label names (lowercase letters,
// digits, hyphens) — the only valid worker CR names, and the only characters
// safe to embed in the upstream container URL.
var workerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// CheckpointHandler proxies worker checkpoint read endpoints.
type CheckpointHandler struct {
	client    client.Client
	namespace string
	kubeMode  string
	http      *http.Client
	// containerPrefix is the effective worker container name prefix — the
	// same value the docker backend uses for container naming (derived from
	// AGENTTEAMS_PROXY_CONTAINER_PREFIX / AGENTTEAMS_RESOURCE_PREFIX /
	// auto-prefix; empty when auto-prefixing is disabled).
	containerPrefix string
	// workerBaseURL resolves a worker name to its qwenpaw app base URL from
	// the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
}

// NewCheckpointHandler creates the handler with the default embedded-mode
// worker address resolution. containerPrefix must be the effective prefix
// from controller configuration (see config.ContainerPrefix).
func NewCheckpointHandler(c client.Client, namespace, kubeMode, containerPrefix string) *CheckpointHandler {
	h := &CheckpointHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: checkpointProxyTimeout},
		containerPrefix: containerPrefix,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves a worker's qwenpaw app base URL from the
// effective container prefix and the effective console port. The port goes
// through service.EffectiveWorkerConsolePort — the same system-wins env
// chain used at container creation — so the proxy can never target a port
// the container does not listen on (a conflicting spec.env value is
// discarded before the container is created, so the raw spec.env must not
// be read here).
func (h *CheckpointHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// proxyCheckpoint handles GET /api/v1/workers/{name}/checkpoints/{sub}.
// Scoped callers (team leaders / L2 humans) may only inspect workers in the
// teams they control — mirrors GET /api/v1/workers/{name} (W8: 404, not 403,
// so worker existence cannot be probed).
func (h *CheckpointHandler) proxyCheckpoint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sub := r.PathValue("sub")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	if !checkpointSubpaths[sub] {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported checkpoint subpath")
		return
	}
	// Kube-mode check runs before any worker lookup: the endpoints are
	// entirely unavailable in kube mode, and a uniform 503 (rather than a
	// per-worker 404 vs 503 split) avoids leaking worker existence.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker checkpoint inspection requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker checkpoints", err)
		return
	}
	// Resolve the owning team for the scoped-caller check (same chain as
	// ResourceHandler.GetWorker: standalone workers hide as 404).
	_, team, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker checkpoints", err)
		return
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(team) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// Resolve the upstream address from the effective container prefix and
	// the worker's own env (per-worker console port override).
	target := h.workerBaseURL(name, worker.Spec.Env) + "/workspace/checkpoints/" + sub
	// Strict query whitelist: graph supports only ?limit= (1..1000, the same
	// bounds the upstream endpoint enforces); status takes no parameters.
	// Unknown parameters are rejected rather than silently dropped so client
	// mistakes surface immediately.
	q := r.URL.Query()
	for key := range q {
		if !(sub == "graph" && key == "limit") {
			httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return
		}
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be an integer between 1 and 1000")
			return
		}
		target += "?limit=" + raw
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build checkpoint request: "+err.Error())
		return
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker checkpoint API unreachable")
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	case http.StatusNotFound:
		// QwenPaw < 2.1 has no checkpoint router; translate the upstream
		// 404 into an actionable message instead of leaking it verbatim.
		httputil.WriteError(w, http.StatusBadGateway, "checkpoint API unavailable (requires QwenPaw 2.1)")
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("checkpoint API error (status %d): %s", resp.StatusCode, string(body)))
	}
}
