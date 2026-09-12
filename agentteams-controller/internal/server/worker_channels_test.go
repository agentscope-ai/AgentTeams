package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// channelsTestUpstream records the proxied call and replays a canned
// response.
type channelsTestUpstream struct {
	method, path, query, body string
	dialed                    bool
	status                    int
	response                  string
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.response))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newTestChannelsHandler(t *testing.T, kubeMode string, ts *httptest.Server, store *ossfake.Memory, objs ...runtime.Object) *ChannelsHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewChannelsHandler(k8s, "default", kubeMode, "agentteams-worker-", nil)
	if store != nil {
		h.oss = store
	}
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	h.readbackInterval = time.Millisecond
	return h
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

const testAgentJSONKey = "agents/daily-luo/.qwenpaw/workspaces/default/agent.json"

// ---------------------------------------------------------------------------
// Forwarding (read endpoints)
// ---------------------------------------------------------------------------

func TestChannelsGetAll_ForwardsVerbatim(t *testing.T) {
	const payload = `{"qq":{"enabled":true,"isBuiltin":true},"matrix":{"enabled":false,"isBuiltin":true}}`
	u := &channelsTestUpstream{status: http.StatusOK, response: payload}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodGet || u.path != "/config/channels" {
		t.Fatalf("upstream=%s %s, want GET /config/channels", u.method, u.path)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestChannelsGetTypesAndSchemas(t *testing.T) {
	for _, sub := range []string{"types", "schemas"} {
		u := &channelsTestUpstream{status: http.StatusOK, response: `{"x":"` + sub + `"}`}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
		rec := httptest.NewRecorder()
		h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/"+sub, "", "name", "daily-luo", "sub", sub)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", sub, rec.Code, rec.Body.String())
		}
		if u.path != "/config/channels/"+sub {
			t.Fatalf("%s: upstream path=%s", sub, u.path)
		}
	}
}

func TestChannelsGetSingleChannel(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true,"app_id":"123"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/qq", "", "name", "daily-luo", "sub", "qq")))
	if rec.Code != http.StatusOK || u.path != "/config/channels/qq" {
		t.Fatalf("status=%d upstream=%s", rec.Code, u.path)
	}
}

// ---------------------------------------------------------------------------
// PUT + MinIO read-back
// ---------------------------------------------------------------------------

func TestChannelsPut_ForwardsBodyAndReadbackTrue(t *testing.T) {
	const putBody = `{"enabled":true,"app_id":"1904153419","client_secret":"s3cr3t","markdown_enabled":true}`
	// Upstream response = the persisted channel config (pydantic dump).
	u := &channelsTestUpstream{status: http.StatusOK, response: putBody}
	store := ossfake.NewMemory()
	// Field order deliberately differs from putBody: canonicalization must
	// make the comparison order-independent.
	agentJSON := `{"channels":{"qq":{"markdown_enabled":true,"client_secret":"s3cr3t","app_id":"1904153419","enabled":true},"matrix":{"enabled":false}}}`
	if err := store.PutObject(context.Background(), testAgentJSONKey, []byte(agentJSON)); err != nil {
		t.Fatal(err)
	}
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", putBody, "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodPut || u.path != "/config/channels/qq" || u.body != putBody {
		t.Fatalf("upstream=%s %s body=%s", u.method, u.path, u.body)
	}
	if rec.Body.String() != putBody {
		t.Fatalf("response body not verbatim: %s", rec.Body.String())
	}
	if got := rec.Header().Get(minioPersistedHeader); got != "true" {
		t.Fatalf("minio persisted header=%q, want true", got)
	}
}

func TestChannelsPut_ReadbackConvergesOnSecondAttempt(t *testing.T) {
	const putBody = `{"enabled":true,"app_id":"1"}`
	u := &channelsTestUpstream{status: http.StatusOK, response: putBody}
	store := ossfake.NewMemory()
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	// Simulate push_loop lag: the baseline appears after the first read.
	h.oss = &appearingStore{Memory: store, after: 1,
		key:   testAgentJSONKey,
		value: `{"channels":{"qq":` + putBody + `}}`}
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", putBody, "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(minioPersistedHeader); got != "true" {
		t.Fatalf("minio persisted header=%q, want true (converged on attempt 2)", got)
	}
}

func TestChannelsPut_ReadbackFalseWhenBaselineMissing(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	store := ossfake.NewMemory() // empty: push_loop never wrote the baseline
	h := newTestChannelsHandler(t, "embedded", u.server(t), store, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get(minioPersistedHeader); got != "false" {
		t.Fatalf("minio persisted header=%q, want false", got)
	}
}

func TestChannelsPut_NoOSS_Skipped(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-luo", "channel", "qq")))
	if got := rec.Header().Get(minioPersistedHeader); got != "skipped" {
		t.Fatalf("minio persisted header=%q, want skipped", got)
	}
}

func TestChannelsPut_EmptyBody400(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", "", "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for an empty body (would wipe the channel)")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestChannelsInvalidChannelName400(t *testing.T) {
	for _, bad := range []string{"../qq", "QQ", "a.b", "x/y"} {
		u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
		h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
		rec := httptest.NewRecorder()
		h.putChannel(rec, adminCaller(channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/bad", `{}`, "name", "daily-luo", "channel", bad)))
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
		append(checkpointTeamWithWorkers("team-a", "daily-luo"),
			checkpointTeamWithWorkers("team-b", "sunzong-worker")...)...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"team-b"}})
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
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"team-a"}})
	h.getChannels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for same-team L2 read", rec.Code)
	}
}

