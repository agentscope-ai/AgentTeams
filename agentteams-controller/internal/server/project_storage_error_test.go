package server

// PR-M2 (issue #1704): a store that does not answer is 503
// storage_unavailable at every projects-API surface that could otherwise
// surface the fault as a 500 (or, on the list endpoint, as a 200 whose
// shorter body reads as absence — the D1 failure mode) — while the
// semantics the mapping must never disturb (genuine absence 404, malformed
// meta skip, If-Match conflict 409) stay put. The fault is injected at the
// storage layer on the in-memory fake (one storage operation per endpoint),
// mirroring faultOSS / assertStorageUnavailable in
// human_files_storage_error_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// projectFaultOSS fails the selected storage operations with a wrapped
// oss.ErrStorageUnavailable for keys (or list prefixes) under prefix
// ("" = every key); everything else delegates. Like faultOSS it stands in
// for a store that is down at exactly the layer each endpoint drives, but
// it is built on mcLikeOSS (mc-LS-style ListObjects, the semantics
// project_handler.go programs against) and covers the operations the
// projects API uses.
type projectFaultOSS struct {
	*mcLikeOSS
	prefix  string
	methods map[string]bool
}

func newProjectFaultOSS(t *testing.T, store *ossfake.Memory, prefix string, methods ...string) *projectFaultOSS {
	t.Helper()
	f := &projectFaultOSS{mcLikeOSS: &mcLikeOSS{Memory: store}, prefix: prefix, methods: make(map[string]bool, len(methods))}
	for _, m := range methods {
		f.methods[m] = true
	}
	return f
}

func (f *projectFaultOSS) down(key, method string) bool {
	return f.methods[method] && (f.prefix == "" || strings.HasPrefix(key, f.prefix))
}

func (f *projectFaultOSS) fault(key, method string) error {
	return fmt.Errorf("project store down (%s %s): %w", method, key, oss.ErrStorageUnavailable)
}

func (f *projectFaultOSS) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	if f.down(prefix, "ListObjects") {
		return nil, f.fault(prefix, "ListObjects")
	}
	return f.mcLikeOSS.ListObjects(ctx, prefix)
}

func (f *projectFaultOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if f.down(key, "GetObject") {
		return nil, f.fault(key, "GetObject")
	}
	return f.mcLikeOSS.GetObject(ctx, key)
}

func (f *projectFaultOSS) Stat(ctx context.Context, key string) error {
	if f.down(key, "Stat") {
		return f.fault(key, "Stat")
	}
	return f.Memory.Stat(ctx, key)
}

func (f *projectFaultOSS) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	if f.down(key, "StatMeta") {
		return oss.ObjectMeta{}, f.fault(key, "StatMeta")
	}
	return f.Memory.StatMeta(ctx, key)
}

func (f *projectFaultOSS) PutObject(ctx context.Context, key string, data []byte) error {
	if f.down(key, "PutObject") {
		return f.fault(key, "PutObject")
	}
	return f.Memory.PutObject(ctx, key, data)
}

func (f *projectFaultOSS) PutObjectIfMatchStrict(ctx context.Context, key string, data []byte, matchETag string) error {
	if f.down(key, "PutObjectIfMatchStrict") {
		return f.fault(key, "PutObjectIfMatchStrict")
	}
	return f.Memory.PutObjectIfMatchStrict(ctx, key, data, matchETag)
}

// seedProjectListStore puts one team-scoped and one standalone project meta
// into the store, the shape TestListProjects_AdminScansAllPrefixes uses.
func seedProjectListStore(t *testing.T, store *ossfake.Memory) {
	t.Helper()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "P2", "status": "active", "plan_type": "loop",
	})
}

// TestProjectStorageFault_ListProjects_ListFault: the candidate-set listing
// itself never got a semantic response — the list answers retry-later.
func TestProjectStorageFault_ListProjects_ListFault(t *testing.T) {
	store := ossfake.NewMemory()
	seedProjectListStore(t, store)
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "", "ListObjects")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)
	assertStorageUnavailable(t, rec, "list projects (ListObjects fault)")
}

// TestProjectStorageFault_ListProjects_MetaReadFault is the D1 guard: every
// meta.json read hit a store-availability fault, so the list must NOT come
// back as a 200 with a (possibly empty) projects array — that shorter body
// reads as "these projects were deleted" and a sync client would delete its
// local copies. The whole list answers 503 storage_unavailable.
func TestProjectStorageFault_ListProjects_MetaReadFault(t *testing.T) {
	store := ossfake.NewMemory()
	seedProjectListStore(t, store)
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "", "GetObject")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)
	assertStorageUnavailable(t, rec, "list projects (per-object meta read fault)")
}

// TestProjectStorageFault_GetProjectWorkflow: resolving the project meta
// never got a semantic response — retry-later, not a 500 server fault.
func TestProjectStorageFault_GetProjectWorkflow(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "", "GetObject")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	assertStorageUnavailable(t, rec, "get project workflow (meta read fault)")
}

