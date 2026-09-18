package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// channelsTestUpstream records the proxied calls and replays a canned
// response. getResponse serves the saved channel config for the PUT
// baseline fetch (GET); when empty it falls back to response.
type channelsTestUpstream struct {
	method, path, query, body string
	getResponse               string
	dialed                    bool
	status                    int
	response                  string
	hits                      []string // "METHOD path" per request, in order
}

func (u *channelsTestUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.dialed = true
		u.method = r.Method
		u.path = r.URL.Path
		u.query = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		u.body = string(b)
		u.hits = append(u.hits, r.Method+" "+r.URL.Path)
		out := u.response
		if r.Method == http.MethodGet && u.getResponse != "" {
			out = u.getResponse
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newTestChannelsHandler(t *testing.T, kubeMode string, ts *httptest.Server, store *ossfake.Memory, objs ...runtime.Object) *ChannelsHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewChannelsHandler(k8s, "default", kubeMode, "agentteams-worker-", nil, nil)
	if store != nil {
		h.oss = store
		// Mirror the production wiring (http.go): the audit client and the
		// baseline read-back share the same storage store.
		h.audit = audit.NewClient(store)
	}
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	h.readbackDelay = time.Millisecond
	return h
}

// readAuditLines reads the durable audit object from the shared ossfake
// store (same key contract as production: audit/<UTC date>.jsonl).
func readAuditLines(t *testing.T, store *ossfake.Memory) []string {
	t.Helper()
	key := "audit/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	data, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("audit object not written: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// channelsRequest builds a request with the path values the mux would set
// (name / sub / channel), so handler validation — not URL parsing — is what
// gets tested.
func channelsRequest(method, url, body string, kv ...string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		req.SetPathValue(kv[i], kv[i+1])
	}
	return req
}

const testAgentJSONKey = "agents/daily-carol/.qwenpaw/workspaces/default/agent.json"

// ---------------------------------------------------------------------------
// Forwarding (read endpoints)
// ---------------------------------------------------------------------------

func TestChannelsGetAll_ForwardsVerbatim(t *testing.T) {
	const payload = `{"qq":{"enabled":true,"isBuiltin":true},"matrix":{"enabled":false,"isBuiltin":true}}`
	u := &channelsTestUpstream{status: http.StatusOK, response: payload}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodGet || u.path != "/api/config/channels" {
		t.Fatalf("upstream=%s %s, want GET /config/channels", u.method, u.path)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestChannelsGetTypesAndSchemas(t *testing.T) {
	for _, sub := range []string{"types", "schemas"} {
		u := &channelsTestUpstream{status: http.StatusOK, response: `{"x":"` + sub + `"}`}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/"+sub, "", "name", "daily-carol", "sub", sub)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", sub, rec.Code, rec.Body.String())
		}
		if u.path != "/api/config/channels/"+sub {
			t.Fatalf("%s: upstream path=%s", sub, u.path)
		}
	}
}

func TestChannelsGetSingleChannel(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true,"app_id":"123"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq", "", "name", "daily-carol", "sub", "qq")))
	if rec.Code != http.StatusOK || u.path != "/api/config/channels/qq" {
		t.Fatalf("status=%d upstream=%s", rec.Code, u.path)
	}
}

// ---------------------------------------------------------------------------
// PUT + MinIO read-back
// ---------------------------------------------------------------------------

func TestChannelsPut_ReadbackPendingThenConverged(t *testing.T) {
	const putBody = `{"enabled":true,"app_id":"1904153419","client_secret":"s3cr3t","markdown_enabled":true}`
	// Upstream response = the persisted channel config (pydantic dump).
	// The saved config carries the previous secret: the PUT is a real
	// credential replacement (audited), not an unchanged round-trip.
	u := &channelsTestUpstream{
		status:      http.StatusOK,
		response:    putBody,
		getResponse: `{"enabled":true,"app_id":"1904153419","client_secret":"old-secret","markdown_enabled":true}`,
	}
	store := ossfake.NewMemory()
	// Field order deliberately differs from putBody: canonicalization must
	// make the comparison order-independent.
	agentJSON := `{"channels":{"qq":{"markdown_enabled":true,"client_secret":"s3cr3t","app_id":"1904153419","enabled":true},"matrix":{"enabled":false}}}`
	if err := store.PutObject(context.Background(), testAgentJSONKey, []byte(agentJSON)); err != nil {
		t.Fatal(err)
	}
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	done := make(chan bool, 1)
	h.onReadback = func(converged bool) { done <- converged }
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", putBody, "name", "daily-carol", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodPut || u.path != "/api/config/channels/qq" || u.body != putBody {
		t.Fatalf("upstream=%s %s body=%s", u.method, u.path, u.body)
	}
	if rec.Body.String() != putBody {
		t.Fatalf("response body not verbatim: %s", rec.Body.String())
	}
	// Async contract: the PUT answers "pending" immediately; convergence is
	// a background outcome, not an in-request one (#1220 §11).
	if got := rec.Header().Get(minioPersistedHeader); got != "pending" {
		t.Fatalf("minio persisted header=%q, want pending", got)
	}
	select {
	case converged := <-done:
		if !converged {
			t.Fatal("background readback reported not converged, want converged")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background readback did not fire within 2s")
	}
	// The admin credential write and the readback outcome are both in the
	// durable audit object (write recorded before the response, readback
	// after the background delay).
	lines := readAuditLines(t, store)
	if len(lines) != 2 {
		t.Fatalf("want 2 audit lines (credential_write + readback), got %d", len(lines))
	}
	var ev struct {
		Who        string `json:"who"`
		Role       string `json:"role"`
		Target     string `json:"target"`
		Action     string `json:"action"`
		Capability string `json:"capability"`
		Detail     string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("line 1 parse: %v", err)
	}
	if ev.Action != "channel_credential_write" || ev.Who != "admin" || ev.Target != "daily-carol/qq" ||
		ev.Capability != "channel_secrets" || !strings.Contains(ev.Detail, "client_secret") {
		t.Fatalf("line 1 = %+v", ev)
	}
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("line 2 parse: %v", err)
	}
	if ev.Action != "channel_readback" || !strings.Contains(ev.Detail, "converged=true") {
		t.Fatalf("line 2 = %+v", ev)
	}
}

