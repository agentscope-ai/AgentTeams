// The worker MCP registry proxy exposes a read-only view of the MCP
// servers a worker registered ITSELF in its qwenpaw app (the app-level MCP
// store: builtin plugin servers such as teamharness/workerflow, plus
// anything the worker added at runtime). The deployment-level catalog
// (GET /api/v1/mcp-servers) cannot see this half — it merges only the
// shared registry and the worker's spec.mcpServers — so the workbench had
// no way to show "what MCP servers does this worker actually run". The
// controller resolves the worker, enforces the same read boundary as the
// worker channels proxy (W8: 404, never 403, so worker existence cannot be
// probed), and forwards to the worker's own qwenpaw app over loopback.
//
// The response contract, for EVERY reader, is a JSON array (no
// re-wrapping). L1 (admin/manager) is the only reader that gets the
// upstream array verbatim. Every other caller (L2 team leaders and
// humans, L3 worker-scoped humans, and the worker's own service account)
// gets a server-side whitelist PROJECTION before the response is written:
// per server only name (falling back to id/key), transport (falling back
// to type), enabled, and a redacted url (host+path, the redactMCPURL
// contract). env, headers, args, command — and every other field — are
// omitted WHOLE, not masked: an open namespace cannot be scrubbed field
// by field (lesson #610: fail-closed shrinking only), and args may carry
// literal credentials (--token=...) that no field-level strip catches.
// Nothing is lost by the projection for the "what is declared" question —
// the declared catalog (spec.mcpServers) is already readable verbatim
// through the existing worker GET — but this endpoint exposes the
// RUNTIME-injected infrastructure credentials (e.g.
// AGENTTEAMS_WORKER_MATRIX_TOKEN, AGENTTEAMS_FS_ACCESS_KEY/SECRET_KEY in
// env), which no reader below L1 may see.
//
// Upstream faults are uniform and echo-free: a 200 body over 64 KiB is
// 502 (worker MCP response exceeds 64 KiB — the body is read cap+1 bytes
// so the (cap+1)th byte is the evidence; a silent truncation would hand
// back a broken array as success); a 200 body that does not parse as a
// JSON array is 502 (worker MCP API returned malformed JSON — never
// passed through, never degraded to an empty array); ANY other upstream
// status is a uniform 502 (worker MCP API error) WITHOUT echoing the
// upstream body (the body is an open upstream namespace).
//
// Embedded mode only: the proxy targets the per-worker console app, which
// the Kubernetes deployment model does not expose per worker, so the route
// 503s uniformly there (same rationale as the channels/checkpoint
// proxies).
package server

import (
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
)

const (
	// workerMCPProxyTimeout bounds each upstream call to the worker's
	// qwenpaw app (same bound as the channels/checkpoint proxies).
	workerMCPProxyTimeout = 5 * time.Second

	// workerMCPBodyCap bounds the proxied response body — the qwenpaw
	// /api/mcp list is a small array of server entries; the cap guards
	// against a misbehaving upstream. The read is LimitReader(cap+1) and
	// a body strictly longer than the cap is 502 (never truncated and
	// returned as a broken-but-successful array).
	workerMCPBodyCap = 64 << 10
)

// workerMCPProxy carries the per-deployment state of the worker-level MCP
// registry proxy (the embedded-mode gate, the upstream dial client, and
// the address resolution). Worker/team resolution reuses the
// ResourceHandler's client and namespace, so no state is duplicated.
type workerMCPProxy struct {
	rh              *ResourceHandler
	kubeMode        string
	containerPrefix string
	http            *http.Client
	// workerBaseURL resolves a worker name to its qwenpaw app base URL
	// from the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
}

