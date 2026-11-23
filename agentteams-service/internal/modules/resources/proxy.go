package resources

// The transparent PUT/DELETE proxy.
//
// R1 keeps the controller as the write authority for update/delete: the
// service forwards the request byte-for-byte to the controller's public API
// and copies the reply back unchanged, so both proxied routes stay
// observationally identical to the controller's own handlers (G1). The
// service still owns res.workers, so it mirrors the outcome:
//
//	PUT    → on 2xx, refresh the row's spec from the response body
//	         (write-through sync) — zero extra controller calls.
//	DELETE → on 204 (deleted) and 404 (already gone) soft-delete the row; a
//	         409 (team member — the caller must use the teams API) leaves the
//	         row untouched.
//
// Mirroring is best-effort on purpose: the controller write already
// happened and the client must see the controller's bytes, so a DB failure
// is logged and never turned into another upstream status (transparent
// proxy constraint). The audit rows (proxied_update / proxied_delete)
// are written by the Store in the same transaction as the row change.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httpapi"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

const (
	// proxyDefaultTimeout bounds one forwarded call when HTTP is nil. It is
	// a bound, not a retry: retries belong to the caller.
	proxyDefaultTimeout = 30 * time.Second
	// proxyMaxResponseBytes caps the upstream body copied back to the
	// client. A larger body answers 502 instead of being truncated, so a
	// client never observes a partial controller response.
	proxyMaxResponseBytes = 4 << 20 // 4 MiB
	// proxySyncAttempts is one write-through attempt plus one retry after an
	// optimistic-lock conflict.
	proxySyncAttempts = 2
	// proxyRequestIDHeader carries the service-side request id to the
	// controller. The kernel middleware generates the id when the client
	// sent none, so httpapi.RequestIDFromContext is the primary source.
	proxyRequestIDHeader = "X-Request-Id"

	// Audit actions for the two proxied writes.
	actionProxiedUpdate = "worker.proxied_update"
	actionProxiedDelete = "worker.proxied_delete"
)

// proxyRequestHeaders are the inbound headers forwarded verbatim to the
// controller: the caller's bearer token keeps authorizing the
// controller call, and the conditional/idempotency headers keep their
// semantics end to end.
var proxyRequestHeaders = []string{"Authorization", "Content-Type", "Accept", "If-Match", "Idempotency-Key"}

// proxyResponseHeaders are the upstream headers copied back to the client
// Everything else (including hop-by-hop headers) is dropped.
var proxyResponseHeaders = []string{"Content-Type", "ETag", "Cache-Control", "Vary"}

// Proxy serves PUT /api/v1/workers/{name} and DELETE
// /api/v1/workers/{name} by proxying them to agentteams-controller and
// mirroring the outcome into res.workers.
type Proxy struct {
	// Store is the res.workers repository the outcome is mirrored into.
	Store Store
	// ControllerURL is the controller's public API root, e.g.
	// http://agentteams-controller:8090. A trailing slash is tolerated.
	ControllerURL string
	// HTTP is the forwarding transport; nil → a 30s-timeout client. A
	// caller-supplied client is copied per request so the proxy can always
	// install its no-redirect policy.
	HTTP *http.Client
	// Log is the proxy logger; nil → slog.Default().
	Log *slog.Logger
	// Now is the proxy clock; nil → time.Now. The Store stamps the row
	// timestamps itself, so this exists for symmetry with the other
	// handlers and for tests that assert time-derived behaviour.
	Now func() time.Time
}

// Register mounts the two proxied routes on mux using Go 1.22 method+path
// patterns. The native GET /api/v1/workers/{name} route is registered by
// Handler; the method-scoped patterns coexist on the same mux.
func (p *Proxy) Register(m *http.ServeMux) {
	m.HandleFunc("PUT /api/v1/workers/{name}", p.handleUpdate)
	m.HandleFunc("DELETE /api/v1/workers/{name}", p.handleDelete)
}

// upstreamResponse is the buffered controller reply. The body is kept as
// bytes so the client receives it unchanged (G1 compares bytes); the status
// and the four pass-through headers are preserved as well.
type upstreamResponse struct {
	status int
	header http.Header
	body   []byte
}