func TestChannelsPut_ReadbackNotConvergedWhenBaselineMissing(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	store := ossfake.NewMemory() // empty: push_loop never wrote the baseline
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	done := make(chan bool, 1)
	h.onReadback = func(converged bool) { done <- converged }
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	// Non-credential write: header is still pending (read-back runs), but no
	// credential-write audit line — only the readback outcome is recorded.
	if got := rec.Header().Get(minioPersistedHeader); got != "pending" {
		t.Fatalf("minio persisted header=%q, want pending", got)
	}
	select {
	case converged := <-done:
		if converged {
			t.Fatal("background readback reported converged on an empty store")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background readback did not fire within 2s")
	}
	lines := readAuditLines(t, store)
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line (readback only), got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"action":"channel_readback"`) ||
		!strings.Contains(lines[0], "converged=false") {
		t.Fatalf("readback line = %s", lines[0])
	}
}

// TestChannelsPut_ReadbackPendingWhilePushLoopLags pins the §11 timing
// contract: when the push_loop period is longer than the read-back delay,
// the response still answers "pending" immediately (it never waits for the
// push_loop, unlike the old in-request budget) and the check records
// converged=false — a signal to audit, not a hang.
func TestChannelsPut_ReadbackPendingWhilePushLoopLags(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	inner := ossfake.NewMemory()
	// The baseline materializes 50ms after the PUT — longer than the 1ms
	// injected readbackDelay, i.e. the push_loop has not synced by check
	// time.
	h := newTestChannelsHandler(t, "embedded", u.server(t), inner, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	h.oss = &laggingStore{Memory: inner, openAt: time.Now().Add(50 * time.Millisecond), key: testAgentJSONKey,
		value: `{"channels":{"qq":{"enabled":true}}}`}
	done := make(chan bool, 1)
	h.onReadback = func(converged bool) { done <- converged }
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(minioPersistedHeader); got != "pending" {
		t.Fatalf("minio persisted header=%q, want pending (response must not wait for the push_loop)", got)
	}
	select {
	case converged := <-done:
		if converged {
			t.Fatal("readback converged before the push_loop materialized the baseline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background readback did not fire within 2s")
	}
}

// laggingStore serves one object only after openAt — simulating a
// push_loop whose sync period outlasts the read-back delay.
type laggingStore struct {
	*ossfake.Memory
	openAt time.Time
	key    string
	value  string
}

func (l *laggingStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == l.key {
		if time.Now().Before(l.openAt) {
			return nil, os.ErrNotExist
		}
		return []byte(l.value), nil
	}
	return l.Memory.GetObject(ctx, key)
}

func TestChannelsPut_NoOSS_Skipped(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")))
	if got := rec.Header().Get(minioPersistedHeader); got != "skipped" {
		t.Fatalf("minio persisted header=%q, want skipped", got)
	}
}

func TestChannelsPut_EmptyBody400(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", "", "name", "daily-carol", "channel", "qq")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for an empty body (would wipe the channel)")
	}
}

// ---------------------------------------------------------------------------
// Write contract: credential diff against the saved config
// ---------------------------------------------------------------------------

// TestChannelsPut_UnchangedCredentialIsOrdinaryEdit: a full-config edit
// that carries the round-tripped saved credential value unchanged is an
// ordinary edit — 200 for an L2 caller without channel_secrets, the client
// body forwarded verbatim, and no credential-write audit line.
func TestChannelsPut_UnchangedCredentialIsOrdinaryEdit(t *testing.T) {
	const saved = `{"enabled":true,"app_id":"123","client_secret":"s3cr3t","markdown_enabled":true}`
	u := &channelsTestUpstream{status: http.StatusOK, response: saved, getResponse: saved}
	store := ossfake.NewMemory()
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	done := make(chan bool, 1)
	h.onReadback = func(converged bool) { done <- converged }
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", saved, "name", "daily-carol", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
	h.putChannel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (unchanged credential is an ordinary edit)", rec.Code, rec.Body.String())
	}
	if u.body != saved {
		t.Fatalf("forwarded body=%s, want the client's body verbatim", u.body)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background readback did not fire within 2s")
	}
	for _, line := range readAuditLines(t, store) {
		if strings.Contains(line, `"channel_credential_write"`) {
			t.Fatalf("unchanged credential round-trip must not be audit-logged: %s", line)
		}
	}
}

// TestChannelsPut_OmittedCredentialBackfilled: upstream replaces the whole
// channel (config_class(**body)), so an omitted credential would be erased
// by the model default. The handler back-fills the saved value, so a
// normal edit that does not mention the secret preserves it.
func TestChannelsPut_OmittedCredentialBackfilled(t *testing.T) {
	const saved = `{"enabled":true,"app_id":"123","client_secret":"s3cr3t"}`
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":false,"app_id":"123"}`, "name", "daily-carol", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
	h.putChannel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (omitted credential is preserved, not gated)", rec.Code, rec.Body.String())
	}
	var forwarded map[string]any
	if err := json.Unmarshal([]byte(u.body), &forwarded); err != nil {
		t.Fatalf("forwarded body is not JSON: %s", u.body)
	}
	if forwarded["client_secret"] != "s3cr3t" {
		t.Fatalf("omitted credential not back-filled; forwarded=%s", u.body)
	}
	if forwarded["enabled"] != false || forwarded["app_id"] != "123" {
		t.Fatalf("edited fields lost in back-fill: %s", u.body)
	}
}