// WorkerMCP returns the read-only handler for
//
//	GET /api/v1/workers/{name}/mcp
//
// which forwards to the worker's own qwenpaw app GET /api/mcp and passes
// the JSON array through verbatim to L1 (admin/manager); every other
// reader's copy is projected to safe metadata server-side (see the
// package doc).
//
// kubeMode and containerPrefix come from the ServerDeps at route
// registration time: the ResourceHandler is mode-agnostic (unlike the
// dedicated worker-proxy handler structs), so the proxy state lives in
// the closure-bound workerMCPProxy instead of new handler fields.
//
// The pipeline mirrors the worker channels proxy: worker-name
// validation, the embedded-mode gate (uniform 503 in kube mode, before
// any worker lookup, so worker existence cannot be probed), worker
// resolution (404 when unknown), the W8 team boundary (404, never 403;
// L3 humans may read exactly their assigned workers), the qwenpaw
// runtime guard (400), the bounded dial, and the bounded body — with
// the MCP-specific contract on top: oversize 502, malformed-JSON 502,
// and a uniform echo-free 502 for every non-200 upstream status.
func (h *ResourceHandler) WorkerMCP(kubeMode, containerPrefix string) http.HandlerFunc {
	p := &workerMCPProxy{
		rh:              h,
		kubeMode:        kubeMode,
		containerPrefix: containerPrefix,
		http:            &http.Client{Timeout: workerMCPProxyTimeout},
	}
	p.workerBaseURL = p.defaultWorkerBaseURL
	return p.serve
}

// defaultWorkerBaseURL resolves the worker's qwenpaw app base URL via the
// effective container prefix and the system-wins console port — identical
// to the channels/checkpoint proxies, so the dial always targets the port
// the container listens on (a conflicting spec.env value is discarded at
// creation time).
func (p *workerMCPProxy) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", p.containerPrefix, name, port)
}

// serve performs the shared request pipeline for the MCP registry route:
// scope checks, the upstream dial, and the status mapping (see the
// package doc for the contract).
func (p *workerMCPProxy) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	// Kube-mode check runs before any worker lookup: uniform 503 (rather
	// than a per-worker 404 vs 503 split) so worker existence cannot be
	// probed — same discipline as the channels proxy.
	if p.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker MCP registry requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := p.rh.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: p.rh.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker mcp registry", err)
		return
	}
	// findTeamMember's second return value is the member (worker) name, not
	// the team name — the scope check compares against the Team CR name.
	// Standalone workers (no team) resolve to "" which TeamMatches rejects,
	// hiding them from team-scoped callers as 404 (L3 humans with the
	// worker in their accessibleWorkers are the exception — the read leg
	// below).
	teamObj, _, _, err := findTeamMember(r.Context(), p.rh.client, p.rh.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker mcp registry", err)
		return
	}
	teamName := ""
	if teamObj != nil {
		teamName = teamObj.Name
	}
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		(caller.Role == authpkg.RoleTeamLeader || caller.Role == authpkg.RoleHuman) {
		allowed := caller.TeamMatches(teamName)
		if !allowed {
			// Read leg: L3 (worker-scoped) humans may read exactly their
			// assigned workers (the channels proxy's read boundary).
			allowed = caller.WorkerReadable(teamName, name)
		}
		if !allowed {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
	}

	// runtime-aware: the MCP registry is qwenpaw-specific. This runs
	// after the team-scope check above so an out-of-scope caller sees 404
	// (existence hidden) regardless of the worker's runtime.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "worker MCP registry is only supported for qwenpaw workers")
		return
	}

	// The container identity is the effective runtime name (spec.workerName
	// when set, the CR name otherwise) — dialing by the CR name would miss
	// the container for imported/renamed workers. The authorization checks
	// above deliberately stay on the original resource/team identity.
	base := p.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/api/mcp", nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build worker MCP request: "+err.Error())
		return
	}
	resp, err := p.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker MCP API unreachable")
		return
	}
	defer resp.Body.Close()

	// cap+1 read: a body strictly longer than the cap keeps the
	// (cap+1)th byte as evidence and is answered 502 — a silent
	// truncation would hand back a broken array as success.
	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, workerMCPBodyCap+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read worker MCP response: "+err.Error())
		return
	}
	if len(upstreamBody) > workerMCPBodyCap {
		httputil.WriteError(w, http.StatusBadGateway, "worker MCP response exceeds 64 KiB")
		return
	}
	// Only a 200 is success for this proxy. Any other upstream status is
	// a uniform 502 that does NOT echo the upstream body (the body is an
	// open upstream namespace — fail-closed: its bytes never enter the
	// response; the old pass-through of 400/404/409/422 details is gone,
	// see #1453).
	if resp.StatusCode != http.StatusOK {
		httputil.WriteError(w, http.StatusBadGateway, "worker MCP API error")
		return
	}
	// The JSON array shape is the contract for EVERY reader (L1
	// included): a 200 body that does not parse as an array is a broken
	// upstream, answered 502 — never passed through raw, never degraded
	// to an empty array (the old [] fallback reported upstream breakage
	// as "no servers", see #1453).
	var entries []any
	if err := json.Unmarshal(upstreamBody, &entries); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker MCP API returned malformed JSON")
		return
	}
	// Every reader below L1 (L2 team readers, L3 worker-scoped, and the
	// worker's own SA) gets the whitelist projection — frontend-only
	// masking is not a boundary, the response written here is the
	// contract (same discipline as the channels proxy).
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil && !isMCPProxyL1(caller) {
		projected, err := json.Marshal(projectSafeMCPServers(entries))
		if err != nil { // unreachable: entries came from json.Unmarshal
			httputil.WriteError(w, http.StatusInternalServerError, "project worker MCP metadata")
			return
		}
		upstreamBody = projected
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(upstreamBody)
}

