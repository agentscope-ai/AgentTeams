package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newProjectTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1beta1 scheme: %v", err)
	}
	return scheme
}

// mcLikeOSS mimics the real MinIOClient.ListObjects semantics (mc ls returns
// direct child names, e.g. "p1/" for a project directory) on top of the
// in-memory fake, which itself returns full object keys.
type mcLikeOSS struct {
	*ossfake.Memory
	listCalls int
	failList  bool
	failGet   bool
}

func (m *mcLikeOSS) ListObjects(_ context.Context, prefix string) ([]string, error) {
	m.listCalls++
	if m.failList {
		return nil, errors.New("oss list failed")
	}
	keys, err := m.Memory.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			continue
		}
		dir := parts[0] + "/"
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *mcLikeOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if m.failGet {
		return nil, errors.New("oss get failed")
	}
	return m.Memory.GetObject(ctx, key)
}

// newProjectTestHandler builds a ProjectHandler with an in-memory OSS store and
// a fake K8s client containing the given Teams.
func newProjectTestHandler(t *testing.T, store *ossfake.Memory, teams ...*v1beta1.Team) *ProjectHandler {
	t.Helper()
	objs := make([]runtime.Object, 0, len(teams))
	for _, tm := range teams {
		objs = append(objs, tm)
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	var o oss.StorageClient = &mcLikeOSS{Memory: store}
	return NewProjectHandler(k8s, "default", o)
}

func withCaller(req *http.Request, c *authpkg.CallerIdentity) *http.Request {
	if c == nil {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), c))
}

func putProject(store *ossfake.Memory, key string, meta map[string]any) {
	data, _ := json.Marshal(meta)
	_ = store.PutObject(context.Background(), key, data)
}

func team(name string) *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: name},
	}
}

func TestListProjects_AdminScansAllPrefixes(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha Project", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Standalone", "status": "active", "plan_type": "loop",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("total=%d, want 2 (team + standalone)", resp.Total)
	}
}

func TestListProjects_TeamLeaderSeesOwnTeamOnly(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha Project", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/beta-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Beta Project", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	putProject(store, "shared/projects/p3/meta.json", map[string]any{
		"project_id": "p3", "title": "Standalone", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, p := range resp.Projects {
		ids[p["project_id"].(string)] = true
	}
	if !ids["p1"] {
		t.Fatalf("team-leader should see own team project, got %v", ids)
	}
	if ids["p2"] || ids["p3"] {
		t.Fatalf("team-leader should NOT see beta or standalone projects, got %v", ids)
	}
}

func TestListProjects_EmptyStoreReturnsEmpty(t *testing.T) {
	h := newProjectTestHandler(t, ossfake.NewMemory())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 with empty list", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"projects":[]`) {
		t.Fatalf("expected empty projects list, got %s", rec.Body.String())
	}
}

func TestGetProjectWorkflow_DagNormalization(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
			{"task_id": "t2", "title": "Task 2", "assigned_to": "@w2", "depends_on": []string{"t1"}, "status": "assigned"},
			{"task_id": "t3", "title": "Task 3", "assigned_to": "@w3", "depends_on": []string{"t2"}, "status": "planned"},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Nodes) != 3 {
		t.Fatalf("nodes=%d, want 3", len(wf.Nodes))
	}
	if len(wf.Edges) != 2 {
		t.Fatalf("edges=%d, want 2 (t1→t2, t2→t3)", len(wf.Edges))
	}
	// t2 is ready (dep t1 completed); t3 waits on t2.
	if len(wf.Next) != 1 || wf.Next[0] != "t2" {
		t.Fatalf("next=%v, want [t2]", wf.Next)
	}
	// status normalization: assigned → delegated, planned → pending
	statuses := map[string]string{}
	for _, n := range wf.Nodes {
		statuses[n.ID] = n.Status
	}
	if statuses["t1"] != "completed" || statuses["t2"] != "delegated" || statuses["t3"] != "pending" {
		t.Fatalf("status normalization wrong: %v", statuses)
	}
	// values summary (StateSnapshot analog)
	if wf.Values == nil {
		t.Fatal("values summary missing")
	}
	if wf.Values.TaskCount["completed"] != 1 || wf.Values.TaskCount["delegated"] != 1 || wf.Values.TaskCount["pending"] != 1 {
		t.Fatalf("values.task_count wrong: %+v", wf.Values.TaskCount)
	}
	if wf.Values.Status != "active" || wf.Values.PlanType != "dag" {
		t.Fatalf("values project fields wrong: %+v", wf.Values)
	}
}

func TestGetProjectWorkflow_LoopReadyFromLoopTasks(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Loop Project",
		"status":     "active",
		"plan_type":  "loop",
		"tasks":      []map[string]any{}, // loop projects keep tasks empty
		"loop": map[string]any{
			"goal": "g", "stop_condition": "s", "current_iteration": 1, "max_iterations": 5, "status": "running",
			"tasks": []map[string]any{
				{"task_id": "l1", "title": "Loop Step 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
				{"task_id": "l2", "title": "Loop Step 2", "assigned_to": "@w2", "depends_on": []string{"l1"}, "status": "assigned"},
			},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Nodes) != 2 {
		t.Fatalf("nodes=%d, want 2 (from loop.tasks)", len(wf.Nodes))
	}
	if len(wf.Next) != 1 || wf.Next[0] != "l2" {
		t.Fatalf("next=%v, want [l2] (loop ready semantics)", wf.Next)
	}
}

func TestGetProjectWorkflow_TaskLifecycleStatusNormalization(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "P",
		"status":     "active",
		"plan_type":  "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "assigned_to": "@w1", "depends_on": []string{}, "status": "in_progress"},
			{"task_id": "t2", "title": "T2", "assigned_to": "@w2", "depends_on": []string{"t1"}, "status": "submitted"},
			{"task_id": "t3", "title": "T3", "assigned_to": "@w3", "depends_on": []string{"t2"}, "status": "planned"},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	statuses := map[string]string{}
	for _, n := range wf.Nodes {
		statuses[n.ID] = n.Status
	}
	if statuses["t1"] != "in-progress" || statuses["t2"] != "in-progress" {
		t.Fatalf("in_progress/submitted should normalize to in-progress, got %v", statuses)
	}
	// t3 is ready (deps t1/t2 not completed yet? no — t1/t2 are not completed,
	// so t3 is NOT ready). t2 submitted is not ready either.
	if len(wf.Next) != 0 {
		t.Fatalf("next=%v, want [] (no completed dependencies)", wf.Next)
	}
}

func TestListProjects_EffectiveTeamNameMapping(t *testing.T) {
	store := ossfake.NewMemory()
	// Team CR name "alpha-cr" with spec.teamName "alpha-team": storage is
	// supposed to live under teams/alpha-team/ (EffectiveTeamName wins).
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha Project", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	// Decoy under the CR-name prefix simulates a worker that synced using the
	// Worker API team field (resp.Team = CR name). Both prefixes are scanned
	// to tolerate the mismatch; TeamLeader scoping still uses effective name.
	putProject(store, "teams/alpha-cr/shared/projects/decoy/meta.json", map[string]any{
		"project_id": "decoy", "title": "Decoy", "status": "active", "plan_type": "dag",
	})
	alphaCR := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-cr", Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: "alpha-team"},
	}
	h := newProjectTestHandler(t, store, alphaCR)

	// Admin sees both prefixes (tolerance scan).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("admin should see both prefixes (effective + CR name), got total=%d %+v", resp.Total, resp.Projects)
	}

	// TeamLeader (CR name alpha-cr) is scoped to the effective-name prefix only.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-cr"})
	rec2 := httptest.NewRecorder()
	h.ListProjects(rec2, req2)

	var resp2 struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Total != 1 || resp2.Projects[0]["project_id"] != "p1" {
		t.Fatalf("team-leader should see effective-name project only, got total=%d %+v", resp2.Total, resp2.Projects)
	}
}

func TestListProjects_MissingMetaSkipped(t *testing.T) {
	store := ossfake.NewMemory()
	// p1 has meta.json; p2 is a project dir without meta.json yet.
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	_ = store.PutObject(context.Background(), "shared/projects/p2/.agentteams-keep", []byte(""))
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (missing meta skipped)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Projects[0]["project_id"] != "p1" {
		t.Fatalf("want only p1 (p2 missing meta skipped), got total=%d %+v", resp.Total, resp.Projects)
	}
}

func TestHTTPServer_RegistersProjectRoutes(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).Build()
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    k8s,
		Namespace: "default",
		OSS:       ossStore,
		AuthMw:    authpkg.NewMiddleware(nil, nil, nil, nil, ""),
	})

	// GET /api/v1/projects
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/projects status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"project_id":"p1"`) {
		t.Fatalf("expected p1 in list, got %s", rec.Body.String())
	}

	// GET /api/v1/projects/p1/workflow
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/projects/p1/workflow status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"project_id":"p1"`) {
		t.Fatalf("expected p1 in workflow, got %s", rec2.Body.String())
	}

	// GET /api/v1/projects/p1/tasks/t1/artifact (route registered; no task
	// meta/artifact stored → 404 rather than 405/404-for-unmatched)
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
	})
	reqArt := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	recArt := httptest.NewRecorder()
	srv.Mux.ServeHTTP(recArt, reqArt)
	if recArt.Code != http.StatusNotFound {
		t.Fatalf("GET artifact status=%d, want 404 (no result_path)", recArt.Code)
	}

	// Unmatched route should 404 (no wildcard shadowing)
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/nope", nil)
	rec3 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec3, req3)
	if rec3.Code == http.StatusOK {
		t.Fatalf("unmatched sub-route should not 200, got %d", rec3.Code)
	}
}

func TestGetProjectWorkflow_CorruptMetaNotFound(t *testing.T) {
	store := ossfake.NewMemory()
	_ = store.PutObject(context.Background(), "shared/projects/p1/meta.json", []byte("{truncated"))
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for corrupt meta (not 500)", rec.Code, rec.Body.String())
	}
}