// TestChannelsPut_ExplicitClearRequiresCapability: an explicit "" is a
// clear, not a placeholder — the old non-empty-only check let
// {"client_secret": ""} through the gate and silently erased the saved
// secret. Now the clear requires the capability (and is audit-logged).
func TestChannelsPut_ExplicitClearRequiresCapability(t *testing.T) {
	const saved = `{"enabled":true,"client_secret":"s3cr3t"}`
	const clearBody = `{"enabled":true,"client_secret":""}`
	l2 := func(caps ...string) *authpkg.CallerIdentity {
		return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}, Capabilities: caps}
	}

	// Without the capability: 403 naming the field; no write attempted.
	u1 := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
	h1 := newTestChannelsHandler(t, "embedded", u1.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec1 := httptest.NewRecorder()
	req1 := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", clearBody, "name", "daily-carol", "channel", "qq")
	h1.putChannel(rec1, withCaller(req1, l2()))
	if rec1.Code != http.StatusForbidden || !strings.Contains(rec1.Body.String(), "client_secret") {
		t.Fatalf("clear without capability: status=%d body=%s, want 403 naming the field", rec1.Code, rec1.Body.String())
	}
	if len(u1.hits) != 1 || u1.hits[0] != "GET /api/config/channels/qq" {
		t.Fatalf("upstream hits=%v, want only the read-only baseline GET", u1.hits)
	}

	// With the capability: 200 and the clear is audit-logged.
	store2 := ossfake.NewMemory()
	u2 := &channelsTestUpstream{status: http.StatusOK, response: clearBody, getResponse: saved}
	h2 := newTestChannelsHandler(t, "embedded", u2.server(t), store2, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	done := make(chan bool, 1)
	h2.onReadback = func(bool) { done <- false }
	rec2 := httptest.NewRecorder()
	req2 := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", clearBody, "name", "daily-carol", "channel", "qq")
	h2.putChannel(rec2, withCaller(req2, l2(string(authpkg.CapabilityChannelSecrets))))
	if rec2.Code != http.StatusOK {
		t.Fatalf("clear with capability: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background readback did not fire within 2s")
	}
	found := false
	for _, line := range readAuditLines(t, store2) {
		if strings.Contains(line, `"channel_credential_write"`) && strings.Contains(line, "client_secret") {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit clear must be audit-logged as a credential write")
	}
}

// TestChannelsPut_ExtendedCredentialKeysGated: every credential field of
// the qwenpaw 2.2.x channel models is gated — including the fields beyond
// the original denylist (app_token, verification_token, twilio_auth_token,
// livekit_api_secret, http_proxy_auth).
func TestChannelsPut_ExtendedCredentialKeysGated(t *testing.T) {
	for _, field := range []string{"app_token", "verification_token", "twilio_auth_token", "livekit_api_secret", "sip_password", "dashscope_api_key", "livekit_api_key", "http_proxy_auth"} {
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		body := `{"` + field + `":"x"}`
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", body, "name", "daily-carol", "channel", "qq")
		req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
		h.putChannel(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), field) {
			t.Fatalf("%s: status=%d body=%s, want 403 naming the field", field, rec.Code, rec.Body.String())
		}
	}
}

// TestChannelsPut_ProxyAuthCredentialLifecycle: http_proxy_auth is the
// proxy "user:password" pair carried by the qwenpaw 2.2.1 Discord and
// Telegram channel models (config.py). Before it joined the denylist, a
// replacement ("old-user:old-password" → "new-user:new-password") was
// reported as no changed credentials and bypassed the channel_secrets
// gate. It must now go through the same four lifecycle states as every
// other credential field: unchanged → ordinary edit, replacement →
// gated, omitted → back-filled, explicit "" → gated clear.
func TestChannelsPut_ProxyAuthCredentialLifecycle(t *testing.T) {
	const oldAuth = "old-user:old-password"
	const newAuth = "new-user:new-password"
	const saved = `{"enabled":true,"http_proxy_auth":"` + oldAuth + `"}`
	l2 := func(caps ...string) *authpkg.CallerIdentity {
		return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}, Capabilities: caps}
	}

	// Unchanged round-trip: an ordinary edit — no gate, no audit.
	{
		store := ossfake.NewMemory()
		u := &channelsTestUpstream{status: http.StatusOK, response: saved, getResponse: saved}
		h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		done := make(chan bool, 1)
		h.onReadback = func(converged bool) { done <- converged }
		rec := httptest.NewRecorder()
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/discord", saved, "name", "daily-carol", "channel", "discord")
		h.putChannel(rec, withCaller(req, l2()))
		if rec.Code != http.StatusOK {
			t.Fatalf("unchanged: status=%d body=%s, want 200 (unchanged credential is an ordinary edit)", rec.Code, rec.Body.String())
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("background readback did not fire within 2s")
		}
		for _, line := range readAuditLines(t, store) {
			if strings.Contains(line, `"channel_credential_write"`) {
				t.Fatalf("unchanged proxy auth must not be audit-logged: %s", line)
			}
		}
	}

	// Replacement without the capability: 403 naming the field, and the
	// replacement never reaches upstream.
	{
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		body := `{"enabled":true,"http_proxy_auth":"` + newAuth + `"}`
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/discord", body, "name", "daily-carol", "channel", "discord")
		h.putChannel(rec, withCaller(req, l2()))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "http_proxy_auth") {
			t.Fatalf("replacement: status=%d body=%s, want 403 naming the field", rec.Code, rec.Body.String())
		}
		if len(u.hits) != 1 || u.hits[0] != "GET /api/config/channels/discord" {
			t.Fatalf("replacement without capability: upstream hits=%v, want only the read-only baseline GET", u.hits)
		}
	}

	// Omitted: back-filled from the saved value (upstream replaces the
	// whole channel, so omission would otherwise erase the pair).
	{
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/discord", `{"enabled":false}`, "name", "daily-carol", "channel", "discord")
		h.putChannel(rec, withCaller(req, l2()))
		if rec.Code != http.StatusOK {
			t.Fatalf("omitted: status=%d body=%s, want 200 (omitted credential is preserved, not gated)", rec.Code, rec.Body.String())
		}
		var forwarded map[string]any
		if err := json.Unmarshal([]byte(u.body), &forwarded); err != nil {
			t.Fatalf("forwarded body is not JSON: %s", u.body)
		}
		if forwarded["http_proxy_auth"] != oldAuth {
			t.Fatalf("omitted proxy auth not back-filled; forwarded=%s", u.body)
		}
		if forwarded["enabled"] != false {
			t.Fatalf("edited field lost in back-fill: %s", u.body)
		}
	}

	// Explicit "" without the capability: 403 naming the field.
	{
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		clearBody := `{"enabled":true,"http_proxy_auth":""}`
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/discord", clearBody, "name", "daily-carol", "channel", "discord")
		h.putChannel(rec, withCaller(req, l2()))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "http_proxy_auth") {
			t.Fatalf("clear: status=%d body=%s, want 403 naming the field", rec.Code, rec.Body.String())
		}
		if len(u.hits) != 1 || u.hits[0] != "GET /api/config/channels/discord" {
			t.Fatalf("clear without capability: upstream hits=%v, want only the read-only baseline GET", u.hits)
		}
	}

	// Explicit "" with the capability: 200 and the clear is audit-logged.
	{
		store := ossfake.NewMemory()
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`, getResponse: saved}
		h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		done := make(chan bool, 1)
		h.onReadback = func(bool) { done <- false }
		rec := httptest.NewRecorder()
		clearBody := `{"enabled":true,"http_proxy_auth":""}`
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/discord", clearBody, "name", "daily-carol", "channel", "discord")
		h.putChannel(rec, withCaller(req, l2(string(authpkg.CapabilityChannelSecrets))))
		if rec.Code != http.StatusOK {
			t.Fatalf("clear with capability: status=%d body=%s", rec.Code, rec.Body.String())
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("background readback did not fire within 2s")
		}
		found := false
		for _, line := range readAuditLines(t, store) {
			if strings.Contains(line, `"channel_credential_write"`) && strings.Contains(line, "http_proxy_auth") {
				found = true
			}
		}
		if !found {
			t.Fatal("explicit proxy-auth clear must be audit-logged as a credential write")
		}
	}
}

// TestChannelsPut_SupersededWriteNotReportedAsFailure: when a newer PUT
// lands before the older write's background re-check runs, the older
// re-check is skipped — a newer successful write must not be reported as
// a persistence failure of the older (superseded) write.
func TestChannelsPut_SupersededWriteNotReportedAsFailure(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	inner := ossfake.NewMemory()
	h := newTestChannelsHandler(t, "embedded", u.server(t), inner, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// The baseline materializes after the superseded write's re-check but
	// before the newer write's re-check. The key is derived from this
	// test's worker fixture (not the shared constant) so the test is
	// independent of other tests' fixtures.
	h.oss = &laggingStore{Memory: inner, openAt: time.Now().Add(70 * time.Millisecond),
		key:   "agents/daily-carol/.qwenpaw/workspaces/default/agent.json",
		value: `{"channels":{"qq":{"enabled":true}}}`}
	h.readbackDelay = 60 * time.Millisecond
	var mu sync.Mutex
	results := []bool{}
	h.onReadback = func(converged bool) {
		mu.Lock()
		results = append(results, converged)
		mu.Unlock()
	}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", body, "name", "daily-carol", "channel", "qq")
		h.putChannel(rec, adminCaller(req))
		return rec
	}

	// P1: the write that will be superseded.
	if rec := put(`{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("P1 status=%d", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	// P2: the newer write — its expected config is what the baseline
	// converges to.
	if rec := put(`{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("P2 status=%d", rec.Code)
	}

	// Both re-checks have run (60ms after their own PUT).
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if len(results) != 1 || !results[0] {
		t.Fatalf("readback results=%v, want exactly the newer write's converged=true (the superseded re-check must be skipped)", results)
	}
	mu.Unlock()
	for _, line := range readAuditLines(t, inner) {
		if strings.Contains(line, "converged=false") {
			t.Fatalf("superseded write reported as a persistence failure: %s", line)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestChannelsInvalidChannelName400(t *testing.T) {
	for _, bad := range []string{"../qq", "QQ", "a.b", "x/y"} {
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/bad", `{}`, "name", "daily-carol", "channel", bad)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("channel=%q status=%d, want 400", bad, rec.Code)
		}
		if u.dialed {
			t.Fatalf("channel=%q: upstream dialed despite invalid name", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Scope (W8 + real boundary)
// ---------------------------------------------------------------------------

func TestChannelsCrossTeam404(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil,
		append(checkpointTeamWithWorkers("team-a", "daily-carol"),
			checkpointTeamWithWorkers("team-b", "bob-worker")...)...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-b"}})
	h.getChannels(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (W8)", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for a cross-team caller")
	}
}

func TestChannelsL2SameTeamAllowed(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"qq":{"enabled":true}}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
	h.getChannels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for same-team L2 read", rec.Code)
	}
}

// Handler-level: the L2 write boundary in this handler is the team scope +
// the credential split (the middleware's worker-scoped update rule admits
// the same-team L2 write; non-credential fields ride on it by default).
func TestChannelsL2SameTeamPutHandlerAllows(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
	h.putChannel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (handler boundary is team scope)", rec.Code)
	}
}

// TestChannelsPut_CredentialGate covers the write-surface split
// (#1220 §6.4): non-credential channel fields ride on the default L2
// worker-scoped update; a body carrying credential fields requires the
// channel_secrets capability (403 names the offending fields).
func TestChannelsPut_CredentialGate(t *testing.T) {
	const credBody = `{"enabled":true,"client_secret":"s3cr3t"}`
	const plainBody = `{"enabled":true,"app_id":"123"}`

	l2 := func(caps ...string) *authpkg.CallerIdentity {
		return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}, Capabilities: caps}
	}
	put := func(h *ChannelsHandler, u *channelsTestUpstream, body string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", body, "name", "daily-carol", "channel", "qq")
		h.putChannel(rec, withCaller(req, caller))
		return rec
	}

	// 1. L2 without the capability, credential body: 403 naming the
	//    capability and the field. The only upstream call is the read-only
	//    baseline fetch (the gate needs the saved config to know the value
	//    changed); no write is attempted.
	u1 := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h1 := newTestChannelsHandler(t, "embedded", u1.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec1 := put(h1, u1, credBody, l2())
	if rec1.Code != http.StatusForbidden {
		t.Fatalf("no-cap credential PUT status=%d, want 403", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "channel_secrets") || !strings.Contains(rec1.Body.String(), "client_secret") {
		t.Fatalf("403 body=%q, want capability + field named", rec1.Body.String())
	}
	if len(u1.hits) != 1 || u1.hits[0] != "GET /api/config/channels/qq" {
		t.Fatalf("upstream hits=%v, want only the read-only baseline GET", u1.hits)
	}

	// 2. L2 with the capability: 200 + a durable audit line. The saved
	//    config has no secret, so the incoming one is a real write.
	store2 := ossfake.NewMemory()
	u2 := &channelsTestUpstream{status: http.StatusOK, response: credBody, getResponse: `{"enabled":true}`}
	h2 := newTestChannelsHandler(t, "embedded", u2.server(t), store2, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	h2.onReadback = func(bool) {} // drain: baseline absent, outcome irrelevant
	rec2 := put(h2, u2, credBody, l2(string(authpkg.CapabilityChannelSecrets)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("with-cap credential PUT status=%d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
	// The credential_write line is recorded synchronously before the
	// response — no wait needed (a later readback line may or may not be
	// present; the scan below is line-order agnostic).
	lines2 := readAuditLines(t, store2)
	found := false
	for _, line := range lines2 {
		var ev struct {
			Who        string `json:"who"`
			Role       string `json:"role"`
			Action     string `json:"action"`
			Capability string `json:"capability"`
			Detail     string `json:"detail"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Action == "channel_credential_write" {
			found = true
			if ev.Who != "bob" || ev.Role != string(authpkg.RoleHuman) ||
				ev.Capability != "channel_secrets" || !strings.Contains(ev.Detail, "client_secret") {
				t.Fatalf("credential_write line = %+v", ev)
			}
		}
	}
	if !found {
		t.Fatalf("no channel_credential_write audit line in: %v", lines2)
	}

	// 3. L2 without the capability, non-credential body: 200 by default.
	u3 := &channelsTestUpstream{status: http.StatusOK, response: plainBody}
	h3 := newTestChannelsHandler(t, "embedded", u3.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec3 := put(h3, u3, plainBody, l2())
	if rec3.Code != http.StatusOK {
		t.Fatalf("no-cap non-credential PUT status=%d, want 200", rec3.Code)
	}
}

func TestChannelsTeamLeaderMutation403(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// Same-team team leader: the middleware allows ActionUpdate
	// (requireSameTeam), so the handler is the real boundary — read-only.
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "daily-carol", Team: "team-a", WorkerName: "daily-carol"})
	h.putChannel(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for team-leader mutation", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for a team-leader mutation")
	}
	// A credential body from a leader hits the read-only 403, not the
	// capability gate (the gate is L2-only; the message must stay the
	// read-only one).
	recL := httptest.NewRecorder()
	reqL := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"client_secret":"x"}`, "name", "daily-carol", "channel", "qq")
	reqL = withCaller(reqL, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "daily-carol", Team: "team-a", WorkerName: "daily-carol"})
	h.putChannel(recL, reqL)
	if recL.Code != http.StatusForbidden {
		t.Fatalf("leader credential PUT status=%d, want 403", recL.Code)
	}
	if !strings.Contains(recL.Body.String(), "read-only") {
		t.Fatalf("leader credential PUT body=%q, want the read-only message", recL.Body.String())
	}
	// Reads still work for team leaders.
	rec2 := httptest.NewRecorder()
	req2 := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "daily-carol", Team: "team-a", WorkerName: "daily-carol"})
	h.getChannels(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("team-leader read status=%d, want 200", rec2.Code)
	}
}

func TestChannelsKubeMode503(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "kube", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
	if u.dialed {
		t.Fatal("kube mode must not dial the worker")
	}
}

func TestChannelsUnknownWorker404(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "ghost")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for an unknown worker")
	}
}

func TestChannelsStandaloneWorkerHiddenFromHuman(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	// Worker CR without a team membership.
	objs := checkpointTeamWithWorkers("team-a", "daily-carol")
	objs = append(objs, checkpointWorker("solo"))
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, objs...)

	// L2 human: standalone workers resolve to no team → 404.
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "solo")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-a"}})
	h.getChannels(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("standalone worker for L2 human: status=%d, want 404", rec.Code)
	}

	// Admin: unrestricted.
	rec2 := httptest.NewRecorder()
	h.getChannels(rec2, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "solo")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("standalone worker for admin: status=%d, want 200", rec2.Code)
	}
}

// ---------------------------------------------------------------------------
// Upstream status mapping
// ---------------------------------------------------------------------------

func TestChannelsUpstream404Passthrough(t *testing.T) {
	// Version gate / unknown channel: the upstream 404 detail is the
	// contract — passed through verbatim, not laundered into a 502.
	u := &channelsTestUpstream{status: http.StatusNotFound, response: `{"detail":"Channel 'nonexistent' not found"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/nonexistent", "", "name", "daily-carol", "sub", "nonexistent")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough", rec.Code)
	}
	if rec.Body.String() != u.response {
		t.Fatalf("body not verbatim: %s", rec.Body.String())
	}
}

