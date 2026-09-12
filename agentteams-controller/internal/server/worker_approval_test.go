package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestApprovalHandler builds an ApprovalHandler against a fake API and an
// optional upstream worker app (nil = the default dialer, which fails
// closed for the 502 test).
func newTestApprovalHandler(t *testing.T, kubeMode string, ts *httptest.Server, objs ...runtime.Object) *ApprovalHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewApprovalHandler(k8s, "default", kubeMode, "agentteams-worker-")
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

// approvalTeam / approvalWorker / approvalTeamWithWorkers build the CR
// fixtures (same shape as the checkpoint test fixtures, distinct names to
// keep the two suites independent).
func approvalTeam(name string, workers ...string) *v1beta1.Team {
	refs := make([]v1beta1.TeamWorkerRef, 0, len(workers))
	for _, w := range workers {
		refs = append(refs, v1beta1.TeamWorkerRef{Name: w})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: name, WorkerMembers: refs},
	}
}

func approvalWorker(name string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: name},
	}
}

func approvalTeamWithWorkers(name string, workers ...string) []runtime.Object {
	objs := make([]runtime.Object, 0, len(workers)+1)
	objs = append(objs, approvalTeam(name, workers...))
	for _, w := range workers {
		objs = append(objs, approvalWorker(w))
	}
	return objs
}

// approvalRequest builds a request with the {name} path value set.
func approvalRequest(method, name, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/workers/"+name+"/approval", reader)
	req.SetPathValue("name", name)
	return req
}

// approvalUpstream simulates the worker's /workspace/running-config: GET
// returns the full config, PUT validates the full-object round trip and
// echoes the updated config.
func approvalUpstream(t *testing.T, current string, gotPUT *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/workspace/running-config") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"approval_level":` + jsonQuote(current) + `,"reme_light_memory_config":{"needs_reindex":false},"daily_memory_dir":"memory"}`))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			*gotPUT = body
			var cfg map[string]any
			if err := json.Unmarshal(body, &cfg); err != nil || cfg["approval_level"] == nil {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"detail":"approval_level missing"}`))
				return
			}
			level, _ := cfg["approval_level"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"approval_level":` + jsonQuote(level) + `,"daily_memory_dir":"memory"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- GET ---

func TestApprovalGet_InScopeL2Human(t *testing.T) {
	up := approvalUpstream(t, "SMART", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodGet, "market-analyst", ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"approval_level":"SMART"}` {
		t.Fatalf("body=%s, want the minimal approval_level response", got)
	}
}

func TestApprovalGet_CrossTeamHidden(t *testing.T) {
	up := approvalUpstream(t, "SMART", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodGet, "market-analyst", ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"biz-team"}})
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team read (W8)", rec.Code)
	}
}

func TestApprovalGet_TeamLeaderInScope(t *testing.T) {
	up := approvalUpstream(t, "STRICT", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodGet, "market-analyst", ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "market-analyst", Team: "market-team"})
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for in-scope leader read", rec.Code)
	}
}

