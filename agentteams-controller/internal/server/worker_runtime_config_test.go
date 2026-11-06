package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeNotifier records the loop-change notifications the handler sends.
type fakeNotifier struct {
	domain       string
	sentRoom     string
	sentBody     string
	sentMentions []string
	sentCount    int
}

func (f *fakeNotifier) UserID(localpart string) string {
	return "@" + localpart + ":" + f.domain
}

func (f *fakeNotifier) SendNotification(ctx context.Context, roomID, body string, mentionUserIDs []string) error {
	f.sentRoom, f.sentBody, f.sentMentions = roomID, body, mentionUserIDs
	f.sentCount++
	return nil
}

func newTestRuntimeConfigHandler(t *testing.T, kubeMode string, ts *httptest.Server, fn loopNotifier, objs ...runtime.Object) *RuntimeConfigHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewRuntimeConfigHandler(k8s, "default", kubeMode, "agentteams-worker-", fn)
	if ts != nil {
		h.workerBaseURL = func(string, map[string]string) string { return ts.URL }
	}
	return h
}

// rcWorker builds a Worker CR with an explicit runtime.
func rcWorker(name, runtime string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: name, Runtime: runtime},
	}
}

// rcTeamWithLeader builds a Team whose WorkerMembers include a team_leader
// plus the worker, and which has a TeamRoomID (for notification).
func rcTeamWithLeader(name, leader, worker string) *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			TeamName: name,
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: leader, Role: "team_leader"},
				{Name: worker, Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{TeamRoomID: "!room:" + name + ":matrix"},
	}
}

func rcRequest(method, name, sub string, body []byte) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/workers/"+name+"/"+sub, reader)
	req.SetPathValue("name", name)
	return req
}

func rcAdminCaller(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
}

func rcL2Caller(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"team-a"}})
}

// TestRuntimeConfig_Get_ForwardsVerbatim: admin reads a qwenpaw worker's
// running-config — proxied to the worker's qwenpaw app at the real
// running-config contract path (verified against the pinned 2.0.1 wheel:
// /api/workspace/running-config, NOT /api/runtime-config).
func TestRuntimeConfig_Get_ForwardsVerbatim(t *testing.T) {
	const payload = `{"max_iters":15,"loop":{"enabled":true}}`
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/workspace/running-config" {
		t.Fatalf("upstream path=%q, want /api/workspace/running-config", gotPath)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body not verbatim: got %s want %s", rec.Body.String(), payload)
	}
}

// TestRuntimeConfig_L2PutWhitelistRejected: an L2 caller writing a non-
// whitelisted top-level key is rejected 403 (fail-closed, #1216 pattern).
func TestRuntimeConfig_L2PutWhitelistRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	body := []byte(`{"max_iters":10,"dangerous_field":true}`)
	rec := httptest.NewRecorder()
	h.Handle(rec, rcL2Caller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", body)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 for non-whitelisted L2 field", rec.Code, rec.Body.String())
	}
}

// TestRuntimeConfig_L2PutWhitelistAllowed is the read-merge-write
// regression: an L2 single-tab update (max_iters only) must preserve the
// unrelated persisted settings (loop, llm_retry_enabled) — the upstream
// PUT body is the merged whole object, never the bare partial.
func TestRuntimeConfig_L2PutWhitelistAllowed(t *testing.T) {
	const current = `{"max_iters":100,"loop":{"enabled":true},"llm_retry_enabled":true}`
	var putBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(current))
		case http.MethodPut:
			putBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	body := []byte(`{"max_iters":10}`)
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", body)
	req.Header.Set("If-Match", contentETag([]byte(current)))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcL2Caller(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for whitelisted L2 write", rec.Code, rec.Body.String())
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(putBody, &merged); err != nil {
		t.Fatalf("upstream PUT body not JSON: %v (%s)", err, string(putBody))
	}
	if got := merged["max_iters"]; got != float64(10) {
		t.Fatalf("merged max_iters=%v, want 10 (submitted value wins)", got)
	}
	if merged["loop"] == nil {
		t.Fatalf("merged body dropped unrelated loop config: %s", string(putBody))
	}
	if merged["llm_retry_enabled"] != true {
		t.Fatalf("merged body dropped unrelated llm_retry_enabled: %s", string(putBody))
	}
}

