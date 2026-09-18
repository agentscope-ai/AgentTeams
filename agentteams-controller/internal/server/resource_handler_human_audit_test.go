package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newHumanScopeAuditRig builds a resource handler with a durable audit
// store and neutral fixture names (no personal or node identifiers).
func newHumanScopeAuditRig(t *testing.T) (*ResourceHandler, *ossfake.Memory) {
	t.Helper()
	scheme := newServerTestScheme(t)
	teamA := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "market-team", Namespace: "default"}}
	teamB := &v1beta1.Team{ObjectMeta: metav1.ObjectMeta{Name: "ops-team", Namespace: "default"}}
	workerA := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "market-dev", Namespace: "default"}}
	workerB := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "ops-dev", Namespace: "default"}}
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "scoped-user", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:       "Scoped User",
			PermissionLevel:   2,
			AccessibleTeams:   []string{"market-team"},
			AccessibleWorkers: []string{"market-dev"},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(teamA, teamB, workerA, workerB, human).Build()
	sc := ossfake.NewMemory()
	handler := NewResourceHandler(k8sClient, "default", nil, "", audit.NewClient(sc))
	return handler, sc
}

func adminScopeCtx() context.Context {
	return context.WithValue(context.Background(), authpkg.CallerKeyForTest(),
		&authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"})
}

func putScope(t *testing.T, handler *ResourceHandler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/"+name, bytes.NewReader([]byte(body)))
	req = req.WithContext(adminScopeCtx())
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)
	return rec
}

func auditEvents(t *testing.T, sc *ossfake.Memory) []string {
	t.Helper()
	key := "audit/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	data, err := sc.GetObject(context.Background(), key)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func eventFields(t *testing.T, line string) map[string]any {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("audit line parse: %v: %s", err, line)
	}
	return ev
}

