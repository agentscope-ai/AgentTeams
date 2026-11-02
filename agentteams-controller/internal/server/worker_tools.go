package server

// Worker built-in tool settings proxy
// (GET /api/v1/workers/{name}/tools, PATCH /api/v1/workers/{name}/tools/{tool}).
//
// Each QwenPaw worker's agent profile carries a per-tool table
// (tools.builtin_tools): which built-in tools are enabled and which execute
// asynchronously. The worker's qwenpaw app serves the table on
// GET /api/tools and mutates single entries via
// PATCH /api/tools/{tool}/toggle (no body: flips `enabled`) and
// PATCH /api/tools/{tool}/async-execution (body {"async_execution": bool});
// both save the agent profile and hot-reload the agent (the same live-effect
// path as the approval and runtime-config endpoints, and the worker's own
// sync loop persists the profile to the shared store). This proxy gives
// L2 humans an in-band path for the tools of workers in their own teams —
// no `docker exec` required.
//
// The controller-facing tool entry exposes state and metadata only
// (name / enabled / description / asyncExecution / icon / requiresConfig).
// The upstream config_fields and config_values are never forwarded: tool
// configuration can hold credentials.
//
// Upstream contract (verified against the pinned qwenpaw 2.2.1 wheel and
// the 2.0.1 / 2.2.0 wheels — the tools router is line-identical in all
// three, so there is no version gate to maintain):
//
//	GET   /api/tools                          -> [ToolInfo, ...]
//	PATCH /api/tools/{tool}/toggle            -> ToolInfo (flips enabled)
//	PATCH /api/tools/{tool}/async-execution   -> ToolInfo (body: async_execution)
//	unknown tool -> 404 {"detail":"Tool 'x' not found"}
//
// The controller-facing field names are camelCase (asyncExecution /
// requiresConfig); the worker-local wire names are snake_case — the mapping
// happens only in this proxy and never round-trips a client object.
//
// The PATCH is declarative: the handler reads the current table first and
// issues the worker-local mutation only for each field whose requested value
// differs from the current one. The local toggle endpoint flips state without
// a body, so a bare forward would double-flip `enabled` on a retried request;
// a PATCH that changes nothing is a 200 no-op with zero upstream writes.
//
// Authorization (same boundary as the approval proxy, #1216): admin/manager
// any worker; L2 humans workers in their own teams (cross-team hides as 404,
// W8 anti-probing — the authorizer allows the action so the handler can hide
// with 404 instead of the authorizer answering 403); team leaders read-only
// (the authorizer denies the write action). Toggles that expand execution or
// network reach may deserve a stricter gate than ordinary edits — when the
// L2 capability model (#1220) lands, an elevated capability is the intended
// home for such gates. Until then this surface mirrors #1216, which lets L2
// humans relax the guard among the guarded levels and gates only the
// most-permissive value.
//
// No team-room notification is emitted on a tool toggle (v1) — see the
// follow-up note in the PR description.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
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
	// toolsProxyTimeout bounds each upstream call (same as the approval proxy).
	toolsProxyTimeout = 5 * time.Second
	// toolsListMax caps the buffered upstream list response (1 MiB — the
	// same loud-fail discipline as the approval proxy: detect over-size,
	// never truncate).
	toolsListMax = 1 << 20
	// toolsPatchBodyMax caps the PATCH request body.
	toolsPatchBodyMax = 4096
)

// toolNamePattern allows identifier-shaped tool names (built-in and plugin
// tools are Python/JS function names). The name is forwarded into the
// worker-local URL path, so anything else is rejected before dialing.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ToolEntry is the controller-facing tool state: state and metadata only.
// config_fields/config_values never cross the proxy boundary (see the
// package doc).
type ToolEntry struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	Description    string `json:"description"`
	AsyncExecution bool   `json:"asyncExecution"`
	Icon           string `json:"icon"`
	RequiresConfig bool   `json:"requiresConfig"`
}

// ToolsHandler proxies the worker tool-settings endpoints.
type ToolsHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	containerPrefix string
	http            *http.Client
	// workerBaseURL resolves a worker name to its qwenpaw app base URL.
	// Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
	// Per-(worker, tool) write locks: the upstream enabled mutation is a
	// blind TOGGLE, so the read->decide->mutate sequence of
	// patchWorkerTool must be atomic per tool. Without it, two overlapping
	// PATCH {"enabled":true} requests both read false, both toggle, and the
	// final state is false although both callers asked for true. Keyed by
	// base URL + tool; bounded by workers x built-in tools.
	toolWriteMu    sync.Mutex
	toolWriteLocks map[string]*sync.Mutex
}

