package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newLoopTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1beta1 scheme: %v", err)
	}
	return scheme
}

func newLoopTestHandler(t *testing.T, objects ...client.Object) *LoopHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(objects))
	for _, o := range objects {
		objs = append(objs, o)
	}
	c := fake.NewClientBuilder().WithScheme(newLoopTestScheme(t)).WithRuntimeObjects(objs...).Build()
	return &LoopHandler{Client: c, Namespace: "default"}
}

func newLoopTestHandlerWithOSS(t *testing.T, store *ossfake.Memory, objects ...client.Object) *LoopHandler {
	t.Helper()
	h := newLoopTestHandler(t, objects...)
	h.OSS = store
	return h
}

func testLoop(name, teamName, taskID string) *v1beta1.Loop {
	return &v1beta1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.LoopSpec{TeamName: teamName, TaskID: taskID},
	}
}

func loopRequest(t *testing.T, id, verb, body string, caller *authpkg.CallerIdentity, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+id+"/"+verb, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+id+"/"+verb, nil)
	}
	req.SetPathValue("name", id)
	if caller != nil {
		req = withCaller(req, caller)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// seedLoopProject writes the two-hop business state a gated loop relies
// on: task meta (task_id + project_id) and project meta (with the loop
// record when loopMeta != nil, without it otherwise).
func seedLoopProject(t *testing.T, store *ossfake.Memory, team, projectID, taskID string, loopMeta map[string]any) {
	t.Helper()
	taskMeta := map[string]any{"task_id": taskID, "project_id": projectID}
	if team != "" {
		putProject(store, "teams/"+team+"/shared/tasks/"+taskID+"/meta.json", taskMeta)
	} else {
		putProject(store, "shared/tasks/"+taskID+"/meta.json", taskMeta)
	}
	project := map[string]any{"project_id": projectID}
	if team != "" {
		project["team_id"] = team
	}
	if loopMeta != nil {
		project["loop"] = loopMeta
	}
	if team != "" {
		putProject(store, "teams/"+team+"/shared/projects/"+projectID+"/meta.json", project)
	} else {
		putProject(store, "shared/projects/"+projectID+"/meta.json", project)
	}
}

func readProjectMeta(t *testing.T, store *ossfake.Memory, team, projectID string) map[string]any {
	t.Helper()
	key := "shared/projects/" + projectID + "/meta.json"
	if team != "" {
		key = "teams/" + team + "/shared/projects/" + projectID + "/meta.json"
	}
	raw, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("read project meta %s: %v", key, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode project meta %s: %v", key, err)
	}
	return meta
}

func metaPauseSources(t *testing.T, meta map[string]any) []string {
	t.Helper()
	sources := []string{}
	if loop, ok := meta["loop"].(map[string]any); ok && loop != nil {
		if raw, ok := loop["pause_sources"].([]any); ok {
			for _, item := range raw {
				if value, ok := item.(string); ok {
					sources = append(sources, value)
				}
			}
		}
	}
	return sources
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

func getLoop(t *testing.T, h *LoopHandler, id string) *v1beta1.Loop {
	t.Helper()
	loop := &v1beta1.Loop{}
	if err := h.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: id}, loop); err != nil {
		t.Fatalf("get loop: %v", err)
	}
	return loop
}

// failingOSS wraps a storage client so every GetObject fails — a
// simulated storage outage mid-request.
type failingOSS struct {
	oss.StorageClient
}

func (failingOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	return nil, errors.New("oss unavailable")
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Group 1: pause writes the business state (project meta) and leaves the
// Loop CRD completely untouched.
func TestPauseLoop_WritesProjectMetaNotLoopCRD(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))

	rec := loopRequest(t, "loop-1", "pause", `{"source":"user"}`,
		&authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["requested"] != true {
		t.Fatalf("requested=%v want true", resp["requested"])
	}
	if resp["paused"] != true {
		t.Fatalf("paused=%v want true", resp["paused"])
	}
	rawSources, ok := resp["sources"].([]any)
	if !ok || len(rawSources) != 1 || rawSources[0] != "user" {
		t.Fatalf("sources=%v want [user]", resp["sources"])
	}

	// The CRD must be byte-for-byte untouched.
	loop := getLoop(t, h, "loop-1")
	if loop.Spec.Paused {
		t.Fatal("spec.paused must not be written by the new handler")
	}
	if len(loop.Annotations) != 0 {
		t.Fatalf("annotations must not be written: %v", loop.Annotations)
	}

	// The business state carries the source.
	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"user"}) {
		t.Fatalf("meta pause_sources=%v want [user]", got)
	}
}