// handleUpdate forwards a PUT and, on 2xx, refreshes the local spec from the
// response body before returning the controller's bytes.
func (p *Proxy) handleUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		// The controller answers the same envelope for a missing name; the
		// route wildcard should make this unreachable.
		httputil.WriteError(w, http.StatusBadRequest, httputil.CodeInvalidName, "worker name is required")
		return
	}

	up, ok := p.forward(w, r)
	if !ok {
		return
	}
	// Any 2xx PUT response is a complete WorkerResponse, which
	// is the sync source. Errors are passed through untouched (the
	// controller's old envelope stays byte-identical).
	if up.status >= 200 && up.status <= 299 {
		p.syncSpec(r, name, up.body)
	}
	writeUpstream(w, up)
}

// handleDelete forwards a DELETE and mirrors the status decision into
// res.workers.
func (p *Proxy) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, httputil.CodeInvalidName, "worker name is required")
		return
	}

	up, ok := p.forward(w, r)
	if !ok {
		return
	}
	if up.status == http.StatusNoContent || up.status == http.StatusNotFound {
		// 204: the controller deleted the worker CR. 404: it no longer
		// knows the worker (deleted out of band) — soft-delete either way so
		// list/get stop offering a ghost.
		p.softDelete(r, name)
	}
	// Any other status (including the 409 for a team member: "remove via the
	// teams API") is passed through with the row untouched.
	writeUpstream(w, up)
}

// forward sends the inbound request to the controller and buffers the
// reply. On a transport failure or an over-limit body it writes the frozen
// 502 envelope and returns ok=false: at that point the controller write did
// not happen, so there is nothing to mirror and no consistency problem.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) (*upstreamResponse, bool) {
	name := r.PathValue("name")
	// Defense-in-depth for embedded handlers; the kernel middleware also
	// caps the body.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.target(name, r.URL.RawQuery), r.Body)
	if err != nil {
		p.logger().Error("proxy: build controller request failed",
			"worker", name, "method", r.Method, "err", err, "request_id", requestID(r))
		writeUpstreamUnavailable(w, "controller unavailable")
		return nil, false
	}
	// Preserve the inbound framing: a body-less request stays empty and a
	// chunked request keeps ContentLength -1 (streamed through, never
	// buffered by the proxy).
	if r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}
	for _, h := range proxyRequestHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if id := requestID(r); id != "" {
		req.Header.Set(proxyRequestIDHeader, id)
	}

	resp, err := p.httpClient().Do(req)
	if err != nil {
		p.logger().Error("proxy: controller unavailable",
			"worker", name, "method", r.Method, "err", err, "request_id", requestID(r))
		writeUpstreamUnavailable(w, "controller unavailable")
		return nil, false
	}
	defer func() {
		// Drain so the connection returns to the pool, bounded so a broken
		// controller cannot stream without a limit.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, proxyMaxResponseBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, proxyMaxResponseBytes+1))
	if err != nil {
		p.logger().Error("proxy: read controller response failed",
			"worker", name, "method", r.Method, "err", err, "request_id", requestID(r))
		writeUpstreamUnavailable(w, "controller unavailable")
		return nil, false
	}
	if len(body) > proxyMaxResponseBytes {
		p.logger().Error("proxy: controller response exceeds the proxy limit",
			"worker", name, "method", r.Method, "limit", proxyMaxResponseBytes, "request_id", requestID(r))
		writeUpstreamUnavailable(w,
			fmt.Sprintf("controller response exceeds %d MiB", proxyMaxResponseBytes>>20))
		return nil, false
	}
	return &upstreamResponse{status: resp.StatusCode, header: resp.Header, body: body}, true
}

