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
// Upstream contract: requests are forwarded to the worker's qwenpaw config
// API under /api/config/channels/... — the path the worker's own client
// (qwenpaw_worker/api.py) and integration coverage use. The proxy is
// version-agnostic: the 9-route core contract is identical across the
// official QwenPaw 2.0.1 / 2.2.0 / 2.2.1 releases (see the version-contract
// section of docs/design/worker-channels-api.md). conflict-check is an
// additive 2.2.x-only route: 2.2.x workers serve it (config.py:379), and a
// worker on an older build returns its own 404, which the proxy passes
// through verbatim (version gate).
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
//   - Read contract (round-trip, by design): credential values round-trip
//     unmasked. Rationale: scoped callers can only reach agents in their own
//     teams (W8), and the config form needs the saved values to round-trip
//     unchanged. Decided per #1220 §13 Q5 (2026-09-16) — maintainer
//     confirmation requested in the PR; a masked read would be a separate
//     change (mask helper + a reveal capability), not a config flag.
//   - Write contract: the handler is the real boundary (middleware
//     requires): team leaders are read-only on channels (403 on mutations);
//     L2 humans are scoped to their accessibleTeams (W8: 404, never 403, so
//     cross-team existence cannot be probed) and may write non-credential
//     fields by default. A PUT is diffed against the saved channel config
//     before the write: credential fields (channelCredentialKeys) whose
//     value is unchanged are ordinary fields; replacing a value or
//     explicitly clearing it (empty string) additionally requires the
//     channel_secrets capability for L2 callers and is audit-logged for
//     every role. Credential fields omitted from the body are back-filled
//     from the saved values, because upstream replaces the whole channel
//     (config_class(**body)) — an omitted secret would otherwise be erased
//     by the model default. admin/manager are exempt from the gate (their
//     changed credential writes are audit-logged, not gated).
//   - Read-back contract (async): a successful PUT answers immediately with
//     X-AgentTeams-MinIO-Persisted: "pending"; a background re-check at a
//     conservative bound beyond the worker push_loop sync interval records
//     the convergence result in the audit log. The old bounded in-request
//     polling budget (3x2s) was systematically false-negative against the
//     push_loop (#1220 §11).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
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

	// minioPersistedHeader reports the read-back validation state of a
	// successful PUT without touching the verbatim upstream response body.
	// Values: "pending" (background re-check scheduled; the convergence
	// result lands in the audit log as action "channel_readback"),
	// "skipped" (no storage client configured). The former synchronous
	// "true"/"false" values are retired with the async read-back.
	minioPersistedHeader = "X-AgentTeams-MinIO-Persisted"

	// channelReadbackDelay is when the background baseline re-check runs
	// after a successful PUT. The worker push_loop sync interval is short
	// (check_interval=5s in the current qwenpaw worker), so this is a
	// conservative bound far beyond any normal push_loop lag: a healthy
	// push_loop has converged long before the check, and
	// "converged=false" is a signal of a sustained persistence problem,
	// not a race.
	channelReadbackDelay = 2 * time.Minute

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
	// audit records channel credential writes and read-back outcomes.
	// nil = audit disabled (tests without an audit store).
	audit *audit.Client
	// workerBaseURL resolves a worker name to its qwenpaw app base URL from
	// the effective prefix and the worker's env. Injectable for tests.
	workerBaseURL func(name string, env map[string]string) string
	// readbackDelay is when the background baseline re-check runs after a
	// successful PUT (injectable so tests do not wait the production 2min).
	readbackDelay time.Duration
	// onReadback, when set, is called from the background re-check goroutine
	// with the convergence result (test hook for deterministic assertions).
	onReadback func(converged bool)
	// readbackGen serializes superseded-write detection: each PUT that
	// schedules a baseline re-check claims the next generation for its
	// (worker, channel); a re-check whose generation is no longer the
	// newest (a newer PUT superseded the write) is skipped, so a newer
	// successful write is never reported as a persistence failure of an
	// older one.
	readbackMu  sync.Mutex
	readbackGen map[string]int64
}