// Group 2: overlapping pauses do not erase each other — releasing "user"
// while "project" remains returns 200 (not the old 409) with the loop
// still paused.
func TestResumeLoop_RemainingSourceKeepsPause(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{"project", "user"}})
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))

	rec := loopRequest(t, "loop-1", "resume", `{"source":"user"}`,
		&authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, h.ResumeLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (a partial release is a success, not 409) body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["paused"] != true {
		t.Fatalf("paused=%v want true (project source still active)", resp["paused"])
	}
	if resp["requested"] != true {
		t.Fatalf("requested=%v want true", resp["requested"])
	}
	rawSources, ok := resp["sources"].([]any)
	if !ok || len(rawSources) != 1 || rawSources[0] != "project" {
		t.Fatalf("sources=%v want [project]", resp["sources"])
	}
	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"project"}) {
		t.Fatalf("meta pause_sources=%v want [project]", got)
	}
}

// Group 3: releasing the user's veto is admin-only; everyone else gets 403
// and the meta is untouched.
func TestResumeLoop_UserSourceRequiresAdmin(t *testing.T) {
	mk := func() (*LoopHandler, *ossfake.Memory) {
		store := ossfake.NewMemory()
		seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{"user"}})
		return newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01")), store
	}

	denied := []struct {
		name   string
		caller *authpkg.CallerIdentity
	}{
		{"manager", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}},
		{"same-team human", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "human-a", Teams: []string{"alpha-team"}}},
		{"same-team leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "leader-a", Team: "alpha-team"}},
		{"worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w1", Team: "alpha-team"}},
	}
	for _, tc := range denied {
		h, store := mk()
		rec := loopRequest(t, "loop-1", "resume", `{"source":"user"}`, tc.caller, h.ResumeLoop)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s resume status=%d want 403 body=%s", tc.name, rec.Code, rec.Body.String())
		}
		got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
		if !equalStrings(got, []string{"user"}) {
			t.Fatalf("%s: meta mutated despite 403: %v", tc.name, got)
		}
	}

	// A nil caller (no identity in context) fails closed too.
	h, store := mk()
	rec := loopRequest(t, "loop-1", "resume", `{"source":"user"}`, nil, h.ResumeLoop)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nil caller resume status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); !equalStrings(got, []string{"user"}) {
		t.Fatalf("nil caller: meta mutated despite 403: %v", got)
	}

	// Admin releases it: 200, the source is gone, nothing left paused.
	h, store = mk()
	rec = loopRequest(t, "loop-1", "resume", `{"source":"user"}`,
		&authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, h.ResumeLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin resume status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["paused"] != false {
		t.Fatalf("paused=%v want false after the last source is released", resp["paused"])
	}
	if sources, ok := resp["sources"].([]any); !ok || len(sources) != 0 {
		t.Fatalf("sources=%v want []", resp["sources"])
	}
	if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); len(got) != 0 {
		t.Fatalf("meta pause_sources=%v want empty", got)
	}
}