// A native-style response keeps its own field order; the returned ETag
// must remain usable for the next write without an intervening GET.
func TestRuntimeConfig_PutETagSupportsConsecutiveNativeStyleWrites(t *testing.T) {
	current := `{"max_iters":100,"llm_retry_enabled":true}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var submitted map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode submitted config: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Native QwenPaw returns its typed model representation rather
			// than the controller's map serialization of the submitted body.
			current = fmt.Sprintf(`{"max_iters":%d,"llm_retry_enabled":true}`, int(submitted["max_iters"].(float64)))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(current))
	}))
	defer upstream.Close()
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	read := httptest.NewRecorder()
	h.Handle(read, rcAdminCaller(rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)))
	etag := read.Header().Get("ETag")
	for _, value := range []int{101, 102} {
		req := rcAdminCaller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(fmt.Sprintf(`{"max_iters":%d}`, value))))
		req.Header.Set("If-Match", etag)
		write := httptest.NewRecorder()
		h.Handle(write, req)
		if write.Code != http.StatusOK {
			t.Fatalf("consecutive PUT status=%d, want 200", write.Code)
		}
		etag = write.Header().Get("ETag")
		if etag != contentETag(write.Body.Bytes()) {
			t.Fatalf("PUT ETag=%q does not describe the current native response", etag)
		}
	}
}

// TestRuntimeConfig_PutNoOpSkipsWrite: a PUT whose submitted values equal
// the persisted ones performs exactly one upstream call (the GET) and
// returns the current config as 200 — no redundant worker write.
func TestRuntimeConfig_PutNoOpSkipsWrite(t *testing.T) {
	const current = `{"max_iters":100,"loop":{"enabled":true}}`
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(current))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"max_iters":100}`))
	req.Header.Set("If-Match", contentETag([]byte(current)))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 no-op", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 (GET only, no redundant PUT)", calls)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("no-op response not JSON: %v", err)
	}
	if out["max_iters"] != float64(100) {
		t.Fatalf("no-op response max_iters=%v, want current 100", out["max_iters"])
	}
}

// TestRuntimeConfig_PutApprovalLevelRejected: approval_level is owned by
// the #1216 endpoint (PUT /api/v1/workers/{name}/approval); runtime-config
// rejects it for every role (L1 and L2), fail-closed.
func TestRuntimeConfig_PutApprovalLevelRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called for approval_level writes")
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	body := []byte(`{"approval_level":"OFF"}`)

	for name, req := range map[string]*http.Request{
		"L1": rcAdminCaller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", body)),
		"L2": rcL2Caller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", body)),
	} {
		rec := httptest.NewRecorder()
		h.Handle(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", name, rec.Code, rec.Body.String())
		}
		if !containsAll(rec.Body.String(), "/approval") {
			t.Fatalf("%s: body=%s, want pointer to the #1216 /approval endpoint", name, rec.Body.String())
		}
	}
}

// TestRuntimeConfig_RuntimeAware400: a non-qwenpaw worker is rejected 400
// (the running-config model is qwenpaw-specific).
func TestRuntimeConfig_RuntimeAware400(t *testing.T) {
	h := newTestRuntimeConfigHandler(t, "embedded", nil, nil,
		rcWorker("oc-worker", "openclaw"), rcTeamWithLeader("team-a", "team-a-lead", "oc-worker"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "oc-worker", "runtime-config", nil)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for non-qwenpaw worker", rec.Code)
	}
	if !containsAll(rec.Body.String(), "only supported for qwenpaw") {
		t.Fatalf("body=%s, want qwenpaw-specific message", rec.Body.String())
	}
}