func TestListProjects_SortedAndTeamFiltered(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "P2", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	putProject(store, "teams/beta-team/shared/projects/p3/meta.json", map[string]any{
		"project_id": "p3", "title": "P3", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	// Sorted by project_id for admin.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Fatalf("total=%d, want 3", resp.Total)
	}
	ids := []string{}
	for _, p := range resp.Projects {
		ids = append(ids, p["project_id"].(string))
	}
	if ids[0] != "p1" || ids[1] != "p2" || ids[2] != "p3" {
		t.Fatalf("projects not sorted: %v", ids)
	}

	// ?team=alpha-team filters (admin).
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects?team=alpha-team", nil)
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec2 := httptest.NewRecorder()
	h.ListProjects(rec2, req2)
	var resp2 struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Total != 1 || resp2.Projects[0]["project_id"] != "p2" {
		t.Fatalf("team filter failed, got %+v", resp2.Projects)
	}
}

func TestGetProjectWorkflow_LoopWaitingUserHasNoNext(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Loop Project",
		"status":     "active",
		"plan_type":  "loop",
		"tasks":      []map[string]any{},
		"loop": map[string]any{
			"goal": "g", "stop_condition": "s", "current_iteration": 1, "max_iterations": 5, "status": "waiting_user",
			"tasks": []map[string]any{
				{"task_id": "l1", "title": "Loop Step 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "assigned"},
			},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// waiting_user loop has no ready nodes even though l1 deps are empty
	// (mirrors _ready_loop_nodes: loop.status in {completed, blocked, waiting_user}).
	if len(wf.Next) != 0 {
		t.Fatalf("next=%v, want [] for waiting_user loop", wf.Next)
	}
	// interrupt should surface.
	found := false
	for _, in := range wf.Interrupts {
		if in.ID == "loop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected loop interrupt, got %+v", wf.Interrupts)
	}
}

func TestGetProjectWorkflow_BlockedCreatesInterrupt(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "P",
		"status":     "active",
		"plan_type":  "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "assigned_to": "@w1", "depends_on": []string{}, "status": "blocked"},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, in := range wf.Interrupts {
		if in.ID == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected interrupt for blocked t1, got %+v", wf.Interrupts)
	}
}

func TestGetProjectWorkflow_LoopWaitingUserCreatesInterrupt(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "P",
		"status":     "active",
		"plan_type":  "loop",
		"tasks":      []map[string]any{},
		"loop": map[string]any{
			"goal": "g", "stop_condition": "s", "current_iteration": 1, "max_iterations": 5, "status": "waiting_user",
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wf.Loop == nil || wf.Loop.Status != "waiting_user" {
		t.Fatalf("loop not passed through: %+v", wf.Loop)
	}
	found := false
	for _, in := range wf.Interrupts {
		if in.ID == "loop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected loop interrupt, got %+v", wf.Interrupts)
	}
}

func TestGetProjectWorkflow_NotFound(t *testing.T) {
	h := newProjectTestHandler(t, ossfake.NewMemory())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope/workflow", nil)
	req.SetPathValue("id", "nope")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// TestGetProjectWorkflow_PausedInterrupt guards W1: a paused project surfaces
// as a human interrupt (LangGraph semantics) in addition to status=paused.
// Since the lifecycle write API, the interrupt carries an action_request (resume) + config
// (allow_accept) aligned with the LangChain Agent Inbox HumanInterrupt model,
// so a dashboard can render a "Resume" button.
func TestGetProjectWorkflow_PausedInterrupt(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/paused1/meta.json", map[string]any{
		"project_id": "paused1", "title": "Paused", "status": "paused", "plan_type": "dag",
		"pause_reason": "customer review", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/paused1/workflow", nil)
	req.SetPathValue("id", "paused1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *workflowInterrupt
	for i := range wf.Interrupts {
		if wf.Interrupts[i].ID == "project" && wf.Interrupts[i].Value == "paused" {
			found = &wf.Interrupts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected paused project interrupt, got %+v", wf.Interrupts)
	}
	// the lifecycle write API: action_request + config + description aligned with Agent Inbox.
	if found.ActionRequest == nil || found.ActionRequest.Action != "resume" {
		t.Fatalf("paused interrupt action_request=%+v, want resume", found.ActionRequest)
	}
	if found.Config == nil || !found.Config.AllowAccept {
		t.Fatalf("paused interrupt config=%+v, want allow_accept", found.Config)
	}
	if !strings.Contains(found.Description, "customer review") {
		t.Fatalf("paused interrupt description=%q, want to include pause reason", found.Description)
	}
}

// TestListProjects_SkipGetObjectFailure guards W5: a per-object GetObject
// failure must be skipped (not 500 the whole list); infrastructure-level
// ListObjects failures still 500 (covered by TestListProjects_OSSErrorReturns500).
func TestListProjects_SkipGetObjectFailure(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/good1/meta.json", map[string]any{
		"project_id": "good1", "title": "Good", "status": "active", "plan_type": "dag",
	})
	putProject(store, "shared/projects/bad1/meta.json", map[string]any{
		"project_id": "bad1", "title": "Bad", "status": "active", "plan_type": "dag",
	})
	m := &mcLikeOSS{Memory: store, failGet: true}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	h := NewProjectHandler(k8s, "default", m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	// W5: GetObject failures are skipped, so the list still succeeds and
	// simply contains no projects from the failing store.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (per-object failure skipped)", rec.Code)
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 {
		t.Fatalf("total=%d, want 0 (all GetObject failed, skipped)", resp.Total)
	}
}

func TestGetProjectWorkflow_TeamLeaderCrossTeamDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/beta-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Beta", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	h := newProjectTestHandler(t, store, team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p2/workflow", nil)
	req.SetPathValue("id", "p2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access (W4: hide project existence)", rec.Code)
	}
}

func TestGetProjectWorkflow_TeamLeaderStandaloneDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p3/meta.json", map[string]any{
		"project_id": "p3", "title": "Standalone", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p3/workflow", nil)
	req.SetPathValue("id", "p3")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for standalone project (W4: hide existence from team leader)", rec.Code)
	}
}

// countingClient wraps the fake client to count K8s List round-trips.
type countingClient struct {
	client.Client
	listCalls int
}

func (c *countingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.listCalls++
	return c.Client.List(ctx, list, opts...)
}

// TestGetProjectWorkflow_SingleK8sList guards O1: a workflow request must pay
// exactly one K8s TeamList round-trip — shared between meta resolution and the
// team-leader access check — not two.
func TestGetProjectWorkflow_SingleK8sList(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	cc := &countingClient{Client: k8s}
	var o oss.StorageClient = &mcLikeOSS{Memory: store}
	h := NewProjectHandler(cc, "default", o)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cc.listCalls != 1 {
		t.Fatalf("K8s List calls=%d, want 1 (single list shared between meta resolution and access check)", cc.listCalls)
	}
}

// TestListProjects_TeamFilterSkipsOtherPrefixes guards O2: a ?team= filter
// must skip non-matching prefixes before hitting OSS (ListObjects), not scan
// every prefix and filter afterwards.
func TestListProjects_TeamFilterSkipsOtherPrefixes(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "P2", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/beta-team/shared/projects/p3/meta.json", map[string]any{
		"project_id": "p3", "title": "P3", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	m := &mcLikeOSS{Memory: store}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team"), team("beta-team")).Build()
	h := NewProjectHandler(k8s, "default", m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects?team=alpha-team", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Projects[0]["project_id"] != "p2" {
		t.Fatalf("team filter result wrong: %+v", resp.Projects)
	}
	// alpha-team prefix only — beta-team and shared prefixes must not be listed.
	if m.listCalls != 1 {
		t.Fatalf("ListObjects calls=%d, want 1 (only the alpha-team prefix scanned)", m.listCalls)
	}
}

// TestGetProjectWorkflow_NextOnlyPlannedAssigned guards O5: only tasks whose
// raw status is planned/assigned can appear in next — empty or unknown
// statuses must not (upstream _ready_nodes skips them), even when their
// dependencies are all completed.
func TestGetProjectWorkflow_NextOnlyPlannedAssigned(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}},
			{"task_id": "t2", "title": "T2", "status": "", "depends_on": []string{}},
			{"task_id": "t3", "title": "T3", "status": "weird", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Next) != 1 || wf.Next[0] != "t1" {
		t.Fatalf("next=%v, want [t1] only (empty/unknown statuses are not ready)", wf.Next)
	}
}

// TestGetProjectWorkflow_SourceField guards O8: the workflow response must
// expose the project source label (matrix/dingtalk/wechat...) that TeamHarness
// writes into meta.json — humans need to know which channel a project came from.
func TestGetProjectWorkflow_SourceField(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team", "source": "dingtalk",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wf.Source != "dingtalk" {
		t.Fatalf("source=%q, want dingtalk", wf.Source)
	}
}

// TestListProjects_L2HumanAggregatesTeams guards the multi-tenant L2 path: a
// human with AccessibleTeams [alpha, beta] sees projects from BOTH teams in a
// single list (no per-team SA switching).
func TestListProjects_L2HumanAggregatesTeams(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pa/meta.json", map[string]any{
		"project_id": "pa", "title": "PA", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/beta-team/shared/projects/pb/meta.json", map[string]any{
		"project_id": "pb", "title": "PB", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	putProject(store, "teams/gamma-team/shared/projects/pc/meta.json", map[string]any{
		"project_id": "pc", "title": "PC", "status": "active", "plan_type": "dag", "team_id": "gamma-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"), team("gamma-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	// L2 human with two accessible teams (Human CR accessibleTeams = CR names).
	req = withCaller(req, &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team", "beta-team"},
	})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("total=%d, want 2 (alpha+beta only, gamma hidden); got %+v", resp.Total, resp.Projects)
	}
	ids := map[string]bool{}
	for _, p := range resp.Projects {
		ids[p["project_id"].(string)] = true
	}
	if !ids["pa"] || !ids["pb"] || ids["pc"] {
		t.Fatalf("projects=%v, want pa+pb, no pc", ids)
	}
}

// TestGetProjectWorkflow_L2HumanAnyAccessibleTeam guards the multi-tenant L2
// read path: an L2 human can read a workflow from any accessible team but is
// denied projects outside their accessible set.
func TestGetProjectWorkflow_L2HumanAnyAccessibleTeam(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pa/meta.json", map[string]any{
		"project_id": "pa", "title": "PA", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}}},
	})
	putProject(store, "teams/gamma-team/shared/projects/pc/meta.json", map[string]any{
		"project_id": "pc", "title": "PC", "status": "active", "plan_type": "dag", "team_id": "gamma-team",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}}},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("gamma-team"))
	l2 := &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"},
	}

	// Accessible team -> OK.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pa/workflow", nil)
	req.SetPathValue("id", "pa")
	req = withCaller(req, l2)
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("accessible team status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Non-accessible team -> 404 (W4: hide project existence).
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pc/workflow", nil)
	req2.SetPathValue("id", "pc")
	req2 = withCaller(req2, l2)
	rec2 := httptest.NewRecorder()
	h.GetProjectWorkflow(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("non-accessible team status=%d, want 404 (W4: hide existence)", rec2.Code)
	}
}

// TestListProjects_OSSErrorReturns500 guards the explicit-failure path: an
// object-store failure surfaces as 500 (never a silently truncated list).
func TestListProjects_OSSErrorReturns500(t *testing.T) {
	store := ossfake.NewMemory()
	m := &mcLikeOSS{Memory: store, failList: true}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	h := NewProjectHandler(k8s, "default", m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 on OSS failure", rec.Code)
	}
}

// TestGetProjectWorkflow_K8sErrorReturns500 guards the K8s failure path:
// TeamList resolution errors surface as 500, not a false 404.
func TestGetProjectWorkflow_K8sErrorReturns500(t *testing.T) {
	store := ossfake.NewMemory()
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	failing := &failingListClient{Client: k8s}
	var o oss.StorageClient = &mcLikeOSS{Memory: store}
	h := NewProjectHandler(failing, "default", o)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 on K8s failure", rec.Code)
	}
}

// failingListClient fails every K8s List call.
type failingListClient struct {
	client.Client
}

func (f *failingListClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errors.New("k8s list failed")
}

// TestGetProjectWorkflow_GetObjectErrorReturns500 guards the meta read
// failure path: a non-NotFound GetObject error surfaces as 500 (not a
// silently skipped project / false 404).
func TestGetProjectWorkflow_GetObjectErrorReturns500(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	m := &mcLikeOSS{Memory: store, failGet: true}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	h := NewProjectHandler(k8s, "default", m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 on GetObject failure", rec.Code)
	}
}

// TestGetProjectWorkflow_ProjectInLaterPrefix guards multi-prefix resolution:
// resolveProjectMeta scans prefixes in order and finds the project when it
// lives under a later team prefix (alpha empty, beta holds it).
func TestGetProjectWorkflow_ProjectInLaterPrefix(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/beta-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "beta-team",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}}},
	})
	m := &mcLikeOSS{Memory: store}
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team"), team("beta-team")).Build()
	h := NewProjectHandler(k8s, "default", m)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (project found in later prefix); body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wf.TeamID != "beta-team" {
		t.Fatalf("team_id=%q, want beta-team", wf.TeamID)
	}
}

// TestListProjects_L2HumanTeamFilter guards the L2 + ?team= combination: an
// L2 human aggregating two teams can narrow to one of their own teams; asking
// for a team outside the accessible set returns nothing (never leaks).
func TestListProjects_L2HumanTeamFilter(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pa/meta.json", map[string]any{
		"project_id": "pa", "title": "PA", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/beta-team/shared/projects/pb/meta.json", map[string]any{
		"project_id": "pb", "title": "PB", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))
	l2 := &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team", "beta-team"},
	}

	// Narrow to one accessible team.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects?team=alpha-team", nil)
	req = withCaller(req, l2)
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Projects[0]["project_id"] != "pa" {
		t.Fatalf("team filter (own team) wrong: %+v", resp.Projects)
	}

	// Ask for a team outside the accessible set -> nothing.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects?team=gamma-team", nil)
	req2 = withCaller(req2, l2)
	rec2 := httptest.NewRecorder()
	h.ListProjects(rec2, req2)
	var resp2 struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Total != 0 {
		t.Fatalf("team filter (outside accessible) leaked %d projects: %+v", resp2.Total, resp2.Projects)
	}
}

// staticWhoami implements authpkg.MatrixWhoami for the HTTP auth-chain test.
type staticWhoami struct {
	validToken string
	userID     string
}

func (s *staticWhoami) Whoami(_ context.Context, token string) (string, error) {
	if token != s.validToken {
		return "", errors.New("invalid matrix token")
	}
	return s.userID, nil
}

// alwaysFailAuth always fails (simulates SA TokenReview rejecting a Matrix token).
type alwaysFailAuth struct{}

func (a *alwaysFailAuth) Authenticate(_ context.Context, _ string) (*authpkg.CallerIdentity, error) {
	return nil, errors.New("SA token review failed")
}

