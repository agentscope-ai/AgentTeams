package server

// Worker tool-approval control (GET/PUT /api/v1/workers/{name}/approval).
//
// Each QwenPaw worker's agent profile (agent.json) carries an
// `approval_level` — the tool-execution security level that decides which
// tool calls run automatically and which pause for a human approval:
// STRICT (every tool needs approval), SMART (low-risk tools auto-allowed),
// AUTO (only guarded tools — the upstream default), or OFF (guard
// disabled). The worker's qwenpaw app exposes it on
// GET/PUT /workspace/running-config: the field round-trips through the
// running-config object and is written back into the agent profile by the
// app itself.
//
// The Controller proxies a minimal surface so L2 humans can read and set
// the level of workers in their own teams (L1/admin/manager: any team;
// team leaders: read-only — consistent with the knowledge base write
// boundary):
//
//	GET  /api/v1/workers/{name}/approval  -> {"approval_level": "AUTO"}
//	PUT  /api/v1/workers/{name}/approval  <- {"approval_level": "STRICT"}
//
// Write safety: the upstream PUT expects the *full* running-config object
// (it persists whatever is sent, with a per-file path lock), so the proxy
// performs the safe-write pattern: GET the current object, change only
// `approval_level`, PUT the whole object back. Every other field round
// trips verbatim. The approval value is validated against the four known
// levels *before* the worker is touched (the upstream model accepts any
// string, so an unvalidated proxy would write garbage into agent.json).
//
// Embedded mode only (same addressing as the checkpoint and workspace-file
// proxies: effective container prefix + system-wins console port). Kube
// mode returns a uniform 503 before any worker lookup. Cross-team workers
// hide as 404 (W8 anti-probing, same as reads). A worker on a QwenPaw
// version without the running-config router surfaces the upstream 404
// verbatim (version-gate contract). Every successful change is
// audit-logged.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	// approvalProxyTimeout bounds each upstream call.
	approvalProxyTimeout = 5 * time.Second
	// upstreamConfigMax caps the upstream running-config response. The
	// safe-write pattern round-trips the FULL object, so a small hard cap
	// would silently truncate agent.json on the way back; instead the cap
	// sits well above any realistic running-config and exceeding it fails
	// loudly (502) rather than truncating.
	upstreamConfigMax = 1 << 20 // 1 MiB
)

// fetchUpstreamConfig performs the context-aware GET of the worker's
// running-config and enforces upstreamConfigMax with a loud 502 instead of
// silent truncation. It writes its own error responses and returns
// (body, status, ok).
func (h *ApprovalHandler) fetchUpstreamConfig(w http.ResponseWriter, r *http.Request, baseURL string) ([]byte, int, bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+"/workspace/running-config", nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build running-config request: "+err.Error())
		return nil, 0, false
	}
	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker approval API unreachable: "+err.Error())
		return nil, 0, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamConfigMax+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read running-config response: "+err.Error())
		return nil, 0, false
	}
	if len(body) > upstreamConfigMax {
		httputil.WriteError(w, http.StatusBadGateway, "worker running-config exceeds the 1 MiB proxy cap")
		return nil, 0, false
	}
	return body, resp.StatusCode, true
}

// approvalLevels is the fixed set of tool-execution security levels the
// QwenPaw agent profile understands (see AgentProfileConfig.approval_level
// in the QwenPaw config module). The upstream running-config model does
// not validate the value, so the proxy is the validation boundary.
var approvalLevels = map[string]bool{
	"STRICT": true, // every tool call needs approval
	"SMART":  true, // low-risk tools auto-allowed
	"AUTO":   true, // only guarded tools (upstream default)
	"OFF":    true, // guard disabled
}

// ApprovalHandler proxies the worker tool-approval endpoints.
type ApprovalHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	http            *http.Client
	containerPrefix string
	// workerBaseURL resolves a worker name to its qwenpaw app base URL
	// from the effective prefix and the worker's env. Injectable for
	// tests.
	workerBaseURL func(name string, env map[string]string) string
}