// TestRuntimeConfig_CrossTeam404: a team-leader from a different team gets
// 404 (no existence probe, W8).
func TestRuntimeConfig_CrossTeam404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("beta-team", "beta-lead", "daily-carol"))
	req := rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access", rec.Code)
	}
}

// TestRuntimeConfig_KubeMode503.
func TestRuntimeConfig_KubeMode503(t *testing.T) {
	h := newTestRuntimeConfigHandler(t, "k8s", nil, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 in kube mode", rec.Code)
	}
}

// TestRuntimeConfig_UnknownWorker404.
func TestRuntimeConfig_UnknownWorker404(t *testing.T) {
	h := newTestRuntimeConfigHandler(t, "embedded", nil, nil)
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "ghost", "runtime-config", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

// TestRuntimeConfig_LoopChangeNotifiesLeaderAndChanger is the regression
// for the 9/6 decision (aligned with #1206): a SUCCESSFUL custom-loop write
// (2xx) notifies the team room mentioning BOTH the team leader and the
// changer.
func TestRuntimeConfig_LoopChangeNotifiesLeaderAndChanger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"my-loop"}`))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	body := []byte(`{"id":"my-loop"}`)
	req := rcRequest(http.MethodPost, "daily-carol", "loops/custom", body)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("write status=%d body=%s, want 201 passthrough", rec.Code, rec.Body.String())
	}
	if fn.sentCount != 1 {
		t.Fatalf("notification count=%d, want 1", fn.sentCount)
	}
	if fn.sentRoom != "!room:team-a:matrix" {
		t.Fatalf("notified room=%q, want team room", fn.sentRoom)
	}
	if !mentionsContain(fn.sentMentions, "@team-a-lead:matrix.local") {
		t.Fatalf("mentions=%v, missing leader", fn.sentMentions)
	}
	if !mentionsContain(fn.sentMentions, "@alice:matrix.local") {
		t.Fatalf("mentions=%v, missing changer (9/6: @list 含改动者)", fn.sentMentions)
	}
	if !containsAll(fn.sentBody, "daily-carol") || !containsAll(fn.sentBody, "新增") {
		t.Fatalf("notify body=%q, want worker + verb", fn.sentBody)
	}
}

// TestRuntimeConfig_LoopConflictNoNotify: a REJECTED custom-loop write
// (upstream 409 duplicate id) is passed through to the client as 409 (the
// conflict stays actionable) and emits NO notification.
func TestRuntimeConfig_LoopConflictNoNotify(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"Mode ID already exists"}`))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPost, "daily-carol", "loops/custom", []byte(`{"id":"taken"}`))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 passthrough", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), "Mode ID already exists") {
		t.Fatalf("body=%s, want the upstream conflict detail", rec.Body.String())
	}
	if fn.sentCount != 0 {
		t.Fatalf("notification count=%d, want 0 (rejected write must not notify)", fn.sentCount)
	}
}

// TestRuntimeConfig_LoopUpdateNotFound404: the per-loop 404 ("Custom mode
// not found") is actionable and passes through to the client (it must not
// be masked as 502 "API unavailable"); no notification.
func TestRuntimeConfig_LoopUpdateNotFound404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Custom mode not found"}`))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "loops/custom/ghost-loop", []byte(`{"id":"ghost-loop"}`))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 passthrough", rec.Code, rec.Body.String())
	}
	if fn.sentCount != 0 {
		t.Fatalf("notification count=%d, want 0", fn.sentCount)
	}
}

// TestRuntimeConfig_Upstream5xxIs502: an upstream 5xx is a proxy failure
// (502 with the upstream status surfaced) and emits no notification.
func TestRuntimeConfig_Upstream5xxIs502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPost, "daily-carol", "loops/custom", []byte(`{"id":"x"}`))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for upstream 500", rec.Code)
	}
	if !containsAll(rec.Body.String(), "500") {
		t.Fatalf("body=%s, want upstream status surfaced", rec.Body.String())
	}
	if fn.sentCount != 0 {
		t.Fatalf("notification count=%d, want 0", fn.sentCount)
	}
}