// TestProjectHTTP_L2AuthChain exercises the full HTTP chain — bearer token
// extraction, composite authentication (SA fails, Matrix whoami succeeds),
// identity enrichment, authorization, and the project handler — for the L2
// human path.
func TestProjectHTTP_L2AuthChain(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pa/meta.json", map[string]any{
		"project_id": "pa", "title": "PA", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	scheme := newProjectTestScheme(t)
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "maizong", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{Username: "maizong", PermissionLevel: 2, AccessibleTeams: []string{"alpha-team"}},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(human, team("alpha-team")).Build()

	matrixAuth := authpkg.NewMatrixTokenAuthenticator(k8s, "default", &staticWhoami{validToken: "matrix-token", userID: "@maizong:matrix.local"})
	composite := authpkg.NewCompositeAuthenticator(&alwaysFailAuth{}, matrixAuth)
	enricher := authpkg.NewCREnricher(k8s, "default")
	mw := authpkg.NewMiddleware(composite, enricher, authpkg.NewAuthorizer(), k8s, "default")

	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    k8s,
		Namespace: "default",
		OSS:       ossStore,
		AuthMw:    mw,
	})

	// L2 human with Matrix token -> aggregated list for accessible team.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer matrix-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("L2 list status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"project_id":"pa"`) {
		t.Fatalf("expected pa in L2 list, got %s", rec.Body.String())
	}

	// Invalid token -> 401.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req2.Header.Set("Authorization", "Bearer bad-token")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d, want 401", rec2.Code)
	}

	// No token -> 401.
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec3 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("no token status=%d, want 401", rec3.Code)
	}

	// L2 human reads a workflow via the same HTTP chain (accessible team).
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pa/workflow", nil)
	req4.SetPathValue("id", "pa")
	req4.Header.Set("Authorization", "Bearer matrix-token")
	rec4 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("L2 workflow status=%d, want 200; body=%s", rec4.Code, rec4.Body.String())
	}

	// W4: L2 human requests a project from a non-accessible team through the
	// full HTTP chain -> 404 (existence hidden, same as a missing project).
	putProject(store, "teams/beta-team/shared/projects/pb/meta.json", map[string]any{
		"project_id": "pb", "title": "PB", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	req5 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/pb/workflow", nil)
	req5.SetPathValue("id", "pb")
	req5.Header.Set("Authorization", "Bearer matrix-token")
	rec5 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusNotFound {
		t.Fatalf("L2 cross-team workflow status=%d, want 404 (W4: hide existence)", rec5.Code)
	}
}

// TestProjectHTTP_L2WriteChain exercises the the lifecycle write API write path through the
// full HTTP chain — bearer extraction -> composite auth -> identity
// enrichment -> authorizer (ActionUpdate + project -> requireSameTeam) ->
// handler (checkProjectAccess + mtime lock) — for an L2 human:
//
//   - pause an accessible-team project via the real HTTP stack -> 200
//   - pause a non-accessible team project -> 404 (existence hidden)
//   - create with a non-accessible team -> 404
func TestProjectHTTP_L2WriteChain(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/pa/meta.json", map[string]any{
		"project_id": "pa", "title": "PA", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	scheme := newProjectTestScheme(t)
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "maizong", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{Username: "maizong", PermissionLevel: 2, AccessibleTeams: []string{"alpha-team"}},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(human, team("alpha-team"), team("beta-team")).Build()

	matrixAuth := authpkg.NewMatrixTokenAuthenticator(k8s, "default", &staticWhoami{validToken: "matrix-token", userID: "@maizong:matrix.local"})
	composite := authpkg.NewCompositeAuthenticator(&alwaysFailAuth{}, matrixAuth)
	enricher := authpkg.NewCREnricher(k8s, "default")
	mw := authpkg.NewMiddleware(composite, enricher, authpkg.NewAuthorizer(), k8s, "default")

	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{
		Client:    k8s,
		Namespace: "default",
		OSS:       ossStore,
		AuthMw:    mw,
	})

	// L2 human pauses an accessible team's project via the full HTTP chain.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/pa/pause", strings.NewReader(`{"reason":"review"}`))
	req.Header.Set("Authorization", "Bearer matrix-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("L2 pause status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "teams/alpha-team/shared/projects/pa/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	if meta["status"] != "paused" || meta["pause_reason"] != "review" || meta["updated_by"] != "maizong (human)" {
		t.Fatalf("meta after pause=%v, want paused/review/maizong (human)", meta)
	}

	// Cross-team pause -> 404 (existence hidden through the write path too).
	putProject(store, "teams/beta-team/shared/projects/pb/meta.json", map[string]any{
		"project_id": "pb", "title": "PB", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/pb/pause", nil)
	req2.Header.Set("Authorization", "Bearer matrix-token")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("L2 cross-team pause status=%d, want 404 (existence hidden)", rec2.Code)
	}

	// L2 create in a non-accessible team -> 404.
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"title":"X","team_id":"beta-team"}`))
	req3.Header.Set("Authorization", "Bearer matrix-token")
	rec3 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("L2 cross-team create status=%d, want 404", rec3.Code)
	}
}

// alwaysAdminAuth is a fake Authenticator that always returns an admin
// identity for the admin HTTP write-chain test.
type alwaysAdminAuth struct{}

func (a *alwaysAdminAuth) Authenticate(_ context.Context, _ string) (*authpkg.CallerIdentity, error) {
	return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, nil
}

// TestProjectHTTP_AdminWriteChain verifies an admin can create + complete a
// project through the full HTTP chain (no team restriction).
func TestProjectHTTP_AdminWriteChain(t *testing.T) {
	store := ossfake.NewMemory()
	scheme := newProjectTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(team("alpha-team")).Build()
	enricher := authpkg.NewCREnricher(k8s, "default")
	mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, enricher, authpkg.NewAuthorizer(), k8s, "default")
	var ossStore oss.StorageClient = &mcLikeOSS{Memory: store}
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: ossStore, AuthMw: mw})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"title":"New","team_id":"alpha-team"}`))
	req.Header.Set("Authorization", "Bearer sa-token")
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ProjectID == "" {
		t.Fatal("create returned empty project_id")
	}

	// The created project is immediately visible to the admin via the
	// workflow endpoint (dual-prefix scan finds the team-scoped meta).
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+created.ProjectID+"/workflow", nil)
	req2.SetPathValue("id", created.ProjectID)
	req2.Header.Set("Authorization", "Bearer sa-token")
	rec2 := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin workflow-after-create status=%d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestGetProjectWorkflow_PassThroughAuditFields guards W2: human-intervention
// audit fields written by the lifecycle write API (updated_by/updated_at/pause_reason) are
// passed through the workflow response.
func TestGetProjectWorkflow_PassThroughAuditFields(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/audit1/meta.json", map[string]any{
		"project_id": "audit1", "title": "Audit", "status": "paused", "plan_type": "dag",
		"updated_by": "luo", "updated_at": "2026-08-12T10:00:00Z", "pause_reason": "hold for review",
		"tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/audit1/workflow", nil)
	req.SetPathValue("id", "audit1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wf.UpdatedBy != "luo" || wf.UpdatedAt != "2026-08-12T10:00:00Z" || wf.PauseReason != "hold for review" {
		t.Fatalf("audit fields not passed through: %+v", wf)
	}
}

// TestListProjects_ConcurrentFetch guards W7: the concurrent GetObject pool
// returns the same complete, deduplicated list as serial iteration would.
func TestListProjects_ConcurrentFetch(t *testing.T) {
	store := ossfake.NewMemory()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("cp%02d", i)
		putProject(store, "shared/projects/"+id+"/meta.json", map[string]any{
			"project_id": id, "title": "CP " + id, "status": "active", "plan_type": "dag",
		})
	}
	// Same project id on a team prefix: NOT deduplicated away — project ids
	// are only unique per workspace upstream, so (team, project_id) is the
	// identity and both must appear (reviewer feedback).
	putProject(store, "teams/alpha-team/shared/projects/cp01/meta.json", map[string]any{
		"project_id": "cp01", "title": "CP cp01", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 21 {
		t.Fatalf("total=%d, want 21 (20 global + 1 team-scoped cp01)", resp.Total)
	}
	// Both cp01 entries appear, disambiguated by team_id.
	cp01Teams := map[string]bool{}
	for _, p := range resp.Projects {
		if id, _ := p["project_id"].(string); id == "cp01" {
			cp01Teams[str(p["team_id"])] = true
		}
	}
	if len(cp01Teams) != 2 || !cp01Teams[""] || !cp01Teams["alpha-team"] {
		t.Fatalf("cp01 team_ids=%v, want both '' and alpha-team", cp01Teams)
	}
	// Deterministic ordering preserved.
	last := ""
	for _, p := range resp.Projects {
		id, _ := p["project_id"].(string)
		if last != "" && last > id {
			t.Fatalf("not sorted: %s > %s", last, id)
		}
		last = id
	}
}

func TestGetProjectWorkflow_IncludeTasksDetail(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
			{"task_id": "t2", "title": "Task 2", "assigned_to": "@w2", "depends_on": []string{"t1"}, "status": "assigned"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id":       "t1",
		"project_id":    "p1",
		"status":        "completed",
		"spec_path":     "shared/tasks/t1/spec.md",
		"assigned_to":   "@w1",
		"summary":       "Alpha report done",
		"result_status": "SUCCESS",
		"deliverables":  []any{map[string]any{"type": "file", "path": "shared/tasks/t1/output.pdf"}},
		"result_path":   "shared/tasks/t1/result.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id":     "t2",
		"project_id":  "p1",
		"status":      "assigned",
		"spec_path":   "shared/tasks/t2/spec.md",
		"assigned_to": "@w2",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.TasksDetail) != 2 {
		t.Fatalf("tasks_detail=%d, want 2", len(wf.TasksDetail))
	}
	byID := map[string]taskDetail{}
	for _, d := range wf.TasksDetail {
		byID[d.TaskID] = d
	}
	if wf.TasksDetail[0].TaskID != "t1" || wf.TasksDetail[1].TaskID != "t2" {
		t.Fatalf("tasks_detail order wrong: %+v", wf.TasksDetail)
	}
	d1 := byID["t1"]
	if d1.Summary != "Alpha report done" || d1.ResultStatus != "SUCCESS" || d1.ResultPath != "shared/tasks/t1/result.md" {
		t.Fatalf("t1 detail wrong: %+v", d1)
	}
	if len(d1.Deliverables) != 1 {
		t.Fatalf("t1 deliverables=%d, want 1", len(d1.Deliverables))
	}
	if d1.SpecPath != "shared/tasks/t1/spec.md" {
		t.Fatalf("t1 spec_path wrong: %s", d1.SpecPath)
	}
	d2 := byID["t2"]
	if d2.SpecPath != "shared/tasks/t2/spec.md" || d2.ProjectID != "p1" {
		t.Fatalf("t2 detail wrong: %+v", d2)
	}
	if d2.ResultStatus != "" || d2.Summary != "" {
		t.Fatalf("t2 should not have result fields: %+v", d2)
	}
}

func TestGetProjectWorkflow_IncludeTasksDefaultOmitted(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "status": "completed", "summary": "secret detail",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.TasksDetail) != 0 {
		t.Fatalf("tasks_detail=%d, want 0 without includeTasks", len(wf.TasksDetail))
	}
}

func TestGetProjectWorkflow_IncludeTasksMissingMetaSkipped(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Standalone",
		"status":     "active",
		"plan_type":  "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "assigned"},
			{"task_id": "t2", "title": "Task 2", "status": "planned"},
		},
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "assigned", "spec_path": "shared/tasks/t1/spec.md",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.Nodes) != 2 {
		t.Fatalf("nodes=%d, want 2", len(wf.Nodes))
	}
	if len(wf.TasksDetail) != 1 || wf.TasksDetail[0].TaskID != "t1" {
		t.Fatalf("tasks_detail=%+v, want only t1", wf.TasksDetail)
	}
}

func TestGetProjectWorkflow_IncludeTasksTeamPrefixWins(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "assigned"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "assigned", "summary": "team copy",
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "assigned", "summary": "global copy",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.TasksDetail) != 1 {
		t.Fatalf("tasks_detail=%d, want 1", len(wf.TasksDetail))
	}
	if wf.TasksDetail[0].Summary != "team copy" {
		t.Fatalf("team prefix should win, got summary=%q", wf.TasksDetail[0].Summary)
	}
}

func TestGetProjectWorkflow_IncludeTasksLoopTasks(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Loop", "status": "active", "plan_type": "loop",
		"loop": map[string]any{
			"goal": "iterate", "current_iteration": 1, "max_iterations": 3, "status": "running",
			"tasks": []map[string]any{
				{"task_id": "iter-1", "title": "Iter 1", "status": "completed"},
				{"task_id": "iter-2", "title": "Iter 2", "status": "assigned"},
			},
		},
	})
	putProject(store, "shared/tasks/iter-2/meta.json", map[string]any{
		"task_id": "iter-2", "project_id": "p1", "status": "assigned", "summary": "second iteration",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wf workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(wf.TasksDetail) != 1 || wf.TasksDetail[0].TaskID != "iter-2" {
		t.Fatalf("tasks_detail=%+v, want iter-2 only", wf.TasksDetail)
	}
}

func TestGetTaskArtifact_Download(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/result.md", map[string]any{
		"hello": "artifact body",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("Content-Type=%q, want text/markdown", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "result.md") {
		t.Fatalf("Content-Disposition=%q, want attachment with result.md", cd)
	}
	if !strings.Contains(rec.Body.String(), "artifact body") {
		t.Fatalf("body=%q, want artifact content", rec.Body.String())
	}
}

func TestGetTaskArtifact_ProjectNotFound(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nope/tasks/t1/artifact", nil)
	req.SetPathValue("id", "nope")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestGetTaskArtifact_TaskNotInProject(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	// t2 exists in shared storage but belongs to another project.
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p2", "status": "completed",
		"result_path": "shared/tasks/t2/result.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/result.md", map[string]any{"x": "y"})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t2/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (task not in project graph)", rec.Code)
	}
}