// NewApprovalHandler creates the handler with the default embedded-mode
// worker address resolution (same chain as the checkpoint proxy).
func NewApprovalHandler(c client.Client, namespace, kubeMode, containerPrefix string) *ApprovalHandler {
	h := &ApprovalHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: approvalProxyTimeout},
		containerPrefix: containerPrefix,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves the worker's qwenpaw app base URL via the
// effective container prefix and the system-wins console port — identical
// to the checkpoint proxy, so the proxy always dials the port the
// container listens on.
func (h *ApprovalHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// approvalScope resolves the worker and its owning team, enforcing the
// W8 rule for scoped callers (cross-team workers hide as 404). It returns
// the team name and the resolved base URL for the upstream dial.
func (h *ApprovalHandler) approvalScope(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker tool approval requires embedded mode")
		return "", false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return "", false
		}
		writeK8sError(w, "get worker approval", err)
		return "", false
	}
	// findTeamMember's second return value is the member (worker) name,
	// not the team name — the scope check compares against the Team CR
	// name (first return value), the same chain as GetWorker.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker approval", err)
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

// getWorkerApproval handles GET /api/v1/workers/{name}/approval.
func (h *ApprovalHandler) getWorkerApproval(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	baseURL, ok := h.approvalScope(w, r, name)
	if !ok {
		return
	}
	body, status, ok := h.fetchUpstreamConfig(w, r, baseURL)
	if !ok {
		return
	}
	switch status {
	case http.StatusOK:
		var cfg map[string]any
		if err := json.Unmarshal(body, &cfg); err != nil {
			httputil.WriteError(w, http.StatusBadGateway, "worker returned an unparsable running config")
			return
		}
		// The upstream GET always populates approval_level from the agent
		// profile (defaulting to "AUTO" when the profile has none). A
		// non-string value is a data-shape mismatch, not a silent fallback.
		switch level := cfg["approval_level"].(type) {
		case nil:
			writeApprovalJSON(w, "AUTO")
		case string:
			if level == "" {
				level = "AUTO"
			}
			writeApprovalJSON(w, level)
		default:
			httputil.WriteError(w, http.StatusBadGateway, "worker returned a non-string approval_level")
		}
	case http.StatusNotFound:
		// Version gate: a QwenPaw version without the running-config
		// router 404s here — passed through verbatim (status AND body) so
		// clients can show "worker upgrade required" instead of "worker
		// missing".
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	default:
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker approval API error (status %d): %s", status, string(body)))
	}
}

