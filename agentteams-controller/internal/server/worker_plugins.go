// The worker plugin registry proxy exposes a read-only view of the plugins
// installed in a worker's qwenpaw app (the app-level plugin store: builtin
// plugin servers such as teamharness/workerflow, plus anything the worker
// added at runtime). The deployment-level views (the worker CR, the skill
// catalog) cannot see this half — the workbench has no way to show "which
// plugins does this worker actually have installed, at what version". The
// controller resolves the worker, enforces the same read boundary as the
// worker channels proxy (W8: 404, never 403, so worker existence cannot be
// probed), and forwards to the worker's own qwenpaw app over loopback.
//
// The response contract, for EVERY reader, is a JSON array of plugin
// entries (no re-wrapping). The array is a server-side whitelist
// PROJECTION, not a pass-through: per entry only id, name, version,
// description, author, enabled, loaded, plugin_type, and frontend_entry
// are emitted; every other field is omitted WHOLE, and a missing field is
// left absent rather than back-filled with null. The metadata-only
// surface is pinned by construction — a field qwenpaw adds in a future
// release is invisible here until it is whitelisted deliberately (the
// original verbatim pass-through widened the visible surface
// automatically with every upstream release, #1454). No reader (L1
// admin/manager included) gets a raw leg: the question this endpoint
// answers is "which plugins are installed, at what version" — model or
// switch fields are never part of the answer.
//
// Upstream faults are uniform and echo-free: a 200 body over 64 KiB is
// 502 (worker plugin registry response exceeds 64 KiB — the body is read
// cap+1 bytes so the (cap+1)th byte is the evidence; a silent
// truncation would hand back a broken array as success); a 200 body that
// does not parse as an array of plugin objects is 502 (worker plugins API
// returned malformed JSON — never passed through, never degraded to an
// empty array); ANY other upstream status is a uniform 502 (worker
// plugins API error) WITHOUT echoing the upstream body (the body is an
// open upstream namespace).
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
	// workerPluginsProxyTimeout bounds each upstream call to the worker's
	// qwenpaw app (same bound as the channels/checkpoint proxies).
	workerPluginsProxyTimeout = 5 * time.Second

	// workerPluginsBodyCap bounds the proxied response body — the qwenpaw
	// /api/plugins list is a small array of plugin entries; the cap guards
	// against a misbehaving upstream. The read is LimitReader(cap+1) and
	// a body strictly longer than the cap is 502 (never truncated and
	// returned as a broken-but-successful array).
	workerPluginsBodyCap = 64 << 10
)

// workerPluginMetadataKeys is the whitelist of per-entry fields the
// proxy may emit — the pinned metadata surface (#1454). A field qwenpaw
// adds in a future release is invisible to every reader until it is
// added here deliberately; model/switch fields never belong in the
// projection (which plugins exist and whether they are on is the only
// question this endpoint answers).
var workerPluginMetadataKeys = []string{
	"id", "name", "version", "description", "author",
	"enabled", "loaded", "plugin_type", "frontend_entry",
}

// workerPluginsProxy carries the per-deployment state of the worker-level
// plugin registry proxy (the embedded-mode gate, the upstream dial client,
// and the address resolution). Worker/team resolution reuses the
// ResourceHandler's client and namespace, so no state is duplicated.
type workerPluginsProxy struct {
	rh              *ResourceHandler
	kubeMode        string
	containerPrefix string
	http            *http.Client
	// workerBaseURL resolves a worker name to its qwenpaw app base URL
	// from the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
}

// WorkerPlugins returns the read-only handler for
//
//	GET /api/v1/workers/{name}/plugins
//
// which forwards to the worker's own qwenpaw app GET /api/plugins and
// serves every scoped reader the whitelist projection of the
// installed-plugin array (metadata only by construction — see the
// package doc for the contract; unlike the MCP registry proxy there is
// no L1-only raw leg).
//
// kubeMode and containerPrefix come from the ServerDeps at route
// registration time: the ResourceHandler is mode-agnostic (unlike the
// dedicated worker-proxy handler structs), so the proxy state lives in
// the closure-bound workerPluginsProxy instead of new handler fields.
//
// The pipeline mirrors the worker channels proxy: worker-name
// validation, the embedded-mode gate (uniform 503 in kube mode, before
// any worker lookup, so worker existence cannot be probed), worker
// resolution (404 when unknown), the W8 team boundary (404, never 403;
// L3 humans may read exactly their assigned workers), the qwenpaw
// runtime guard (400), the bounded dial, and the bounded body — with
// the plugins-specific contract on top: oversize 502, malformed-JSON
// 502, and a uniform echo-free 502 for every non-200 upstream status.
func (h *ResourceHandler) WorkerPlugins(kubeMode, containerPrefix string) http.HandlerFunc {
	p := &workerPluginsProxy{
		rh:              h,
		kubeMode:        kubeMode,
		containerPrefix: containerPrefix,
		http:            &http.Client{Timeout: workerPluginsProxyTimeout},
	}
	p.workerBaseURL = p.defaultWorkerBaseURL
	return p.serve
}