// Handler-level: the L2 write boundary in this handler is the team scope;
// the middleware denies L2 worker updates until the worker-scoped write
// policy lands (authorizer), after which this same-team path serves.
func TestChannelsL2SameTeamPutHandlerAllows(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-luo", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"team-a"}})
	h.putChannel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (handler boundary is team scope)", rec.Code)
	}
}

func TestChannelsTeamLeaderMutation403(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"enabled":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	// Same-team team leader: the middleware allows ActionUpdate
	// (requireSameTeam), so the handler is the real boundary — read-only.
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodPut, "/api/v1/workers/placeholder/channels/qq", `{"enabled":true}`, "name", "daily-luo", "channel", "qq")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "daily-luo", Team: "team-a", WorkerName: "daily-luo"})
	h.putChannel(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for team-leader mutation", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream must not be dialed for a team-leader mutation")
	}
	// Reads still work for team leaders.
	rec2 := httptest.NewRecorder()
	req2 := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "daily-luo", Team: "team-a", WorkerName: "daily-luo"})
	h.getChannels(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("team-leader read status=%d, want 200", rec2.Code)
	}
}

func TestChannelsKubeMode503(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "kube", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
	if u.dialed {
		t.Fatal("kube mode must not dial the worker")
	}
}

func TestChannelsUnknownWorker404(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
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
	objs := checkpointTeamWithWorkers("team-a", "daily-luo")
	objs = append(objs, checkpointWorker("solo"))
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, objs...)

	// L2 human: standalone workers resolve to no team → 404.
	rec := httptest.NewRecorder()
	req := channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "solo")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"team-a"}})
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
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.getChannelResource(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/nonexistent", "", "name", "daily-luo", "sub", "nonexistent")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough", rec.Code)
	}
	if rec.Body.String() != u.response {
		t.Fatalf("body not verbatim: %s", rec.Body.String())
	}
}

func TestChannelsUpstreamError502(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusInternalServerError, response: `{"detail":"boom"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.getChannels(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels", "", "name", "daily-luo")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Mutation + query endpoints
// ---------------------------------------------------------------------------

func TestChannelsRestart_Forwards(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"restarted":true}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.restartChannel(rec, adminCaller(channelsRequest(http.MethodPost, "/api/v1/workers/placeholder/channels/qq/restart", "", "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusOK || u.method != http.MethodPost || u.path != "/config/channels/qq/restart" {
		t.Fatalf("status=%d upstream=%s %s", rec.Code, u.method, u.path)
	}
}

func TestChannelsConflictCheck_ForwardsBody(t *testing.T) {
	const body = `{"enabled":true,"app_id":"1904153419"}`
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"conflict":false,"agents":[]}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)
	rec := httptest.NewRecorder()
	h.checkChannelConflict(rec, adminCaller(channelsRequest(http.MethodPost, "/api/v1/workers/placeholder/channels/qq/conflict-check", body, "name", "daily-luo", "channel", "qq")))
	if rec.Code != http.StatusOK || u.method != http.MethodPost || u.path != "/config/channels/qq/conflict-check" || u.body != body {
		t.Fatalf("status=%d upstream=%s %s body=%s", rec.Code, u.method, u.path, u.body)
	}
}

func TestChannelsQrcodeStatus_QueryWhitelist(t *testing.T) {
	u := &channelsTestUpstream{status: http.StatusOK, response: `{"status":"scanned"}`}
	h := newTestChannelsHandler(t, "embedded", u.server(t), nil, checkpointTeamWithWorkers("team-a", "daily-luo")...)

	// Missing token → 400, no dial.
	rec := httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status", "", "name", "daily-luo", "channel", "wechat")))
	if rec.Code != http.StatusBadRequest || u.dialed {
		t.Fatalf("missing token: status=%d dialed=%v, want 400/no dial", rec.Code, u.dialed)
	}

	// Unknown parameter → 400, no dial.
	rec = httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status?token=a&evil=1", "", "name", "daily-luo", "channel", "wechat")))
	if rec.Code != http.StatusBadRequest || u.dialed {
		t.Fatalf("evil param: status=%d dialed=%v, want 400/no dial", rec.Code, u.dialed)
	}

	// Valid token → forwarded.
	rec = httptest.NewRecorder()
	h.getQrcodeStatus(rec, adminCaller(channelsRequest(http.MethodGet, "/api/v1/workers/placeholder/channels/wechat/qrcode/status?token=abc123", "", "name", "daily-luo", "channel", "wechat")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if u.path != "/config/channels/wechat/qrcode/status" || u.query != "token=abc123" {
		t.Fatalf("upstream=%s?%s", u.path, u.query)
	}
}

// appearingStore wraps ossfake.Memory and materializes one object only after
// N GetObject misses — simulating push_loop lag between the qwenpaw write
// and the MinIO baseline.
type appearingStore struct {
	*ossfake.Memory
	after int
	seen  int
	key   string
	value string
}

func (a *appearingStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	if key == a.key {
		a.seen++
		if a.seen <= a.after {
			return nil, os.ErrNotExist
		}
		return []byte(a.value), nil
	}
	return a.Memory.GetObject(ctx, key)
}