// updateWorkerApproval handles PUT /api/v1/workers/{name}/approval.
func (h *ApprovalHandler) updateWorkerApproval(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, http.StatusForbidden, "caller identity required")
		return
	}
	// Team leaders stay read-only on the approval API (their tool-policy
	// surface is the chat/owner path, not REST) — same boundary as the
	// knowledge base write API.
	if caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders can read tool approval but not change it")
		return
	}
	baseURL, ok := h.approvalScope(w, r, name)
	if !ok {
		return
	}
	// Body: {"approval_level": "<LEVEL>"} — the only accepted shape.
	var payload struct {
		ApprovalLevel string `json:"approval_level"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object with an approval_level string field")
		return
	}
	if !approvalLevels[payload.ApprovalLevel] {
		httputil.WriteError(w, http.StatusBadRequest, "approval_level must be one of STRICT, SMART, AUTO, or OFF")
		return
	}
	// approval_level=OFF disables Tool Guard entirely (every tool call
	// executes without approval) — that is a security-policy operation,
	// not ordinary worker configuration. Default L2 humans may switch
	// among the guarded levels (STRICT/SMART/AUTO) but cannot turn the
	// guard off; OFF requires the elevated tool-approval capability that
	// the L2 permission design (#1220) defines and admins grant
	// explicitly. Admin/manager keep the full range (any-worker scope).
	// TODO(#1220): when this hardcoded role check is replaced by the
	// capability lookup, the capability design must EXPLICITLY
	// enumerate which roles/capabilities may set OFF (an elevated
	// tool-approval grant) — do not default OFF to "any non-human
	// principal", which would silently let worker/leader roles
	// disable Tool Guard.
	if payload.ApprovalLevel == "OFF" && caller.Role == authpkg.RoleHuman {
		httputil.WriteError(w, http.StatusForbidden,
			"setting approval_level=OFF requires the elevated tool-approval capability (L2 permission design, #1220); use STRICT, SMART, or AUTO")
		return
	}
	// Safe write: the upstream PUT persists the *full* running-config
	// object, so fetch the current one first and change only this field.
	// Known limitation (documented per review): the GET→modify→PUT
	// sequence is not atomic. Two concurrent callers editing the same
	// worker can interleave, and the later PUT wins (last-writer-wins).
	// The upstream 409 covers path-lock/reindex contention only, not
	// general concurrent modification, and the upstream API exposes no
	// concurrency token, so optimistic locking is not available.
	// Acceptable for this low-frequency configuration operation.
	cfgBody, status, ok := h.fetchUpstreamConfig(w, r, baseURL)
	if !ok {
		return
	}
	switch status {
	case http.StatusNotFound:
		// Version gate: passed through verbatim (status AND body), same as
		// the read path.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if len(cfgBody) > 0 {
			_, _ = w.Write(cfgBody)
		}
		return
	case http.StatusOK:
		// fall through
	default:
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker approval API error (status %d): %s", status, string(cfgBody)))
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(cfgBody, &cfg); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker returned an unparsable running config")
		return
	}
	if cfg == nil {
		// A JSON null body: writing the empty object back would wipe the
		// worker's config — fail loudly instead of writing {}.
		httputil.WriteError(w, http.StatusBadGateway, "worker returned an empty running-config object")
		return
	}
	cfg["approval_level"] = payload.ApprovalLevel
	body, err := json.Marshal(cfg)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "marshal running-config update: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, baseURL+"/workspace/running-config", bytes.NewReader(body))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build approval update request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	up, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker approval API unreachable: "+err.Error())
		return
	}
	defer up.Body.Close()
	// Same cap discipline as fetchUpstreamConfig: read one byte beyond
	// the limit so an oversized response is detected loudly (502)
	// instead of silently truncated — a truncated upstream response
	// would mask upstream anomalies and make the read/write paths
	// inconsistent.
	upBody, err := io.ReadAll(io.LimitReader(up.Body, upstreamConfigMax+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read running-config update response: "+err.Error())
		return
	}
	if len(upBody) > upstreamConfigMax {
		httputil.WriteError(w, http.StatusBadGateway, "worker running-config update response exceeds the 1 MiB proxy cap")
		return
	}
	switch up.StatusCode {
	case http.StatusOK:
		var updated map[string]any
		if json.Unmarshal(upBody, &updated) == nil {
			if level, ok := updated["approval_level"].(string); ok && level != "" {
				payload.ApprovalLevel = level
			}
		}
		writeApprovalJSON(w, payload.ApprovalLevel)
		if logger := log.FromContext(r.Context()).WithName("worker-approval"); logger.Enabled() {
			logger.Info("worker tool approval level changed",
				"worker", name, "approval_level", payload.ApprovalLevel,
				"caller", caller.Username, "role", caller.Role)
		}
	case http.StatusConflict:
		// Concurrent upstream config change (path lock / reindex in
		// flight) — pass through (status AND body) so the client retries
		// with a fresh GET.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(upBody)
	default:
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker approval API error (status %d): %s", up.StatusCode, string(upBody)))
	}
}

// writeApprovalJSON renders the minimal client-facing response:
// {"approval_level": "<LEVEL>"}.
func writeApprovalJSON(w http.ResponseWriter, level string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"approval_level":` + mustJSONString(level) + `}`))
}

// mustJSONString renders a string as a JSON string literal. The approval
// level is one of four fixed uppercase tokens, so escaping can never
// fail for the accepted inputs.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"AUTO"`
	}
	return string(b)
}