// NewToolsHandler creates the handler with the default embedded-mode worker
// address resolution (same chain as the checkpoint / approval proxies).
func NewToolsHandler(c client.Client, namespace, kubeMode, containerPrefix string) *ToolsHandler {
	h := &ToolsHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		containerPrefix: containerPrefix,
		http:            &http.Client{Timeout: toolsProxyTimeout},
		toolWriteLocks:  map[string]*sync.Mutex{},
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

func (h *ToolsHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// toolsScope resolves the worker and its owning team, enforcing the shared
// gates in order: embedded-only (uniform 503, no existence leak) -> worker
// lookup (404) -> runtime-aware (400 for non-qwenpaw) -> W8 team scope
// (scoped callers: cross-team hides as 404). It returns the team object and
// the upstream base URL.
func (h *ToolsHandler) toolsScope(w http.ResponseWriter, r *http.Request, name string) (*v1beta1.Team, string, bool) {
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker tool settings require embedded mode")
		return nil, "", false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return nil, "", false
		}
		writeK8sError(w, "get worker tools", err)
		return nil, "", false
	}
	// runtime-aware: the tool-settings model is qwenpaw-specific.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "tool settings are only supported for qwenpaw workers")
		return nil, "", false
	}
	// findTeamMember's second return value is the member (worker) name, not
	// the team name — the scope check compares against the Team CR name.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker tools", err)
		return nil, "", false
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) &&
		!caller.TeamMatches(teamNameOf(teamObj)) {
		httputil.WriteError(w, http.StatusNotFound, "worker not found")
		return nil, "", false
	}
	return teamObj, h.workerBaseURL(name, worker.Spec.Env), true
}

// fetchTools performs the upstream GET /api/tools and enforces toolsListMax
// with a loud 502 instead of a silent truncation. Status mapping: 200 ->
// the body; a route-level 404 (a QwenPaw build without the tools router) ->
// 502 "API unavailable" (there is no per-item 404 on the list endpoint);
// any other upstream status -> 502 with the status surfaced.
func (h *ToolsHandler) fetchTools(w http.ResponseWriter, r *http.Request, baseURL string) ([]byte, bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+"/api/tools", nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build tools request: "+err.Error())
		return nil, false
	}
	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker tools API unreachable: "+err.Error())
		return nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, toolsListMax+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read tools response: "+err.Error())
		return nil, false
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if len(body) > toolsListMax {
			httputil.WriteError(w, http.StatusBadGateway, "worker tools list exceeds the 1 MiB proxy cap")
			return nil, false
		}
		return body, true
	case resp.StatusCode == http.StatusNotFound:
		httputil.WriteError(w, http.StatusBadGateway,
			"tools API unavailable (requires a QwenPaw build with /api/tools)")
		return nil, false
	default:
		if len(body) > 500 {
			body = body[:500]
		}
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker tools API error (status %d): %s",
			resp.StatusCode, string(body)))
		return nil, false
	}
}

// parseToolItems converts the upstream [ToolInfo, ...] array into raw item
// maps. It fails closed on a non-array body or an entry missing its name —
// a tool list that silently drops entries would look authoritative to
// operators (the same stance as the model list endpoint).
func parseToolItems(w http.ResponseWriter, body []byte) []map[string]any {
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker returned an unparsable tools list (expected a JSON array)")
		return nil
	}
	for _, item := range items {
		if n, _ := item["name"].(string); n == "" {
			httputil.WriteError(w, http.StatusBadGateway, "worker returned a tools entry without a name")
			return nil
		}
	}
	return items
}

// toToolEntry converts one raw upstream ToolInfo map into the controller
// ToolEntry, dropping config_fields/config_values. A type mismatch on a
// field the upstream response model guarantees (str/bool) means a broken
// upstream — 502, not a silently coerced value.
func toToolEntry(w http.ResponseWriter, item map[string]any) (ToolEntry, bool) {
	name, _ := item["name"].(string)
	enabled, okE := item["enabled"].(bool)
	description, okD := item["description"].(string)
	async, okA := item["async_execution"].(bool)
	icon, okI := item["icon"].(string)
	requiresConfig, okR := item["requires_config"].(bool)
	if !okE || !okD || !okA || !okI || !okR {
		httputil.WriteError(w, http.StatusBadGateway,
			"worker returned a tools entry with an unexpected field type")
		return ToolEntry{}, false
	}
	return ToolEntry{
		Name:           name,
		Enabled:        enabled,
		Description:    description,
		AsyncExecution: async,
		Icon:           icon,
		RequiresConfig: requiresConfig,
	}, true
}