func TestApprovalGet_StandaloneHiddenFromScoped(t *testing.T) {
	up := approvalUpstream(t, "AUTO", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up, approvalWorker("lone-worker"))
	req := withCaller(approvalRequest(http.MethodGet, "lone-worker", ""),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for standalone worker (scoped caller)", rec.Code)
	}
	// Admin still sees it.
	rec2 := httptest.NewRecorder()
	h.getWorkerApproval(rec2, adminCaller(approvalRequest(http.MethodGet, "lone-worker", "")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin status=%d, want 200", rec2.Code)
	}
}

func TestApprovalGet_DefaultLevelWhenMissing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No approval_level key at all in the config object.
		_, _ = w.Write([]byte(`{"daily_memory_dir":"memory"}`))
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := adminCaller(approvalRequest(http.MethodGet, "market-analyst", ""))
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"approval_level":"AUTO"}` {
		t.Fatalf("body=%s, want the AUTO default", got)
	}
}

func TestApprovalGet_VersionGate404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough (pre-2.x version gate)", rec.Code)
	}
}

func TestApprovalGet_Unreachable502(t *testing.T) {
	h := newTestApprovalHandler(t, "embedded", nil,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 when the worker app is unreachable", rec.Code)
	}
}

func TestApprovalGet_KubeModeUnavailable(t *testing.T) {
	h := newTestApprovalHandler(t, "kube", nil,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// --- PUT ---

func TestApprovalPut_InScopeL2Human(t *testing.T) {
	var putBody []byte
	up := approvalUpstream(t, "AUTO", &putBody)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"STRICT"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for in-scope L2 human write", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"approval_level":"STRICT"}` {
		t.Fatalf("body=%s, want the updated level echoed", got)
	}
	// The upstream PUT must carry the FULL config (safe write) with only
	// the approval level changed.
	if len(putBody) == 0 {
		t.Fatal("upstream PUT never called")
	}
	var cfg map[string]any
	if err := json.Unmarshal(putBody, &cfg); err != nil {
		t.Fatalf("upstream PUT body is not JSON: %v", err)
	}
	if cfg["approval_level"] != "STRICT" {
		t.Fatalf("upstream approval_level=%v, want STRICT", cfg["approval_level"])
	}
	if cfg["daily_memory_dir"] != "memory" {
		t.Fatalf("unrelated field lost in the round trip: %v", cfg)
	}
}

func TestApprovalPut_LeaderDenied(t *testing.T) {
	up := approvalUpstream(t, "AUTO", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "market-analyst", Team: "market-team"})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for in-scope leader (read-only)", rec.Code)
	}
}

func TestApprovalPut_CrossTeamHidden(t *testing.T) {
	up := approvalUpstream(t, "AUTO", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"biz-team"}})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team write (W8)", rec.Code)
	}
}

func TestApprovalPut_AdminAllowed(t *testing.T) {
	var putBody []byte
	up := approvalUpstream(t, "AUTO", &putBody)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"SMART"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for admin", rec.Code)
	}
	if len(putBody) == 0 {
		t.Fatal("upstream PUT never called")
	}
}

func TestApprovalPut_ManagerAllowed(t *testing.T) {
	// Managers keep the full level range (including OFF), same as
	// admin — guards the role boundary documented in the design.
	var putBody []byte
	up := approvalUpstream(t, "AUTO", &putBody)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 for manager (full range incl. OFF)", rec.Code)
	}
	if len(putBody) == 0 {
		t.Fatal("upstream PUT never called")
	}
}

func TestApprovalPut_InvalidLevelRejected(t *testing.T) {
	up := approvalUpstream(t, "AUTO", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	for _, level := range []string{"strict", "YOLO", "", "Auto"} {
		req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":`+jsonQuote(level)+`}`),
			&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
		rec := httptest.NewRecorder()
		h.updateWorkerApproval(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("level=%q status=%d, want 400 (value not in the fixed set)", level, rec.Code)
		}
	}
}

func TestApprovalPut_InvalidBody(t *testing.T) {
	up := approvalUpstream(t, "AUTO", nil)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	for _, body := range []string{`[]`, `{"noLevel":1}`, `not-json`} {
		req := adminCaller(approvalRequest(http.MethodPut, "market-analyst", body))
		rec := httptest.NewRecorder()
		h.updateWorkerApproval(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d, want 400", body, rec.Code)
		}
	}
}

func TestApprovalPut_ConflictPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"approval_level":"AUTO"}`))
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"configuration changed concurrently"}`))
		}
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 passthrough", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "configuration changed concurrently") {
		t.Fatalf("body=%s, want the upstream conflict detail", rec.Body.String())
	}
}

func TestApprovalPut_VersionGate404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 passthrough (pre-2.x version gate)", rec.Code)
	}
}