// syncSpec applies the write-through sync: the 2xx PUT body is
// decoded once, the current row is read, and its spec is refreshed under
// the version optimistic lock. A conflict re-reads and recomputes once;
// after that the row is left stale and the client still sees the upstream
// response. A missing row means the worker was created out of band, and R1
// deliberately does not materialize rows from a proxy response.
func (p *Proxy) syncSpec(r *http.Request, name string, body []byte) {
	var resp WorkerResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		p.logger().Warn("proxy: decode controller PUT response failed; spec not synced",
			"worker", name, "err", err, "request_id", requestID(r))
		return
	}
	ctx := r.Context()
	audit := proxyAuditMeta(r, actionProxiedUpdate)

	for attempt := 0; attempt < proxySyncAttempts; attempt++ {
		cur, err := p.Store.Get(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				p.logger().Warn("proxy: worker row missing after controller update; skipping write-through sync",
					"worker", name, "request_id", requestID(r))
				return
			}
			p.logger().Error("proxy: read worker for write-through sync failed",
				"worker", name, "err", err, "request_id", requestID(r))
			return
		}
		spec := specWithResponse(cur.Spec(), resp)
		if _, err := p.Store.SyncSpec(ctx, name, spec, cur.Version, audit); err != nil {
			if errors.Is(err, ErrConflict) {
				p.logger().Warn("proxy: write-through sync conflict; recomputing",
					"worker", name, "attempt", attempt+1, "request_id", requestID(r))
				continue
			}
			p.logger().Error("proxy: write-through sync failed",
				"worker", name, "err", err, "request_id", requestID(r))
			return
		}
		return
	}
	p.logger().Warn("proxy: write-through sync still conflicting after retry; worker row left stale",
		"worker", name, "request_id", requestID(r))
}

// specWithResponse overlays the spec fields the controller echoes in its
// PUT response on top of the stored spec.
//
// Everything the response does not carry keeps its row value: team_slug,
// expose and resources are DB-owned (the team API / the request-side fields
// the controller does not echo) and env, remote_skills, package_ref,
// container_managed, desired_state are outside the frozen sync subset. The
// overlay is therefore a field-wise refresh, never a rebuild.
func specWithResponse(cur Spec, resp WorkerResponse) Spec {
	cur.WorkerName = resp.WorkerName
	cur.Model = resp.Model
	cur.ModelProvider = resp.ModelProvider
	cur.SubagentModel = resp.SubagentModel

	cur.PackageRef = resp.Package
	cur.ContainerManaged = resp.ContainerManaged
	cur.DesiredState = resp.State
	// env is echoed only to callers allowed to manage it; nil means "not
	// visible to this caller", never "empty", so a non-admin PUT must not
	// wipe the stored environment.
	if resp.Env != nil {
		cur.Env = resp.Env
	}

	cur.LLMStreamFirstContentTimeout = resp.LlmStreamFirstContentTimeout
	cur.LLMStreamIdleTimeout = resp.LlmStreamIdleTimeout
	cur.LLMMaxRetries = resp.LlmMaxRetries
	cur.LLMBackoffBase = resp.LlmBackoffBase
	cur.LLMBackoffCap = resp.LlmBackoffCap
	cur.LLMMaxConcurrent = resp.LlmMaxConcurrent
	cur.LLMMaxQPM = resp.LlmMaxQpm
	cur.LLMRateLimitPause = resp.LlmRateLimitPause
	cur.LLMRateLimitJitter = resp.LlmRateLimitJitter
	cur.LLMAcquireTimeout = resp.LlmAcquireTimeout

	cur.Runtime = resp.Runtime
	cur.Image = resp.Image
	cur.Identity = resp.Identity

	cur.Soul = resp.Soul
	cur.Agents = resp.Agents

	cur.Skills = resp.Skills
	cur.MCPServers = resp.McpServers
	cur.ChannelPolicy = resp.ChannelPolicy

	return cur
}

