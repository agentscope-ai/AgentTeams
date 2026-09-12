package server

// Worker runtime-config proxy (GET/PUT /api/v1/workers/{name}/runtime-config
// + /loops + /loops/status + /loops/custom[/{loop}]).
//
// QwenPaw 2.x exposes its runtime-tunable "running-config" (the workbench
// 5-tab settings: ReAct / Loop / LLM-retry / long-term-memory / tool-level,
// plus the Loop Engine catalog, per-loop status and custom-loop CRUD) on each
// worker's qwenpaw app at :8088. The Controller proxies these fixed subpaths
// so L1 humans / the workbench plugin can inspect and adjust a worker's
// runtime behavior without reaching into the docker network.
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
//     whitelisted top-level keys (ReAct/Loop/LLM-retry/long-term-memory/
//     tool-level); unknown keys are rejected, not silently dropped (#1216).
//     L1 PUT is passed through untouched.
//   - loop-change notification: any write to /loops/custom (create/update/
//     delete) notifies the team room with @leader + @changer (Matrix
//     m.mentions). Notification is fire-and-forget (never blocks the write).
//
// Embedded mode only (worker app reachable by container name); kube mode →
// uniform 503 (no stable in-cluster worker DNS name, and a per-worker split
// would leak existence).
//
// Fixed-path forwarding only — never a generic reverse proxy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"context"
	"net/http"
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
	// than checkpoints' 5s: a PUT may take a beat to validate+persist).
	runtimeConfigProxyTimeout = 8 * time.Second
	// maxRuntimeConfigBody caps the request/response body we buffer (1 MiB).
	maxRuntimeConfigBody = 1 << 20
)

// l2RuntimeConfigWhitelist is the set of top-level keys an L2 caller may
// write via PUT runtime-config — the workbench 5-tab fields. Everything else
// is rejected for L2 (fail-closed). L1 is unrestricted.
var l2RuntimeConfigWhitelist = map[string]bool{
	// ReAct 循环
	"max_iterations": true,
	"auto_continue":  true,
	// Loop Engine
	"loop": true,
	// LLM 重试
	"max_retries": true,
	// 长期记忆
	"memory": true,
	// 工具级别 / 权限
	"tool_level": true,
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

// filterL2RuntimeConfig enforces the 5-tab whitelist for L2 writes: every
// top-level key must be whitelisted, else the whole write is rejected.
func filterL2RuntimeConfig(body []byte) error {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("runtime-config body must be a JSON object")
	}
	if m == nil {
		return fmt.Errorf("runtime-config body must be a non-empty JSON object")
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

	// Read the request body (for writes) and apply the L2 whitelist.
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
		if sub == "runtime-config" && r.Method == http.MethodPut && !isL1(authpkg.CallerFromContext(r.Context())) {
			if err := filterL2RuntimeConfig(body); err != nil {
				httputil.WriteError(w, http.StatusForbidden, err.Error())
				return
			}
		}
	default:
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Forward to the worker's qwenpaw app (fixed subpath, same base URL as
	// CheckpointHandler: container-prefix + name + effective console port).
	target := h.workerBaseURL(name, worker.Spec.Env) + "/api/" + sub
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build runtime-config request: "+err.Error())
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker runtime-config API unreachable")
		return
	}
	defer resp.Body.Close()

	// Loop-change notification (fire-and-forget): any write to /loops/custom
	// alerts the team room with @leader + @changer.
	if isWrite(r.Method) && strings.HasPrefix(sub, "loops/custom") {
		h.notifyLoopChange(r.Context(), name, r.Method, teamObj)
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRuntimeConfigBody))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	case http.StatusNotFound:
		// QwenPaw build without the runtime-config router.
		httputil.WriteError(w, http.StatusBadGateway, "runtime-config API unavailable (requires a QwenPaw build with /api/runtime-config)")
	default:
		errMsg := string(respBody)
		if len(errMsg) > 500 {
			errMsg = errMsg[:500]
		}
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("runtime-config API error (status %d): %s", resp.StatusCode, errMsg))
	}
}

// notifyLoopChange alerts the worker's team room about a custom-loop write,
// mentioning the team leader and the changer. Non-fatal: any failure is
// swallowed so the config write itself is never blocked by notification.
func (h *RuntimeConfigHandler) notifyLoopChange(ctx context.Context, workerName, method string, team *v1beta1.Team) {
	if h.matrix == nil || team == nil {
		return
	}
	room := team.Status.TeamRoomID
	if room == "" {
		return
	}
	verb := map[string]string{
		http.MethodPost:   "新增",
		http.MethodPut:    "修改",
		http.MethodDelete: "删除",
	}[method]
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
	body := fmt.Sprintf("⚠️ Loop 变更：%s 的 custom loop 被%s（runtime-config API）。@%s @%s 请确认。",
		workerName, verb, leaderName, changerName)
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