// TestRuntimeConfig_Upstream404Unavailable: a route-level 404 (this qwenpaw
// build lacks the endpoint) maps to 502 "API unavailable" — the client must
// see a capability error, not a fake 404.
func TestRuntimeConfig_Upstream404Unavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 for route-level 404", rec.Code)
	}
	if !containsAll(rec.Body.String(), "API unavailable") {
		t.Fatalf("body=%s, want the capability message", rec.Body.String())
	}
}

// TestRuntimeConfig_L2PutEmptyRejected: an empty L2 update is rejected
// (fail-closed; a no-change PUT has no business meaning for a tab save).
func TestRuntimeConfig_L2PutEmptyRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcL2Caller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{}`))))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for empty L2 update", rec.Code)
	}
}

// TestRuntimeConfig_LoopChangeNoMatrixSkipsNotify: with no notifier, the
// write still succeeds (notification is non-fatal).
func TestRuntimeConfig_LoopChangeNoMatrixSkipsNotify(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPost, "daily-carol", "loops/custom", []byte(`{"id":"x"}`))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 even with nil notifier", rec.Code)
	}
}

// mentionsContain reports whether the mention list contains want exactly.
func mentionsContain(mentions []string, want string) bool {
	for _, m := range mentions {
		if m == want {
			return true
		}
	}
	return false
}

// containsAll is a small test helper (avoids importing strings just for
// one check) — true if s contains every substring in subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !bytes.Contains([]byte(s), []byte(sub)) {
			return false
		}
	}
	return true
}

// TestRuntimeConfig_LoopSectionChangeNotifies: a runtime-config PUT that
// changes the loop section notifies the team room (F22 9/6 decision) after
// the successful write.
func TestRuntimeConfig_LoopSectionChangeNotifies(t *testing.T) {
	const current = `{"max_iters":100,"loop":{"enabled":false}}`
	var putCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(current))
		case http.MethodPut:
			putCalls++
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"loop":{"enabled":true}}`))
	req.Header.Set("If-Match", contentETag([]byte(current)))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if putCalls != 1 {
		t.Fatalf("upstream PUT calls=%d, want 1", putCalls)
	}
	if fn.sentCount != 1 {
		t.Fatalf("notification count=%d, want 1 (loop section changed)", fn.sentCount)
	}
	if !containsAll(fn.sentBody, "loop 配置") {
		t.Fatalf("notify body=%q, want the running-config loop scope", fn.sentBody)
	}
}

// TestRuntimeConfig_NonLoopChangeNoNotify: a runtime-config PUT that
// touches only non-loop fields performs the merge write but notifies NOBODY
// (the loop-change alert is loop-scoped, not a generic config alert).
func TestRuntimeConfig_NonLoopChangeNoNotify(t *testing.T) {
	const current = `{"max_iters":100,"loop":{"enabled":false}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(current))
		case http.MethodPut:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"max_iters":50}`))
	req.Header.Set("If-Match", contentETag([]byte(current)))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if fn.sentCount != 0 {
		t.Fatalf("notification count=%d, want 0 (no loop change)", fn.sentCount)
	}
}