// NewChannelsHandler creates the handler with the default embedded-mode
// worker address resolution (same chain as the checkpoint proxy).
func NewChannelsHandler(c client.Client, namespace, kubeMode, containerPrefix string, o oss.StorageClient, ac *audit.Client) *ChannelsHandler {
	h := &ChannelsHandler{
		client:          c,
		namespace:       namespace,
		kubeMode:        kubeMode,
		http:            &http.Client{Timeout: channelProxyTimeout},
		containerPrefix: containerPrefix,
		oss:             o,
		audit:           ac,
		readbackDelay:   channelReadbackDelay,
		readbackGen:     map[string]int64{},
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
	body     []byte // JSON request body (PUT channel config)
	readback bool   // verify the MinIO baseline after a successful 200
	mutates  bool   // true for state-changing calls (audit-logged)
	// changedCreds lists the credential fields this write changed or
	// cleared, pre-diffed against the saved config (PUT only). The audit
	// records exactly these — not a re-scan of the (possibly back-filled)
	// body, which would false-positive on round-tripped saved values.
	changedCreds []string
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
			persisted = h.scheduleMinioReadback(r.Context(), name, r.PathValue("channel"), upstreamBody)
			w.Header().Set(minioPersistedHeader, persisted)
			h.auditChannelWrite(r.Context(), name, r.PathValue("channel"), route.changedCreds, caller)
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
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/api/config/channels"})
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
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/api/config/channels/" + sub})
}

// putChannel handles PUT /api/v1/workers/{name}/channels/{channel}. The body
// must be the full channel config object; upstream validates it (pydantic)
// and the validation errors pass through verbatim.
//
// Credential semantics (the write contract, see the package doc): the
// request is diffed against the saved channel config before the write.
// Unchanged credential values are ordinary fields (a full-config edit
// carrying the round-tripped saved values is a normal edit); replacing a
// value or explicitly clearing it (empty string) requires the
// channel_secrets capability for L2 humans; omitted credential fields are
// back-filled from the saved values, because upstream replaces the whole
// channel (config_class(**body)) and an omitted secret would otherwise be
// erased by the model default.
func (h *ChannelsHandler) putChannel(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	// Team leaders manage nothing — before any upstream dial, so a leader
	// mutation never reaches the worker (serveChannels re-checks for its
	// other mutating routes).
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil && caller.Role == authpkg.RoleTeamLeader {
		httputil.WriteError(w, http.StatusForbidden, "team leaders have read-only access to worker channels")
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

	ch := r.PathValue("channel")
	base, ok := h.channelsScope(w, r, r.PathValue("name"))
	if !ok {
		return
	}

	// Fetch the saved channel config so the write can be diffed against it
	// (credential gate + back-fill). A read that fails the same way the
	// write would: the PUT is not attempted when the baseline is unknown.
	existing, exists, err := h.fetchChannelConfig(r.Context(), base, ch)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "worker channel API unreachable")
		return
	}

	// Unparseable bodies pass through untouched: the credential diff and
	// the back-fill both need a document, and upstream's pydantic
	// validation rejects the body anyway (no persistence without a 200).
	var incoming any
	changed, backfilled := []string{}, false
	if err := json.Unmarshal(body, &incoming); err == nil {
		changed, backfilled = reconcileCredentialFields(&incoming, existing, exists)
	}

	// Credential gate (L2 humans only; admin/manager writes are exempt but
	// audit-logged in the 200 branch). Unchanged values do not count —
	// only replacements and explicit clears do. The 403 names the
	// offending fields so the client knows exactly what to drop or keep
	// unchanged.
	if caller := authpkg.CallerFromContext(r.Context()); caller != nil &&
		caller.Role == authpkg.RoleHuman &&
		len(changed) > 0 && !authpkg.HasCapability(caller, authpkg.CapabilityChannelSecrets) {
		httputil.WriteError(w, http.StatusForbidden,
			"writing or clearing channel credentials requires the channel_secrets capability; credential fields changed: "+strings.Join(changed, ", "))
		return
	}

	out := body
	if backfilled {
		if encoded, err := json.Marshal(incoming); err == nil {
			out = encoded
		}
	}
	h.serveChannels(w, r, channelRoute{
		method:       http.MethodPut,
		upstream:     "/api/config/channels/" + ch,
		body:         out,
		readback:     true,
		mutates:      true,
		changedCreds: changed,
	})
}

