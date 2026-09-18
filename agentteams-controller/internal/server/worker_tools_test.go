package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- fixtures / helpers (distinct names keep this suite independent from
// the approval / runtime-config suites) ---

// toolsUpstreamState is the mutable tool table the fake worker app serves.
// Maps use the worker-local snake_case wire names (the proxy must translate).
type toolsUpstreamState struct {
	tools       []map[string]any
	toggleCalls int
	asyncCalls  int
	lastAsync   []byte // raw body of the last async-execution call
	mutateCode  int    // 0 = 200; otherwise forced for BOTH mutation endpoints
	mutateGone  bool   // force the per-tool 404 (TOCTOU simulation)

	// One-shot rendezvous barrier on the GET /api/tools read path: when
	// readBarrierSize > 1, the first readBarrierSize-1 readers block until
	// the last one arrives, then all proceed and the barrier disarms. Used
	// to deterministically overlap the initial reads of concurrent PATCH
	// requests (the maintainer's barrier reproduction).
	readBarrierSize int
	readBarrierOpen chan struct{}
	readArrivals    int
	readBarrierMu   sync.Mutex
}

func newTestToolsHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *ToolsHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewToolsHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

func toolsTeam(name string, workers ...string) *v1beta1.Team {
	refs := make([]v1beta1.TeamWorkerRef, 0, len(workers))
	for _, w := range workers {
		refs = append(refs, v1beta1.TeamWorkerRef{Name: w})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: name, WorkerMembers: refs},
	}
}

func toolsWorker(name string, runtime string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: name, Runtime: runtime},
	}
}

func toolsTeamWithWorkers(name string, workers ...string) []runtime.Object {
	objs := make([]runtime.Object, 0, len(workers)+1)
	objs = append(objs, toolsTeam(name, workers...))
	for _, w := range workers {
		objs = append(objs, toolsWorker(w, "qwenpaw"))
	}
	return objs
}

// defaultToolsTable is the standard fixture: two built-in tools, one of which
// carries config metadata the proxy MUST redact.
func defaultToolsTable() []map[string]any {
	return []map[string]any{
		{
			"name": "execute_shell_command", "enabled": false,
			"description":     "Execute a shell command in the worker workspace",
			"async_execution": false, "icon": "🔧", "requires_config": false,
		},
		{
			"name": "web_search", "enabled": true,
			"description":     "Search the web",
			"async_execution": true, "icon": "🔎", "requires_config": true,
			"config_fields": []any{map[string]any{"name": "api_key", "label": "API Key", "type": "password"}},
			"config_values": map[string]any{"api_key": "sk-secret-value"},
		},
	}
}

func toolsRequest(method, name, tool, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	path := "/api/v1/workers/" + name + "/tools"
	if tool != "" {
		path += "/" + tool
	}
	req := httptest.NewRequest(method, path, reader)
	req.SetPathValue("name", name)
	req.SetPathValue("tool", tool)
	return req
}

// toolsUpstream simulates the worker's /api/tools router over the mutable
// state: GET lists the table; the two mutation endpoints look up the tool
// (404 when missing), honour a forced mutateCode, and return the updated
// single ToolInfo object (as the real endpoints do).
func toolsUpstream(t *testing.T, st *toolsUpstreamState) *httptest.Server {
	t.Helper()
	toolFromPath := func(p string) string {
		rest := strings.TrimPrefix(p, "/api/tools/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			return ""
		}
		return parts[0]
	}
	apply := func(w http.ResponseWriter, r *http.Request, mutate func(map[string]any)) {
		tool := toolFromPath(r.URL.Path)
		if st.mutateCode != 0 {
			w.WriteHeader(st.mutateCode)
			_, _ = w.Write([]byte(`{"detail":"forced upstream status"}`))
			return
		}
		if st.mutateGone {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Tool '` + tool + `' not found"}`))
			return
		}
		var target map[string]any
		for _, m := range st.tools {
			if m["name"] == tool {
				target = m
				break
			}
		}
		if target == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Tool '` + tool + `' not found"}`))
			return
		}
		mutate(target)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(target)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tools":
			if st.readBarrierSize > 1 {
				st.readBarrierMu.Lock()
				st.readArrivals++
				n := st.readArrivals
				st.readBarrierMu.Unlock()
				if n < st.readBarrierSize {
					<-st.readBarrierOpen
				} else if n == st.readBarrierSize {
					close(st.readBarrierOpen)
				}
				// n > size: barrier disarmed, later reads pass through.
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st.tools)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/toggle"):
			st.toggleCalls++
			apply(w, r, func(m map[string]any) {
				cur, _ := m["enabled"].(bool)
				m["enabled"] = !cur
			})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/async-execution"):
			st.asyncCalls++
			body, _ := io.ReadAll(r.Body)
			st.lastAsync = append([]byte(nil), body...)
			apply(w, r, func(m map[string]any) {
				var p struct {
					Async bool `json:"async_execution"`
				}
				if err := json.Unmarshal(body, &p); err == nil {
					m["async_execution"] = p.Async
				}
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// simpleUpstream serves a single fixed status for every request (route-level
// 404 / upstream 500 / malformed list tests).
func simpleUpstream(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

func luoL2Human() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}}
}

func luoAdmin() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
}