func TestGetTaskArtifact_NoResultPath(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "assigned"},
		},
	})
	// TaskMeta exists but has no result_path (not yet submitted).
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "assigned",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (no result_path)", rec.Code)
	}
}

func TestGetTaskArtifact_MissingArtifactFile(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	// result.md does NOT exist in storage.
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (artifact file missing)", rec.Code)
	}
}

func TestGetTaskArtifact_PathTraversalRejected(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	// Malicious worker sets a traversal result_path.
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/../../secret/credentials.json",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (traversal rejected)", rec.Code)
	}
}

func TestGetTaskArtifact_EscapePrefixRejected(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	// result_path points at an unrelated shared object (not this task/project).
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/OTHER/result.md",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (escape prefix rejected)", rec.Code)
	}
}

func TestGetTaskArtifact_L2Scoped(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/result.md", map[string]any{"ok": true})
	// L2 human with alpha-team accessible.
	l2 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sun", Teams: []string{"alpha-team"}}
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, l2)
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (L2 own team)", rec.Code)
	}
}

func TestGetTaskArtifact_L2CrossTeam404(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/result.md", map[string]any{"ok": true})
	// L2 human controlling only beta-team cannot read alpha-team artifact.
	l2 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "ma", Teams: []string{"beta-team"}}
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, l2)
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (L2 cross-team existence hidden)", rec.Code)
	}
}

func TestGetTaskArtifact_GlobalPrefixFallback(t *testing.T) {
	store := ossfake.NewMemory()
	// Standalone project (no team) -> global shared/ prefixes.
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Standalone", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.pdf",
	})
	putProject(store, "shared/tasks/t1/result.pdf", map[string]any{"hello": "pdf body"})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (global prefix)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type=%q, want application/pdf", ct)
	}
}

func TestGetTaskArtifact_ByPathDeliverable(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path":  "shared/tasks/t1/result.md",
		"deliverables": []any{"shared/tasks/t1/output.pdf"},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/output.pdf", map[string]any{"pdf": "content"})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact?path=shared/tasks/t1/output.pdf", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type=%q, want application/pdf", ct)
	}
	if !strings.Contains(rec.Body.String(), "content") {
		t.Fatalf("body=%q, want deliverable content", rec.Body.String())
	}
}

func TestGetTaskArtifact_ByPathSpec(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
		"spec_path":   "shared/tasks/t1/spec.md",
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/spec.md", map[string]any{"spec": "the spec"})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact?path=shared/tasks/t1/spec.md", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "the spec") {
		t.Fatalf("body=%q, want spec content", rec.Body.String())
	}
}

func TestGetTaskArtifact_ByPathNotDeclared(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag",
		"team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	// secret.json exists in the task dir but is NOT declared as an artifact.
	putProject(store, "teams/alpha-team/shared/tasks/t1/secret.json", map[string]any{"secret": "value"})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact?path=shared/tasks/t1/secret.json", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (path not declared as artifact)", rec.Code)
	}
}

func TestGetTaskArtifact_ChineseFilenameEncoding(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path": "shared/tasks/t1/result.md",
	})
	putProject(store, "shared/tasks/t1/result.md", map[string]any{"ok": true})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// result.md is ASCII -> plain filename=result.md (no RFC 5987 needed).
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "result.md") {
		t.Fatalf("Content-Disposition=%q, want filename=result.md", cd)
	}
}

func TestGetTaskArtifact_ChineseDeliverableFilenameRFC5987(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "completed"},
		},
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"result_path":  "shared/tasks/t1/result.md",
		"deliverables": []any{"shared/tasks/t1/季度报告.pdf"},
	})
	putProject(store, "shared/tasks/t1/季度报告.pdf", map[string]any{"report": "chinese content"})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact?path=shared/tasks/t1/季度报告.pdf", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	// RFC 5987: non-ASCII filename must be encoded as filename*=utf-8''...
	if !strings.Contains(cd, "filename*=utf-8''") {
		t.Fatalf("Content-Disposition=%q, want RFC 5987 filename*=utf-8'' for Chinese filename", cd)
	}
	if !strings.Contains(cd, "%E5%AD%A3%E5%BA%A6") { // 季度 in UTF-8 percent-encoding
		t.Fatalf("Content-Disposition=%q, want percent-encoded 季度", cd)
	}
}

// --- spawn aggregation tests (GET /api/v1/projects/{id}/spawns) ---

func workerCR(name, storageName string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{WorkerName: storageName},
	}
}

// putChats writes a worker chats.json into the in-memory store at the
// mirror path agents/{worker}/.qwenpaw/workspaces/default/chats.json.
func putChats(store *ossfake.Memory, worker string, chats []map[string]any) {
	data, _ := json.Marshal(map[string]any{"version": 1, "chats": chats})
	_ = store.PutObject(context.Background(), "agents/"+worker+"/"+workerChatsPath, data)
}

// putTaskMeta writes a TaskMeta object for a task owned by the given team
// ("" writes the global shared/tasks/ prefix).
func putTaskMeta(store *ossfake.Memory, team, taskID string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	// TeamHarness TaskMeta always carries task_id + project_id; default the
	// task_id so test data matches what the ownership checks require.
	if _, ok := fields["task_id"]; !ok {
		fields["task_id"] = taskID
	}
	data, _ := json.Marshal(fields)
	prefix := "shared/tasks/"
	if team != "" {
		prefix = "teams/" + team + "/shared/tasks/"
	}
	_ = store.PutObject(context.Background(), prefix+taskID+"/meta.json", data)
}

func spawnChat(sessionID string, meta map[string]any) map[string]any {
	chat := map[string]any{
		"id":         "c-" + sessionID,
		"name":       "spawn task",
		"session_id": sessionID,
		"user_id":    "default",
		"channel":    "console",
		"status":     "running",
		"source":     "chat",
	}
	if meta != nil {
		chat["meta"] = meta
	}
	return chat
}

// newSpawnTestHandler builds a handler with Teams and Workers in the fake
// K8s client.
func newSpawnTestHandler(t *testing.T, store *ossfake.Memory, objs ...runtime.Object) *ProjectHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	var o oss.StorageClient = &mcLikeOSS{Memory: store}
	return NewProjectHandler(k8s, "default", o)
}

func teamWithWorkers(name string, members ...v1beta1.TeamWorkerRef) *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{TeamName: name, WorkerMembers: members},
	}
}

func TestGetProjectSpawns_AggregatesWorkerSpawns(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "in_progress"},
		},
	})
	// The project's task room ties spawns to this project (root_session_id
	// is the session that called spawn_subagent).
	putTaskMeta(store, "alpha-team", "t1", map[string]any{
		"project_id": "p1", "room_id": "!room:server", "status": "in_progress",
	})
	// 2.1-style data: meta.spawn + meta.root_session_id persisted.
	putChats(store, "alpha-lead", []map[string]any{
		spawnChat("sub-3f2a9b1c", map[string]any{
			"spawn": true, "root_session_id": "matrix:!room:server",
			"subagent_allowed_tools": []any{"read_file", "write_file"},
			"subagent_skills":        []any{"pdf", "xlsx"},
		}),
		{"id": "c-normal", "session_id": "matrix:!room:server", "channel": "matrix", "status": "idle"},
	})
	putChats(store, "alpha-dev", []map[string]any{
		spawnChat("sub-9d1e2f3a", map[string]any{"spawn": true, "root_session_id": "!room:server"}),
	})
	teamCR := teamWithWorkers("alpha-team",
		v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"},
		v1beta1.TeamWorkerRef{Name: "alpha-dev-cr", Role: "worker"},
	)
	h := newSpawnTestHandler(t, store, teamCR,
		workerCR("alpha-lead-cr", "alpha-lead"),
		workerCR("alpha-dev-cr", "alpha-dev"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}})
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ProjectID != "p1" {
		t.Fatalf("project_id=%q, want p1", resp.ProjectID)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("workers=%d, want 2", len(resp.Workers))
	}
	byWorker := map[string]workerSpawns{}
	for _, w := range resp.Workers {
		byWorker[w.Worker] = w
	}
	lead := byWorker["alpha-lead"]
	if len(lead.Spawns) != 1 {
		t.Fatalf("alpha-lead spawns=%d, want 1 (non-spawn chat filtered)", len(lead.Spawns))
	}
	s := lead.Spawns[0]
	if s.SessionID != "sub-3f2a9b1c" || !s.Spawn {
		t.Fatalf("spawn=%+v, want session sub-3f2a9b1c spawn=true", s)
	}
	if s.RootSessionID == nil || *s.RootSessionID != "matrix:!room:server" {
		t.Fatalf("root_session_id=%v, want matrix:!room:server", s.RootSessionID)
	}
	if len(s.AllowedTools) != 2 || s.AllowedTools[0] != "read_file" {
		t.Fatalf("allowed_tools=%v, want [read_file write_file]", s.AllowedTools)
	}
	if len(s.Skills) != 2 || s.Skills[1] != "xlsx" {
		t.Fatalf("skills=%v, want [pdf xlsx]", s.Skills)
	}
	// Bare room id in root_session_id normalizes to the matrix: form.
	dev := byWorker["alpha-dev"].Spawns[0]
	if dev.RootSessionID == nil || *dev.RootSessionID != "matrix:!room:server" {
		t.Fatalf("dev root_session_id=%v, want normalized matrix:!room:server", dev.RootSessionID)
	}
}

func TestGetProjectSpawns_LegacySpawnOmitted(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "status": "in_progress"},
		},
	})
	putTaskMeta(store, "alpha-team", "t1", map[string]any{
		"project_id": "p1", "room_id": "!room:server", "status": "in_progress",
	})
	// 2.0.1-style data: no meta.spawn, no root_session_id — only the sub-
	// prefix identifies the spawn session. Without a root it cannot be
	// associated with any project safely, so it is omitted (reviewer
	// feedback: legacy data must not be attached to every project).
	putChats(store, "alpha-lead", []map[string]any{
		{"id": "c1", "session_id": "sub-legacy01", "channel": "console", "status": "idle"},
	})
	teamCR := teamWithWorkers("alpha-team", v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"})
	h := newSpawnTestHandler(t, store, teamCR, workerCR("alpha-lead-cr", "alpha-lead"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Workers) != 1 || len(resp.Workers[0].Spawns) != 0 {
		t.Fatalf("workers=%+v, want 1 worker with 0 spawns (legacy omitted)", resp.Workers)
	}
}

func TestGetProjectSpawns_NoSpawnsEmptyArray(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putChats(store, "alpha-lead", []map[string]any{
		{"id": "c1", "session_id": "matrix:!room:server", "channel": "matrix", "status": "idle"},
	})
	teamCR := teamWithWorkers("alpha-team", v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"})
	h := newSpawnTestHandler(t, store, teamCR, workerCR("alpha-lead-cr", "alpha-lead"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("workers=%d, want 1", len(resp.Workers))
	}
	if resp.Workers[0].Spawns == nil || len(resp.Workers[0].Spawns) != 0 {
		t.Fatalf("spawns=%v, want empty array (JSON [])", resp.Workers[0].Spawns)
	}
	if !strings.Contains(rec.Body.String(), `"spawns":[]`) {
		t.Fatalf("body=%s, want literal empty array", rec.Body.String())
	}
}

func TestGetProjectSpawns_MissingOrCorruptChatsSkipped(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	// alpha-dev has no chats.json at all; alpha-writer has corrupt JSON.
	_ = store.PutObject(context.Background(), "agents/alpha-writer/"+workerChatsPath, []byte("{not json"))
	teamCR := teamWithWorkers("alpha-team",
		v1beta1.TeamWorkerRef{Name: "alpha-dev-cr", Role: "worker"},
		v1beta1.TeamWorkerRef{Name: "alpha-writer-cr", Role: "worker"},
	)
	h := newSpawnTestHandler(t, store, teamCR,
		workerCR("alpha-dev-cr", "alpha-dev"),
		workerCR("alpha-writer-cr", "alpha-writer"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (missing/corrupt chats skipped, not 500)", rec.Code)
	}
	var resp projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("workers=%d, want 2 (both listed, empty spawns)", len(resp.Workers))
	}
	for _, w := range resp.Workers {
		if len(w.Spawns) != 0 {
			t.Fatalf("worker %s spawns=%d, want 0", w.Worker, len(w.Spawns))
		}
	}
}

func TestGetProjectSpawns_CrossTeamDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/beta-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Beta", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	h := newProjectTestHandler(t, store, team("beta-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p2/spawns", nil)
	req.SetPathValue("id", "p2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for cross-team access (W4)", rec.Code)
	}
}

func TestGetProjectSpawns_TwoProjectsSameTeamIsolated(t *testing.T) {
	store := ossfake.NewMemory()
	// Two projects on the same team, each with its own task room.
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	putProject(store, "teams/alpha-team/shared/projects/p2/meta.json", map[string]any{
		"project_id": "p2", "title": "Beta", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t2", "status": "in_progress"}},
	})
	putTaskMeta(store, "alpha-team", "t1", map[string]any{"project_id": "p1", "room_id": "!room-p1:server"})
	putTaskMeta(store, "alpha-team", "t2", map[string]any{"project_id": "p2", "room_id": "!room-p2:server"})
	putChats(store, "alpha-lead", []map[string]any{
		spawnChat("sub-p1a", map[string]any{"spawn": true, "root_session_id": "matrix:!room-p1:server"}),
		spawnChat("sub-p2a", map[string]any{"spawn": true, "root_session_id": "matrix:!room-p2:server"}),
		spawnChat("sub-other", map[string]any{"spawn": true, "root_session_id": "matrix:!other-room:server"}),
	})
	teamCR := teamWithWorkers("alpha-team", v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"})
	h := newSpawnTestHandler(t, store, teamCR, workerCR("alpha-lead-cr", "alpha-lead"))

	// p1 sees only its own room's spawn.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}})
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("p1 status=%d, want 200", rec.Code)
	}
	var r1 projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r1.Workers) != 1 || len(r1.Workers[0].Spawns) != 1 {
		t.Fatalf("p1 spawns=%+v, want exactly sub-p1a", r1.Workers)
	}
	if r1.Workers[0].Spawns[0].SessionID != "sub-p1a" {
		t.Fatalf("p1 spawn=%q, want sub-p1a", r1.Workers[0].Spawns[0].SessionID)
	}

	// p2 sees only its own room's spawn.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p2/spawns", nil)
	req2.SetPathValue("id", "p2")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}})
	rec2 := httptest.NewRecorder()
	h.GetProjectSpawns(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("p2 status=%d, want 200", rec2.Code)
	}
	var r2 projectSpawnsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &r2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r2.Workers) != 1 || len(r2.Workers[0].Spawns) != 1 {
		t.Fatalf("p2 spawns=%+v, want exactly sub-p2a", r2.Workers)
	}
	if r2.Workers[0].Spawns[0].SessionID != "sub-p2a" {
		t.Fatalf("p2 spawn=%q, want sub-p2a", r2.Workers[0].Spawns[0].SessionID)
	}
}

