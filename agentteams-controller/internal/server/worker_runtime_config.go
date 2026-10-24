package server

// Worker runtime-config proxy (GET/PUT /api/v1/workers/{name}/runtime-config
// + /loops + /loops/status + /loops/custom[/{loop}]).
//
// QwenPaw exposes its runtime-tunable "running-config" (the workbench 5-tab
// settings: ReAct / Loop / LLM-retry / long-term-memory / tool-level, plus
// the Loop Engine catalog, per-loop status and custom-loop CRUD) on each
// worker's qwenpaw app at :8088. The Controller proxies these fixed
// subpaths so L1 humans / the workbench plugin can inspect and adjust a
// worker's runtime behavior without reaching into the docker network.
//
// Contract re-verified against the pinned qwenpaw 2.0.1 wheel on 2026-09-15
// (PR #1231 review); unchanged in the 2.2.x pin this branch now carries.
//
// Upstream contract (verified against the pinned qwenpaw 2.0.1 wheel,
// still present in 2.2.x):
//   - GET/PUT /api/workspace/running-config  (the "runtime-config" subpath;
//     active agent resolved by the app — an AgentTeams worker is
//     single-agent). PUT replaces the whole running section, so the proxy
//     performs read-merge-write for partial updates (below).
//   - GET  /api/loops
//   - GET  /api/loops/status?chat_id=&session_id=  (the proxy forwards
//     both selectors verbatim; the upstream answers "idle" without them)
//   - GET  /api/loops/custom
//   - POST /api/loops/custom            (201; 409 on duplicate mode id)
//   - PUT  /api/loops/custom/{mode}     (200; 404 not found; 422 on
//     validation / id change)
//   - DELETE /api/loops/custom/{mode}   (204; 404 not found)
//
// Design (aligned with the workbench 5-tab, #1216 safe-write pattern, #1206
// notification infra):
//   - runtime-aware: a worker whose spec.runtime != "qwenpaw" gets 400 (the
//     running-config model is qwenpaw-specific; openclaw/copaw/hermes/
//     deepseek-harness have no equivalent).
//   - RBAC: L1 (admin/manager) full access; L2 (team-leader / L2 human) is
//     team-scoped via findTeamMember + TeamMatches — workers outside the
//     caller's teams hide as 404 (no existence probe), mirroring CheckpointHandler.
//   - 5-tab field whitelist: an L2 PUT runtime-config only passes the
//     whitelisted top-level keys (the workbench 5-tab fields of the qwenpaw
//     AgentsRunningConfig model); unknown keys are rejected, not silently
//     dropped (#1216). L1 PUT passes every key except approval_level.
//   - approval_level is owned by the #1216 endpoint
//     (PUT /api/v1/workers/{name}/approval) with its own safety-write and
//     audit semantics; runtime-config rejects it for every role.
//   - read-merge-write: PUT runtime-config reads the current running-config
//     from the worker, merges the submitted top-level keys onto it, and
//     writes the merged whole object back — a partial (single-tab) update
//     never clobbers unrelated persisted settings. An update that changes
//     nothing skips the upstream write (idempotent no-op, 200).
//   - upstream status mapping: 2xx pass through; actionable upstream 4xx
//     (409 conflict, 422 validation, and the per-loop 404) pass through to
//     the client; an upstream 404 on a collection/route subpath means the
//     pinned worker build lacks the endpoint → 502 "API unavailable";
//     connection failures and upstream 5xx → 502.
//   - loop-change notification: a write to /loops/custom notifies the team
//     room with @leader + @changer (Matrix m.mentions) ONLY after the
//     upstream write succeeded (2xx); a rejected write (409/422/404/5xx)
//     never notifies. Notification is fire-and-forget (never blocks the
//     write).
//
// Embedded mode only (worker app reachable by container name); kube mode →
// uniform 503 (no stable in-cluster worker DNS name, and a per-worker split
// would leak existence).
//
// Fixed-path forwarding only — never a generic reverse proxy.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// runtimeConfigProxyTimeout bounds each upstream call (slightly looser
	// than checkpoints' 5s: a PUT may take a beat to validate+persist; the
	// merge flow performs two sequential calls, GET then PUT).
	runtimeConfigProxyTimeout = 8 * time.Second
	// maxRuntimeConfigBody caps the request/response body we buffer (1 MiB).
	maxRuntimeConfigBody = 1 << 20
)