// fetchChannelConfig reads the saved channel config from the worker's
// qwenpaw app. exists=false means upstream reported the channel unknown
// (404) — the write can still proceed (the PUT passes through upstream's
// own 404), but nothing can be back-filled or diffed. Any other transport
// or status error is returned: a PUT whose baseline cannot be read must not
// proceed, because an omitted secret would be erased on a 200.
func (h *ChannelsHandler) fetchChannelConfig(ctx context.Context, base, channel string) (raw []byte, exists bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/config/channels/"+channel, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, channelBodyCap))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return data, true, nil
}

// getChannelHealth handles GET .../channels/{channel}/health.
func (h *ChannelsHandler) getChannelHealth(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/api/config/channels/" + ch + "/health"})
}

// getChannelQrcode handles GET .../channels/{channel}/qrcode (QR-auth
// channels: wechat / dingtalk scan login).
func (h *ChannelsHandler) getChannelQrcode(w http.ResponseWriter, r *http.Request) {
	if !validateChannelName(w, r.PathValue("channel")) {
		return
	}
	ch := r.PathValue("channel")
	h.serveChannels(w, r, channelRoute{method: http.MethodGet, upstream: "/api/config/channels/" + ch + "/qrcode"})
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
		upstream: "/api/config/channels/" + ch + "/qrcode/status",
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
		upstream: "/api/config/channels/" + ch + "/restart",
		mutates:  true,
	})
}

// checkChannelConflict handles POST .../channels/{channel}/conflict-check
// (detects other agents holding the same channel credentials — the QQ
// double-AppID kick-out guard). Non-mutating: a read-only check run before
// a channel write. The route is additive to the 9-route core contract:
// QwenPaw 2.2.x exposes it (config.py:379, verified against the official
// wheels); a worker on a 2.0.x build returns its own 404, which the proxy
// passes through verbatim (version gate).
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
		upstream: "/api/config/channels/" + ch + "/conflict-check",
		body:     body,
	})
}

// agentJSONMinIOKey is the MinIO key (relative to the storage prefix) of the
// worker's qwenpaw agent profile file that push_loop keeps in sync.
func agentJSONMinIOKey(worker string) string {
	return "agents/" + worker + "/.qwenpaw/workspaces/default/agent.json"
}

// scheduleMinioReadback answers "pending" immediately and defers the
// baseline convergence check to a background goroutine that runs once, at
// readbackDelay after the PUT, and records the outcome in the audit log
// (action "channel_readback"). The in-request polling budget it replaced
// (3x2s) was systematically false-negative against the worker push_loop
// sync interval in production (#1220 §11): a healthy push_loop could
// never converge inside the request, so every PUT looked unpersisted.
// Each PUT claims the next readback generation for its (worker, channel);
// a re-check whose write was superseded by a newer PUT is skipped, so a
// newer successful write is never reported as a persistence failure of an
// older one. Single-writer discipline is unchanged: the re-check only
// reads; the worker's push_loop owns the MinIO copy.
func (h *ChannelsHandler) scheduleMinioReadback(ctx context.Context, worker, channel string, saved []byte) string {
	if h.oss == nil {
		return "skipped"
	}
	// Claim this write's generation: if a newer PUT lands before the
	// re-check runs, the older re-check must not report its (superseded)
	// expected config against the newer baseline.
	key := worker + "/" + channel
	h.readbackMu.Lock()
	h.readbackGen[key]++
	generation := h.readbackGen[key]
	h.readbackMu.Unlock()
	// Detached from the request: the response is written before the check
	// runs, and the check outlives the HTTP exchange by design.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	go func() {
		defer cancel()
		select {
		case <-bg.Done():
			return
		case <-time.After(h.readbackDelay):
		}
		h.readbackMu.Lock()
		superseded := h.readbackGen[key] != generation
		h.readbackMu.Unlock()
		if superseded {
			// A newer successful write owns the baseline now; this write's
			// convergence is neither true nor false — it is superseded,
			// and reporting it would false-alarm the older actor.
			log.FromContext(bg).Info("worker channel readback skipped: write superseded by a newer PUT",
				"worker", worker, "channel", channel)
			return
		}
		converged := h.checkMinioConvergence(bg, worker, channel, saved)
		if h.audit != nil {
			// The event is attributed to the actor who performed the PUT
			// (the caller identity survives context.WithoutCancel because
			// values are copied, not just the cancel signal).
			caller := authpkg.CallerFromContext(bg)
			who, role := "unknown", "unknown"
			if caller != nil {
				who, role = caller.Username, caller.Role
			}
			h.audit.Record(bg, audit.Event{
				Who:    who,
				Role:   role,
				Target: worker + "/" + channel,
				Action: "channel_readback",
				Detail: "converged=" + strconv.FormatBool(converged),
			})
		}
		if h.onReadback != nil {
			h.onReadback(converged)
		}
	}()
	return "pending"
}

