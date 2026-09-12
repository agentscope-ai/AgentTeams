package server

// Worker channel configuration (GET/PUT /api/v1/workers/{name}/channels/...).
//
// Each worker's qwenpaw app (effective console port, default 8088) exposes
// the full channel-configuration API used by the QwenPaw console Channels
// page. The Controller proxies a fixed surface so that L1 admins and L2
// humans can graphically connect channels (QQ / Matrix / DingTalk / ...) to
// agents in their teams without SSH or docker access:
//
//	GET  /api/v1/workers/{name}/channels                 all channel configs
//	GET  /api/v1/workers/{name}/channels/types           channel name list
//	GET  /api/v1/workers/{name}/channels/schemas         per-channel form schemas (UI render driver)
//	GET  /api/v1/workers/{name}/channels/{channel}       single channel config
//	PUT  /api/v1/workers/{name}/channels/{channel}       update (body = full channel config)
//	GET  /api/v1/workers/{name}/channels/{channel}/health
//	GET  /api/v1/workers/{name}/channels/{channel}/qrcode
//	GET  /api/v1/workers/{name}/channels/{channel}/qrcode/status
//	POST /api/v1/workers/{name}/channels/{channel}/restart
//	POST /api/v1/workers/{name}/channels/{channel}/conflict-check
//
// Design notes (full contract in docs/design/worker-channels-api.md):
//
//   - Single-agent worker context: without an X-Agent-Id header the worker's
//     qwenpaw app resolves the "active agent from config", which in a
//     single-profile worker container is the worker's own agent — so the
//     global (non-agent-scoped) path targets the right agent.
//   - PUT is the qwenpaw-authoritative write path: upstream validates the
//     payload (pydantic), persists it into agent.json and hot-reloads the
//     channel (no worker restart). The worker's push_loop then propagates
//     the file to the MinIO baseline that mirror_all pulls on rebuild; the
//     read-back below covers the persistence gap that burned manual edits.
//   - Credentials round-trip unmasked by design: scoped callers can only
//     reach agents in their own teams, and the config form needs the saved
//     values to round-trip unchanged.
//   - The handler is the real boundary (middleware requires): team leaders
//     are read-only on channels (403 on mutations), L2 humans are scoped to
//     their accessibleTeams (W8: 404, never 403, so cross-team existence
//     cannot be probed). L2 write authorization at the middleware rides on
//     the worker-scoped update policy — until it lands, L2 PUTs are denied
//     by the middleware and this PR's L2 path is inert-but-correct.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// channelProxyTimeout bounds each upstream call to the worker's qwenpaw
	// app (same bound as the checkpoint proxy).
	channelProxyTimeout = 5 * time.Second

	// minioPersistedHeader reports the read-back validation outcome of a
	// successful PUT without touching the verbatim upstream response body.
	// Values: "true" (baseline verified), "false" (baseline not converged
	// within the read-back budget), "skipped" (no storage client configured).
	minioPersistedHeader = "X-AgentTeams-MinIO-Persisted"

	channelReadbackAttempts = 3
	channelReadbackInterval = 2 * time.Second

	// channelBodyCap bounds the proxied request/response bodies — channel
	// configs and schemas are small documents; the cap guards against a
	// misbehaving upstream.
	channelBodyCap = 64 << 10
)

// channelNamePattern matches valid qwenpaw channel names (lowercase
// letters/digits, hyphen, underscore — e.g. qq, matrix, dingtalk,
// agentteams_matrix). Anything else (path separators, dots, uppercase) is
// rejected before the dial so it can never be injected into the upstream URL.
var channelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// reservedChannelSubpaths occupy the single-segment channel position but are
// fixed resources, never channel names.
var reservedChannelSubpaths = map[string]bool{"types": true, "schemas": true}

