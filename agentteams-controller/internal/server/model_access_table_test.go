package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- PR-A1a §8: Human.modelAccess + ModelAccessTable tests ---

// errorBody extracts the "message" field of a WriteError response so
// assertions are not coupled to JSON quote escaping.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var errResp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	return errResp.Message
}

func TestValidateModelAccessTableXOR(t *testing.T) {
	handler := newHumanUpdateRig(t)

	// A scope may not carry both a table and inline surfaces.
	rec := putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","table":"shared","main":{"entries":[{"provider":"p","model":"m"}]}}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("table+inline: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), "mutually exclusive") {
		t.Errorf("want mutually-exclusive message, got %q", errorBody(t, rec))
	}

	// A scope with neither a table nor any inline surface is useless.
	rec = putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker"}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("neither: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// A table reference that does not exist is a dangling reference (400,
	// table name in the message).
	rec = putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","table":"nope"}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown table: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), `unknown model access table "nope"`) {
		t.Errorf("want unknown-table message, got %q", errorBody(t, rec))
	}
}

func TestValidateModelAccessRoleDuplicate(t *testing.T) {
	handler := newHumanUpdateRig(t)

	// The same role may not appear in two scopes.
	rec := putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","main":{"entries":[{"provider":"p","model":"a"}]}},{"role":"worker","main":{"entries":[{"provider":"p","model":"b"}]}}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("two workers: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), "duplicate role") {
		t.Errorf("want duplicate-role message, got %q", errorBody(t, rec))
	}

	// "" and "*" are the same (any-role) scope.
	rec = putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"","main":{"entries":[{"provider":"p","model":"a"}]}},{"role":"*","main":{"entries":[{"provider":"p","model":"b"}]}}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("\"\" + \"*\": expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), "duplicate role") {
		t.Errorf("want duplicate-role message, got %q", errorBody(t, rec))
	}
}

func TestValidateModelAccessEntries(t *testing.T) {
	handler := newHumanUpdateRig(t)

	// A blank provider (after TrimSpace) is rejected.
	rec := putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","main":{"entries":[{"provider":"  ","model":"m"}]}}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty provider: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), "empty provider") {
		t.Errorf("want empty-provider message, got %q", errorBody(t, rec))
	}

	// The same model twice within one surface is rejected.
	rec = putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","main":{"entries":[{"provider":"p","model":"m"},{"provider":"q","model":"m"}]}}]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dup model: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(errorBody(t, rec), "duplicate model") {
		t.Errorf("want duplicate-model message, got %q", errorBody(t, rec))
	}
}

func TestUpdateHumanModelAccess(t *testing.T) {
	handler := newHumanUpdateRig(t)
	rec := putHuman(t, handler, "alice", `{"modelAccess":{"scopes":[{"role":"worker","main":{"entries":[{"provider":"openai","model":"gpt-x"}]}}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ModelAccess == nil || len(resp.ModelAccess.Scopes) != 1 {
		t.Fatalf("modelAccess not echoed: %+v", resp.ModelAccess)
	}
	sc := resp.ModelAccess.Scopes[0]
	if sc.Role != "worker" || sc.Main == nil || len(sc.Main.Entries) != 1 {
		t.Fatalf("scope = %+v", sc)
	}
	if sc.Main.Entries[0].Provider != "openai" || sc.Main.Entries[0].Model != "gpt-x" {
		t.Errorf("entry = %+v", sc.Main.Entries[0])
	}
}

func TestModelAccessTableCRUD(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := `{"name":"shared","main":{"entries":[{"provider":"openai","model":"gpt-x"}]}}`

	// Create -> 201.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/model-access-tables", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.CreateModelAccessTable(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Duplicate create -> 409.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/model-access-tables", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.CreateModelAccessTable(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Get -> 200 with the stored fields.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/model-access-tables/shared", nil)
	req.SetPathValue("name", "shared")
	rec = httptest.NewRecorder()
	handler.GetModelAccessTable(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got ModelAccessTableResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if got.Name != "shared" || got.Main == nil || len(got.Main.Entries) != 1 || got.Main.Entries[0].Model != "gpt-x" {
		t.Fatalf("get = %+v", got)
	}

	// Update -> 200, full replace (main cleared, spawn set).
	ubody := `{"name":"shared","spawn":{"entries":[{"provider":"anthropic","model":"claude-x"}]}}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/model-access-tables/shared", bytes.NewReader([]byte(ubody)))
	req.SetPathValue("name", "shared")
	rec = httptest.NewRecorder()
	handler.UpdateModelAccessTable(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/model-access-tables/shared", nil)
	req.SetPathValue("name", "shared")
	rec = httptest.NewRecorder()
	handler.GetModelAccessTable(rec, req)
	// Reset before re-unmarshal: json.Unmarshal leaves fields absent from
	// the new payload (main is omitempty) at their previous values.
	got = ModelAccessTableResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get after update: %v", err)
	}
	if got.Main != nil {
		t.Errorf("main should be cleared by full replace, got %+v", got.Main)
	}
	if got.Spawn == nil || len(got.Spawn.Entries) != 1 || got.Spawn.Entries[0].Model != "claude-x" {
		t.Fatalf("spawn = %+v", got.Spawn)
	}

	// List -> 200 with the table.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/model-access-tables", nil)
	rec = httptest.NewRecorder()
	handler.ListModelAccessTables(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list ModelAccessTableListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if list.Total != 1 || len(list.Tables) != 1 || list.Tables[0].Name != "shared" {
		t.Fatalf("list = %+v", list)
	}

	// Delete -> 204, then Get -> 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/model-access-tables/shared", nil)
	req.SetPathValue("name", "shared")
	rec = httptest.NewRecorder()
	handler.DeleteModelAccessTable(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/model-access-tables/shared", nil)
	req.SetPathValue("name", "shared")
	rec = httptest.NewRecorder()
	handler.GetModelAccessTable(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Authorization: the middleware maps authorizer denial to 403. Humans
	// are denied on every model-access-table action by the authorizer's
	// default branch (no dedicated branch needed); admin/manager are full.
	a := authpkg.NewAuthorizer()
	for _, action := range []authpkg.Action{authpkg.ActionList, authpkg.ActionCreate, authpkg.ActionGet, authpkg.ActionUpdate, authpkg.ActionDelete} {
		if err := a.Authorize(&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Teams: []string{"t1"}}, authpkg.AuthzRequest{Action: action, ResourceKind: "model-access-table"}); err == nil {
			t.Errorf("human %s model-access-table allowed, want deny", action)
		}
	}
	for _, role := range []string{authpkg.RoleAdmin, authpkg.RoleManager} {
		if err := a.Authorize(&authpkg.CallerIdentity{Role: role}, authpkg.AuthzRequest{Action: authpkg.ActionCreate, ResourceKind: "model-access-table"}); err != nil {
			t.Errorf("%s create model-access-table denied: %v", role, err)
		}
	}
}