// checkMinioConvergence is the single background baseline read: does
// channels.<channel> in the MinIO agent.json equal the config the worker's
// qwenpaw app acknowledged? One attempt by design — the generous delay
// (not retries) is what absorbs normal push_loop lag, so "false" at check
// time is a real signal worth auditing, not a race.
func (h *ChannelsHandler) checkMinioConvergence(ctx context.Context, worker, channel string, saved []byte) bool {
	savedJSON, ok := canonicalJSON(saved)
	if !ok {
		return false // non-JSON upstream 200: unverifiable
	}
	data, err := h.oss.GetObject(ctx, agentJSONMinIOKey(worker))
	if err != nil {
		return false // baseline missing or transient storage error
	}
	persisted, ok := extractChannelSection(data, channel)
	return ok && bytes.Equal(persisted, savedJSON)
}

// auditChannelWrite records a successful channel PUT that changed or
// cleared credential fields (who, which fields). The list is pre-diffed
// against the saved config in putChannel, so round-tripped unchanged
// values are not audit-logged. Non-credential writes are not audit-logged
// either — they are covered by the existing "worker channel updated"
// access log in serveChannels.
func (h *ChannelsHandler) auditChannelWrite(ctx context.Context, worker, channel string, changedCreds []string, caller *authpkg.CallerIdentity) {
	if h.audit == nil || caller == nil || len(changedCreds) == 0 {
		return
	}
	h.audit.Record(ctx, audit.Event{
		Who:        caller.Username,
		Role:       caller.Role,
		Target:     worker + "/" + channel,
		Action:     "channel_credential_write",
		Capability: string(authpkg.CapabilityChannelSecrets),
		Detail:     "credential_fields=" + strings.Join(changedCreds, ","),
	})
}

// channelCredentialKeys names the fields of qwenpaw 2.2.x channel config
// models (config.py) that carry secret material. The list is the contract
// for the channel_secrets gate: adding a new qwenpaw secret field here is
// what keeps the gate current. Generic "token"/"secret" are included so an
// unknown-but-secret-shaped key over-blocks (403 naming the field) rather
// than under-blocks.
var channelCredentialKeys = map[string]bool{
	"access_token":       true,
	"bot_token":          true,
	"token":              true,
	"app_secret":         true,
	"app_token":          true,
	"client_secret":      true,
	"secret":             true,
	"encrypt_key":        true,
	"verification_token": true,
	"password":           true,
	"sip_password":       true,
	"http_proxy_auth":    true,
	"api_key":            true,
	"dashscope_api_key":  true,
	"livekit_api_key":    true,
	"livekit_api_secret": true,
	"twilio_auth_token":  true,
}