func TestGetProjectSpawns_SpawnRootMismatchOmitted(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	putTaskMeta(store, "alpha-team", "t1", map[string]any{"project_id": "p1", "room_id": "!room:server"})
	// A spawn whose root belongs to a different room (e.g. another project
	// or a team-wide session) must not attach to this project.
	putChats(store, "alpha-lead", []map[string]any{
		spawnChat("sub-x", map[string]any{"spawn": true, "root_session_id": "matrix:!elsewhere:server"}),
	})
	teamCR := teamWithWorkers("alpha-team", v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"})
	h := newSpawnTestHandler(t, store, teamCR, workerCR("alpha-lead-cr", "alpha-lead"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns", nil)
	req.SetPathValue("id", "p1")
	rec := httptest.NewRecorder()
	h.GetProjectSpawns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp projectSpawnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Workers) != 1 || len(resp.Workers[0].Spawns) != 0 {
		t.Fatalf("spawns=%+v, want 0 (root mismatch omitted)", resp.Workers)
	}
}

func TestGetProjectWorkflow_AmbiguousProjectID(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	putProject(store, "teams/beta-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Beta", "status": "active", "plan_type": "dag", "team_id": "beta-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	// No ?team=: two teams hold p1 → 409 (never a silent first match).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (ambiguous)", rec.Code)
	}

	// ?team=alpha-team disambiguates.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?team=alpha-team", nil)
	req2.SetPathValue("id", "p1")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec2 := httptest.NewRecorder()
	h.GetProjectWorkflow(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 with ?team=alpha-team", rec2.Code)
	}

	// A scoped caller sees only their own team's p1 — no ambiguity.
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow", nil)
	req3.SetPathValue("id", "p1")
	req3 = withCaller(req3, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}})
	rec3 := httptest.NewRecorder()
	h.GetProjectWorkflow(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("scoped status=%d, want 200 (own team's p1)", rec3.Code)
	}
}

func TestTaskDetail_SkipsGlobalFallback(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	// The team owns t1; the global prefix holds an unrelated t1 from another
	// project. The team project must read its own TaskMeta only — the global
	// one (different project_id, different status) must never mix in.
	putTaskMeta(store, "alpha-team", "t1", map[string]any{
		"project_id": "p1", "status": "in_progress", "summary": "team-owned",
	})
	putTaskMeta(store, "", "t1", map[string]any{
		"project_id": "other-project", "status": "completed", "summary": "global-intruder",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "team-owned") {
		t.Fatalf("body=%s, want team-owned task detail", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "global-intruder") {
		t.Fatalf("body=%s, must not contain the global intruder task", rec.Body.String())
	}
}

func TestTaskDetail_MissingTeamMetaNoGlobalLeak(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	// NO team-scoped TaskMeta. The global prefix holds a TaskMeta for the
	// same task id with a MATCHING project_id — the exact leak the reviewer
	// reproduced. With scope-only keys the global entry is never read.
	putTaskMeta(store, "", "t1", map[string]any{
		"project_id": "p1", "status": "completed", "summary": "global-leak",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?includeTasks=true", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "global-leak") {
		t.Fatalf("body=%s, must not leak the global TaskMeta (no team meta)", rec.Body.String())
	}
}

func TestTaskArtifact_TeamProjectNoGlobalFallback(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	// TaskMeta + artifact exist ONLY on the global prefix (e.g. another
	// project's task sharing the id). A team project must not fall back to
	// global: artifact read fails 404.
	putTaskMeta(store, "", "t1", map[string]any{
		"project_id": "other-project", "status": "completed", "result_path": "shared/tasks/t1/result.md",
	})
	_ = store.PutObject(context.Background(), "shared/tasks/t1/result.md", []byte("intruder result"))
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/artifact", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskArtifact(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (no global fallback for team projects)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "intruder") {
		t.Fatalf("body=%s, must not serve the global intruder artifact", rec.Body.String())
	}
}

func TestNormalizeSessionKey_Variants(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"!abc:server:1234", "matrix:!abc:server:1234"},
		{"matrix:!abc:server:1234", "matrix:!abc:server:1234"},
		{"Matrix:!abc:server:1234", "matrix:!abc:server:1234"},
		{"MATRIX:!ABC:server", "matrix:!ABC:server"},
		{"console:user:default", "console:user:default"},
		{"  !abc:server  ", "matrix:!abc:server"},
		{"", ""},
		{"   ", ""},
		{"!bare-no-server", "matrix:!bare-no-server"},
	}
	for _, c := range cases {
		if got := normalizeSessionKey(c.in); got != c.want {
			t.Errorf("normalizeSessionKey(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsSpawnChat_Detection(t *testing.T) {
	cases := []struct {
		chat workerChat
		want bool
	}{
		{workerChat{SessionID: "sub-abc", Meta: map[string]any{"spawn": true}}, true},
		{workerChat{SessionID: "sub-abc"}, true},                                       // 2.0.1 prefix fallback
		{workerChat{SessionID: "c1", Source: "spawn"}, true},                           // future source flag
		{workerChat{SessionID: "matrix:!room:server"}, false},                          // team room chat
		{workerChat{SessionID: "console:user:default"}, false},                         // human console chat
		{workerChat{SessionID: "sub-abc", Meta: map[string]any{"spawn": false}}, true}, // prefix wins
	}
	for i, c := range cases {
		if got := isSpawnChat(c.chat); got != c.want {
			t.Errorf("case %d: isSpawnChat(%+v)=%v, want %v", i, c.chat, got, c.want)
		}
	}
}

// --- spawn messages (GET /api/v1/projects/{id}/spawns/{sessionId}/messages) ---

// makeHistoryDBBytes builds a real SQLite history.db (v2.0.1/2.1
// conversation_history schema) in a temp dir and returns its bytes, so the
// in-memory OSS mock serves what the worker FileSync would mirror.
func makeHistoryDBBytes(t *testing.T, rows []historyRow) []byte {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "history.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open temp sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE conversation_history (
			seq          INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id   TEXT NOT NULL,
			agent_id     TEXT,
			kind         TEXT NOT NULL,
			role         TEXT,
			name         TEXT,
			content      TEXT,
			tool_call_id TEXT,
			tool_input   TEXT,
			tool_state   TEXT,
			headline     TEXT,
			blocks       TEXT,
			metadata     TEXT,
			created_at   TEXT,
			dedup_key    TEXT
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(
			"INSERT INTO conversation_history (session_id, kind, role, name, content, tool_state, headline, created_at) VALUES (?,?,?,?,?,?,?,?)",
			r.sessionID, r.kind, r.role, r.name, r.content, r.toolState, r.headline, r.createdAt,
		); err != nil {
			t.Fatalf("insert row: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close temp sqlite: %v", err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read temp sqlite: %v", err)
	}
	return data
}

type historyRow struct {
	sessionID string
	kind      string
	role      string
	name      string
	content   string
	toolState string
	headline  string
	createdAt string
}

func putHistory(store *ossfake.Memory, worker string, data []byte) {
	_ = store.PutObject(context.Background(), "agents/"+worker+"/"+workerHistoryDBPath, data)
}

// spawnMessagesRequest drives GET /api/v1/projects/{id}/spawns/{sid}/messages
// through the handler and returns the recorder.
func spawnMessagesRequest(h *ProjectHandler, caller *authpkg.CallerIdentity, projectID, sessionID, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+projectID+"/spawns/"+sessionID+"/messages"+query, nil)
	req.SetPathValue("id", projectID)
	req.SetPathValue("sessionId", sessionID)
	req = withCaller(req, caller)
	rec := httptest.NewRecorder()
	h.GetProjectSpawnMessages(rec, req)
	return rec
}

func spawnMsgEnv(t *testing.T, history []byte) (*ProjectHandler, *ossfake.Memory) {
	t.Helper()
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "Alpha", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
		"tasks": []map[string]any{{"task_id": "t1", "status": "in_progress"}},
	})
	putTaskMeta(store, "alpha-team", "t1", map[string]any{
		"project_id": "p1", "room_id": "!room:server", "status": "in_progress",
	})
	putChats(store, "alpha-lead", []map[string]any{
		spawnChat("sub-3f2a9b1c", map[string]any{"spawn": true, "root_session_id": "matrix:!room:server"}),
	})
	if history != nil {
		putHistory(store, "alpha-lead", history)
	}
	teamCR := teamWithWorkers("alpha-team", v1beta1.TeamWorkerRef{Name: "alpha-lead-cr", Role: "team_leader"})
	h := newSpawnTestHandler(t, store, teamCR, workerCR("alpha-lead-cr", "alpha-lead"))
	return h, store
}

func humanCaller() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"alpha-team"}}
}

func TestGetProjectSpawnMessages_ReturnsStreamAndTask(t *testing.T) {
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
		// First user message carries the spawn task text.
		{kind: "context_msg", role: "user", content: "Design the auth module", sessionID: "sub-3f2a9b1c"},
		{kind: "model_turn", role: "assistant", name: "read_file", content: "Reading the spec first", headline: "read spec", sessionID: "sub-3f2a9b1c"},
		{kind: "tool_result", role: "assistant", name: "read_file", content: "spec.md contents", toolState: "success", sessionID: "sub-3f2a9b1c"},
		{kind: "model_turn", role: "assistant", content: "Drafting the design", sessionID: "sub-3f2a9b1c"},
		// Another session must not leak.
		{kind: "model_turn", role: "assistant", content: "other session", sessionID: "sub-other00"},
	}))

	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp spawnMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SessionID != "sub-3f2a9b1c" {
		t.Fatalf("session_id=%q", resp.SessionID)
	}
	if resp.Task != "Design the auth module" {
		t.Fatalf("task=%q, want first user message", resp.Task)
	}
	if resp.HasMore {
		t.Fatalf("has_more=true, want false (4 rows, limit 20)")
	}
	if len(resp.Messages) != 4 {
		t.Fatalf("messages=%d, want 4", len(resp.Messages))
	}
	// Ascending order, first row is the task message.
	if resp.Messages[0].Kind != "context_msg" || resp.Messages[0].Role != "user" {
		t.Fatalf("messages[0]=%+v, want user context_msg", resp.Messages[0])
	}
	// Tool-result frame exposes the tool name and state.
	tr := resp.Messages[2]
	if tr.Kind != "tool_result" || tr.Name != "read_file" || tr.ToolState != "success" {
		t.Fatalf("tool result=%+v, want kind=tool_result name=read_file tool_state=success", tr)
	}
	// Model turn carries its headline.
	if resp.Messages[1].Headline != "read spec" {
		t.Fatalf("headline=%q, want read spec", resp.Messages[1].Headline)
	}
}