// ChannelsHandler proxies the worker channel-configuration endpoints.
type ChannelsHandler struct {
	client          client.Client
	namespace       string
	kubeMode        string
	http            *http.Client
	containerPrefix string
	// oss is the storage client for the MinIO baseline read-back (nil =
	// read-back skipped; the header reports "skipped").
	oss oss.StorageClient
	// workerBaseURL resolves a worker name to its qwenpaw app base URL from
	// the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
	// readbackInterval spaces the MinIO read-back attempts (injectable so
	// tests do not sleep the production 2s cadence).
	readbackInterval time.Duration
}

// NewChannelsHandler creates the handler with the default embedded-mode
// worker address resolution (same chain as the checkpoint proxy).
func NewChannelsHandler(c client.Client, namespace, kubeMode, containerPrefix string, o oss.StorageClient) *ChannelsHandler {
	h := &ChannelsHandler{
		client:           c,
		namespace:        namespace,
		kubeMode:         kubeMode,
		http:             &http.Client{Timeout: channelProxyTimeout},
		containerPrefix:  containerPrefix,
		oss:              o,
		readbackInterval: channelReadbackInterval,
	}
	h.workerBaseURL = h.defaultWorkerBaseURL
	return h
}

// defaultWorkerBaseURL resolves the worker's qwenpaw app base URL via the
// effective container prefix and the system-wins console port — identical to
// the checkpoint proxy, so the dial always targets the port the container
// listens on (a conflicting spec.env value is discarded at creation time).
func (h *ChannelsHandler) defaultWorkerBaseURL(name string, env map[string]string) string {
	port := service.EffectiveWorkerConsolePort(env)
	return fmt.Sprintf("http://%s%s:%s", h.containerPrefix, name, port)
}

// channelsScope validates the worker name, enforces embedded mode and the W8
// team boundary, and returns the upstream base URL. It writes the HTTP error
// response itself and returns ok=false on any rejection.
func (h *ChannelsHandler) channelsScope(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	if name == "" || !workerNamePattern.MatchString(name) {
		httputil.WriteError(w, http.StatusBadRequest, "worker name is required and must be a valid DNS label")
		return "", false
	}
	// Kube-mode check runs before any worker lookup: uniform 503 (rather
	// than a per-worker 404 vs 503 split) so worker existence cannot be
	// probed — same discipline as the checkpoint proxy.
	if h.kubeMode != "embedded" {
		httputil.WriteError(w, http.StatusServiceUnavailable, "worker channel configuration requires embedded mode")
		return "", false
	}
	var worker v1beta1.Worker
	if err := h.client.Get(r.Context(), client.ObjectKey{Name: name, Namespace: h.namespace}, &worker); err != nil {
		if apierrors.IsNotFound(err) {
			httputil.WriteError(w, http.StatusNotFound, "worker not found")
			return "", false
		}
		writeK8sError(w, "get worker channels", err)
		return "", false
	}
	// findTeamMember's second return value is the member (worker) name, not
	// the team name — the scope check compares against the Team CR name.
	// Standalone workers (no team) resolve to "" which TeamMatches rejects,
	// hiding them from scoped callers as 404.
	teamObj, _, _, err := findTeamMember(r.Context(), h.client, h.namespace, name)
	if err != nil {
		writeK8sError(w, "get worker channels", err)
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

// channelRoute describes one fixed upstream mapping — never a generic
// reverse proxy, so the attack surface stays bounded to the documented
// qwenpaw endpoints.
type channelRoute struct {
	method   string
	upstream string // path appended to the worker base URL
	query    string // pre-validated query string (qrcode/status token only)
	body     []byte // JSON request body (PUT channel config / conflict-check)
	readback bool   // verify the MinIO baseline after a successful 200
	mutates  bool   // true for state-changing calls (audit-logged)
}

// serveChannels performs the shared scope check, dials the worker's qwenpaw
// app and maps the response (see the package doc for the status contract).
func (h *ChannelsHandler) serveChannels(w http.ResponseWriter, r *http.Request, route channelRoute) {
	name := r.PathValue("name")
	caller := authpkg.CallerFromContext(r.Context())

	// Real boundary (the middleware only requires): team leaders manage
	// nothing — channel configuration is a human/admin operation, so their
	// mutations are rejected with an honest 403 (the team matched, unlike
	// the cross-team 404 above).
	if route.mutates && caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker channels")
		return
	}

	base, ok := h.channelsScope(w, r, name)
	if !ok {
		return
	}
	target := base + route.upstream
	if route.query != "" {
		target += "?" + route.query
	}
	var bodyReader io.Reader
	if route.body != nil {
		bodyReader = bytes.NewReader(route.body)
	}
	req, err := http.NewRequestWithContext(r.Context(), route.method, target, bodyReader)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "build channel request: "+err.Error())
		return
	}
	if route.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// Connection refused (worker stopped), DNS failure, timeout.
		httputil.WriteError(w, http.StatusBadGateway, "worker channel API unreachable")
		return
	}
	defer resp.Body.Close()

	upstreamBody, err := io.ReadAll(io.LimitReader(resp.Body, channelBodyCap))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "read worker channel response: "+err.Error())
		return
	}
	switch resp.StatusCode {
	case http.StatusOK:
		persisted := ""
		if route.readback {
			persisted = h.verifyMinioPersistence(r.Context(), name, r.PathValue("channel"), upstreamBody)
			w.Header().Set(minioPersistedHeader, persisted)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstreamBody)
		if route.mutates {
			log.FromContext(r.Context()).Info("worker channel updated",
				"worker", name,
				"upstream", route.upstream,
				"actor", authzActor(caller),
				"minio_persisted", persisted)
		}
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		// Passed through verbatim: 404 = unknown channel or a QwenPaw
		// version without the router (version gate); 400/422 = upstream
		// pydantic validation with an actionable detail; 409 = upstream
		// conflict. The upstream detail is the contract here.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(upstreamBody)
	default:
		httputil.WriteError(w, http.StatusBadGateway,
			fmt.Sprintf("worker channel API error (status %d): %s", resp.StatusCode, truncateForMessage(string(upstreamBody))))
	}
}