// Group 4: pausing the same source twice is idempotent — the source set
// never grows duplicates.
func TestPauseLoop_Idempotent(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))

	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
	for i := 0; i < 2; i++ {
		rec := loopRequest(t, "loop-1", "pause", `{"source":"user"}`, admin, h.PauseLoop)
		if rec.Code != http.StatusOK {
			t.Fatalf("pause #%d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
		resp := decodeBody(t, rec)
		if resp["paused"] != true {
			t.Fatalf("pause #%d: paused=%v want true", i+1, resp["paused"])
		}
		got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
		if !equalStrings(got, []string{"user"}) {
			t.Fatalf("pause #%d: meta pause_sources=%v want exactly [user]", i+1, got)
		}
	}
}

// Group 5: a loop whose project state has no loop record (or no project
// meta at all) is a legitimate not-paused state: 200 with paused=false and
// a reason — never a claim that the loop is paused.
func TestPauseLoop_NoLoopRecordInMeta(t *testing.T) {
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	// 5a: project meta exists but carries no "loop" key.
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", nil)
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))

	rec := loopRequest(t, "loop-1", "pause", `{"source":"user"}`, admin, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("5a status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["paused"] != false {
		t.Fatalf("5a: paused=%v want false (no loop record to gate)", resp["paused"])
	}
	if reason, _ := resp["reason"].(string); !strings.Contains(reason, "no loop record") {
		t.Fatalf("5a: reason=%q must explain why nothing was gated", reason)
	}
	if _, ok := readProjectMeta(t, store, "alpha-team", "p1")["loop"]; ok {
		t.Fatal("5a: a loop record must not be invented by a pause")
	}

	// 5b: only the task meta exists; the project meta is missing entirely.
	store = ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json",
		map[string]any{"task_id": "t1", "project_id": "p2"})
	h = newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "t1"))

	rec = loopRequest(t, "loop-1", "pause", `{"source":"user"}`, admin, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("5b status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp = decodeBody(t, rec)
	if resp["paused"] != false {
		t.Fatalf("5b: paused=%v want false (no project meta to gate)", resp["paused"])
	}
	if reason, _ := resp["reason"].(string); !strings.Contains(reason, "p2") {
		t.Fatalf("5b: reason=%q must name the unresolved project", reason)
	}
}

// Group 6: a storage failure mid-request is an error (409), never a silent
// success.
func TestPauseResume_OSSFailure409(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandler(t, testLoop("loop-1", "alpha-team", "p1-i001-01"))
	h.OSS = failingOSS{store}

	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
	for _, verb := range []string{"pause", "resume"} {
		rec := loopRequest(t, "loop-1", verb, `{"source":"user"}`, admin, h.PauseLoop)
		if verb == "resume" {
			rec = loopRequest(t, "loop-1", verb, `{"source":"user"}`, admin, h.ResumeLoop)
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s with failing OSS status=%d want 409 body=%s", verb, rec.Code, rec.Body.String())
		}
		var errResp struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil || errResp.Message == "" {
			t.Fatalf("%s: 409 body must carry an explicit error: %s", verb, rec.Body.String())
		}
	}
}

// Group 7: an unknown loop is 404 on both endpoints.
func TestPauseResume_MissingLoop404(t *testing.T) {
	h := newLoopTestHandlerWithOSS(t, ossfake.NewMemory())
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRequest(t, "ghost", "pause", "", admin, h.PauseLoop)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pause missing status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	rec = loopRequest(t, "ghost", "resume", "", admin, h.ResumeLoop)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resume missing status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// Group 8: pause keeps the original INT-7b role matrix — worker is denied,
// team scope is enforced at the handler.
func TestPauseLoop_TeamScopeAndWorkerDeny(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))

	// Worker is denied even for pause (execution side has no scheduling veto).
	rec := loopRequest(t, "loop-1", "pause", "",
		&authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w1", Team: "alpha-team"}, h.PauseLoop)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("worker pause status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}

	// Team leader of another team is denied (handler-side team boundary).
	rec = loopRequest(t, "loop-1", "pause", "",
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "leader-b", Team: "beta-team"}, h.PauseLoop)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-team leader pause status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}

	// Same-team leader is allowed and the source lands in the project meta.
	rec = loopRequest(t, "loop-1", "pause", "",
		&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "leader-a", Team: "alpha-team"}, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-team leader pause status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); !equalStrings(got, []string{"user"}) {
		t.Fatalf("same-team leader pause: meta=%v want [user]", got)
	}

	// Same-team human with the project source: additive with the user source.
	rec = loopRequest(t, "loop-1", "pause", `{"source":"project"}`,
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "human-a", Teams: []string{"alpha-team"}}, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-team human pause status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"project", "user"}) {
		t.Fatalf("meta=%v want [project user] (additive)", got)
	}

	// Human of another team is denied.
	h2 := newLoopTestHandlerWithOSS(t, store, testLoop("loop-2", "alpha-team", "p1-i001-01"))
	rec = loopRequest(t, "loop-2", "pause", "",
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "human-b", Teams: []string{"beta-team"}}, h2.PauseLoop)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-team human pause status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
}

