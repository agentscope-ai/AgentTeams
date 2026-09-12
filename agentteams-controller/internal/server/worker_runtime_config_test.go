package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeNotifier records the single loop-change notification the handler sends.
type fakeNotifier struct {
	domain     string
	sentRoom   string
	sentBody   string
	sentMentions []string
	sentCount  int
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

// TestRuntimeConfig_Get_ForwardsVerbatim: admin reads a qwenpaw worker's
// running-config — proxied verbatim to the worker's qwenpaw app.
func TestRuntimeConfig_Get_ForwardsVerbatim(t *testing.T) {
	const payload = `{"max_iterations":15,"loop":"auto","memory":{"enabled":true}}`
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-luo", "runtime-config", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/runtime-config" {
		t.Fatalf("upstream path=%q, want /api/runtime-config", gotPath)
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
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	body := []byte(`{"max_iterations":10,"dangerous_field":true}`)
	req := rcRequest(http.MethodPut, "daily-luo", "runtime-config", body)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 for non-whitelisted L2 field", rec.Code, rec.Body.String())
	}
}

// TestRuntimeConfig_L2PutWhitelistAllowed: an L2 caller writing only
// whitelisted keys is proxied (200).
func TestRuntimeConfig_L2PutWhitelistAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := newTestRuntimeConfigHandler(t, "embedded", upstream, nil,
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	body := []byte(`{"max_iterations":10,"loop":"auto"}`)
	req := rcRequest(http.MethodPut, "daily-luo", "runtime-config", body)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for whitelisted L2 write", rec.Code, rec.Body.String())
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
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("beta-team", "beta-lead", "daily-luo"))
	req := rcRequest(http.MethodGet, "daily-luo", "runtime-config", nil)
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
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	rec := httptest.NewRecorder()
	h.Handle(rec, rcAdminCaller(rcRequest(http.MethodGet, "daily-luo", "runtime-config", nil)))
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

// TestRuntimeConfig_LoopChangeNotifiesLeaderAndChanger is the regression for
// the 9/6 decision (aligned with #1206): a custom-loop write notifies the team
// room mentioning BOTH the team leader and the changer (the @list includes
// the person who made the change).
func TestRuntimeConfig_LoopChangeNotifiesLeaderAndChanger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"created":"my-loop"}`))
	}))
	defer upstream.Close()

	fn := &fakeNotifier{domain: "matrix.local"}
	h := newTestRuntimeConfigHandler(t, "embedded", upstream, fn,
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	body := []byte(`{"name":"my-loop","enabled":true}`)
	req := rcRequest(http.MethodPost, "daily-luo", "loops/custom", body)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"team-a"}})
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("write status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if fn.sentCount != 1 {
		t.Fatalf("notification count=%d, want 1", fn.sentCount)
	}
	if fn.sentRoom != "!room:team-a:matrix" {
		t.Fatalf("notified room=%q, want team room", fn.sentRoom)
	}
	// @list must include BOTH the leader and the changer.
	if !mentionsContain(fn.sentMentions, "@team-a-lead:matrix.local") {
		t.Fatalf("mentions=%v, missing leader", fn.sentMentions)
	}
	if !mentionsContain(fn.sentMentions, "@maizong:matrix.local") {
		t.Fatalf("mentions=%v, missing changer (9/6: @list 含改动者)", fn.sentMentions)
	}
	if !containsAll(fn.sentBody, "daily-luo") || !containsAll(fn.sentBody, "新增") {
		t.Fatalf("notify body=%q, want worker + verb", fn.sentBody)
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
		rcWorker("daily-luo", "qwenpaw"), rcTeamWithLeader("team-a", "team-a-lead", "daily-luo"))
	req := rcRequest(http.MethodPost, "daily-luo", "loops/custom", []byte(`{"name":"x"}`))
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