// truncateForMessage caps an upstream body embedded in an error message.
func truncateForMessage(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// validateChannelName enforces the channel-name charset (injection guard).
// ok=false means the 400 was already written.
func validateChannelName(w http.ResponseWriter, ch string) bool {
	if !channelNamePattern.MatchString(ch) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid channel name")
		return false
	}
	return true
}

// getChannels handles GET /api/v1/workers/{name}/channels.
func (h *ChannelsHandler) getChannels(w http.ResponseWriter, r *http.Request) {
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/config/channels"})
}

// getChannelResource handles GET /api/v1/workers/{name}/channels/{sub} where
// sub is "types", "schemas" or a channel name.
func (h *ChannelsHandler) getChannelResource(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	if sub == "" {
		httputil.WriteError(w, http.StatusBadRequest, "channel resource is required")
		return
	}
	if !reservedChannelSubpaths[sub] && !validateChannelName(w, sub) {
		return
	}
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/config/channels/" + sub})
}

// putChannel handles PUT /api/v1/workers/{name}/channels/{channel}. The body
// must be the full channel config object; upstream validates it (pydantic)
// and the validation errors pass through verbatim.
func (h *ChannelsHandler) putChannel(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, channelBodyCap))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	// Reject empty bodies before the dial: upstream would treat them as an
	// empty config object and wipe the saved channel.
	if len(bytes.TrimSpace(body)) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "request body must be the full channel config object (JSON)")
		return
	}
	h.serveChannels(w, r, channelRoute{
		method:   http.MethodPut,
		upstream: "/config/channels/" + r.PathValue("channel"),
		body:     body,
		readback: true,
		mutates:  true,
	})
}