// Group 9: the project source on resume keeps the original ruling — admin
// plus an L2 human of the loop's own team; manager/leader/worker/cross-team
// human are denied.
func TestResumeLoop_ProjectSourceRoles(t *testing.T) {
	mk := func() (*LoopHandler, *ossfake.Memory) {
		store := ossfake.NewMemory()
		seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{"project"}})
		return newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01")), store
	}

	denied := []struct {
		name   string
		caller *authpkg.CallerIdentity
	}{
		{"manager", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}},
		{"same-team leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "leader-a", Team: "alpha-team"}},
		{"worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w1", Team: "alpha-team"}},
		{"cross-team human", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "human-b", Teams: []string{"beta-team"}}},
	}
	for _, tc := range denied {
		h, store := mk()
		rec := loopRequest(t, "loop-1", "resume", `{"source":"project"}`, tc.caller, h.ResumeLoop)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s resume (project source) status=%d want 403 body=%s", tc.name, rec.Code, rec.Body.String())
		}
		if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); !equalStrings(got, []string{"project"}) {
			t.Fatalf("%s: meta mutated despite 403: %v", tc.name, got)
		}
	}

	// Same-team L2 human releases the project source.
	h, store := mk()
	rec := loopRequest(t, "loop-1", "resume", `{"source":"project"}`,
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "human-a", Teams: []string{"alpha-team"}}, h.ResumeLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-team human resume (project source) status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	if resp["paused"] != false {
		t.Fatalf("paused=%v want false after the project source is released", resp["paused"])
	}
	if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); len(got) != 0 {
		t.Fatalf("meta pause_sources=%v want empty", got)
	}
}

// Invalid sources are rejected with 400 on both endpoints before any state
// is touched.
func TestPauseResume_InvalidSource400(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandlerWithOSS(t, store, testLoop("loop-1", "alpha-team", "p1-i001-01"))
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRequest(t, "loop-1", "pause", `{"source":"bogus"}`, admin, h.PauseLoop)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pause bogus source status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	rec = loopRequest(t, "loop-1", "resume", `{"source":"bogus"}`, admin, h.ResumeLoop)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("resume bogus source status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1")); len(got) != 0 {
		t.Fatalf("meta mutated despite 400: %v", got)
	}
}

// ---- concurrency (ETag CAS) ----------------------------------------------

// pauseBarrierToken tags one request inside the pauseCASBarrier wrapper;
// only its own request goroutine ever touches it.
type pauseBarrierToken struct {
	reads   int  // GetObject calls on the gated key
	wroteOK bool // this request's conditional write landed
}

type pauseBarrierTokenKey struct{}

// pauseCASBarrier forces the interleaving the unconditional
// read/modify/write lost (#1431): no write may land before BOTH tagged
// requests have finished their CAS read of the gated key, and no response
// re-read may run before both writes have landed — so every response
// deterministically reports the merged durable set. (Each request's FIRST
// GetObject on the key is the resolveLoopProject probe, which must not
// count; the CAS read is the second.)
type pauseCASBarrier struct {
	oss.StorageClient
	key            string
	wantReaders    int
	mu             sync.Mutex
	readers        int
	writes         int
	readReleased   chan struct{}
	writesReleased chan struct{}
}

func newPauseCASBarrier(store oss.StorageClient, key string, wantReaders int) *pauseCASBarrier {
	return &pauseCASBarrier{
		StorageClient:  store,
		key:            key,
		wantReaders:    wantReaders,
		readReleased:   make(chan struct{}),
		writesReleased: make(chan struct{}),
	}
}