// TestProjectStorageFault_GetTaskInspection_TaskMetaReadFault: the project
// meta resolves fine; the TaskMeta read under the team prefix hit a store
// fault — the inspection endpoint answers retry-later.
func TestProjectStorageFault_GetTaskInspection_TaskMetaReadFault(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks":    []map[string]any{{"task_id": "t1", "status": "planned"}},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "teams/alpha-team/shared/tasks/", "GetObject")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/inspection", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	assertStorageUnavailable(t, rec, "get task inspection (task meta read fault)")
}

// TestProjectStorageFault_GetTaskArtifact_ScopeReadFault: the team-scoped
// artifact read hit a store fault while the global fallback read reports
// genuine absence. The artifact's state in the team scope is then UNKNOWN
// (not "the global copy is absent"), so the endpoint answers retry-later
// instead of the 404 the last-error would have produced.
func TestProjectStorageFault_GetTaskArtifact_ScopeReadFault(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks":    []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	putTaskMeta(store, "alpha-team", "t1", map[string]any{
		"project_id": "p1", "status": "in_progress", "result_path": "shared/projects/p1/result.md",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	// Fault only the team-scoped artifact key; the global key stays readable
	// (absent).
	h.oss = newProjectFaultOSS(t, store, "teams/alpha-team/shared/projects/p1/result.md", "GetObject")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)
	assertStorageUnavailable(t, rec, "get task artifact (team-scope read fault, global read absent)")
}

// TestProjectStorageFault_GetProjectHistory_ListFault: the snapshot listing
// under the project history prefix never got a semantic response.
func TestProjectStorageFault_GetProjectHistory_ListFault(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "teams/alpha-team/shared/projects/p1/history/", "ListObjects")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/history", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectHistory(rec, req)
	assertStorageUnavailable(t, rec, "get project history (history list fault)")
}

// TestProjectStorageFault_SpawnMessages_HistoryDBFault: the worker's
// history.db read hit a store fault. Before the fix the endpoint answered
// 404 "spawn messages unavailable" — absence, the D1 failure mode; now it
// answers the stable 503 storage_unavailable.
func TestProjectStorageFault_SpawnMessages_HistoryDBFault(t *testing.T) {
	rows := []historyRow{
		{kind: "context_msg", role: "user", content: "Design the auth module", sessionID: "sub-3f2a9b1c"},
	}
	h, store := spawnMsgEnv(t, makeHistoryDBBytes(t, rows))
	// Fault only the history.db object (the chats.json discovery reads stay
	// readable, so the endpoint reaches the db read with the session found).
	h.oss = newProjectFaultOSS(t, store, "agents/alpha-lead/"+workerHistoryDBPath, "GetObject")

	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
	assertStorageUnavailable(t, rec, "spawn messages (history.db read fault)")
}

// TestProjectStorageFault_PutProjectMeta_StrictPutFault: the strict
// conditional commit never got a semantic response — the write's fate is
// unknown, so the endpoint answers retry-later (the caller may safely
// re-issue the same commit) instead of a 500 server fault.
func TestProjectStorageFault_PutProjectMeta_StrictPutFault(t *testing.T) {
	store := ossfake.NewMemory()
	key := metaCommitKey("alpha-team", "pm1")
	original := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team"}`)
	if err := store.PutObject(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	read, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	h.oss = newProjectFaultOSS(t, store, key, "PutObjectIfMatchStrict")

	body := []byte(`{"project_id":"pm1","title":"PM1","status":"active","plan_type":"dag","team_id":"alpha-team","note":"committed-bytes"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/pm1/meta", strings.NewReader(string(body)))
	req.SetPathValue("id", "pm1")
	req.Header.Set("If-Match", read.ETag)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.PutProjectMeta(rec, req)
	assertStorageUnavailable(t, rec, "commit project meta (strict put fault)")

	// The faulted write must not have landed: the object is byte-identical.
	stored, err := store.GetObject(context.Background(), key)
	if err != nil || string(stored) != string(original) {
		t.Fatalf("object changed after 503: %q (err=%v)", stored, err)
	}
}

// TestProjectStorageFault_CreateProject_PutFault: existence was verified
// (404-absent, not a fault), then the create write hit a store fault —
// retry-later, never a 500.
func TestProjectStorageFault_CreateProject_PutFault(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))
	h.oss = newProjectFaultOSS(t, store, "teams/alpha-team/shared/projects/", "PutObject")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"title":"New","team_id":"alpha-team"}`))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)
	assertStorageUnavailable(t, rec, "create project (put fault)")
}

// TestProjectStorageFault_AbsenceStays404 is the guard against over-mapping:
// a project that genuinely does not exist stays a 404 (no fault injected) —
// the storage_unavailable body must appear only when the store did not
// answer.
func TestProjectStorageFault_AbsenceStays404(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/does-not-exist/workflow", nil)
	req.SetPathValue("id", "does-not-exist")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for a genuinely absent project; body=%s", rec.Code, rec.Body.String())
	}
}
