package server

// Worker skill runtime state and preload policy
// (GET /api/v1/workers/{name}/skills and
// PUT /api/v1/workers/{name}/skills/{skill_name}/preload).
//
// Each worker's qwenpaw app (effective console port, default 8088) exposes
// the per-skill runtime API under /api/skills/.... The Controller proxies a
// fixed two-route surface so L1 admins and L2 humans can inspect — and set
// — a worker's skill runtime state, in particular the QwenPaw 2.2.1
// "preload" policy (the full skill text is injected into the agent's system
// prompt for every session; a deliberate always-on capability with a
// per-session token cost), from the workbench without reaching into the
// docker network:
//
//	GET  /api/v1/workers/{name}/skills                      per-worker skill list (runtime state incl. preload / enabled)
//	PUT  /api/v1/workers/{name}/skills/{skill_name}/preload set the preload policy (body {"preload": bool})
//
// Upstream contract (pinned in the test file's version-contract section):
//
//   - PUT is validated and persisted by the worker itself (its skill
//     manifest, skill.json) and the agent is hot-reloaded — no worker
//     restart, and the controller never touches worker-side qwenpaw-owned
//     state files (all operations go through the worker's own API).
//   - Version gate: workers running QwenPaw < 2.2.1 do not have the
//     preload router; the upstream 404 passes through verbatim (the same
//     signal the workspace-file proxy uses for its pre-2.1
//     router-missing heuristic), and clients render a placeholder. The
//     proxy itself is version-agnostic.
//   - No MinIO baseline read-back: unlike the channel proxy, skill.json has
//     no MinIO baseline copy (the push_loop baseline is agent.json), so
//     there is nothing to verify; re-applying a lost policy is a single
//     PUT away.
//
// Fixed-path forwarding only (the two routes above) — never a generic
// reverse proxy — so the attack surface stays bounded to two QwenPaw
// endpoints.
//
// Write scope: admin/manager (L1) may set preload on any worker; an L2
// human may set it on workers in their own teams (authorizer
// requireSameTeam + the handler's W8 404 for cross-team); team leaders are
// read-only (authorizer default deny, 403). Cross-team workers hide as 404
// so worker existence cannot be probed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// skillProxyTimeout bounds each upstream call to the worker's qwenpaw
	// app (same bound as the channel / checkpoint proxies).
	skillProxyTimeout = 5 * time.Second

	// skillBodyCap bounds the proxied request/response bodies — skill
	// lists and preload payloads are small documents; the cap guards
	// against a misbehaving upstream.
	skillBodyCap = 64 << 10
)

// skillNamePattern matches valid qwenpaw skill names: the same charset the
// manager skill-sync script accepts (alphanumeric, dot, dash, underscore;
// first character alphanumeric). Path separators, leading dots and ".."
// are rejected before the dial so a crafted name can never be injected
// into the upstream URL.
var skillNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// WorkerSkillsHandler proxies the worker skill runtime endpoints.
type WorkerSkillsHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	http            *http.Client
	containerPrefix string
	// workerBaseURL resolves a worker name to its qwenpaw app base URL
	// from the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
}

// NewWorkerSkillsHandler creates the handler with the default embedded-mode
// worker address resolution (same chain as the workspace-file and channel
// proxies). containerPrefix must be the effective prefix from controller
// configuration (see config.ContainerPrefix).
func NewWorkerSkillsHandler(c client.Client, namespace, kubeMode, containerPrefix string) *WorkerSkillsHandler {
	h := &WorkerSkillsHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: skillProxyTimeout},
		containerPrefix: containerPrefix,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves the worker's qwenpaw app base URL via the
// effective container prefix and the system-wins console port — identical
// to the workspace-file and channel proxies, so the dial always targets the
// port the container listens on (a conflicting spec.env value is discarded
// at creation time).
func (h *WorkerSkillsHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// skillScope validates the worker name, enforces embedded mode and the W8
// team boundary, and returns the upstream base URL. It writes the HTTP
// error response itself and returns ok=false on any rejection.
func (h *WorkerSkillsHandler) skillScope(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return "", false
	}
	// Kube-mode check runs before any worker lookup: uniform 503 (rather
	// than a per-worker 404 vs 503 split) so worker existence cannot be
	// probed — same discipline as the checkpoint proxy.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker skill inspection requires embedded mode")
		return "", false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return "", false
		}
		writeK8sError(w, "get worker skills", err)
		return "", false
	}
	// findTeamMember's second return value is the member (worker) name, not
	// the team name — the scope check compares against the Team CR name.
	// Standalone workers (no team) resolve to "" which TeamMatches rejects,
	// hiding them from scoped callers as 404.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker skills", err)
		return "", false
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(teamName) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return "", false
	}
	return h.workerBaseURL(name, worker.Spec.Env), true
}