// l2RuntimeConfigWhitelist is the set of top-level keys an L2 caller may
// write via PUT runtime-config — the workbench 5-tab fields of the qwenpaw
// AgentsRunningConfig model (verified against the pinned 2.0.1 wheel; all
// present in 2.2.x). Everything else is rejected for L2 (fail-closed).
// L1 is unrestricted except approval_level (see guardApprovalLevel).
var l2RuntimeConfigWhitelist = map[string]bool{
	// ReAct 智能体 tab
	"max_iters": true,
	// 智能体 Loop 设置 tab
	"loop": true,
	// LLM 自动重试 tab
	"llm_retry_enabled": true,
	"llm_max_retries":   true,
	"llm_backoff_base":  true,
	"llm_backoff_cap":   true,
	// 长期记忆 tab
	"memory_manager_backend":   true,
	"reme_light_memory_config": true,
	"adbpg_memory_config":      true,
	// 工具执行级别 tab = approval_level: intentionally NOT whitelisted —
	// it is owned by PUT /api/v1/workers/{name}/approval (#1216).
}

// loopNotifier is the minimal Matrix surface the handler needs for loop-change
// alerts. Satisfied by matrix.Client (deps.MatrixClient) in production; tests
// inject a two-method fake.
type loopNotifier interface {
	UserID(localpart string) string
	SendNotification(ctx context.Context, roomID, body string, mentionUserIDs []string) error
}

// RuntimeConfigHandler proxies worker runtime-config endpoints.
type RuntimeConfigHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	containerPrefix string
	http            *http.Client
	// matrix is the notification client (loop-change alerts). nil disables
	// notification (tests / environments without Matrix).
	matrix loopNotifier
	// workerBaseURL resolves a worker name to its qwenpaw app base URL.
	workerBaseURL func(name string, env map[string]string) string
}