func decodeEntry(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not a JSON object: %s (%v)", body, err)
	}
	return m
}

// --- GET ---

func TestToolsGet_Admin200_RedactedAndRenamed(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var wrapped struct {
		Tools []map[string]any `json:"tools"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &wrapped); err != nil || len(wrapped.Tools) != 2 || wrapped.Total != 2 {
		t.Fatalf("wrapped response wrong: %s", body)
	}
	// config metadata must never cross the boundary.
	for _, frag := range []string{"config_values", "config_fields", "api_key", "sk-secret-value", "async_execution", "requires_config"} {
		if strings.Contains(body, frag) {
			t.Fatalf("response leaks %q: %s", frag, body)
		}
	}
	// the controller-facing names are camelCase.
	if !strings.Contains(body, `"asyncExecution"`) || !strings.Contains(body, `"requiresConfig"`) {
		t.Fatalf("expected camelCase controller fields, got: %s", body)
	}
	// redaction kept the flag itself.
	if wrapped.Tools[1]["requiresConfig"] != true {
		t.Fatalf("requiresConfig flag lost in redaction: %s", body)
	}
}

func TestToolsGet_InScopeL2Human(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoL2Human())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human", rec.Code, rec.Body.String())
	}
}

func TestToolsGet_CrossTeamHidden(t *testing.T) {
	up := toolsUpstream(t, &toolsUpstreamState{tools: defaultToolsTable()})
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up,
		toolsTeamWithWorkers("biz-team", "biz-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "biz-analyst", "", ""), luoL2Human())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (W8 anti-probing)", rec.Code)
	}
}

func TestToolsGet_UnknownWorker(t *testing.T) {
	// No upstream server: a dial attempt would surface as 502, so a 404
	// proves the handler rejected before dialing.
	h := newTestToolsHandler(t, "embedded", nil)
	req := withCaller(toolsRequest(http.MethodGet, "ghost-worker", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestToolsGet_BadWorkerName400(t *testing.T) {
	h := newTestToolsHandler(t, "embedded", nil)
	req := withCaller(toolsRequest(http.MethodGet, "Bad_Name", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestToolsGet_NonQwenpawRuntime400(t *testing.T) {
	up := toolsUpstream(t, &toolsUpstreamState{tools: defaultToolsTable()})
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up,
		toolsTeam("biz-team", "biz-agent"),
		toolsWorker("biz-agent", "hermes"))
	req := withCaller(toolsRequest(http.MethodGet, "biz-agent", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for non-qwenpaw runtime", rec.Code)
	}
}

func TestToolsGet_KubeMode503(t *testing.T) {
	h := newTestToolsHandler(t, "kube", nil, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

func TestToolsGet_RouteLevel404_Becomes502(t *testing.T) {
	up := simpleUpstream(t, http.StatusNotFound, `{"detail":"Not Found"}`)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 for a build without /api/tools", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tools API unavailable") {
		t.Fatalf("want the version-gate message, got: %s", rec.Body.String())
	}
}

func TestToolsGet_Upstream500_Becomes502(t *testing.T) {
	up := simpleUpstream(t, http.StatusInternalServerError, `{"detail":"boom"}`)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
}

func TestToolsGet_MalformedList_FailsClosed(t *testing.T) {
	up := simpleUpstream(t, http.StatusOK, `{"tools":[]}`) // object, not array
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502 (never a partial list)", rec.Code, rec.Body.String())
	}
}

func TestToolsGet_EntryMissingName_FailsClosed(t *testing.T) {
	up := simpleUpstream(t, http.StatusOK, `[{"enabled":true}]`)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for a nameless entry", rec.Code)
	}
}

func TestToolsGet_EmptyList200(t *testing.T) {
	up := simpleUpstream(t, http.StatusOK, `[]`)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodGet, "market-analyst", "", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.listWorkerTools(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("status=%d body=%s, want 200 with an empty list", rec.Code, rec.Body.String())
	}
}

// --- PATCH ---

func TestToolsPatch_EnableInScope(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()} // execute_shell_command starts disabled
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "execute_shell_command", `{"enabled":true}`), luoL2Human())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if st.toggleCalls != 1 || st.asyncCalls != 0 {
		t.Fatalf("dials: toggle=%d async=%d, want 1/0", st.toggleCalls, st.asyncCalls)
	}
	entry := decodeEntry(t, rec.Body.String())
	if entry["enabled"] != true || entry["name"] != "execute_shell_command" {
		t.Fatalf("entry=%v, want the updated entry with enabled=true", entry)
	}
}

func TestToolsPatch_DisableInScope(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()} // web_search starts enabled
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"enabled":false}`), luoL2Human())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusOK || st.toggleCalls != 1 {
		t.Fatalf("status=%d toggles=%d, want 200/1", rec.Code, st.toggleCalls)
	}
	if e := decodeEntry(t, rec.Body.String()); e["enabled"] != false {
		t.Fatalf("entry=%v, want enabled=false", e)
	}
}