// validateSkillName enforces the skill-name charset (injection guard).
// ok=false means the 400 was already written.
func validateSkillName(w http.ResponseWriter, skill string) bool {
	if !skillNamePattern.MatchString(skill) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid skill name")
		return false
	}
	return true
}

// getWorkerSkills handles GET /api/v1/workers/{name}/skills. The upstream
// list endpoint takes no query parameters; any query parameter is rejected
// rather than forwarded (same discipline as the checkpoint proxy) so client
// mistakes surface immediately.
func (h *WorkerSkillsHandler) getWorkerSkills(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for key := range r.URL.Query() {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
		return
	}
	if base, ok := h.skillScope(w, r, name); !ok {
		return
	} else {
		h.serveSkills(w, r, base, http.MethodGet, "/api/skills", nil, false)
	}
}

// putWorkerSkillPreload handles PUT
// /api/v1/workers/{name}/skills/{skill_name}/preload. The body must be a
// JSON object with exactly one meaningful field, preload (bool); the raw
// body is forwarded verbatim (upstream is the authority on validation and
// persistence).
func (h *WorkerSkillsHandler) putWorkerSkillPreload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	skill := r.PathValue("skill_name")
	if !validateSkillName(w, skill) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, skillBodyCap+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if len(raw) > skillBodyCap {
		httputil.WriteError(w, http.StatusBadRequest, "request body is too large")
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object with a preload boolean field")
		return
	}
	// Fail fast on a malformed payload before the dial: the field must be
	// present and boolean ({"preload": true} / {"preload": false}). The
	// raw body is still forwarded verbatim when valid, so upstream
	// validation remains the final authority.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object with a preload boolean field")
		return
	}
	preloadRaw, present := doc["preload"]
	if !present {
		httputil.WriteError(w, http.StatusBadRequest, "request body is missing the preload field")
		return
	}
	// Strict boolean check: Go's json.Unmarshal treats null as a no-op for
	// bool targets (no error), so the literal must be matched explicitly —
	// upstream (pydantic) would reject null with a 422, and failing fast
	// with a clear 400 before the dial is the contract here.
	trimmed := strings.TrimSpace(string(preloadRaw))
	if trimmed != "true" && trimmed != "false" {
		httputil.WriteError(w, http.StatusBadRequest, "the preload field must be a boolean (true or false)")
		return
	}
	if base, ok := h.skillScope(w, r, name); !ok {
		return
	} else {
		h.serveSkills(w, r, base, http.MethodPut, "/api/skills/"+skill+"/preload", raw, true)
	}
}

// serveSkills dials the worker's qwenpaw app and maps the response (see
// the package doc for the status contract). mutates marks the audit-logged
// preload write.
func (h *WorkerSkillsHandler) serveSkills(w http.ResponseWriter, r *http.Request, base, method, upstream string, body []byte, mutates bool) {
	target := base + upstream
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target, bodyReader)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build skill request: "+err.Error())
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker skill API unreachable")
		return
	}
	defer resp.Body.Close()

	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, skillBodyCap))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read worker skill response: "+err.Error())
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	switch resp.StatusCode {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstreamBody)
		if mutates {
			log.FromContext(r.Context()).Info("worker skill preload policy updated",
				"worker", r.PathValue("name"),
				"skill", r.PathValue("skill_name"),
				"upstream", upstream,
				"actor", authzActor(caller))
		}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		// Passed through verbatim: 404 = unknown skill or a QwenPaw
		// version without the preload router (version gate); 400/422 =
		// upstream validation with an actionable detail; 409 = upstream
		// conflict. The upstream detail is the contract here.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(upstreamBody)
	default:
		httputil.WriteError(w, http.StatusBadGateway,
			fmt.Sprintf("worker skill API error (status %d): %s", resp.StatusCode, truncateForSkillMessage(string(upstreamBody))))
	}
}

// truncateForSkillMessage caps an upstream body embedded in an error
// message. (Deliberately distinct from the channel proxy's helper so the
// two files stay independently mergeable.)
func truncateForSkillMessage(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}