func TestGetProjectSpawnMessages_LimitAndHasMore(t *testing.T) {
	rows := make([]historyRow, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, historyRow{
			kind: "tool_result", role: "assistant", name: "execute_shell_command",
			content: fmt.Sprintf("step %d", i), toolState: "success",
			sessionID: "sub-3f2a9b1c",
		})
	}
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, rows))

	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "?limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp spawnMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Messages) != 5 {
		t.Fatalf("messages=%d, want 5", len(resp.Messages))
	}
	if !resp.HasMore {
		t.Fatalf("has_more=false, want true (12 rows, limit 5)")
	}
	// The newest 5, in ascending order: steps 7..11.
	if resp.Messages[0].Content != "step 7" || resp.Messages[4].Content != "step 11" {
		t.Fatalf("window=%q..%q, want step 7..step 11", resp.Messages[0].Content, resp.Messages[4].Content)
	}
}

func TestGetProjectSpawnMessages_LimitCappedAt50(t *testing.T) {
	rows := make([]historyRow, 0, 55)
	for i := 0; i < 55; i++ {
		rows = append(rows, historyRow{kind: "tool_result", role: "assistant", name: "t", content: fmt.Sprintf("s%d", i), sessionID: "sub-3f2a9b1c"})
	}
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, rows))

	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "?limit=999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp spawnMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Messages) != 50 {
		t.Fatalf("messages=%d, want 50 (cap)", len(resp.Messages))
	}
	if !resp.HasMore {
		t.Fatalf("has_more=false, want true (55 rows, capped 50)")
	}
}

func TestGetProjectSpawnMessages_HistoryMissing404(t *testing.T) {
	h, _ := spawnMsgEnv(t, nil) // chats.json present, history.db absent
	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (history.db missing)", rec.Code)
	}
}

func TestGetProjectSpawnMessages_CorruptHistory404(t *testing.T) {
	h, _ := spawnMsgEnv(t, []byte("this is not a sqlite database"))
	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (corrupt db, never 500)", rec.Code)
	}
}

func TestGetProjectSpawnMessages_EmptySession(t *testing.T) {
	// Valid db, but no rows for this session: 200 with an empty array.
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
		{kind: "model_turn", role: "assistant", content: "unrelated", sessionID: "sub-other99"},
	}))
	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-3f2a9b1c", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var resp spawnMessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Messages == nil || len(resp.Messages) != 0 {
		t.Fatalf("messages=%v, want empty array (not null)", resp.Messages)
	}
	if resp.Task != "" || resp.HasMore {
		t.Fatalf("task=%q has_more=%v, want empty/false", resp.Task, resp.HasMore)
	}
}

func TestGetProjectSpawnMessages_SpawnNotInProject404(t *testing.T) {
	h, store := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
		{kind: "context_msg", role: "user", content: "task", sessionID: "sub-3f2a9b1c"},
	}))
	// A second spawn owned by the same worker but rooted in a room that does
	// not belong to this project: hidden as 404.
	putChats(store, "alpha-lead", []map[string]any{
		spawnChat("sub-3f2a9b1c", map[string]any{"spawn": true, "root_session_id": "matrix:!room:server"}),
		spawnChat("sub-other", map[string]any{"spawn": true, "root_session_id": "matrix:!elsewhere:server"}),
	})
	rec := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spawns/sub-other/messages", nil)
	rec.SetPathValue("id", "p1")
	rec.SetPathValue("sessionId", "sub-other")
	rec = withCaller(rec, humanCaller())
	w := httptest.NewRecorder()
	h.GetProjectSpawnMessages(w, rec)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (spawn not in project rooms)", w.Code)
	}
}

func TestGetProjectSpawnMessages_CrossTeamDenied(t *testing.T) {
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
		{kind: "context_msg", role: "user", content: "task", sessionID: "sub-3f2a9b1c"},
	}))
	// A human scoped to another team sees 404, not 403 (existence hidden).
	rec := spawnMessagesRequest(h, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "mallory", Teams: []string{"beta-team"}}, "p1", "sub-3f2a9b1c", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (cross-team hidden)", rec.Code)
	}
}

func TestGetProjectSpawnMessages_SessionNotFound(t *testing.T) {
	h, _ := spawnMsgEnv(t, makeHistoryDBBytes(t, []historyRow{
		{kind: "context_msg", role: "user", content: "task", sessionID: "sub-3f2a9b1c"},
	}))
	// A session id no team worker owns: 404 (same hiding rationale).
	rec := spawnMessagesRequest(h, humanCaller(), "p1", "sub-unknown", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (session not owned by any worker)", rec.Code)
	}
}

// =====================================================================// the lifecycle write API write endpoints (pause / resume / replan / create / cancel / complete)
// ==============================================================// putTask writes a TaskMeta object for cancel-task tests.
func putTask(store *ossfake.Memory, key string, meta map[string]any) {
	data, _ := json.Marshal(meta)
	_ = store.PutObject(context.Background(), key, data)
}

func TestPauseProject_ActiveToPaused(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", strings.NewReader(`{"reason":"customer review"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Verify the MinIO write-back: status=paused + audit fields.
	data, err := store.GetObject(context.Background(), "teams/alpha-team/shared/projects/p1/meta.json")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if meta["status"] != "paused" {
		t.Fatalf("status=%v, want paused", meta["status"])
	}
	if meta["updated_by"] != "admin" {
		t.Fatalf("updated_by=%v, want admin", meta["updated_by"])
	}
	if meta["updated_at"] == "" {
		t.Fatalf("updated_at missing")
	}
	if meta["pause_reason"] != "customer review" {
		t.Fatalf("pause_reason=%v, want customer review", meta["pause_reason"])
	}
}

func TestPauseProject_AlreadyPaused409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "paused", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (already paused)", rec.Code)
	}
}

func TestPauseProject_NotFound(t *testing.T) {
	h := newProjectTestHandler(t, ossfake.NewMemory())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/nope/pause", nil)
	req.SetPathValue("id", "nope")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// TestPauseProject_MtimeConflict guards the optimistic lock: a worker
// _sync_project push after the Controller read (before the write) must abort
// the write with 409. The ossfake Memory advances its modTime on every
// PutObject, so writing between read and write-back triggers the conflict.
func TestPauseProject_EtagConflict(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	// The handler binds the read to its ETag, then performs a conditional
	// write. Inject a "worker push" on the second StatMeta call (the
	// conditional write's stat): the object's ETag changes, so the
	// If-Match write fails -> 409.
	// The wrapper embeds mcLikeOSS so ListObjects keeps the mc ls semantics
	// (direct child names) that resolveProjectMetaWithKey depends on.
	injector := &conflictInjectingOSS{mcLike: &mcLikeOSS{Memory: store}, injectOnGet: 1}
	h := newProjectTestHandlerWithOSS(t, injector)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409 (mtime conflict)", rec.Code, rec.Body.String())
	}
}

func TestResumeProject_PausedToActive(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "paused", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/resume", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.ResumeProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "teams/alpha-team/shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	if meta["status"] != "active" {
		t.Fatalf("status=%v, want active", meta["status"])
	}
}

func TestResumeProject_NotPaused409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/resume", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ResumeProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (not paused)", rec.Code)
	}
}

// TestPauseProject_L2CrossTeamDenied guards B2c: an L2 human must be denied
// (404, existence hidden) when pausing a project outside their
// accessibleTeams.
func TestPauseProject_L2CrossTeamDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"beta-team"}})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (cross-team existence hidden)", rec.Code)
	}
}

// TestPauseProject_L2SameTeamAllowed guards B2c: an L2 human may pause a
// project in their accessibleTeams.
func TestPauseProject_L2SameTeamAllowed(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"alpha-team", "beta-team"}})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (L2 same-team allowed)", rec.Code, rec.Body.String())
	}
}

func TestReplanProject_ReplaceTasks(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Old T1", "assigned_to": "dev1", "status": "planned", "depends_on": []string{}},
			{"task_id": "t2", "title": "Old T2", "assigned_to": "dev2", "status": "planned", "depends_on": []string{"t1"}},
		},
	})
	h := newProjectTestHandler(t, store)

	body := `{"tasks":[
		{"taskId":"t1","title":"New T1"},
		{"taskId":"t3","title":"T3","assignedTo":"dev3","dependsOn":["t1"]}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	tasks := meta["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks=%v, want 2 after replan", tasks)
	}
	// previous preservation: t1 keeps old title when not re-specified? No —
	// the body DID specify title New T1, so it must be New T1.
	first := tasks[0].(map[string]any)
	if first["title"] != "New T1" || first["status"] != "planned" {
		t.Fatalf("task t1=%v, want New T1/planned", first)
	}
	second := tasks[1].(map[string]any)
	if second["title"] != "T3" || second["assigned_to"] != "dev3" {
		t.Fatalf("task t3=%v, want T3/dev3", second)
	}
}

// TestReplanProject_PreservesPrevious guards _normalize_task parity: a task id
// that already exists keeps its previous title/assigned/status when the raw
// entry omits them.
func TestReplanProject_PreservesPrevious(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Original Title", "assigned_to": "dev1", "status": "assigned", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store)

	body := `{"tasks":[{"taskId":"t1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	tasks := meta["tasks"].([]any)
	first := tasks[0].(map[string]any)
	if first["title"] != "Original Title" || first["assigned_to"] != "dev1" || first["status"] != "assigned" {
		t.Fatalf("preserved task=%v, want Original Title/dev1/assigned", first)
	}
}

func TestReplanProject_DuplicateID400(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	body := `{"tasks":[{"taskId":"t1"},{"taskId":"t1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (duplicate id)", rec.Code)
	}
}

// TestReplanProject_UnsafeTaskID400 guards _safe_id parity: the TeamHarness
// reference rejects task ids outside [A-Za-z0-9][A-Za-z0-9._-]* because ids
// become TaskMeta object-key segments (shared/tasks/{id}/meta.json).
func TestReplanProject_UnsafeTaskID400(t *testing.T) {
	cases := []string{
		"task with space",
		"../escape",
		"t/1",
		"-leading-dash",
		".leading-dot",
		"任务一",
	}
	for _, tc := range cases {
		store := ossfake.NewMemory()
		putProject(store, "shared/projects/p1/meta.json", map[string]any{
			"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "tasks": []map[string]any{},
		})
		h := newProjectTestHandler(t, store)
		body := `{"tasks":[{"taskId":"` + tc + `","title":"X"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
		req.SetPathValue("id", "p1")
		req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
		rec := httptest.NewRecorder()
		h.ReplanProject(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("taskId %q: status=%d, want 400 (unsafe id)", tc, rec.Code)
		}
	}
}

// TestReplanProject_SafeTaskIDAccepted guards the positive side of the
// _safe_id rule: ids mixing letters, digits, dots, dashes and underscores
// (with a leading alphanumeric) remain valid.
func TestReplanProject_SafeTaskIDAccepted(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	body := `{"tasks":[{"taskId":"t-1.x_2","title":"X"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for safe id", rec.Code, rec.Body.String())
	}
}

func TestReplanProject_UnknownDependency400(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	body := `{"tasks":[{"taskId":"t1","dependsOn":["ghost"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (unknown dependency)", rec.Code)
	}
}

func TestReplanProject_Cycle400(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	body := `{"tasks":[{"taskId":"t1","dependsOn":["t2"]},{"taskId":"t2","dependsOn":["t1"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(body))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (cycle)", rec.Code)
	}
}

func TestReplanProject_NotDag409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "loop",
		"loop": map[string]any{"goal": "g", "tasks": []map[string]any{}},
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(`{"tasks":[]}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (loop plan)", rec.Code)
	}
}

func TestReplanProject_NotActive409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "paused", "plan_type": "dag", "tasks": []map[string]any{},
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(`{"tasks":[]}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (not active)", rec.Code)
	}
}

func TestReplanProject_InFlight409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/replan", strings.NewReader(`{"tasks":[{"taskId":"t1"}]}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.ReplanProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (in-flight)", rec.Code)
	}
}

func TestCreateProject_Admin(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	body := `{"title":"New Project","source":"matrix","requester":"@luo:server","team_id":"alpha-team"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProjectID == "" || resp.Status != "active" {
		t.Fatalf("resp=%+v, want generated id + active", resp)
	}
	// Verify the meta exists under the team-scoped prefix.
	data, err := store.GetObject(context.Background(), "teams/alpha-team/shared/projects/"+resp.ProjectID+"/meta.json")
	if err != nil {
		t.Fatalf("created meta not found: %v", err)
	}
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	if meta["status"] != "active" || meta["title"] != "New Project" || meta["team_id"] != "alpha-team" {
		t.Fatalf("meta=%v", meta)
	}
}

func TestCreateProject_L2CrossTeamDenied(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))

	// L2 with only beta-team accessible tries to create in alpha-team.
	body := `{"title":"X","team_id":"alpha-team"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"beta-team"}})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (cross-team create denied)", rec.Code)
	}
}

func TestCreateProject_Duplicate409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store)
	body := `{"title":"Dup","project_id":"p1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (duplicate id)", rec.Code)
	}
}

func TestCancelTask_ActiveTask(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}},
		},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "planned",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel", strings.NewReader(`{"reason":"no longer needed","replacementTaskId":"t9"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	taskData, _ := store.GetObject(context.Background(), "shared/tasks/t1/meta.json")
	var task map[string]any
	_ = json.Unmarshal(taskData, &task)
	if task["status"] != "cancelled" || task["cancel_reason"] != "no longer needed" || task["replacement_task_id"] != "t9" {
		t.Fatalf("task=%v, want cancelled/reason/t9", task)
	}
	projData, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var proj map[string]any
	_ = json.Unmarshal(projData, &proj)
	tasks := proj["tasks"].([]any)
	if tasks[0].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("project node status=%v, want cancelled", tasks[0].(map[string]any)["status"])
	}
}