// TestRuntimeConfig_LoopNoChangeNoNotify: a runtime-config PUT whose loop
// value equals the persisted one is a no-op (no upstream write, no notify).
func TestRuntimeConfig_LoopNoChangeNoNotify(t *testing.T) {
	const current = `{"max_iters":100,"loop":{"enabled":true}}`
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(current))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"loop":{"enabled":true}}`))
	req.Header.Set("If-Match", contentETag([]byte(current)))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 no-op", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 (GET only)", calls)
	}
	if fn.sentCount != 0 {
		t.Fatalf("notification count=%d, want 0 (no-op)", fn.sentCount)
	}
}

// TestRuntimeConfig_LoopsStatusForwardsSessionQuery: the upstream
// loops/status endpoint answers per session (chat_id / session_id) and
// reports "idle" without them — the proxy must forward those two selectors
// (allowlisted, normalized order).
func TestRuntimeConfig_LoopsStatusForwardsSessionQuery(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"awaiting_user"}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodGet, "daily-carol", "loops/status", nil)
	req.URL.RawQuery = "session_id=abc123&chat_id=chat-1"
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/loops/status" {
		t.Fatalf("upstream path=%q, want /api/loops/status", gotPath)
	}
	if gotQuery != "chat_id=chat-1&session_id=abc123" {
		t.Fatalf("upstream query=%q, want chat_id=chat-1&session_id=abc123", gotQuery)
	}
}

// TestRuntimeConfig_UnknownQueryRejected: query parameters are rejected
// everywhere except GET loops/status, and unknown keys are rejected there
// too (strict whitelist discipline).
func TestRuntimeConfig_UnknownQueryRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))

	req := rcRequest(http.MethodGet, "daily-carol", "loops/status", nil)
	req.URL.RawQuery = "session_id=abc&evil=1"
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key: status=%d, want 400", rec.Code)
	}

	req2 := rcRequest(http.MethodGet, "daily-carol", "loops", nil)
	req2.URL.RawQuery = "session_id=abc"
	rec2 := httptest.NewRecorder()
	h.Handle(rec2, rcAdminCaller(req2))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("non-status route: status=%d, want 400", rec2.Code)
	}
}

// ---------------------------------------------------------------------------
// Optimistic concurrency (MX-7): ETag on GET, If-Match required on PUT,
// 409 (with the current version) on a stale write, exactly-one-wins under
// concurrency, and a successful re-read-and-retry.
// ---------------------------------------------------------------------------

// rcStatefulUpstream serves a mutable running-config: GET returns the
// current bytes; PUT replaces them (mutex-protected). The returned
// accessor snapshots the state for assertions.
func rcStatefulUpstream(t *testing.T, initial string) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	state := []byte(initial)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(state)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			state = b
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	current := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), state...)
	}
	return srv, current
}

// rcConflictBody parses a 409 runtime-config response (code/message/
// etag/current) — the shape a client uses to re-read, merge and retry.
type rcConflictBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	ETag    string          `json:"etag"`
	Current json.RawMessage `json:"current"`
}

// TestRuntimeConfig_GetReturnsETag: a successful GET exposes the content
// ETag that conditional PUTs must present via If-Match.
func TestRuntimeConfig_GetReturnsETag(t *testing.T) {
	const payload = `{"max_iters":15,"loop":{"enabled":true}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-carol", "runtime-config", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != contentETag([]byte(payload)) {
		t.Fatalf("ETag=%q, want the content ETag of the payload", got)
	}
}