func TestApprovalPut_KubeModeUnavailable(t *testing.T) {
	h := newTestApprovalHandler(t, "kube", nil,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// approval_level=OFF disables Tool Guard — a security-policy operation
// that default L2 humans must not perform (maintainer review 2026-09-03;
// elevated capability pending the #1220 design). Admin keeps the full
// range.
func TestApprovalPut_OffDeniedForL2Human(t *testing.T) {
	var offPut []byte
	up := approvalUpstream(t, "AUTO", &offPut)
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)

	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for L2 human setting OFF", rec.Code)
	}

	// Admin (any-worker scope) may still set OFF.
	var putBody []byte
	up2 := approvalUpstream(t, "AUTO", &putBody)
	defer up2.Close()
	h2 := newTestApprovalHandler(t, "embedded", up2,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec2 := httptest.NewRecorder()
	h2.updateWorkerApproval(rec2, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"OFF"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin status=%d, want 200 (admin keeps the full level range)", rec2.Code)
	}
	if len(putBody) == 0 {
		t.Fatal("upstream PUT never called for admin OFF")
	}

	// Guarded levels remain L2-writable.
	rec3 := httptest.NewRecorder()
	h2.updateWorkerApproval(rec3, withCaller(
		approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"STRICT"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}}))
	if rec3.Code != http.StatusOK {
		t.Fatalf("L2 guarded-level status=%d, want 200", rec3.Code)
	}
}

// --- upstream failure/shape coverage (bot review 2026-09-03) ---

func TestApprovalGet_Upstream500(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for upstream 500", rec.Code)
	}
}

func TestApprovalGet_NonStringLevel502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"approval_level":3}`))
		}
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for a non-string approval_level", rec.Code)
	}
}

func TestApprovalGet_VersionGateBodyPassthrough(t *testing.T) {
	upBody := `{"detail":"running-config router not available on this QwenPaw version"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(upBody))
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.getWorkerApproval(rec, adminCaller(approvalRequest(http.MethodGet, "market-analyst", "")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 version gate", rec.Code)
	}
	if got := rec.Body.String(); got != upBody {
		t.Fatalf("body=%s, want the upstream 404 body verbatim", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%s, want application/json", ct)
	}
}

// A running-config larger than the old 4 KiB cap must round-trip the FULL
// object through the safe write (fields beyond the cutoff must survive).
func TestApprovalPut_RoundTripLargeConfig(t *testing.T) {
	pad := strings.Repeat("x", 4500) // > old 4096 cap
	cfg := `{"approval_level":"AUTO","large":"` + pad + `","daily_memory_dir":"memory"}`
	var putBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(cfg))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			putBody = body
			var c map[string]any
			if err := json.Unmarshal(body, &c); err != nil || c["approval_level"] == nil {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			_, _ = w.Write([]byte(`{"approval_level":"STRICT"}`))
		}
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	req := withCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"STRICT"}`),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var c map[string]any
	if err := json.Unmarshal(putBody, &c); err != nil {
		t.Fatalf("upstream PUT body is not JSON: %v", err)
	}
	if got, _ := c["large"].(string); got != pad {
		t.Fatalf("large field truncated or lost in the round trip (len=%d, want %d)", len(got), len(pad))
	}
	if c["approval_level"] != "STRICT" {
		t.Fatalf("approval_level=%v, want STRICT", c["approval_level"])
	}
}

// A JSON null running-config must be rejected, not written back as {}
// (which would wipe the worker's config) and not panic.
func TestApprovalPut_NilConfigRejected(t *testing.T) {
	putCalls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`null`))
		case http.MethodPut:
			putCalls++
		}
	}))
	defer up.Close()
	h := newTestApprovalHandler(t, "embedded", up,
		approvalTeamWithWorkers("market-team", "market-analyst")...)
	rec := httptest.NewRecorder()
	h.updateWorkerApproval(rec, adminCaller(approvalRequest(http.MethodPut, "market-analyst", `{"approval_level":"STRICT"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for a null running-config", rec.Code)
	}
	if putCalls != 0 {
		t.Fatalf("upstream PUT called %d times, want 0 (no write-back of an empty object)", putCalls)
	}
}