func TestToolsPatch_NoOp200ZeroWrites(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()} // execute_shell_command already disabled
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "execute_shell_command", `{"enabled":false}`), luoL2Human())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 no-op", rec.Code, rec.Body.String())
	}
	if st.toggleCalls != 0 || st.asyncCalls != 0 {
		t.Fatalf("a no-op PATCH must not touch the worker: toggle=%d async=%d", st.toggleCalls, st.asyncCalls)
	}
	if e := decodeEntry(t, rec.Body.String()); e["enabled"] != false {
		t.Fatalf("entry=%v, want the current (unchanged) entry", e)
	}
}

func TestToolsPatch_AsyncExecution_SnakeBody(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()} // web_search starts async=true
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"asyncExecution":false}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if st.asyncCalls != 1 || st.toggleCalls != 0 {
		t.Fatalf("dials: toggle=%d async=%d, want 0/1", st.toggleCalls, st.asyncCalls)
	}
	// the worker-local wire name is snake_case — the translation happens in
	// the proxy.
	var sent map[string]any
	if err := json.Unmarshal(st.lastAsync, &sent); err != nil || sent["async_execution"] != false {
		t.Fatalf("upstream body=%s, want {\"async_execution\":false}", st.lastAsync)
	}
	if e := decodeEntry(t, rec.Body.String()); e["asyncExecution"] != false {
		t.Fatalf("entry=%v, want asyncExecution=false", e)
	}
}

func TestToolsPatch_BothFields(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()} // web_search: enabled=true, async=true
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"enabled":false,"asyncExecution":false}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if st.toggleCalls != 1 || st.asyncCalls != 1 {
		t.Fatalf("dials: toggle=%d async=%d, want 1/1", st.toggleCalls, st.asyncCalls)
	}
	e := decodeEntry(t, rec.Body.String())
	if e["enabled"] != false || e["asyncExecution"] != false {
		t.Fatalf("entry=%v, want both flipped", e)
	}
}

func TestToolsPatch_CrossTeamHidden404(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up,
		toolsTeamWithWorkers("biz-team", "biz-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "biz-analyst", "web_search", `{"enabled":false}`), luoL2Human())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (W8 anti-probing)", rec.Code)
	}
	if st.toggleCalls != 0 || st.asyncCalls != 0 {
		t.Fatalf("cross-team PATCH must not touch the worker: toggle=%d async=%d", st.toggleCalls, st.asyncCalls)
	}
}