func TestUpdateHuman_TeamScopeChangeIsAudited(t *testing.T) {
	handler, sc := newHumanScopeAuditRig(t)

	// Add one team.
	if rec := putScope(t, handler, "scoped-user", `{"accessibleTeams":["market-team","ops-team"]}`); rec.Code != http.StatusOK {
		t.Fatalf("add: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Remove it again.
	if rec := putScope(t, handler, "scoped-user", `{"accessibleTeams":["market-team"]}`); rec.Code != http.StatusOK {
		t.Fatalf("remove: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// No-op update (scope omitted): no event.
	if rec := putScope(t, handler, "scoped-user", `{"displayName":"Scoped User"}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Identical list re-set: no event.
	if rec := putScope(t, handler, "scoped-user", `{"accessibleTeams":["market-team"]}`); rec.Code != http.StatusOK {
		t.Fatalf("identical: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Re-add, then reorder only: the reorder emits no event (set
	// semantics).
	if rec := putScope(t, handler, "scoped-user", `{"accessibleTeams":["market-team","ops-team"]}`); rec.Code != http.StatusOK {
		t.Fatalf("re-add: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := putScope(t, handler, "scoped-user", `{"accessibleTeams":["ops-team","market-team"]}`); rec.Code != http.StatusOK {
		t.Fatalf("reorder: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	lines := auditEvents(t, sc)
	if len(lines) != 3 {
		t.Fatalf("want 3 audit lines (add + remove + re-add), got %d: %v", len(lines), lines)
	}
	add := eventFields(t, lines[0])
	if add["action"] != "accessible_teams_change" || add["who"] != "admin" || add["target"] != "scoped-user" {
		t.Fatalf("line 1 = %v", add)
	}
	if detail, _ := add["detail"].(string); detail != "added=[ops-team] removed=[]" {
		t.Fatalf("line 1 detail = %q", detail)
	}
	rem := eventFields(t, lines[1])
	if rem["action"] != "accessible_teams_change" {
		t.Fatalf("line 2 action = %v", rem["action"])
	}
	if detail, _ := rem["detail"].(string); detail != "added=[] removed=[ops-team]" {
		t.Fatalf("line 2 detail = %q", detail)
	}
	if before, _ := rem["before"].([]any); len(before) != 2 || before[0] != "market-team" || before[1] != "ops-team" {
		t.Fatalf("line 2 before = %v", rem["before"])
	}
}

func TestUpdateHuman_WorkerScopeChangeIsAudited(t *testing.T) {
	handler, sc := newHumanScopeAuditRig(t)

	// Add a second worker.
	if rec := putScope(t, handler, "scoped-user", `{"accessibleWorkers":["market-dev","ops-dev"]}`); rec.Code != http.StatusOK {
		t.Fatalf("add: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Clear the list with an empty array.
	if rec := putScope(t, handler, "scoped-user", `{"accessibleWorkers":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	lines := auditEvents(t, sc)
	if len(lines) != 2 {
		t.Fatalf("want 2 audit lines (add + clear), got %d: %v", len(lines), lines)
	}
	add := eventFields(t, lines[0])
	if add["action"] != "accessible_workers_change" {
		t.Fatalf("line 1 action = %v", add["action"])
	}
	if detail, _ := add["detail"].(string); detail != "added=[ops-dev] removed=[]" {
		t.Fatalf("line 1 detail = %q", detail)
	}
	clear := eventFields(t, lines[1])
	if detail, _ := clear["detail"].(string); detail != "added=[] removed=[market-dev, ops-dev]" {
		t.Fatalf("line 2 detail = %q", detail)
	}
	if _, ok := clear["after"]; ok {
		t.Fatalf("line 2 should omit empty after, got %v", clear["after"])
	}
}

func TestUpdateHuman_LevelChangeIsAudited(t *testing.T) {
	handler, sc := newHumanScopeAuditRig(t)

	// 2 -> 3.
	if rec := putScope(t, handler, "scoped-user", `{"permissionLevel":3}`); rec.Code != http.StatusOK {
		t.Fatalf("up: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// 3 -> 1.
	if rec := putScope(t, handler, "scoped-user", `{"permissionLevel":1}`); rec.Code != http.StatusOK {
		t.Fatalf("down: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Re-set the current level (1): no event.
	if rec := putScope(t, handler, "scoped-user", `{"permissionLevel":1}`); rec.Code != http.StatusOK {
		t.Fatalf("identical: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	lines := auditEvents(t, sc)
	if len(lines) != 2 {
		t.Fatalf("want 2 audit lines (up + down), got %d: %v", len(lines), lines)
	}
	up := eventFields(t, lines[0])
	if up["action"] != "permission_level_change" {
		t.Fatalf("line 1 action = %v", up["action"])
	}
	if before, _ := up["before"].([]any); len(before) != 1 || before[0] != "2" {
		t.Fatalf("line 1 before = %v", up["before"])
	}
	if after, _ := up["after"].([]any); len(after) != 1 || after[0] != "3" {
		t.Fatalf("line 1 after = %v", up["after"])
	}
	down := eventFields(t, lines[1])
	if detail, _ := down["detail"].(string); detail != "permission_level 3 -> 1" {
		t.Fatalf("line 2 detail = %q", detail)
	}
}

func TestCreateHuman_ScopeIsAudited(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	sc := ossfake.NewMemory()
	handler := NewResourceHandler(k8sClient, "default", nil, "", audit.NewClient(sc))

	post := func(name string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader([]byte(body)))
		req = req.WithContext(adminScopeCtx())
		rec := httptest.NewRecorder()
		handler.CreateHuman(rec, req)
		return rec
	}

	if rec := post("scope-grant", `{"name":"scope-grant","permissionLevel":2,"accessibleTeams":["market-team"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create with scope: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post("plain-user", `{"name":"plain-user"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create plain: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	lines := auditEvents(t, sc)
	if len(lines) != 2 {
		t.Fatalf("want 2 audit lines, got %d: %v", len(lines), lines)
	}
	grant := eventFields(t, lines[0])
	if grant["action"] != "human_created" || grant["target"] != "scope-grant" {
		t.Fatalf("line 1 = %v", grant)
	}
	after, _ := grant["after"].([]any)
	if len(after) != 2 || after[0] != "permission_level=2" || after[1] != "accessible_teams=[market-team]" {
		t.Fatalf("line 1 after = %v", grant["after"])
	}
	plain := eventFields(t, lines[1])
	if plain["action"] != "human_created" || plain["target"] != "plain-user" {
		t.Fatalf("line 2 = %v", plain)
	}
	if _, ok := plain["after"]; ok {
		t.Fatalf("line 2 should omit empty after, got %v", plain["after"])
	}
}

func TestDeleteHuman_ScopeIsAudited(t *testing.T) {
	handler, sc := newHumanScopeAuditRig(t)

	del := func(name string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/humans/"+name, nil)
		req = req.WithContext(adminScopeCtx())
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		handler.DeleteHuman(rec, req)
		return rec
	}

	if rec := del("scoped-user"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// Second delete: 404, no new audit event.
	if rec := del("scoped-user"); rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	lines := auditEvents(t, sc)
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %v", len(lines), lines)
	}
	ev := eventFields(t, lines[0])
	if ev["action"] != "human_deleted" || ev["target"] != "scoped-user" || ev["who"] != "admin" {
		t.Fatalf("line 1 = %v", ev)
	}
	before, _ := ev["before"].([]any)
	if len(before) != 3 ||
		before[0] != "permission_level=2" ||
		before[1] != "accessible_teams=[market-team]" ||
		before[2] != "accessible_workers=[market-dev]" {
		t.Fatalf("line 1 before = %v", ev["before"])
	}
	if _, ok := ev["after"]; ok {
		t.Fatalf("line 1 should omit empty after, got %v", ev["after"])
	}
}
