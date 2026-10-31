package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pluginsTestUpstream records the proxied call and replays a canned
// response (one worker's qwenpaw app GET /api/plugins).
type pluginsTestUpstream struct {
	method, path string
	dialed       bool
	status       int
	response     string
}

func (u *pluginsTestUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.dialed = true
		u.method = r.Method
		u.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.response))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newTestWorkerPluginsProxy builds the proxy over a fake K8s client
// (given teams and workers) whose worker URL points at the given httptest
// server (mimicking one worker's qwenpaw app).
func newTestWorkerPluginsProxy(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *workerPluginsProxy {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	p := &workerPluginsProxy{
		rh:              NewResourceHandler(k8s, "default", nil, "", nil),
		kubeMode:        kubeMode,
		containerPrefix: "agentteams-worker-",
		http:            &http.Client{Timeout: workerPluginsProxyTimeout},
	}
	if ts != nil {
		p.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	} else {
		p.workerBaseURL = p.defaultWorkerBaseURL
	}
	return p
}

// workerPluginsRequest builds a request with the path value the mux would
// set (name), so handler validation — not URL parsing — is what gets
// tested.
func workerPluginsRequest(name string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/placeholder/plugins", nil)
	req.SetPathValue("name", name)
	return req
}

// pluginCapPayload returns a well-formed JSON array payload (a single
// entry with a long description) of exactly wantLen bytes.
func pluginCapPayload(wantLen int) string {
	const prefix = `[{"description":"`
	const suffix = `"}]`
	return prefix + strings.Repeat("a", wantLen-len(prefix)-len(suffix)) + suffix
}

// TestWorkerPlugins_MetadataProjection: a reader gets the worker's qwenpaw
// app GET /api/plugins response as the pinned metadata projection — no
// re-wrapping of the array, no raw pass-through (the visible surface is
// whitelisted server-side, #1454). The fixture's keys are all
// whitelisted, so the projection keeps every field (Go marshals map keys
// sorted — not the upstream order).
func TestWorkerPlugins_MetadataProjection(t *testing.T) {
	const payload = `[{"id":"teamharness","name":"TeamHarness","version":"0.3.0","enabled":true,"loaded":true,"plugin_type":"builtin","frontend_entry":"teamharness.py"}]`
	u := &pluginsTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodGet || u.path != "/api/plugins" {
		t.Fatalf("upstream=%s %s, want GET /api/plugins", u.method, u.path)
	}
	const want = `[{"enabled":true,"frontend_entry":"teamharness.py","id":"teamharness","loaded":true,"name":"TeamHarness","plugin_type":"builtin","version":"0.3.0"}]`
	if rec.Body.String() != want {
		t.Fatalf("body:\n got %s\nwant %s", rec.Body.String(), want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
}

// TestWorkerPlugins_BodyOverCap_502: a 200 body one byte over the cap is
// refused 502 — the (cap+1)th byte is read as evidence, a silent
// truncation is never returned as success (#1454).
func TestWorkerPlugins_BodyOverCap_502(t *testing.T) {
	payload := pluginCapPayload(workerPluginsBodyCap + 1)
	if len(payload) != workerPluginsBodyCap+1 {
		t.Fatalf("test payload len=%d, want %d", len(payload), workerPluginsBodyCap+1)
	}
	u := &pluginsTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for a body over the cap", rec.Code)
	}
	if !containsAll(rec.Body.String(), "exceeds 64 KiB") {
		t.Fatalf("body=%s, want the oversize message", rec.Body.String())
	}
}

// TestWorkerPlugins_BodyAtCap_200: a 200 body of exactly the cap (a
// well-formed array) passes whole — the cap is a bound, not a truncate
// point (#1454).
func TestWorkerPlugins_BodyAtCap_200(t *testing.T) {
	payload := pluginCapPayload(workerPluginsBodyCap)
	if len(payload) != workerPluginsBodyCap {
		t.Fatalf("test payload len=%d, want %d", len(payload), workerPluginsBodyCap)
	}
	u := &pluginsTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for a body of exactly the cap", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body truncated or altered:\n got len=%d\nwant len=%d", len(rec.Body.String()), len(payload))
	}
}