func writeToolEntry(w http.ResponseWriter, entry ToolEntry) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entry)
}

// listWorkerTools handles GET /api/v1/workers/{name}/tools.
func (h *ToolsHandler) listWorkerTools(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	_, baseURL, ok := h.toolsScope(w, r, name)
	if !ok {
		return
	}
	body, ok := h.fetchTools(w, r, baseURL)
	if !ok {
		return
	}
	items := parseToolItems(w, body)
	if items == nil {
		return
	}
	entries := make([]ToolEntry, 0, len(items))
	for _, item := range items {
		entry, ok := toToolEntry(w, item)
		if !ok {
			return
		}
		entries = append(entries, entry)
	}
	resp := struct {
		Tools []ToolEntry `json:"tools"`
		Total int         `json:"total"`
	}{Tools: entries, Total: len(entries)}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// toolWriteLock returns the mutex that serializes the read->decide->mutate
// sequence for one (worker, tool) pair.
func (h *ToolsHandler) toolWriteLock(baseURL, tool string) *sync.Mutex {
	key := baseURL + "\x00" + tool
	h.toolWriteMu.Lock()
	defer h.toolWriteMu.Unlock()
	l, ok := h.toolWriteLocks[key]
	if !ok {
		l = &sync.Mutex{}
		h.toolWriteLocks[key] = l
	}
	return l
}

func findToolItem(items []map[string]any, tool string) map[string]any {
	for _, item := range items {
		if item["name"] == tool {
			return item
		}
	}
	return nil
}

// patchWorkerTool handles PATCH /api/v1/workers/{name}/tools/{tool}.
//
// Body: a JSON object with one or both of "enabled" and "asyncExecution"
// (booleans). Declarative: the local mutation for a field is issued only
// when the requested value differs from the current one (see the package
// doc for why the toggle cannot be forwarded blindly).
//
// Concurrency: the upstream enabled mutation is a bodyless TOGGLE, so a
// stale read-then-toggle pair can invert the requested value. The
// authoritative read, the decision and the mutation therefore run under a
// per-(worker, tool) lock, and the mutation response is verified to carry
// the requested enabled value before the success is reported. The initial
// pre-read stays outside the lock: it only serves the no-op fast path.
func (h *ToolsHandler) patchWorkerTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tool := r.PathValue("tool")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	if tool == "" || !toolNamePattern.MatchString(tool) {
		httputil.WriteError(w, http.StatusBadRequest, "tool name is required and must be an identifier")
		return
	}

	// Read and validate the body (fail-closed for every role: the surface is
	// uniform — exactly two writable fields, unknown keys rejected, not
	// silently dropped).
	raw, err := io.ReadAll(io.LimitReader(r.Body, toolsPatchBodyMax))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	var keys map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &keys) != nil || keys == nil {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be a JSON object")
		return
	}
	for k := range keys {
		if k != "enabled" && k != "asyncExecution" {
			httputil.WriteError(w, http.StatusBadRequest,
				"unknown field "+fmt.Sprintf("%q", k)+" (allowed: asyncExecution, enabled)")
			return
		}
	}
	var payload struct {
		Enabled        *bool `json:"enabled"`
		AsyncExecution *bool `json:"asyncExecution"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "enabled / asyncExecution must be JSON booleans")
		return
	}
	if payload.Enabled == nil && payload.AsyncExecution == nil {
		httputil.WriteError(w, http.StatusBadRequest, "at least one of enabled / asyncExecution is required")
		return
	}

	_, baseURL, ok := h.toolsScope(w, r, name)
	if !ok {
		return
	}

	// Pre-read the current table (fail closed on any upstream error). This
	// read is outside the write lock and is NOT authoritative: it only
	// serves the no-op fast path below.
	body, ok := h.fetchTools(w, r, baseURL)
	if !ok {
		return
	}
	items := parseToolItems(w, body)
	if items == nil {
		return
	}
	current := findToolItem(items, tool)
	if current == nil {
		httputil.WriteError(w, http.StatusNotFound, "tool '"+tool+"' not found")
		return
	}
	curEnabled, _ := current["enabled"].(bool)
	curAsync, _ := current["async_execution"].(bool)

	needToggle := payload.Enabled != nil && *payload.Enabled != curEnabled
	needAsync := payload.AsyncExecution != nil && *payload.AsyncExecution != curAsync
	if !needToggle && !needAsync {
		// Idempotent no-op per the pre-read: nothing to write, so nothing
		// to serialize. Return the current entry without touching the
		// worker (mirrors the runtime-config no-op).
		entry, ok := toToolEntry(w, current)
		if !ok {
			return
		}
		writeToolEntry(w, entry)
		return
	}

	// A mutation is planned: take the per-tool lock and re-read
	// authoritatively — a concurrent writer that passed the same pre-read
	// may already have flipped the state we are about to toggle.
	lock := h.toolWriteLock(baseURL, tool)
	lock.Lock()
	defer lock.Unlock()

	body, ok = h.fetchTools(w, r, baseURL)
	if !ok {
		return
	}
	items = parseToolItems(w, body)
	if items == nil {
		return
	}
	current = findToolItem(items, tool)
	if current == nil {
		httputil.WriteError(w, http.StatusNotFound, "tool '"+tool+"' not found")
		return
	}
	curEnabled, _ = current["enabled"].(bool)
	curAsync, _ = current["async_execution"].(bool)
	needToggle = payload.Enabled != nil && *payload.Enabled != curEnabled
	needAsync = payload.AsyncExecution != nil && *payload.AsyncExecution != curAsync

	// Issue only the mutations that change state; the response of the last
	// mutation is the authoritative updated entry.
	var last map[string]any
	if needToggle {
		info, ok := h.patchToolUpstream(w, r, baseURL+"/api/tools/"+tool+"/toggle", nil)
		if !ok {
			return
		}
		// Verify the resulting state at the mutation boundary: the toggle
		// response is the upstream's own post-mutation entry; a mismatch
		// means the requested value did not land — do not report success.
		if got, _ := info["enabled"].(bool); got != *payload.Enabled {
			httputil.WriteError(w, http.StatusBadGateway,
				"enabled state verification failed: upstream did not land the requested value")
			return
		}
		last = info
	}
	if needAsync {
		upBody, _ := json.Marshal(map[string]any{"async_execution": *payload.AsyncExecution})
		info, ok := h.patchToolUpstream(w, r, baseURL+"/api/tools/"+tool+"/async-execution", upBody)
		if !ok {
			return
		}
		last = info
	}

	final := last
	if final == nil {
		// Re-decided to a no-op under the lock: a concurrent writer
		// already applied the requested state. Return the current entry.
		final = current
	}
	entry, ok := toToolEntry(w, final)
	if !ok {
		return
	}
	writeToolEntry(w, entry)

	// Audit: every successful change logs worker, tool, the changed fields
	// with old/new values, caller, role.
	if needToggle || needAsync {
		if caller := authpkg.CallerFromContext(r.Context()); caller != nil {
			var changes []string
			if needToggle {
				changes = append(changes, fmt.Sprintf("enabled: %t->%t", curEnabled, *payload.Enabled))
			}
			if needAsync {
				changes = append(changes, fmt.Sprintf("async_execution: %t->%t", curAsync, *payload.AsyncExecution))
			}
			logger := log.FromContext(r.Context()).WithName("worker-tools")
			logger.Info("worker tool settings changed",
				"worker", name, "tool", tool, "changes", changes,
				"caller", caller.Username, "role", caller.Role)
		}
	}
}

// patchToolUpstream issues one worker-local mutation (toggle / async-execution)
// and returns the updated ToolInfo object. Status mapping: 2xx -> parse;
// 404 -> passthrough (the list read above already proved the router exists,
// so a 404 here means the tool vanished between the read and this call);
// any other upstream status -> 502 with the status surfaced.
func (h *ToolsHandler) patchToolUpstream(w http.ResponseWriter, r *http.Request, target string, body []byte) (map[string]any, bool) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPatch, target, reader)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build tools mutation request: "+err.Error())
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker tools API unreachable: "+err.Error())
		return nil, false
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, toolsListMax+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read tools mutation response: "+err.Error())
		return nil, false
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var info map[string]any
		if err := json.Unmarshal(respBody, &info); err != nil || info == nil {
			httputil.WriteError(w, http.StatusBadGateway, "worker returned an unparsable tools entry")
			return nil, false
		}
		return info, true
	case http.StatusNotFound:
		// The tool vanished between the list read and this call.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if len(respBody) > 0 {
			_, _ = w.Write(respBody)
		}
		return nil, false
	default:
		if len(respBody) > 500 {
			respBody = respBody[:500]
		}
		httputil.WriteError(w, http.StatusBadGateway, fmt.Sprintf("worker tools API error (status %d): %s",
			resp.StatusCode, string(respBody)))
		return nil, false
	}
}