func TestChannelsUpstreamError502(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusInternalServerError, response: `{"detail":"boom"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Mutation + query endpoints
// ---------------------------------------------------------------------------

func TestChannelsRestart_Forwards(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"restarted":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.restartChannel(rec, adminCaller(channelsRequest(http.MethodPost, "/api/v1/workers/placeholder/channels/qq/restart", "", "name", "daily-carol", "channel", "qq")))
	if rec.Code != http.StatusOK || u.method != http.MethodPost || u.path != "/api/config/channels/qq/restart" {
		t.Fatalf("status=%d upstream=%s %s", rec.Code, u.method, u.path)
	}
}

// TestChannelsUpstreamPathsMatchWorkerContract pins the upstream path every
// public route forwards to the real QwenPaw worker config API contract:
// /api/config/channels/..., the path the worker's own client
// (qwenpaw_worker/api.py) and the integration coverage use. The
// expectations below are written from the worker client, not from this
// handler file — if the worker API ever moves, the worker client and this
// table change together, and this test catches proxy drift in both
// directions (missing or extra prefix). The first 9 rows are the
// version-agnostic core contract, identical across the official QwenPaw
// 2.0.1 / 2.2.0 / 2.2.1 releases (verified against the PyPI wheels); the
// conflict-check row is the additive 2.2.x-only route (older builds answer
// with their own 404, passed through verbatim).
func TestChannelsUpstreamPathsMatchWorkerContract(t *testing.T) {
	cases := []struct {
		name     string
		call     func(h *ChannelsHandler, rec *httptest.ResponseRecorder)
		wantPath string
	}{
		{
			"list",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-carol")))
			},
			"/api/config/channels",
		},
		{
			"types",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/types", "", "name", "daily-carol", "sub", "types")))
			},
			"/api/config/channels/types",
		},
		{
			"schemas",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/schemas", "", "name", "daily-carol", "sub", "schemas")))
			},
			"/api/config/channels/schemas",
		},
		{
			"single",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq", "", "name", "daily-carol", "sub", "qq")))
			},
			"/api/config/channels/qq",
		},
		{
			"put",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq",
		},
		{
			"health",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannelHealth(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq/health", "", "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq/health",
		},
		{
			"qrcode",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getChannelQrcode(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq/qrcode", "", "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq/qrcode",
		},
		{
			"qrcode-status",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq/qrcode/status?token=tok", "", "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq/qrcode/status",
		},
		{
			"restart",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.restartChannel(rec, adminCaller(channelsRequest(http.MethodPost, "/api/v1/workers/placeholder/channels/qq/restart", "", "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq/restart",
		},
		{
			// conflict-check is additive to the 9-route core: it is not in
			// the worker client (api.py), so its expectation is written from
			// the official QwenPaw 2.2.x server route (config.py:379) and
			// the console's checkChannelConflict call — both independent of
			// this handler file.
			"conflict-check",
			func(h *ChannelsHandler, rec *httptest.ResponseRecorder) {
				h.checkChannelConflict(rec, adminCaller(channelsRequest(http.MethodPost, "/api/v1/workers/placeholder/channels/qq/conflict-check", `{"app_id":"1"}`, "name", "daily-carol", "channel", "qq")))
			},
			"/api/config/channels/qq/conflict-check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
			h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
			rec := httptest.NewRecorder()
			tc.call(h, rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200", rec.Code)
			}
			if u.path != tc.wantPath {
				t.Fatalf("forwarded upstream path=%q, want worker contract path %q", u.path, tc.wantPath)
			}
		})
	}
}

func TestChannelsQrcodeStatus_QueryWhitelist(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"status":"scanned"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)

	// Missing token → 400, no dial.
	rec := httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status", "", "name", "daily-carol", "channel", "wechat")))
	if rec.Code != http.StatusBadRequest || u.dialed {
		t.Fatalf("missing token: status=%d dialed=%v, want 400/no dial", rec.Code, u.dialed)
	}

	// Unknown parameter → 400, no dial.
	rec = httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status?token=a&evil=1", "", "name", "daily-carol", "channel", "wechat")))
	if rec.Code != http.StatusBadRequest || u.dialed {
		t.Fatalf("evil param: status=%d dialed=%v, want 400/no dial", rec.Code, u.dialed)
	}

	// Valid token → forwarded.
	rec = httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status?token=abc123", "", "name", "daily-carol", "channel", "wechat")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if u.path != "/api/config/channels/wechat/qrcode/status" || u.query != "token=abc123" {
		t.Fatalf("upstream=%s?%s", u.path, u.query)
	}
}
