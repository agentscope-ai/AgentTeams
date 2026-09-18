package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// skillsTestUpstream records the proxied call and replays a canned
// response.
type skillsTestUpstream struct {
	method, path, query, body string
	dialed                    bool
	status                    int
	response                  string
}

func (u *skillsTestUpstream) server(t *testing.T) *httptest.Server {
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

func newTestSkillsHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *WorkerSkillsHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewWorkerSkillsHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

// skillsRequest builds a request with the path values the mux would set
// (name / skill_name), so handler validation — not URL parsing — is what
// gets tested.
func skillsRequest(method, url, body string, kv ...string) *http.Request {
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

const (
	skillsTeam    = "team-a"
	skillsWorker  = "daily-carol"
	skillsHuman   = "carol"
	skillsOtherHR = "other-team-human"
)

func skillsHumanCaller(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: skillsHuman, Teams: []string{skillsTeam}})
}

func skillsCrossTeamHuman(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: skillsOtherHR, Teams: []string{"another-team"}})
}

func skillsTeamLeader(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: skillsTeam})
}

// ---------------------------------------------------------------------------
// Forwarding (read endpoint)
// ---------------------------------------------------------------------------

func TestSkillsGet_ForwardsVerbatim(t *testing.T) {
	const payload = `[{"name":"make_plan","enabled":true,"preload":false},{"name":"nmh","enabled":true,"preload":true}]`
	u := &skillsTestUpstream{status: http.StatusOK, response: payload}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, adminCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodGet || u.path != "/api/skills" {
		t.Fatalf("upstream=%s %s, want GET /api/skills", u.method, u.path)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim:\n got %s\nwant %s", rec.Body.String(), payload)
	}
}

func TestSkillsGet_UnknownQueryRejected(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `[]`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, adminCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills?filter=x", "", "name", skillsWorker)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream dialed despite rejected query")
	}
}

// ---------------------------------------------------------------------------
// PUT preload
// ---------------------------------------------------------------------------

func TestSkillsPutPreload_ForwardsBodyVerbatim(t *testing.T) {
	const putBody = `{"preload":true}`
	const resp = `{"updated":true,"preload":true}`
	u := &skillsTestUpstream{status: http.StatusOK, response: resp}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", putBody, "name", skillsWorker, "skill_name", "nmh")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if u.method != http.MethodPut || u.path != "/api/skills/nmh/preload" {
		t.Fatalf("upstream=%s %s, want PUT /api/skills/nmh/preload", u.method, u.path)
	}
	if u.body != putBody {
		t.Fatalf("body not verbatim: got %s want %s", u.body, putBody)
	}
	if rec.Body.String() != resp {
		t.Fatalf("response not verbatim: got %s", rec.Body.String())
	}
}

func TestSkillsPutPreload_L2HumanOwnTeam(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `{"updated":true,"preload":false}`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.putWorkerSkillPreload(rec, skillsHumanCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", `{"preload":false}`, "name", skillsWorker, "skill_name", "nmh")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !u.dialed || u.path != "/api/skills/nmh/preload" {
		t.Fatalf("upstream=%s %s dialed=%v", u.method, u.path, u.dialed)
	}
}