// softDelete mirrors a DELETE outcome: 204 and 404 both mean "the controller
// has no live worker with this name". A conflict re-reads and
// retries once; any other failure is logged and swallowed because the
// upstream response is already determined.
func (p *Proxy) softDelete(r *http.Request, name string) {
	ctx := r.Context()
	audit := proxyAuditMeta(r, actionProxiedDelete)

	for attempt := 0; attempt < proxySyncAttempts; attempt++ {
		cur, err := p.Store.Get(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Already gone (or never materialized): nothing to delete.
				return
			}
			p.logger().Error("proxy: read worker for proxied delete failed",
				"worker", name, "err", err, "request_id", requestID(r))
			return
		}
		if err := p.Store.SoftDelete(ctx, name, cur.Version, audit); err != nil {
			if errors.Is(err, ErrConflict) {
				p.logger().Warn("proxy: proxied delete conflict; recomputing",
					"worker", name, "attempt", attempt+1, "request_id", requestID(r))
				continue
			}
			p.logger().Error("proxy: proxied delete failed",
				"worker", name, "err", err, "request_id", requestID(r))
			return
		}
		return
	}
	p.logger().Warn("proxy: proxied delete still conflicting after retry; worker row left live",
		"worker", name, "request_id", requestID(r))
}

// --- transport helpers ---

// target builds ControllerURL + /api/v1/workers/{name} + the original query.
func (p *Proxy) target(name, rawQuery string) string {
	u := strings.TrimRight(p.ControllerURL, "/") + "/api/v1/workers/" + url.PathEscape(name)
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

// httpClient returns the forwarding client with the proxy's no-redirect
// policy. A caller-supplied client is copied (before use) so the policy can
// never be bypassed and the caller's client is left untouched.
func (p *Proxy) httpClient() *http.Client {
	if p.HTTP == nil {
		return &http.Client{Timeout: proxyDefaultTimeout, CheckRedirect: noRedirect}
	}
	c := *p.HTTP
	c.CheckRedirect = noRedirect
	return &c
}

// noRedirect surfaces a controller redirect to the client instead of
// following it: the proxy must not re-target a write.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// writeUpstream copies the controller's status, pass-through headers and
// body bytes to the client without rewriting anything.
func writeUpstream(w http.ResponseWriter, up *upstreamResponse) {
	h := w.Header()
	for _, name := range proxyResponseHeaders {
		for _, v := range up.header.Values(name) {
			h.Add(name, v)
		}
	}
	w.WriteHeader(up.status)
	if len(up.body) == 0 {
		return
	}
	_, _ = w.Write(up.body)
}

// writeUpstreamUnavailable writes the frozen 502 envelope for a controller
// that could not be reached or answered outside the proxy's bounds.
func writeUpstreamUnavailable(w http.ResponseWriter, message string) {
	httputil.WriteError(w, http.StatusBadGateway, httputil.CodeUpstreamUnavailable, message)
}

// --- audit / identity helpers ---

// proxyAuditMeta builds the ops.audit_events context for a proxied write.
// The actor is the token-review identity on the request context; when the
// middleware did not run (or no caller was resolved) the action is
// attributed to "anonymous". token_jti is the token digest, the
// R1 stand-in for the jti the controller does not expose (see
// internal/kernel/identity).
func proxyAuditMeta(r *http.Request, action string) *AuditMeta {
	a := &AuditMeta{
		RequestID: requestID(r),
		Action:    action,
		Client:    r.UserAgent(),
	}
	caller := identity.FromContext(r.Context())
	if caller == nil {
		a.ActorType = "anonymous"
	} else {
		a.ActorType = string(caller.Role)
		a.ActorID = identityActorID(caller)
	}
	if token, ok := bearerToken(r.Header.Get("Authorization")); ok {
		a.TokenJTI = identity.TokenDigest(token)
	}
	return a
}

// identityActorID mirrors the actor-id fallback order used by the native
// handlers (username → matrix user id → worker name).
func identityActorID(caller *identity.Identity) string {
	switch {
	case caller.Username != "":
		return caller.Username
	case caller.MatrixUserID != "":
		return caller.MatrixUserID
	default:
		return caller.WorkerName
	}
}

// bearerToken extracts the opaque token from an Authorization header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// requestID returns the request id the kernel middleware attached (it
// generates one when the client sent none), falling back to the inbound
// header when the proxy runs without the middleware (tests, embedded use).
func requestID(r *http.Request) string {
	if id := httpapi.RequestIDFromContext(r.Context()); id != "" {
		return id
	}
	return httputil.RequestID(r)
}

// logger returns the configured logger or the process default.
func (p *Proxy) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}