// TestRuntimeConfig_PutMissingIfMatch428: a PUT without If-Match is
// rejected before any upstream call (this endpoint never blind-writes).
func TestRuntimeConfig_PutMissingIfMatch428(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"max_iters":50}`))))

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status=%d body=%s, want 428 without If-Match", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), "If-Match") {
		t.Fatalf("body=%s, want the If-Match guidance", rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d, want 0 (no blind write)", calls)
	}
}

// TestRuntimeConfig_PutStaleIfMatch409: a PUT whose If-Match does not
// match the current config is rejected 409 with NO upstream write, and
// the 409 carries the current version (etag + body) for the re-read.
func TestRuntimeConfig_PutStaleIfMatch409(t *testing.T) {
	const v1 = `{"max_iters":100,"loop":{"enabled":true}}`
	upstream, current := rcStatefulUpstream(t, v1)
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(`{"max_iters":50}`))
	req.Header.Set("If-Match", `"stale-etag"`) // quoted, deliberately wrong
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	var cf rcConflictBody
	if err := json.Unmarshal(rec.Body.Bytes(), &cf); err != nil {
		t.Fatalf("409 body not JSON: %v (%s)", err, rec.Body.String())
	}
	if cf.Code != "conflict" {
		t.Fatalf("code=%q, want conflict", cf.Code)
	}
	if cf.ETag != contentETag([]byte(v1)) {
		t.Fatalf("409 etag=%q, want the current version's etag", cf.ETag)
	}
	if !containsAll(cf.Message, "re-read") {
		t.Fatalf("message=%q, want the re-read guidance", cf.Message)
	}
	var cur map[string]interface{}
	if err := json.Unmarshal(cf.Current, &cur); err != nil {
		t.Fatalf("current not JSON: %v", err)
	}
	if cur["loop"] == nil {
		t.Fatalf("current=%s, want the full current config", cf.Current)
	}
	if !bytes.Equal(current(), []byte(v1)) {
		t.Fatalf("upstream state changed on a rejected write: %s", current())
	}
}

// TestRuntimeConfig_ConcurrentPutExactlyOneWins: two concurrent PUTs
// carrying the same If-Match produce exactly one success and one 409 —
// the per-worker lock makes the read-check-merge-write span atomic. The
// 409 carries the new version, and a retry with it succeeds.
func TestRuntimeConfig_ConcurrentPutExactlyOneWins(t *testing.T) {
	const initial = `{"max_iters":100,"loop":{"enabled":true}}`
	initialETag := contentETag([]byte(initial))
	upstream, current := rcStatefulUpstream(t, initial)
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-carol", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-carol"))

	type putResult struct {
		code int
		body []byte
	}
	results := make(chan putResult, 2)
	submissions := []string{`{"max_iters":50}`, `{"max_iters":70}`}
	var wg sync.WaitGroup
	for _, sub := range submissions {
		wg.Add(1)
		go func(sub string) {
			defer wg.Done()
			req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(sub))
			req.Header.Set("If-Match", initialETag)
			rec := httptest.NewRecorder()
			h.Handle(rec, rcAdminCaller(req))
			results <- putResult{rec.Code, rec.Body.Bytes()}
		}(sub)
	}
	wg.Wait()
	close(results)

	var ok, conflict int
	var conflictBody []byte
	for res := range results {
		switch res.code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
			conflictBody = res.body
		default:
			t.Fatalf("unexpected status=%d body=%s, want one 200 and one 409", res.code, res.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("results: ok=%d conflict=%d, want exactly one of each", ok, conflict)
	}

	var cf rcConflictBody
	if err := json.Unmarshal(conflictBody, &cf); err != nil {
		t.Fatalf("409 body not JSON: %v (%s)", err, conflictBody)
	}
	if cf.Code != "conflict" || cf.ETag == "" || len(cf.Current) == 0 {
		t.Fatalf("409 must carry code conflict + etag + current, got %s", conflictBody)
	}
	if !containsAll(cf.Message, "re-read") {
		t.Fatalf("409 message=%q, want the re-read guidance", cf.Message)
	}

	// The losing value is the one NOT present in the 409's current.
	var cur map[string]interface{}
	if err := json.Unmarshal(cf.Current, &cur); err != nil {
		t.Fatalf("409 current not JSON: %v", err)
	}
	loser := float64(50)
	if v, isNum := cur["max_iters"].(float64); isNum && v == 50 {
		loser = 70
	}

	// Re-read-and-retry: the loser's write succeeds against the version
	// the 409 reported.
	retryBody := fmt.Sprintf(`{"max_iters":%d}`, int(loser))
	req := rcRequest(http.MethodPut, "daily-carol", "runtime-config", []byte(retryBody))
	req.Header.Set("If-Match", cf.ETag)
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after re-read: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var final map[string]interface{}
	if err := json.Unmarshal(current(), &final); err != nil {
		t.Fatalf("final state not JSON: %v", err)
	}
	if final["max_iters"] != loser {
		t.Fatalf("final max_iters=%v, want %v (the retried write applied)", final["max_iters"], loser)
	}
}