func TestSkillsPutPreload_BodyValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not-json", `preload=true`},
		{"json-array", `[true]`},
		{"missing-field", `{"enabled":true}`},
		{"non-bool", `{"preload":"yes"}`},
		{"null", `{"preload":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &skillsTestUpstream{status: http.StatusOK, response: `{}`}
			h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
			rec := httptest.NewRecorder()
			h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", tc.body, "name", skillsWorker, "skill_name", "nmh")))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if u.dialed {
				t.Fatal("upstream dialed despite rejected body")
			}
		})
	}
}

func TestSkillsPutPreload_InvalidSkillName(t *testing.T) {
	// Note: "qq..x" is NOT invalid — the charset (same as the manager
	// skill-sync script) allows interior dots; ".." is only dangerous as a
	// full path segment, which the alphanumeric-first rule already
	// prevents.
	invalid := []string{"", ".hidden", "-qq", "_qq", "../qq", "..", "a b", "qq/", "qq\\x"}
	for _, skill := range invalid {
		t.Run("name="+skill, func(t *testing.T) {
			u := &skillsTestUpstream{status: http.StatusOK, response: `{}`}
			h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
			rec := httptest.NewRecorder()
			h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/xxx/preload", `{"preload":true}`, "name", skillsWorker, "skill_name", skill)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("skill %q: status=%d, want 400", skill, rec.Code)
			}
			if u.dialed {
				t.Fatal("upstream dialed despite rejected skill name")
			}
		})
	}
	// Uppercase and dotted names are valid qwenpaw skill names (the charset
	// matches the manager skill-sync script) and must pass validation.
	u := &skillsTestUpstream{status: http.StatusOK, response: `{"updated":true}`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/Make_Plan/preload", `{"preload":true}`, "name", skillsWorker, "skill_name", "Make_Plan")))
	if rec.Code != http.StatusOK || u.path != "/api/skills/Make_Plan/preload" {
		t.Fatalf("status=%d upstream=%s, valid name must forward", rec.Code, u.path)
	}
}

// ---------------------------------------------------------------------------
// Scoping
// ---------------------------------------------------------------------------

func TestSkills_UnknownWorker(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `[]`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, adminCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", "ghost")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream dialed for unknown worker")
	}
}

func TestSkills_KubeMode(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `[]`}
	h := newTestSkillsHandler(t, "kube", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, adminCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream dialed in kube mode")
	}
}

func TestSkills_CrossTeamHuman_Hidden(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `[]`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	for name, call := range map[string]func(rec *httptest.ResponseRecorder){
		"get": func(rec *httptest.ResponseRecorder) {
			h.getWorkerSkills(rec, skillsCrossTeamHuman(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
		},
		"put": func(rec *httptest.ResponseRecorder) {
			h.putWorkerSkillPreload(rec, skillsCrossTeamHuman(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", `{"preload":true}`, "name", skillsWorker, "skill_name", "nmh")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			call(rec)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d, want 404 (W8 anti-probing)", rec.Code)
			}
		})
	}
	if u.dialed {
		t.Fatal("upstream dialed for cross-team caller")
	}
}

func TestSkills_StandaloneWorker_HiddenFromHuman(t *testing.T) {
	// A Worker CR with no owning team: visible to L1, hidden as 404 from
	// scoped callers (standalone hides under the same rule as the channel
	// and workspace-file proxies).
	u := &skillsTestUpstream{status: http.StatusOK, response: `[]`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointWorker(skillsWorker))
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, skillsHumanCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for standalone worker + scoped caller", rec.Code)
	}
	if u.dialed {
		t.Fatal("upstream dialed for standalone worker + scoped caller")
	}
}

func TestSkills_TeamLeader_OwnTeamRead(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusOK, response: `[{"name":"nmh","preload":true}]`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, skillsTeamLeader(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (team leaders keep read access on their own team)", rec.Code, rec.Body.String())
	}
	if u.path != "/api/skills" {
		t.Fatalf("upstream path=%s", u.path)
	}
}

// ---------------------------------------------------------------------------
// Upstream status mapping (incl. the version gate)
// ---------------------------------------------------------------------------

func TestSkillsPutPreload_Upstream404_VersionGate(t *testing.T) {
	// A worker running QwenPaw < 2.2.1 has no preload router: the upstream
	// 404 passes through verbatim (the documented version-gate signal), and
	// an unknown skill on a 2.2.1 worker produces the same shape — the
	// client disambiguates with the worker's reported qwenpaw version.
	const resp = `{"detail":"Skill not found"}`
	u := &skillsTestUpstream{status: http.StatusNotFound, response: resp}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", `{"preload":true}`, "name", skillsWorker, "skill_name", "nmh")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough", rec.Code)
	}
	if rec.Body.String() != resp {
		t.Fatalf("404 body not verbatim: %s", rec.Body.String())
	}
}

func TestSkillsPutPreload_Upstream409_Passthrough(t *testing.T) {
	const resp = `{"detail":{"reason":"conflict"}}`
	u := &skillsTestUpstream{status: http.StatusConflict, response: resp}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.putWorkerSkillPreload(rec, adminCaller(skillsRequest(http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", `{"preload":true}`, "name", skillsWorker, "skill_name", "nmh")))
	if rec.Code != http.StatusConflict || rec.Body.String() != resp {
		t.Fatalf("status=%d body=%s, want 409 verbatim", rec.Code, rec.Body.String())
	}
}

func TestSkills_Upstream5xx_BadGateway(t *testing.T) {
	u := &skillsTestUpstream{status: http.StatusInternalServerError, response: `{"detail":"boom"}`}
	h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
	rec := httptest.NewRecorder()
	h.getWorkerSkills(rec, adminCaller(skillsRequest(http.MethodGet, "/api/v1/workers/placeholder/skills", "", "name", skillsWorker)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "500") {
		t.Fatalf("502 body should carry the upstream status: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Version contract
// ---------------------------------------------------------------------------

// TestSkillsUpstreamContract_PinnedTo221 pins the forwarded upstream paths
// to the real QwenPaw worker skill API contract — expectations written from
// the official 2.2.1 PyPI wheel (app/routers/skills.py), not from this
// handler:
//
//	GET  /api/skills                  skill list; each entry carries
//	                                  "preload": bool (response model, default false)
//	PUT  /api/skills/{name}/preload   body {"preload": bool}; 404 when the
//	                                  skill is unknown; 200 {"updated": true, ...}
//	                                  after persisting the manifest entry and
//	                                  hot-reloading the agent
//
// The preload router is 2.2.1-only: QwenPaw 2.0.1 and 2.2.0 wheels have no
// such route, so a pre-2.2.1 worker answers 404 — the passthrough above is
// the version gate (no probe, no laundering).
func TestSkillsUpstreamContract_PinnedTo221(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		url      string
		body     string
		wantPath string
		wantBody string
	}{
		{"get-list", http.MethodGet, "/api/v1/workers/placeholder/skills", "", "/api/skills", ""},
		{"put-preload", http.MethodPut, "/api/v1/workers/placeholder/skills/nmh/preload", `{"preload":true}`, "/api/skills/nmh/preload", `{"preload":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &skillsTestUpstream{status: http.StatusOK, response: `{}`}
			h := newTestSkillsHandler(t, "embedded", u.server(t), checkpointTeamWithWorkers(skillsTeam, skillsWorker)...)
			rec := httptest.NewRecorder()
			req := adminCaller(skillsRequest(tc.method, tc.url, tc.body, "name", skillsWorker, "skill_name", "nmh"))
			if tc.method == http.MethodGet {
				h.getWorkerSkills(rec, req)
			} else {
				h.putWorkerSkillPreload(rec, req)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if u.method != tc.method || u.path != tc.wantPath {
				t.Fatalf("forwarded %s %s, want %s %s", u.method, u.path, tc.method, tc.wantPath)
			}
			if tc.wantBody != "" && u.body != tc.wantBody {
				t.Fatalf("forwarded body=%s, want %s", u.body, tc.wantBody)
			}
		})
	}
}