// isMCPProxyL1 reports whether the caller is a full-access (L1)
// principal for this proxy — the only reader that receives the upstream
// array unsanitized. It is an allowlist (admin/manager), deliberately
// NOT the package-level isL1 in worker_runtime_config.go, whose negation
// form also treats the worker's own service account as L1: a worker SA
// reading its own registry must still be sanitized, because env carries
// the runtime-injected infrastructure credentials.
func isMCPProxyL1(caller *authpkg.CallerIdentity) bool {
	return caller != nil &&
		(caller.Role == authpkg.RoleAdmin || caller.Role == authpkg.RoleManager)
}

// projectSafeMCPServers is the whitelist projection every reader below
// L1 (L2 team readers, L3 worker-scoped readers, and the worker's own
// service account) gets instead of the raw upstream array: they may read
// WHICH servers a worker runs (name, transport, enabled) and WHERE
// (host+path) but not HOW IT IS AUTHENTICATED — so per server only those
// four fields are emitted, and env, headers, args, command — and every
// other field — are omitted WHOLE, not masked. The old denylist strip
// (delete env/headers, scrub URL userinfo) is superseded: an open
// namespace cannot be scrubbed field by field (lesson #610: fail-closed
// shrinking only), and args may carry literal credentials
// (--token=...) that no field-level strip catches (#1453).
//
// Upstream field drift is tolerated on the whitelisted fields only: a
// missing name falls back to id/key, transport falls back to type. The
// url goes through redactMCPURL (host+path only — userinfo, query
// consumer keys, and fragment are stripped; unparseable URLs redact to
// empty rather than echo raw). Non-object array entries cannot be
// projected and are dropped, so the output always stays a JSON array of
// projectable server objects.
func projectSafeMCPServers(entries []any) []any {
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		server, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		projected := map[string]any{}
		if name, ok := firstStringField(server, "name", "id", "key"); ok {
			projected["name"] = name
		}
		if transport, ok := firstStringField(server, "transport", "type"); ok {
			projected["transport"] = transport
		}
		if enabled, ok := server["enabled"].(bool); ok {
			projected["enabled"] = enabled
		}
		if rawURL, ok := server["url"].(string); ok {
			projected["url"] = redactMCPURL(rawURL)
		}
		out = append(out, projected)
	}
	return out
}

// firstStringField returns the first candidate key holding a non-empty
// string value (upstream field drift between qwenpaw releases).
func firstStringField(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}