// NewRuntimeConfigHandler creates the handler with default embedded-mode
// worker address resolution (same chain as CheckpointHandler).
func NewRuntimeConfigHandler(c client.Client, namespace, kubeMode, containerPrefix string, m loopNotifier) *RuntimeConfigHandler {
	h := &RuntimeConfigHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		containerPrefix: containerPrefix,
		http:            &http.Client{Timeout: runtimeConfigProxyTimeout},
		matrix:          m,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

func (h *RuntimeConfigHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// allowedSub reports whether sub (the path after /workers/{name}/) is a
// forwardable runtime-config endpoint.
func allowedSub(sub string) bool {
	switch sub {
	case "runtime-config", "loops", "loops/status", "loops/custom":
		return true
	}
	if strings.HasPrefix(sub, "loops/custom/") {
		rest := strings.TrimPrefix(sub, "loops/custom/")
		return rest != "" && !strings.Contains(rest, "/") && workerNamePattern.MatchString(rest)
	}
	return false
}

// loopsStatusQueryWhitelist is the set of query parameters forwarded to the
// worker's /api/loops/status upstream. The endpoint answers per session
// (chat_id / session_id); without either it reports "idle", so dropping the
// parameters would make the proxied endpoint useless.
var loopsStatusQueryWhitelist = map[string]bool{"chat_id": true, "session_id": true}

// upstreamSub maps the controller subpath to the worker's qwenpaw app
// subpath. Only runtime-config is remapped: the running-config contract
// lives under /api/workspace/ (active agent resolved by the app). Verified
// against the pinned qwenpaw 2.0.1 wheel (/api/runtime-config does not
// exist in 2.0.1 — see the PR contract note).
func upstreamSub(sub string) string {
	if sub == "runtime-config" {
		return "workspace/running-config"
	}
	return sub
}

// isWrite reports whether the HTTP method mutates worker state.
func isWrite(method string) bool {
	return method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete
}

// isL1 reports whether the caller is a full-access (L1) principal.
func isL1(caller *authpkg.CallerIdentity) bool {
	return caller != nil &&
		caller.Role != authpkg.RoleTeamLeader &&
		caller.Role != authpkg.RoleHuman
}

// bodyHasApprovalLevel reports whether the submitted JSON object tries to
// write approval_level. It is rejected for EVERY role via runtime-config:
// that field is owned by the #1216 endpoint (PUT /api/v1/workers/{name}/approval)
// with its own safety-write and audit semantics — a 400 "wrong endpoint",
// not a permission error.
func bodyHasApprovalLevel(body []byte) bool {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m["approval_level"]
	return ok
}

// validateL2RuntimeConfig enforces the L2 write contract for PUT
// runtime-config: the body must be a non-empty JSON object and every
// top-level key must be whitelisted (fail-closed, #1216 pattern). Callers
// must already have rejected approval_level via bodyHasApprovalLevel.
func validateL2RuntimeConfig(body []byte) error {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("runtime-config body must be a JSON object")
	}
	if m == nil {
		return fmt.Errorf("runtime-config body must be a JSON object")
	}
	if len(m) == 0 {
		return fmt.Errorf("runtime-config update is empty")
	}
	var allowed []string
	for k := range l2RuntimeConfigWhitelist {
		allowed = append(allowed, k)
	}
	sort.Strings(allowed)
	for k := range m {
		if !l2RuntimeConfigWhitelist[k] {
			return fmt.Errorf("field %q not allowed for L2 (allowed: %s)", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// Handle proxies the worker runtime-config endpoints.
func (h *RuntimeConfigHandler) Handle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	// Derive the subpath after /workers/{name}/ and validate the whitelist.
	base := "/api/v1/workers/" + name + "/"
	if !strings.HasPrefix(r.URL.Path, base) {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported runtime-config path")
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, base)
	if !allowedSub(sub) {
		httputil.WriteError(w, http.StatusBadRequest, "unsupported runtime-config subpath")
		return
	}

	// Query surface: only GET loops/status forwards the upstream session
	// selectors (chat_id / session_id) — the worker answers "idle" without
	// a session; every other route takes no query parameters, and unknown
	// keys are rejected instead of forwarded.
	var query string
	if raw := r.URL.Query(); len(raw) > 0 {
		if sub != "loops/status" || r.Method != http.MethodGet {
			httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter")
			return
		}
		values := url.Values{}
		for key, v := range raw {
			if !loopsStatusQueryWhitelist[key] {
				httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
				return
			}
			values[key] = v
		}
		query = "?" + values.Encode()
	}

	// Kube-mode check before any worker lookup (uniform 503, no existence leak).
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker runtime-config requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker runtime-config", err)
		return
	}
	// runtime-aware: the running-config model is qwenpaw-specific.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "runtime-config is only supported for qwenpaw workers")
		return
	}

	// RBAC: scoped callers (team-leader / L2 human) only see workers in their
	// teams; everyone else hides as 404 (no existence probe).
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker runtime-config", err)
		return
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(teamNameOf(teamObj)) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return
	}

	// Read the request body (for writes) and apply the write contract.
	var body []byte
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// no body
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		body, err = io.ReadAll(io.LimitReader(r.Body, maxRuntimeConfigBody))
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
			return
		}
		if sub == "runtime-config" && r.Method == http.MethodPut {
			if bodyHasApprovalLevel(body) {
				httputil.WriteError(w, http.StatusBadRequest,
					"approval_level is not writable via runtime-config — use PUT /api/v1/workers/{name}/approval (#1216)")
				return
			}
			if !isL1(authpkg.CallerFromContext(r.Context())) {
				if err := validateL2RuntimeConfig(body); err != nil {
					httputil.WriteError(w, http.StatusForbidden, err.Error())
					return
				}
			}
		}
	default:
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// PUT runtime-config is read-merge-write: the upstream PUT replaces the
	// whole running section, so a partial (single-tab) body must be merged
	// onto the current config first (see mergeRuntimeConfig).
	var loopSubmitted bool
	if sub == "runtime-config" && r.Method == http.MethodPut {
		// L1 bodies are not pre-validated for shape; reject a non-object
		// body here as 400 (a 502 from the merge flow would be wrong for
		// a client error).
		var submittedMap map[string]interface{}
		if err := json.Unmarshal(body, &submittedMap); err != nil || submittedMap == nil {
			httputil.WriteError(w, http.StatusBadRequest, "runtime-config body must be a JSON object")
			return
		}
		loopSubmitted = submittedMap["loop"] != nil
		merged, current, err := h.mergeRuntimeConfig(r, &worker, body)
		if err != nil {
			httputil.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
		if merged == nil {
			// Idempotent no-op: the update changes nothing. Return the
			// current config as 200 without touching the worker.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(current)
			return
		}
		body = merged
	}

	// Forward to the worker's qwenpaw app (fixed subpath, same base URL as
	// CheckpointHandler: container-prefix + name + effective console port).
	target := h.workerBaseURL(name, worker.Spec.Env) + "/api/" + upstreamSub(sub) + query
	resp, respBody, err := h.doUpstream(r, target, r.Method, body)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker runtime-config API unreachable")
		return
	}
	defer resp.Body.Close()

	// Status mapping (see the header contract note): 2xx passes through;
	// actionable upstream 4xx (409 conflict, 422 validation, and the
	// per-loop 404 "Custom mode not found") pass through to the client;
	// a route-level 404 (the pinned worker build lacks the endpoint) and
	// any upstream 5xx become 502.
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	passThrough := success
	if !passThrough && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		passThrough = resp.StatusCode != http.StatusNotFound ||
			strings.HasPrefix(sub, "loops/custom/")
	}
	if passThrough {
		// Loop-change notification: only after a SUCCESSFUL (2xx) upstream
		// write — a rejected write (409/422/404/5xx) never notifies.
		if success && isWrite(r.Method) && strings.HasPrefix(sub, "loops/custom") {
			verb := map[string]string{http.MethodPost: "新增", http.MethodPut: "修改", http.MethodDelete: "删除"}[r.Method]
			h.notifyLoopChange(r.Context(), name, "custom loop", verb, teamObj)
		}
		if success && sub == "runtime-config" && r.Method == http.MethodPut && loopSubmitted {
			h.notifyLoopChange(r.Context(), name, "loop 配置（running-config）", "修改", teamObj)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}
	if resp.StatusCode == http.StatusNotFound {
		// QwenPaw build without the running-config/loops router.
		httputil.WriteError(w, http.StatusBadGateway,
			"runtime-config API unavailable (requires a QwenPaw build with /api/workspace/running-config and /api/loops*)")
		return
	}
	if len(respBody) > 500 {
		respBody = respBody[:500]
	}
	httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker runtime-config error (upstream status %d): %s",
		resp.StatusCode, string(respBody)))
}