func TestToolsPatch_UnknownTool404(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "no_such_tool", `{"enabled":true}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for an unknown tool", rec.Code, rec.Body.String())
	}
	if st.toggleCalls != 0 || st.asyncCalls != 0 {
		t.Fatalf("unknown tool must not dial mutations: toggle=%d async=%d", st.toggleCalls, st.asyncCalls)
	}
}

func TestToolsPatch_BadBodies400(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable()}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	cases := []struct {
		name string
		body string
	}{
		{"snake_case field is unknown on the controller surface", `{"async_execution":true}`},
		{"unrelated field rejected fail-closed", `{"enabled":true,"model":"x"}`},
		{"non-object body", `[true]`},
		{"both fields null", `{"enabled":null,"asyncExecution":null}`},
		{"non-boolean value", `{"enabled":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", tc.body), luoAdmin())
			rec := httptest.NewRecorder()
			h.patchWorkerTool(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
		})
	}
	if st.toggleCalls != 0 || st.asyncCalls != 0 {
		t.Fatalf("rejected bodies must not touch the worker: toggle=%d async=%d", st.toggleCalls, st.asyncCalls)
	}
}

func TestToolsPatch_EmptyBody400(t *testing.T) {
	h := newTestToolsHandler(t, "embedded", nil, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", ""), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for an empty body", rec.Code)
	}
}

func TestToolsPatch_ToggleFails_NoSecondCall(t *testing.T) {
	st := &toolsUpstreamState{tools: defaultToolsTable(), mutateCode: http.StatusInternalServerError}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"enabled":false,"asyncExecution":false}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	if st.toggleCalls != 1 || st.asyncCalls != 0 {
		t.Fatalf("a failed toggle must stop the sequence: toggle=%d async=%d", st.toggleCalls, st.asyncCalls)
	}
}

func TestToolsPatch_Toggle404Passthrough(t *testing.T) {
	// The list read succeeds; the tool then vanishes (TOCTOU). The 404 from
	// the mutation endpoint passes through (it is the per-tool 404, not a
	// route-level one — the router exists, the list read proved it).
	st := &toolsUpstreamState{tools: defaultToolsTable(), mutateGone: true}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"enabled":false}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want the passthrough 404", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("want the upstream detail passthrough, got: %s", rec.Body.String())
	}
}

func TestToolsPatch_BadToolName400(t *testing.T) {
	h := newTestToolsHandler(t, "embedded", nil, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "bad/name", `{"enabled":true}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for a non-identifier tool name", rec.Code)
	}
}

func TestToolsPatch_WorkerUnreachable502(t *testing.T) {
	// No upstream server: the default dialer fails -> 502 (both methods).
	h := newTestToolsHandler(t, "embedded", nil, toolsTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(toolsRequest(http.MethodPatch, "market-analyst", "web_search", `{"enabled":false}`), luoAdmin())
	rec := httptest.NewRecorder()
	h.patchWorkerTool(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
}

// TestToolsPatch_ConcurrentSameValueBarrierIsAtomic is the maintainer's
// deterministic reproduction: initial enabled=false, a barrier holds both
// concurrent PATCH {"enabled":true} requests after their initial reads, so
// both decisions see the same stale state. With a non-atomic
// read-then-toggle implementation both requests toggle and the final state
// is false although both callers asked for true. The required contract:
// both requests return 200 and the tool ends ENABLED — which also pins the
// mutation count: exactly one toggle may hit the upstream, because the
// second request must observe the first's write and become a no-op.
func TestToolsPatch_ConcurrentSameValueBarrierIsAtomic(t *testing.T) {
	st := &toolsUpstreamState{
		tools: []map[string]any{
			{"name": "execute_shell_command", "enabled": false,
				"description": "Execute a shell command", "async_execution": false,
				"icon": "🔧", "requires_config": false},
		},
		readBarrierSize: 2,
		readBarrierOpen: make(chan struct{}),
	}
	up := toolsUpstream(t, st)
	defer up.Close()
	h := newTestToolsHandler(t, "embedded", up, toolsTeamWithWorkers("market-team", "market-analyst")...)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := withCaller(
				toolsRequest(http.MethodPatch, "market-analyst", "execute_shell_command", `{"enabled":true}`),
				luoL2Human(),
			)
			rec := httptest.NewRecorder()
			h.patchWorkerTool(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent request %d status=%d, want 200", i, code)
		}
	}
	if st.tools[0]["enabled"] != true {
		t.Fatalf("final enabled=%v, want true: two concurrent set-true must leave the tool enabled", st.tools[0]["enabled"])
	}
	if st.toggleCalls != 1 {
		t.Fatalf("toggleCalls=%d, want 1: the second request must re-read under the write lock, see the first's write and no-op", st.toggleCalls)
	}
}