// TestWorkerPlugins_Malformed200_502: a 200 body that is not a JSON array
// of plugin objects is refused 502 — never passed through raw, never
// degraded to an empty array (#1454).
func TestWorkerPlugins_Malformed200_502(t *testing.T) {
	for _, body := range []string{
		`{"plugins":[]}`, // object, not an array
		`"not an array"`, // string, not an array
		`[{"id":`,        // broken JSON
		`null`,           // not an array (a worker with no plugins is [])
	} {
		u := &pluginsTestUpstream{status: http.StatusOK, response: body}
		p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("body=%q status=%d, want 502", body, rec.Code)
		}
		if !containsAll(rec.Body.String(), "malformed JSON") {
			t.Fatalf("body=%q response=%s, want the malformed-JSON message", body, rec.Body.String())
		}
		if rec.Body.String() == body {
			t.Fatalf("body=%q was passed through raw", body)
		}
	}
}

// TestWorkerPlugins_UnknownFieldDropped: a field outside the whitelist
// (present now, or added by a future qwenpaw release) never reaches the
// response — neither its key nor its value (#1454).
func TestWorkerPlugins_UnknownFieldDropped(t *testing.T) {
	const payload = `[{"id":"teamharness","name":"TeamHarness","version":"0.3.0","enabled":true,"loaded":true,"plugin_type":"builtin","frontend_entry":"teamharness.py","future_secret":"SENTINEL"}]`
	u := &pluginsTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if containsAll(rec.Body.String(), "future_secret") || containsAll(rec.Body.String(), "SENTINEL") {
		t.Fatalf("response leaked a non-whitelisted field: %s", rec.Body.String())
	}
}

// TestWorkerPlugins_MetadataSchemaPinned: the fixture is the intake shape
// of qwenpaw /api/plugins (all nine metadata fields); the projected key
// set must equal the whitelist EXACTLY — the pinned metadata surface, no
// more, no less (compared sorted, #1454).
func TestWorkerPlugins_MetadataSchemaPinned(t *testing.T) {
	const payload = `[{"id":"teamharness","name":"TeamHarness","version":"0.3.0","description":"Team harness builtin plugin","author":"agentscope","enabled":true,"loaded":true,"plugin_type":"builtin","frontend_entry":"teamharness.py"}]`
	u := &pluginsTestUpstream{status: http.StatusOK, response: payload}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries=%d, want 1", len(got))
	}
	var keys []string
	for k := range got[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"author", "description", "enabled", "frontend_entry", "id", "loaded", "name", "plugin_type", "version"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("projected keys=%v, want exactly the whitelist %v", keys, want)
	}
}