// doUpstream performs one upstream call with the proxy timeout and returns
// the response plus a fully-read (capped) body.
func (h *RuntimeConfigHandler) doUpstream(r *http.Request, target, method string, body []byte) (*http.Response, []byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target, reader)
	if err != nil {
		return nil, nil, err
	}
	if method != http.MethodGet && method != http.MethodHead && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRuntimeConfigBody))
	if err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	return resp, respBody, nil
}

// mergeRuntimeConfig performs the read-merge-write flow for a PUT
// runtime-config: fetch the current running-config, merge the submitted
// top-level keys onto it, and — only when something changed — write the
// merged whole object back (the upstream PUT replaces the entire running
// section; a bare forward of a partial body would clobber unrelated
// settings). It returns the body to forward; a nil return with a nil error
// is the idempotent no-op (caller returns the current config as 200).
func (h *RuntimeConfigHandler) mergeRuntimeConfig(r *http.Request, worker *v1beta1.Worker, submitted []byte) ([]byte, []byte, error) {
	baseURL := h.workerBaseURL(worker.Name, worker.Spec.Env) + "/api/" + upstreamSub("runtime-config")

	// 1. Read the current running-config.
	curResp, curBody, err := h.doUpstream(r, baseURL, http.MethodGet, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("read current runtime-config: unreachable")
	}
	defer curResp.Body.Close()
	if curResp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("read current runtime-config: upstream status %d", curResp.StatusCode)
	}
	var current map[string]interface{}
	if err := json.Unmarshal(curBody, &current); err != nil || current == nil {
		return nil, nil, fmt.Errorf("read current runtime-config: response is not a JSON object")
	}

	// 2. Merge: submitted top-level keys replace their current values;
	//    absent keys are preserved.
	var submittedMap map[string]interface{}
	if err := json.Unmarshal(submitted, &submittedMap); err != nil {
		return nil, nil, fmt.Errorf("parse submitted update: not a JSON object")
	}
	merged := make(map[string]interface{}, len(current)+len(submittedMap))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range submittedMap {
		merged[k] = v
	}

	curBytes, _ := json.Marshal(current)
	mergedBytes, _ := json.Marshal(merged)
	if bytes.Equal(curBytes, mergedBytes) {
		return nil, curBytes, nil // no-op: nothing changed
	}
	return mergedBytes, curBytes, nil
}