// getChannelHealth handles GET .../channels/{channel}/health.
func (h *ChannelsHandler) getChannelHealth(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/config/channels/" + ch + "/health"})
}

// getChannelQrcode handles GET .../channels/{channel}/qrcode (QR-auth
// channels: wechat / dingtalk scan login).
func (h *ChannelsHandler) getChannelQrcode(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/config/channels/" + ch + "/qrcode"})
}

// getQrcodeStatus handles GET .../channels/{channel}/qrcode/status?token=.
// Strict query whitelist (only token) — same discipline as the checkpoint
// proxy: unknown parameters are rejected rather than forwarded.
func (h *ChannelsHandler) getQrcodeStatus(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	q := r.URL.Query()
	for key := range q {
		if key != "token" {
			httputil.WriteError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return
		}
	}
	token := q.Get("token")
	if token == "" {
		httputil.WriteError(w, http.StatusBadRequest, "token query parameter is required")
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{
		method:   http.MethodGet,
		upstream: "/config/channels/" + ch + "/qrcode/status",
		query:    "token=" + url.QueryEscape(token),
	})
}

// restartChannel handles POST .../channels/{channel}/restart (stop/start the
// channel without restarting the worker agent).
func (h *ChannelsHandler) restartChannel(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{
		method:   http.MethodPost,
		upstream: "/config/channels/" + ch + "/restart",
		mutates:  true,
	})
}

// checkChannelConflict handles POST .../channels/{channel}/conflict-check
// (detects other agents holding the same channel credentials — the QQ
// double-AppID kick-out guard). Non-mutating: a read-only check.
func (h *ChannelsHandler) checkChannelConflict(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, channelBodyCap))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{
		method:   http.MethodPost,
		upstream: "/config/channels/" + ch + "/conflict-check",
		body:     body,
	})
}

// agentJSONMinIOKey is the MinIO key (relative to the storage prefix) of the
// worker's qwenpaw agent profile file that push_loop keeps in sync.
func agentJSONMinIOKey(worker string) string {
	return "agents/" + worker + "/.qwenpaw/workspaces/default/agent.json"
}

// verifyMinioPersistence reads back the MinIO baseline after a successful
// PUT and reports whether the worker's push_loop has converged it. It is
// bounded (a few seconds), never blocks on convergence, and never writes to
// the baseline — single-writer discipline: push_loop owns the MinIO copy.
func (h *ChannelsHandler) verifyMinioPersistence(ctx context.Context, worker, channel string, saved []byte) string {
	if h.oss == nil {
		return "skipped"
	}
	savedJSON, ok := canonicalJSON(saved)
	if !ok {
		// Upstream returned non-JSON on 200: unverifiable, report false
		// (the response body itself still reaches the client verbatim).
		return "false"
	}
	key := agentJSONMinIOKey(worker)
	for attempt := 0; attempt < channelReadbackAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "false"
			case <-time.After(h.readbackInterval):
			}
		}
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // push_loop has not written the baseline yet
			}
			// Transient storage error: one retry round is worth it, but do
			// not mask a hard failure as "converged".
			continue
		}
		if persisted, ok := extractChannelSection(data, channel); ok && bytes.Equal(persisted, savedJSON) {
			return "true"
		}
	}
	return "false"
}

// canonicalJSON re-encodes a JSON document with sorted keys so semantic
// equality does not depend on field ordering (Go marshals maps in key
// order, both sides included).
func canonicalJSON(raw []byte) ([]byte, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return out, true
}

// extractChannelSection pulls channels.<channel> out of an agent.json
// document and returns its canonical form.
func extractChannelSection(agentJSON []byte, channel string) ([]byte, bool) {
	var doc struct {
		Channels map[string]json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(agentJSON, &doc); err != nil {
		return nil, false
	}
	raw, ok := doc.Channels[channel]
	if !ok {
		return nil, false
	}
	return canonicalJSON(raw)
}