// reconcileCredentialFields diffs the incoming channel config against the
// saved one, credential field by credential field (any nesting depth), and
// back-fills omitted credential fields from the saved values:
//
//   - present, non-empty, equal to the saved value → unchanged: neither
//     gated nor audit-logged (a full-config edit carrying the
//     round-tripped saved values is an ordinary edit);
//   - present, different from the saved value → a credential write
//     (gated for L2 humans + audit-logged);
//   - present as an explicit "" → an explicit clear (gated + audit-logged;
//     clearing through an empty string is a credential operation, not a
//     placeholder);
//   - absent from the incoming config but non-empty in the saved config →
//     back-filled into the incoming document (upstream replaces the whole
//     channel — an omitted secret would otherwise be erased by the model
//     default).
//
// It returns the sorted list of changed/cleared credential fields (for the
// gate and the audit) and whether the incoming document was modified. A
// missing or unreadable saved config means every present credential field
// is a write and nothing can be back-filled (fail gated).
func reconcileCredentialFields(incoming *any, existing []byte, exists bool) ([]string, bool) {
	var existingDoc any
	if exists {
		if err := json.Unmarshal(existing, &existingDoc); err != nil {
			exists = false
		}
	}
	inFlat := flattenJSONDoc(*incoming)
	var exFlat map[string]any
	if exists {
		exFlat = flattenJSONDoc(existingDoc)
	}

	changed := map[string]bool{}
	for path, val := range inFlat {
		if !isCredentialField(fieldBaseName(path)) {
			continue
		}
		s, isString := val.(string)
		switch {
		case isString && s != "":
			exVal, present := exFlat[path]
			if present && exVal == s {
				continue // unchanged round-trip value
			}
			changed[path] = true
		case isString:
			changed[path] = true // explicit "" = explicit clear
		default:
			changed[path] = true // non-string value: gate it (upstream validates the type)
		}
	}

	backfilled := false
	for path, val := range exFlat {
		if !isCredentialField(fieldBaseName(path)) {
			continue
		}
		if _, present := inFlat[path]; present {
			continue
		}
		if s, ok := val.(string); ok && s != "" {
			setJSONPath(incoming, path, s)
			backfilled = true
		}
	}

	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, backfilled
}

func isCredentialField(name string) bool {
	return channelCredentialKeys[strings.ToLower(name)]
}

// fieldBaseName extracts the leaf field name from a flat path
// ("a.b[0].c" → "c", "a.bot_token" → "bot_token").
func fieldBaseName(path string) string {
	seg := path
	if i := strings.LastIndex(seg, "."); i >= 0 {
		seg = seg[i+1:]
	}
	if j := strings.LastIndex(seg, "["); j >= 0 {
		seg = seg[:j]
	}
	return seg
}

// flattenJSONDoc maps a JSON document to "dot.path" → leaf value
// (list indices rendered as "[i]"). Empty maps/objects contribute no
// leaves — credential fields are scalar leaves in the channel models.
func flattenJSONDoc(v any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, val any)
	walk = func(prefix string, val any) {
		switch t := val.(type) {
		case map[string]any:
			for k, e := range t {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, e)
			}
		case []any:
			for i, e := range t {
				walk(fmt.Sprintf("%s[%d]", prefix, i), e)
			}
		default:
			out[prefix] = val
		}
	}
	walk("", v)
	return out
}

// setJSONPath assigns value at the dot.path location inside the document,
// creating intermediate objects as needed (back-fill only targets paths
// that exist in the saved config, whose shape the incoming document may
// have partially omitted). Paths that traverse list positions the incoming
// document lacks are left untouched rather than invented.
func setJSONPath(doc *any, path string, value any) {
	type step struct {
		key   string
		index int
		isIdx bool
	}
	var steps []step
	cur, curIsIdx := "", false
	flush := func() {
		if cur == "" {
			return
		}
		if curIsIdx {
			steps = append(steps, step{index: atoiOrNeg(cur), isIdx: true})
		} else {
			steps = append(steps, step{key: cur})
		}
		cur, curIsIdx = "", false
	}
	for _, r := range path {
		switch {
		case r == '.':
			flush()
		case r == '[':
			flush()
			curIsIdx = true
		case r == ']':
			flush()
		default:
			cur += string(r)
		}
	}
	flush()

	node := doc
	for i, st := range steps {
		if st.isIdx {
			list, ok := (*node).([]any)
			if !ok || st.index < 0 || st.index >= len(list) {
				return // cannot back-fill into a list the caller omitted
			}
			node = &list[st.index]
			continue
		}
		m, ok := (*node).(map[string]any)
		if !ok {
			m = map[string]any{}
			*node = m
		}
		if i == len(steps)-1 {
			m[st.key] = value
			return
		}
		next, ok := m[st.key].(map[string]any)
		if !ok {
			// The incoming document omitted (or flattened) an intermediate
			// object the saved config has: recreate it.
			next = map[string]any{}
			m[st.key] = next
		}
		nextAny := any(next)
		node = &nextAny
	}
}

func atoiOrNeg(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
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