// defaultWorkerBaseURL resolves the worker's qwenpaw app base URL via the
// effective container prefix and the system-wins console port — identical
// to the channels/checkpoint proxies, so the dial always targets the port
// the container listens on (a conflicting spec.env value is discarded at
// creation time).
func (p *workerPluginsProxy) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", p.containerPrefix, name, port)
}

// serve performs the shared request pipeline for the plugin registry
// route: scope checks, the upstream dial, and the status mapping (see the
// package doc for the contract).
func (p *workerPluginsProxy) serve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return
	}
	// Kube-mode check runs before any worker lookup: uniform 503 (rather
	// than a per-worker 404 vs 503 split) so worker existence cannot be
	// probed — same discipline as the channels proxy.
	if p.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker plugin registry requires embedded mode")
		return
	}

	var worker v1beta1.Worker
	if err := p.rh.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: p.rh.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return
		}
		writeK8sError(w, "get worker plugins", err)
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
		writeK8sError(w, "get worker plugins", err)
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

	// runtime-aware: the plugin registry is qwenpaw-specific. This runs
	// after the team-scope check above so an out-of-scope caller sees 404
	// (existence hidden) regardless of the worker's runtime.
	if rt := worker.Spec.Runtime; rt != "" && rt != "qwenpaw" {
		httputil.WriteError(w, http.StatusBadRequest, "worker plugin registry is only supported for qwenpaw workers")
		return
	}

	// The container identity is the effective runtime name (spec.workerName
	// when set, the CR name otherwise) — dialing by the CR name would miss
	// the container for imported/renamed workers. The authorization checks
	// above deliberately stay on the original resource/team identity.
	base := p.workerBaseURL(worker.Spec.EffectiveWorkerName(worker.Name), worker.Spec.Env)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/api/plugins", nil)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build worker plugins request: "+err.Error())
		return
	}
	resp, err := p.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker plugins API unreachable")
		return
	}
	defer resp.Body.Close()

	// cap+1 read: a body strictly longer than the cap keeps the
	// (cap+1)th byte as evidence and is answered 502 — a silent
	// truncation would hand back a broken array as success (#1454).
	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, workerPluginsBodyCap+1))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read worker plugins response: "+err.Error())
		return
	}
	if len(upstreamBody) > workerPluginsBodyCap {
		httputil.WriteError(w, http.StatusBadGateway, "worker plugin registry response exceeds 64 KiB")
		return
	}
	// Only a 200 is success for this proxy. Any other upstream status is
	// a uniform 502 that does NOT echo the upstream body (the body is an
	// open upstream namespace — fail-closed: its bytes never enter the
	// response; the old pass-through of 400/404/409/422 details is gone,
	// #1454).
	if resp.StatusCode != http.StatusOK {
		httputil.WriteError(w, http.StatusBadGateway, "worker plugins API error")
		return
	}
	// The JSON array shape is the contract for EVERY reader: a 200 body
	// that does not parse as an array of plugin objects is a broken
	// upstream, answered 502 — never passed through raw, never degraded
	// to an empty array (#1454). A non-object entry (or a JSON null,
	// which unmarshals to a nil slice without error) is not the
	// array-of-entries shape and is refused the same way.
	var entries []map[string]any
	if err := json.Unmarshal(upstreamBody, &entries); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker plugins API returned malformed JSON")
		return
	}
	if entries == nil {
		// json.Unmarshal leaves a JSON null as a nil slice without
		// erroring; the contract is an array (an empty [] — a worker
		// with no plugins — is fine, null is not).
		httputil.WriteError(w, http.StatusBadGateway, "worker plugins API returned malformed JSON")
		return
	}
	// Every reader — L1 admin/manager included — gets the whitelist
	// projection; the pinned metadata surface is the contract (#1454).
	projected, err := json.Marshal(projectPluginMetadata(entries))
	if err != nil { // unreachable: entries came from json.Unmarshal
		httputil.WriteError(w, http.StatusInternalServerError, "project worker plugins metadata")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(projected)
}

// projectPluginMetadata is the whitelist projection every reader gets
// instead of the raw upstream array: per entry only the pinned metadata
// fields (workerPluginMetadataKeys) are emitted, each with its upstream
// value verbatim; every other field is omitted WHOLE (not masked — an
// open namespace cannot be scrubbed field by field, lesson #610), and a
// missing whitelisted field is left absent, never back-filled with
// null. Upstream field drift is tolerated on the whitelist only: an
// entry missing a whitelisted field simply omits it.
func projectPluginMetadata(entries []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		projected := map[string]any{}
		for _, key := range workerPluginMetadataKeys {
			if v, ok := entry[key]; ok {
				projected[key] = v
			}
		}
		out = append(out, projected)
	}
	return out
}