// notifyLoopChange alerts the worker's team room about a loop change (a
// custom-loop CRUD write or a runtime-config PUT that changed the loop
// section), mentioning the team leader and the changer. Called only after a
// successful (2xx) upstream write. Non-fatal: any failure is swallowed so
// the config write itself is never blocked by notification.
func (h *RuntimeConfigHandler) notifyLoopChange(ctx context.Context, workerName, target, verb string, team *v1beta1.Team) {
	if h.matrix == nil || team == nil {
		return
	}
	room := team.Status.TeamRoomID
	if room == "" {
		return
	}
	if verb == "" {
		return
	}
	leaderID, leaderName := "", ""
	if ln := teamLeaderName(team); ln != "" {
		leaderName = ln
		leaderID = h.matrix.UserID(ln)
	}
	caller := authpkg.CallerFromContext(ctx)
	changerID, changerName := "", ""
	if caller != nil && caller.Username != "" {
		changerName = caller.Username
		changerID = h.matrix.UserID(caller.Username)
	}
	mentions := make([]string, 0, 2)
	if leaderID != "" {
		mentions = append(mentions, leaderID)
	}
	if changerID != "" && changerID != leaderID {
		mentions = append(mentions, changerID)
	}
	if len(mentions) == 0 {
		return
	}
	body := fmt.Sprintf("⚠️ Loop 变更：%s 的%s被%s（runtime-config API）。@%s @%s 请确认。",
		workerName, target, verb, leaderName, changerName)
	_ = h.matrix.SendNotification(ctx, room, body, mentions)
}

// teamLeaderName returns the Worker CR name with Role "team_leader", or "".
func teamLeaderName(team *v1beta1.Team) string {
	for _, m := range team.Spec.WorkerMembers {
		if m.Role == "team_leader" {
			return m.Name
		}
	}
	return ""
}

// teamNameOf returns the Team CR name, or "" for a nil team.
func teamNameOf(team *v1beta1.Team) string {
	if team == nil {
		return ""
	}
	return team.Name
}