func TestCancelTask_Terminal409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "completed", "depends_on": []string{}},
		},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel", strings.NewReader(`{"reason":"x"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (terminal task)", rec.Code)
	}
}

func TestCancelTask_NoReason400(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "planned", "depends_on": []string{}},
		},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "planned",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel", strings.NewReader(`{}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (reason required)", rec.Code)
	}
}

func TestCompleteProject_AllTerminal(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "completed", "depends_on": []string{}},
			{"task_id": "t2", "title": "T2", "status": "cancelled", "depends_on": []string{"t1"}},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/complete", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CompleteProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var meta map[string]any
	_ = json.Unmarshal(data, &meta)
	if meta["status"] != "completed" {
		t.Fatalf("status=%v, want completed", meta["status"])
	}
}

func TestCompleteProject_NonTerminal409(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/complete", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CompleteProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (non-terminal)", rec.Code)
	}
}

// TestPauseProject_WorkerDenied guards RBAC: a Worker caller has no project
// case in the authorizer and must be denied by middleware — this test asserts
// the handler-level access check also denies a worker (defense in depth when
// the handler is invoked directly, as in tests).
func TestPauseProject_WorkerDenied(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: "alpha-team"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	// checkProjectAccess only scopes TeamLeader/Human; a Worker passes the
	// handler check (it is not in the scoped-reader set). The authorizer is
	// the Worker boundary — this test documents the handler's defense in
	// depth is the authorizer (middleware), not checkProjectAccess.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s — handler checkProjectAccess intentionally allows workers; the authorizer middleware is the Worker boundary", rec.Code, rec.Body.String())
	}
}

// newProjectTestHandlerWithOSS builds a handler with a custom StorageClient
// (e.g. a wrapper that injects concurrent writes).
func newProjectTestHandlerWithOSS(t *testing.T, o oss.StorageClient) *ProjectHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).Build()
	return NewProjectHandler(k8s, "default", o)
}

// conflictInjectingOSS wraps a Memory store (via mcLikeOSS for ListObjects
// semantics) and, after the Nth GetObject call, writes a concurrent version
// of the object — simulating a worker _sync_project push landing between the
// Controller's read and its conditional write. The Controller binds its
// If-Match ETag to the bytes it read, so the injected write changes the
// remote ETag and the write fails with 409.
type conflictInjectingOSS struct {
	mcLike      *mcLikeOSS
	injectOnGet int
	getCalls    int
}

func (c *conflictInjectingOSS) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	return c.mcLike.ListObjects(ctx, prefix)
}

func (c *conflictInjectingOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	data, err := c.mcLike.GetObject(ctx, key)
	c.getCalls++
	if err == nil && c.getCalls == c.injectOnGet && strings.HasSuffix(key, "/meta.json") {
		// Simulate a worker push between the Controller's read and write.
		_ = c.mcLike.PutObject(ctx, key, []byte(`{"project_id":"p1","title":"P1","status":"active","concurrent":true}`))
	}
	return data, err
}

func (c *conflictInjectingOSS) PutObject(ctx context.Context, key string, data []byte) error {
	return c.mcLike.PutObject(ctx, key, data)
}

// PutObjectIfMatch simulates the conditional write. When injectOnStat has
// been reached (the concurrent-write simulation fired on StatMeta), the
// underlying object's ETag no longer matches the read-time ETag, so the
// conditional write fails — exactly like the MinIO backend would.
func (c *conflictInjectingOSS) PutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error {
	cur, err := c.mcLike.StatMeta(ctx, key)
	if err != nil {
		return err
	}
	if cur.ETag != matchETag {
		return oss.ErrPreconditionFailed
	}
	return c.mcLike.PutObject(ctx, key, data)
}

func (c *conflictInjectingOSS) Stat(ctx context.Context, key string) error {
	return c.mcLike.Stat(ctx, key)
}

func (c *conflictInjectingOSS) PutFile(ctx context.Context, localPath, key string) error {
	return c.mcLike.PutFile(ctx, localPath, key)
}

func (c *conflictInjectingOSS) DeleteObject(ctx context.Context, key string) error {
	return c.mcLike.DeleteObject(ctx, key)
}

func (c *conflictInjectingOSS) Mirror(ctx context.Context, src, dst string, opts oss.MirrorOptions) error {
	return c.mcLike.Mirror(ctx, src, dst, opts)
}

func (c *conflictInjectingOSS) DeletePrefix(ctx context.Context, prefix string) error {
	return c.mcLike.DeletePrefix(ctx, prefix)
}

func (c *conflictInjectingOSS) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	return c.mcLike.StatMeta(ctx, key)
}

// --- project history snapshot tests (writeProjectMeta timeline) ---

// bareLikeOSS mimics mc ls returning bare child names for BOTH files and
// directories (mcLikeOSS only surfaces directories, which history gc needs
// files for).
type bareLikeOSS struct {
	*ossfake.Memory
}

func (m *bareLikeOSS) ListObjects(_ context.Context, prefix string) ([]string, error) {
	keys, err := m.Memory.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	// mc ls semantics: direct children only — directory children keep the
	// trailing slash ("p1/"), file children stay bare ("t1.json").
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		if rest == "" {
			continue
		}
		parts := strings.SplitN(rest, "/", 2)
		child := parts[0]
		if len(parts) > 1 && parts[1] != "" {
			child = parts[0] + "/"
		}
		if !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out, nil
}

func newSnapshotTestHandler(t *testing.T, store *ossfake.Memory, objs ...runtime.Object) *ProjectHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	var o oss.StorageClient = &bareLikeOSS{Memory: store}
	return NewProjectHandler(k8s, "default", o)
}

func historyPrefixFor(key string) string {
	return strings.TrimSuffix(key, "meta.json") + "history/"
}

func TestWriteProjectMeta_SnapshotsPreviousVersion(t *testing.T) {
	store := ossfake.NewMemory()
	key := "teams/alpha-team/shared/projects/p1/meta.json"
	putProject(store, key, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	h := newSnapshotTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", strings.NewReader(`{"reason":"human review"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Exactly one snapshot, holding the PRE-intervention version.
	children, err := store.ListObjects(context.Background(), historyPrefixFor(key))
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("history entries=%d, want 1", len(children))
	}
	snap, err := store.GetObject(context.Background(), children[0])
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var oldMeta map[string]any
	if err := json.Unmarshal(snap, &oldMeta); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if oldMeta["status"] != "active" {
		t.Fatalf("snapshot status=%v, want active (pre-intervention version)", oldMeta["status"])
	}
	// Main meta is the new version.
	data, _ := store.GetObject(context.Background(), key)
	var newMeta map[string]any
	_ = json.Unmarshal(data, &newMeta)
	if newMeta["status"] != "paused" {
		t.Fatalf("main meta status=%v, want paused", newMeta["status"])
	}
}

func TestWriteProjectMeta_SnapshotGCKeepsLimit(t *testing.T) {
	store := ossfake.NewMemory()
	key := "teams/alpha-team/shared/projects/p1/meta.json"
	putProject(store, key, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	// Pre-fill 50 history entries (zero-padded so lexical == chronological).
	for i := 1; i <= projectHistoryLimit; i++ {
		_ = store.PutObject(context.Background(), historyPrefixFor(key)+fmt.Sprintf("%020d.json", i), []byte("{}"))
	}
	h := newSnapshotTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", strings.NewReader(`{"reason":"gc"}`))
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	children, err := store.ListObjects(context.Background(), historyPrefixFor(key))
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(children) != projectHistoryLimit {
		t.Fatalf("history entries=%d, want %d (gc kept limit)", len(children), projectHistoryLimit)
	}
	// Oldest (t000...001.json) must be gone.
	if _, err := store.GetObject(context.Background(), historyPrefixFor(key)+fmt.Sprintf("%020d.json", 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest snapshot still present, want gc'd")
	}
}

func TestWriteProjectMeta_SnapshotBestEffort(t *testing.T) {
	store := ossfake.NewMemory()
	key := "teams/alpha-team/shared/projects/p1/meta.json"
	putProject(store, key, map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag", "team_id": "alpha-team",
	})
	// Snapshot with an OSS whose GetObject always fails: the history write is
	// skipped silently — no panic, no entries — and the main flow is
	// unaffected (the snapshot caller is best-effort by contract).
	h := newSnapshotTestHandler(t, store, team("alpha-team"))
	failing := &ProjectHandler{client: h.client, namespace: h.namespace, oss: &snapshotFailGet{StorageClient: h.oss}}
	failing.snapshotProjectMeta(context.Background(), key)

	children, _ := store.ListObjects(context.Background(), historyPrefixFor(key))
	if len(children) != 0 {
		t.Fatalf("history entries=%d, want 0 (read failure skips snapshot)", len(children))
	}
}

// snapshotFailGet wraps a StorageClient and fails every GetObject, so the
// snapshot's read path silently no-ops (best-effort contract).
type snapshotFailGet struct {
	oss.StorageClient
}

func (f *snapshotFailGet) GetObject(ctx context.Context, key string) ([]byte, error) {
	return nil, errors.New("get failed")
}

func TestCreateProject_NoSnapshot(t *testing.T) {
	store := ossfake.NewMemory()
	h := newSnapshotTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"title":"fresh","team_id":"alpha-team"}`))
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)

	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Fresh project must have no history entries (creation is not a
	// snapshot point — there is nothing to snapshot).
	keys, _ := store.ListObjects(context.Background(), "teams/alpha-team/shared/projects/")
	for _, k := range keys {
		if strings.Contains(k, "/history/") {
			t.Fatalf("fresh project has history entry: %s", k)
		}
	}
}

func TestTeamHarnessSafeProjectID_Contract(t *testing.T) {
	// Contract with TeamHarness _safe_id: [A-Za-z0-9][A-Za-z0-9._-]*.
	// An RFC3339 timestamp (with ':') would be rejected upstream.
	re := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := teamHarnessSafeProjectID()
		if !re.MatchString(id) {
			t.Fatalf("generated id %q does not match TeamHarness _safe_id contract", id)
		}
		if seen[id] {
			t.Fatalf("generated id %q collided", id)
		}
		seen[id] = true
	}
}

func TestCreateProject_DefaultIDSafeForTeamHarness(t *testing.T) {
	store := ossfake.NewMemory()
	h := newProjectTestHandler(t, store, team("alpha-team"))

	body := strings.NewReader(`{"title":"Auto ID"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", body)
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CreateProject(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := resp["project_id"].(string)
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(id) {
		t.Fatalf("default project id %q not TeamHarness-safe", id)
	}
	if strings.Contains(id, ":") {
		t.Fatalf("default project id %q contains ':' (RFC3339 leak)", id)
	}
}

func TestPauseProject_EtagConflictOnConcurrentWrite(t *testing.T) {
	// A Worker write landing between the Controller's read (ETag E0) and its
	// conditional write changes the ETag → the write fails with 409 instead
	// of overwriting. The conflictInjectingOSS wrapper injects the
	// concurrent write on the second StatMeta call (the read-time stat is
	// the first), so the conditional write sees a mismatching ETag.
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
	})
	injector := &conflictInjectingOSS{mcLike: &mcLikeOSS{Memory: store}, injectOnGet: 1}
	h := newProjectTestHandlerWithOSS(t, injector)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.PauseProject(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (etag conflict)", rec.Code)
	}
}