func (p *pauseCASBarrier) GetObject(ctx context.Context, key string) ([]byte, error) {
	tok, _ := ctx.Value(pauseBarrierTokenKey{}).(*pauseBarrierToken)
	if tok == nil || key != p.key {
		return p.StorageClient.GetObject(ctx, key)
	}

	// The snapshot is read BEFORE the barrier decision on purpose for the
	// CAS read: writes wait on readReleased, which is only closed after
	// every reader has registered, so this snapshot is guaranteed to be
	// pre-write. The gated response re-read below deliberately reads
	// again AFTER the wait: returning the pre-wait snapshot would make the
	// response observe a stale state and could not be evidence about the
	// merged durable set.
	data, err := p.StorageClient.GetObject(ctx, key)
	if err != nil {
		return data, err
	}
	tok.reads++
	p.mu.Lock()
	gate := false
	lastReader := false
	switch {
	case tok.reads == 1:
		// resolveLoopProject probe: not the gated read.
	case tok.reads == 2:
		p.readers++
		lastReader = p.readers == p.wantReaders
	case tok.reads > 2:
		// A read after this request's own write landed is its response
		// re-read: hold it until both writes have landed. A retry re-read
		// (wroteOK still false) must stay unheld — the retried write has
		// to run.
		gate = tok.wroteOK
	}
	p.mu.Unlock()
	if lastReader {
		close(p.readReleased)
	}
	if gate {
		<-p.writesReleased
		return p.StorageClient.GetObject(ctx, key)
	}
	return data, err
}

func (p *pauseCASBarrier) PutObjectIfMatch(ctx context.Context, key string, data []byte, match string) error {
	if key == p.key {
		<-p.readReleased
	}
	err := p.StorageClient.PutObjectIfMatch(ctx, key, data, match)
	if key == p.key && err == nil {
		if tok, _ := ctx.Value(pauseBarrierTokenKey{}).(*pauseBarrierToken); tok != nil {
			tok.wroteOK = true
		}
		p.mu.Lock()
		p.writes++
		second := p.writes == 2
		p.mu.Unlock()
		if second {
			close(p.writesReleased)
		}
	}
	return err
}

// barrierLoopRequest is loopRequest plus a per-request barrier token in
// the context, so the pauseCASBarrier wrapper can tell the concurrent
// requests apart.
func barrierLoopRequest(t *testing.T, tok *pauseBarrierToken, id, verb, body string,
	caller *authpkg.CallerIdentity, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+id+"/"+verb, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+id+"/"+verb, nil)
	}
	req.SetPathValue("name", id)
	if caller != nil {
		req = withCaller(req, caller)
	}
	req = req.WithContext(context.WithValue(req.Context(), pauseBarrierTokenKey{}, tok))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// runConcurrentLoopRequests runs a pause and a resume handler call in
// goroutines behind the shared CAS barrier wired into h.OSS, and returns
// their responses (pause first, resume second).
func runConcurrentLoopRequests(t *testing.T, h *LoopHandler, admin *authpkg.CallerIdentity,
	verb1, body1, verb2, body2 string) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	tok1, tok2 := &pauseBarrierToken{}, &pauseBarrierToken{}
	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		recs[0] = barrierLoopRequest(t, tok1, "loop-1", verb1, body1, admin, h.PauseLoop)
	}()
	go func() {
		defer wg.Done()
		recs[1] = barrierLoopRequest(t, tok2, "loop-1", verb2, body2, admin, h.ResumeLoop)
	}()
	wg.Wait()
	return recs[0], recs[1]
}

// Group 10: two overlapping pause requests (user + project) both read the
// empty set before either writes — the exact interleaving the unconditional
// read/modify/write lost. The conditional write must keep BOTH sources.
func TestPauseLoop_ConcurrentSourcesBothSurvive(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	projectKey := "teams/alpha-team/shared/projects/p1/meta.json"
	barrier := newPauseCASBarrier(store, projectKey, 2)
	h := newLoopTestHandler(t, testLoop("loop-1", "alpha-team", "p1-i001-01"))
	h.OSS = barrier
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	tok1, tok2 := &pauseBarrierToken{}, &pauseBarrierToken{}
	recs := make([]*httptest.ResponseRecorder, 2)
	bodies := []string{`{"source":"user"}`, `{"source":"project"}`}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range recs {
		go func(i int) {
			defer wg.Done()
			var tok *pauseBarrierToken
			if i == 0 {
				tok = tok1
			} else {
				tok = tok2
			}
			recs[i] = barrierLoopRequest(t, tok, "loop-1", "pause", bodies[i], admin, h.PauseLoop)
		}(i)
	}
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
		resp := decodeBody(t, rec)
		if resp["paused"] != true {
			t.Fatalf("request %d: paused=%v want true", i+1, resp["paused"])
		}
		rawSources, ok := resp["sources"].([]any)
		if !ok {
			t.Fatalf("request %d: sources missing or not a list: %v", i+1, resp["sources"])
		}
		// Each response reports the durable set at its own write time: it
		// must contain the source this request added, and may contain the
		// other one once that write has landed. The merged set is asserted
		// on the persisted meta below — a response cannot observe a write
		// that has not happened yet.
		own := "user"
		if i == 1 {
			own = "project"
		}
		seenOwn := false
		for _, v := range rawSources {
			if v == own {
				seenOwn = true
			}
		}
		if !seenOwn {
			t.Fatalf("request %d: sources=%v missing its own source %q", i+1, resp["sources"], own)
		}
	}

	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"project", "user"}) {
		t.Fatalf("persisted pause_sources=%v want [project user] (no source may be dropped)", got)
	}
}