// TestWorkerPlugins_KubeMode503: kube mode answers 503 BEFORE any worker
// lookup or dial (uniform, so worker existence cannot be probed).
func TestWorkerPlugins_KubeMode503(t *testing.T) {
	u := &pluginsTestUpstream{status: http.StatusOK, response: `[]`}
	p := newTestWorkerPluginsProxy(t, "kube", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
	if u.dialed {
		t.Fatal("kube mode must not dial the worker")
	}
}

// TestWorkerPlugins_EntryPoint: the production wiring —
// ResourceHandler.WorkerPlugins honoring the mode argument with no
// test-only injection.
func TestWorkerPlugins_EntryPoint(t *testing.T) {
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).
		WithRuntimeObjects(checkpointTeamWithWorkers("team-a", "daily-carol")...).Build()
	handler := NewResourceHandler(k8s, "default", nil, "", nil).WorkerPlugins("kube", "agentteams-worker-")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, adminCaller(workerPluginsRequest("daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// TestWorkerPlugins_UnknownWorker404: an unknown worker hides as 404 (W8).
func TestWorkerPlugins_UnknownWorker404(t *testing.T) {
	p := newTestWorkerPluginsProxy(t, "embedded", nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("ghost-worker")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

// TestWorkerPlugins_CrossTeam404: an out-of-scope caller (team leader or
// L2 human of another team) sees 404 — worker existence is not probeable
// (W8), and the worker is never dialed.
func TestWorkerPlugins_CrossTeam404(t *testing.T) {
	u := &pluginsTestUpstream{status: http.StatusOK, response: `[]`}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("alpha-team", "daily-carol")...)
	for _, caller := range []*authpkg.CallerIdentity{
		{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta-team"},
		{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"beta-team"}},
	} {
		rec := httptest.NewRecorder()
		p.serve(rec, withCaller(workerPluginsRequest("daily-carol"), caller))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("role=%s status=%d, want 404 for cross-team access (W8)", caller.Role, rec.Code)
		}
	}
	if u.dialed {
		t.Fatal("cross-team read must not dial the worker")
	}
}

// TestWorkerPlugins_L3ReadLeg: an L3 (worker-scoped) human may read
// exactly their assigned workers' plugin lists; any other worker hides as
// 404 (the channels proxy's read boundary).
func TestWorkerPlugins_L3ReadLeg(t *testing.T) {
	u := &pluginsTestUpstream{status: http.StatusOK, response: `[{"id":"teamharness","enabled":true}]`}
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol", "other-worker")...)

	assigned := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"daily-carol"}}
	rec := httptest.NewRecorder()
	p.serve(rec, withCaller(workerPluginsRequest("daily-carol"), assigned))
	if rec.Code != http.StatusOK {
		t.Fatalf("assigned L3 status=%d, want 200", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	p.serve(rec2, withCaller(workerPluginsRequest("other-worker"), assigned))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unassigned L3 status=%d, want 404", rec2.Code)
	}
}

// TestWorkerPlugins_RuntimeAware400: a non-qwenpaw worker is rejected 400
// and the worker upstream is never dialed (the plugin registry is
// qwenpaw-specific).
func TestWorkerPlugins_RuntimeAware400(t *testing.T) {
	u := &pluginsTestUpstream{status: http.StatusOK, response: `[]`}
	w := checkpointWorker("oc-worker")
	w.Spec.Runtime = "openclaw"
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t),
		checkpointTeam("team-a", "oc-worker"), w)
	rec := httptest.NewRecorder()
	p.serve(rec, adminCaller(workerPluginsRequest("oc-worker")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for non-qwenpaw worker", rec.Code)
	}
	if !containsAll(rec.Body.String(), "only supported for qwenpaw") {
		t.Fatalf("body=%s, want qwenpaw-specific message", rec.Body.String())
	}
	if u.dialed {
		t.Fatal("non-qwenpaw worker must not be dialed")
	}
}

// TestWorkerPlugins_ScopeBeforeRuntime_CrossTeam404: an out-of-scope
// caller sees 404 (worker existence hidden) even for a non-qwenpaw
// worker — the runtime 400 must not leak worker type across the team
// boundary.
func TestWorkerPlugins_ScopeBeforeRuntime_CrossTeam404(t *testing.T) {
	u := &pluginsTestUpstream{status: http.StatusOK, response: `[]`}
	w := checkpointWorker("oc-worker")
	w.Spec.Runtime = "openclaw"
	p := newTestWorkerPluginsProxy(t, "embedded", u.server(t),
		checkpointTeam("team-a", "oc-worker"), w)
	rec := httptest.NewRecorder()
	req := withCaller(workerPluginsRequest("oc-worker"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-b"}})
	p.serve(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team caller (W8)", rec.Code)
	}
	if containsAll(rec.Body.String(), "only supported for qwenpaw") {
		t.Fatalf("body=%s, runtime guard must not leak across the scope boundary", rec.Body.String())
	}
}

// TestWorkerPlugins_UpstreamNon200_Uniform502: ANY non-200 upstream
// status is a uniform 502 WITHOUT echoing the upstream body (the body is
// an open upstream namespace — #1454 supersedes the old 400/404/409/422
// pass-through, whose detail echo is gone; a QwenPaw build without the
// route now surfaces as 502, not a pass-through 404).
func TestWorkerPlugins_UpstreamNon200_Uniform502(t *testing.T) {
	for _, upstream := range []int{
		http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusTeapot,
		http.StatusInternalServerError,
	} {
		payload := `{"detail":"SENTINEL upstream body"}`
		u := &pluginsTestUpstream{status: upstream, response: payload}
		p := newTestWorkerPluginsProxy(t, "embedded", u.server(t), checkpointTeamWithWorkers("team-a", "daily-carol")...)
		rec := httptest.NewRecorder()
		p.serve(rec, adminCaller(workerPluginsRequest("daily-carol")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream=%d status=%d, want 502", upstream, rec.Code)
		}
		if !containsAll(rec.Body.String(), "worker plugins API error") {
			t.Fatalf("upstream=%d body=%s, want the uniform 502 message", upstream, rec.Body.String())
		}
		if containsAll(rec.Body.String(), "SENTINEL") {
			t.Fatalf("upstream=%d response echoed the upstream body: %s", upstream, rec.Body.String())
		}
	}
}