func TestCancelTask_ProjectWriteFailureLeavesTaskUntouched(t *testing.T) {
	// Reviewer failure-injection case: if the project write conflicts, the
	// task meta must NOT already be cancelled — the old order (task first)
	// left task=cancelled while the project node kept its old status.
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}}},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "in_progress",
	})
	injector := &conflictInjectingOSS{mcLike: &mcLikeOSS{Memory: store}, injectOnGet: 1}
	h := newProjectTestHandlerWithOSS(t, injector)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel",
		strings.NewReader(`{"reason":"obsolete"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 (project write conflict)", rec.Code)
	}

	// Task meta untouched by the failed request.
	taskData, _ := store.GetObject(context.Background(), "shared/tasks/t1/meta.json")
	var task map[string]any
	_ = json.Unmarshal(taskData, &task)
	if task["status"] != "in_progress" {
		t.Fatalf("task status=%v, want in_progress (untouched on project conflict)", task["status"])
	}
}

// failTaskPutOSS wraps a StorageClient and fails the next N PutObject calls
// for a specific key prefix — simulating a task-meta write failure after the
// project write already succeeded (reviewer failure-injection case).
type failTaskPutOSS struct {
	oss.StorageClient
	failPrefix string
	failures   int
}

func (f *failTaskPutOSS) PutObject(ctx context.Context, key string, data []byte) error {
	if f.failures > 0 && strings.HasPrefix(key, f.failPrefix) {
		f.failures--
		return errors.New("injected task-meta write failure")
	}
	return f.StorageClient.PutObject(ctx, key, data)
}

func TestCancelTask_RetryConvergesBothObjects(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P1", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{{"task_id": "t1", "title": "T1", "status": "in_progress", "depends_on": []string{}}},
	})
	putTask(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "in_progress",
	})

	// First attempt: the project write succeeds, the task-meta write fails.
	failFirst := &failTaskPutOSS{StorageClient: &mcLikeOSS{Memory: store}, failPrefix: "shared/tasks/t1/", failures: 1}
	h := newProjectTestHandlerWithOSS(t, failFirst)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel",
		strings.NewReader(`{"reason":"obsolete"}`))
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.CancelTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first attempt status=%d, want 500 (injected task-meta write failure)", rec.Code)
	}

	// Project node is cancelled after the first attempt; task meta is not.
	projData, _ := store.GetObject(context.Background(), "shared/projects/p1/meta.json")
	var proj map[string]any
	_ = json.Unmarshal(projData, &proj)
	tasks, _ := proj["tasks"].([]any)
	if tasks[0].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("project node status=%v, want cancelled after first attempt", tasks[0].(map[string]any)["status"])
	}
	taskData, _ := store.GetObject(context.Background(), "shared/tasks/t1/meta.json")
	var task map[string]any
	_ = json.Unmarshal(taskData, &task)
	if task["status"] != "in_progress" {
		t.Fatalf("task status=%v, want in_progress after failed task-meta write", task["status"])
	}

	// Retry: the node is already cancelled, so the retry converges by
	// re-writing the idempotent task meta — no 409 at the terminal check.
	h2 := newProjectTestHandler(t, store)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/cancel",
		strings.NewReader(`{"reason":"obsolete"}`))
	req2.SetPathValue("id", "p1")
	req2.SetPathValue("taskId", "t1")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec2 := httptest.NewRecorder()
	h2.CancelTask(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s, want 200 (converged)", rec2.Code, rec2.Body.String())
	}

	// Both objects converged.
	taskData2, _ := store.GetObject(context.Background(), "shared/tasks/t1/meta.json")
	var task2 map[string]any
	_ = json.Unmarshal(taskData2, &task2)
	if task2["status"] != "cancelled" || task2["cancel_reason"] != "obsolete" {
		t.Fatalf("task=%v, want cancelled + reason after retry", task2)
	}
}

// --- workflow ?format=mermaid ---

func TestGetProjectWorkflow_MermaidFormat(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
			{"task_id": "t2", "title": "Task 2", "assigned_to": "@w2", "depends_on": []string{"t1"}, "status": "assigned"},
			{"task_id": "t3", "title": "Task 3", "assigned_to": "@w3", "depends_on": []string{"t2"}, "status": "planned"},
		},
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?format=mermaid", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type=%q, want text/plain", ct)
	}
	out := rec.Body.String()
	if !strings.HasPrefix(out, "flowchart LR") {
		t.Fatalf("expected flowchart header, got %q", out)
	}
	if !strings.Contains(out, `t1["Task 1: completed"]:::completed`) {
		t.Fatalf("expected t1 with completed class, got %q", out)
	}
	// t2 is the only ready node: ready class overrides its delegated class.
	if !strings.Contains(out, `t2["Task 2: delegated"]:::ready`) {
		t.Fatalf("expected ready class on t2, got %q", out)
	}
	if !strings.Contains(out, `t3["Task 3: pending"]:::pending`) {
		t.Fatalf("expected t3 with pending class, got %q", out)
	}
	if !strings.Contains(out, "t1 --> t2") || !strings.Contains(out, "t2 --> t3") {
		t.Fatalf("missing edges, got %q", out)
	}
	for _, cd := range []string{
		"classDef ready", "classDef pending", "classDef delegated", "classDef completed", "classDef blocked",
	} {
		if !strings.Contains(out, cd) {
			t.Fatalf("expected %q in output, got %q", cd, out)
		}
	}
}

func TestGetProjectWorkflow_MermaidFormat_Invalid(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/workflow?format=bogus", nil)
	req.SetPathValue("id", "p1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetProjectWorkflow(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

// --- GET /api/v1/projects/{id}/tasks/{taskId} (node-level inspection) ---

func TestGetTaskInspection(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
			{"task_id": "t2", "title": "Task 2", "assigned_to": "@w2", "depends_on": []string{"t1"}, "status": "assigned"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t1/meta.json", map[string]any{
		"task_id":       "t1",
		"project_id":    "p1",
		"status":        "completed",
		"spec_path":     "shared/tasks/t1/spec.md",
		"assigned_to":   "@w1",
		"summary":       "Alpha report done",
		"result_status": "SUCCESS",
		"result_path":   "shared/tasks/t1/result.md",
		"history": []any{
			map[string]any{"ts": "2026-09-05T01:00:00Z", "from": "", "to": "planned", "actor": "manager", "action": "create"},
			map[string]any{"ts": "2026-09-05T02:00:00Z", "from": "in_progress", "to": "submitted", "actor": "w1", "action": "submit_task"},
			"malformed entry must be skipped",
			map[string]any{"note": "no ts and no action — skipped"},
		},
	})
	putProject(store, "teams/alpha-team/shared/tasks/t2/meta.json", map[string]any{
		"task_id": "t2", "project_id": "p1", "status": "assigned",
		"spec_path": "shared/tasks/t2/spec.md", "assigned_to": "@w2",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	// t1: full detail + parsed history (malformed entries skipped) + trace.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("t1 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var insp taskInspection
	if err := json.Unmarshal(rec.Body.Bytes(), &insp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if insp.TaskID != "t1" || insp.ProjectID != "p1" || insp.Status != "completed" {
		t.Fatalf("t1 identity wrong: %+v", insp)
	}
	if insp.SpecPath != "shared/tasks/t1/spec.md" || insp.Summary != "Alpha report done" ||
		insp.ResultStatus != "SUCCESS" || insp.ResultPath != "shared/tasks/t1/result.md" {
		t.Fatalf("t1 detail wrong: %+v", insp)
	}
	if len(insp.History) != 2 {
		t.Fatalf("t1 history=%d, want 2 (malformed entries skipped): %+v", len(insp.History), insp.History)
	}
	if insp.History[1].From != "in_progress" || insp.History[1].To != "submitted" ||
		insp.History[1].Actor != "w1" || insp.History[1].Action != "submit_task" {
		t.Fatalf("t1 history entry wrong: %+v", insp.History[1])
	}
	if len(insp.Dependencies) != 0 {
		t.Fatalf("t1 dependencies=%v, want empty", insp.Dependencies)
	}
	if insp.Trace.ProjectID != "p1" || insp.Trace.TaskID != "t1" {
		t.Fatalf("t1 trace hint wrong: %+v", insp.Trace)
	}

	// t2: dependencies exposed from the graph node.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t2", nil)
	req2.SetPathValue("id", "p1")
	req2.SetPathValue("taskId", "t2")
	req2 = withCaller(req2, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec2 := httptest.NewRecorder()
	h.GetTaskInspection(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("t2 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var insp2 taskInspection
	if err := json.Unmarshal(rec2.Body.Bytes(), &insp2); err != nil {
		t.Fatalf("decode t2: %v", err)
	}
	if len(insp2.Dependencies) != 1 || insp2.Dependencies[0] != "t1" {
		t.Fatalf("t2 dependencies=%v, want [t1]", insp2.Dependencies)
	}
	if insp2.Status != "assigned" || insp2.History != nil {
		t.Fatalf("t2 detail wrong: %+v", insp2)
	}
}

func TestGetTaskInspection_MissingMeta(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "assigned_to": "@w1", "depends_on": []string{}, "status": "in_progress"},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var insp taskInspection
	if err := json.Unmarshal(rec.Body.Bytes(), &insp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No TaskMeta: node summary only, status normalized from the graph node.
	if insp.Status != "in-progress" || insp.SpecPath != "" || insp.Summary != "" {
		t.Fatalf("expected node summary only, got %+v", insp)
	}
	if insp.Trace.TaskID != "t1" {
		t.Fatalf("trace hint missing: %+v", insp.Trace)
	}
}

func TestGetTaskInspection_TaskNotInGraph(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P", "status": "active", "plan_type": "dag",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "T1", "assigned_to": "@w1", "depends_on": []string{}, "status": "planned"},
		},
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t9", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t9")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestGetTaskInspection_InvalidTaskID(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1", "title": "P", "status": "active", "plan_type": "dag",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/bad%21id", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "bad!id")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestGetTaskInspection_LoopTask(t *testing.T) {
	store := ossfake.NewMemory()
	putProject(store, "shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Loop Project",
		"status":     "active",
		"plan_type":  "loop",
		"tasks":      []map[string]any{},
		"loop": map[string]any{
			"goal": "g", "stop_condition": "s", "current_iteration": 1, "max_iterations": 5, "status": "running",
			"tasks": []map[string]any{
				{"task_id": "l1", "title": "Loop Step 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "completed"},
				{"task_id": "l2", "title": "Loop Step 2", "assigned_to": "@w2", "depends_on": []string{"l1"}, "status": "assigned"},
			},
		},
	})
	putProject(store, "shared/tasks/l2/meta.json", map[string]any{
		"task_id": "l2", "project_id": "p1", "status": "assigned",
		"spec_path": "shared/tasks/l2/spec.md",
	})
	h := newProjectTestHandler(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/l2", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "l2")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var insp taskInspection
	if err := json.Unmarshal(rec.Body.Bytes(), &insp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if insp.TaskID != "l2" || insp.SpecPath != "shared/tasks/l2/spec.md" {
		t.Fatalf("loop task detail wrong: %+v", insp)
	}
	if len(insp.Dependencies) != 1 || insp.Dependencies[0] != "l1" {
		t.Fatalf("loop task dependencies=%v, want [l1]", insp.Dependencies)
	}
}

func TestGetTaskInspection_CrossScopeNoFallback(t *testing.T) {
	store := ossfake.NewMemory()
	// Team-owned project; its task meta exists ONLY at the global prefix.
	// A team project must not leak the global TaskMeta (same rule as
	// readTasksDetail / includeTasks).
	putProject(store, "teams/alpha-team/shared/projects/p1/meta.json", map[string]any{
		"project_id": "p1",
		"title":      "Alpha Project",
		"status":     "active",
		"plan_type":  "dag",
		"team_id":    "alpha-team",
		"tasks": []map[string]any{
			{"task_id": "t1", "title": "Task 1", "assigned_to": "@w1", "depends_on": []string{}, "status": "planned"},
		},
	})
	putProject(store, "shared/tasks/t1/meta.json", map[string]any{
		"task_id": "t1", "project_id": "p1", "status": "completed",
		"summary": "should not leak",
	})
	h := newProjectTestHandler(t, store, team("alpha-team"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1", nil)
	req.SetPathValue("id", "p1")
	req.SetPathValue("taskId", "t1")
	req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
	rec := httptest.NewRecorder()
	h.GetTaskInspection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var insp taskInspection
	if err := json.Unmarshal(rec.Body.Bytes(), &insp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if insp.Summary != "" || insp.Status != "pending" {
		t.Fatalf("global TaskMeta leaked into team project: %+v", insp)
	}
}