// Group 11: a pause(user) overlapping a resume(project) must not report the
// loop as unpaused while the concurrently added user veto survives — the old
// code let the resume win the overwrite and returned paused:false.
func TestResumeLoop_DoesNotDropConcurrentPause(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{"project"}})
	projectKey := "teams/alpha-team/shared/projects/p1/meta.json"
	barrier := newPauseCASBarrier(store, projectKey, 2)
	h := newLoopTestHandler(t, testLoop("loop-1", "alpha-team", "p1-i001-01"))
	h.OSS = barrier
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	recPause, recResume := runConcurrentLoopRequests(t, h, admin,
		"pause", `{"source":"user"}`, "resume", `{"source":"project"}`)
	if recPause.Code != http.StatusOK {
		t.Fatalf("pause status=%d body=%s", recPause.Code, recPause.Body.String())
	}
	if recResume.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", recResume.Code, recResume.Body.String())
	}

	// Both write orders are legal: if the resume wins the race it observes the
	// loop as unpaused (the user pause has not landed yet), and the pause lands
	// afterwards. The invariant that matters is the persisted set below — the
	// user veto must never be dropped. When the resume did observe the pause,
	// it must report it as still paused with the user source.
	resume := decodeBody(t, recResume)
	if resume["paused"] == true {
		rawSources, ok := resume["sources"].([]any)
		if !ok || len(rawSources) != 1 || rawSources[0] != "user" {
			t.Fatalf("resume observed a pause: sources=%v want [user]", resume["sources"])
		}
	}
	if pause := decodeBody(t, recPause); pause["paused"] != true {
		t.Fatalf("pause: paused=%v want true", pause["paused"])
	}

	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"user"}) {
		t.Fatalf("persisted pause_sources=%v want [user]", got)
	}
}

// conflictOnceStore fails PutObjectIfMatch a fixed number of times before
// delegating — a deterministic stand-in for a concurrent writer (same
// pattern as internal/audit's conflictOnce).
type conflictOnceStore struct {
	oss.StorageClient
	fails int
}

func (c *conflictOnceStore) PutObjectIfMatch(ctx context.Context, key string, data []byte, match string) error {
	if c.fails > 0 {
		c.fails--
		return oss.ErrPreconditionFailed
	}
	return c.StorageClient.PutObjectIfMatch(ctx, key, data, match)
}

// Group 12: a transient ETag conflict must be retried — the source lands
// after the retry and the response still reports the durable set.
func TestPauseSourceWriteRetriesOnETagConflict(t *testing.T) {
	store := ossfake.NewMemory()
	seedLoopProject(t, store, "alpha-team", "p1", "p1-i001-01", map[string]any{"pause_sources": []string{}})
	h := newLoopTestHandler(t, testLoop("loop-1", "alpha-team", "p1-i001-01"))
	h.OSS = &conflictOnceStore{StorageClient: store, fails: 1}
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	rec := loopRequest(t, "loop-1", "pause", `{"source":"user"}`, admin, h.PauseLoop)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (a retried conflict is not an error) body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	rawSources, ok := resp["sources"].([]any)
	if !ok || len(rawSources) != 1 || rawSources[0] != "user" {
		t.Fatalf("sources=%v want [user]", resp["sources"])
	}
	got := metaPauseSources(t, readProjectMeta(t, store, "alpha-team", "p1"))
	if !equalStrings(got, []string{"user"}) {
		t.Fatalf("persisted pause_sources=%v want [user] (the source must survive the retry)", got)
	}
}
