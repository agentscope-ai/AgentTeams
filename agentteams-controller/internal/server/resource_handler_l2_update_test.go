package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newL2UpdateRig builds a handler with team "alpha-team" (leader + worker)
// and a standalone worker "solo-dev".
func newL2UpdateRig(t *testing.T) (*ResourceHandler, *v1beta1.Worker) {
	t.Helper()
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{
			{Name: "alpha-lead", Role: "team_leader"},
			{Name: "alpha-dev", Role: "worker"},
		}},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	solo := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "solo-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, worker, solo).Build()
	return NewResourceHandler(k8sClient, "default", nil, ""), worker
}

func l2UpdateRequest(t *testing.T, handler *ResourceHandler, name string, body string, caller *authpkg.CallerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/"+name, bytes.NewReader([]byte(body)))
	req.SetPathValue("name", name)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), caller))
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	return rec
}

// An L2 human may update the public-catalog skill assignment (skills) on a
// worker in one of their accessibleTeams.
func TestUpdateWorker_L2HumanInScopeSkillFieldsAllowed(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	body := `{"skills":["file-sync","mcporter"]}`
	rec := l2UpdateRequest(t, handler, "alpha-dev", body, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Skills) != 2 || resp.Skills[0] != "file-sync" {
		t.Errorf("skills not applied, got %v", resp.Skills)
	}
}

// Credential-bearing surfaces are closed to default L2: remoteSkills (registry
// source URIs may embed tokens) and mcpServers (the gateway bearer key is
// injected into every entry verbatim — an attacker-controlled URL exfiltrates
// it) require an elevated capability.
func TestUpdateWorker_L2HumanCredentialSurfacesRejected(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	for _, tc := range []struct{ name, body string }{
		{"remoteSkills", `{"remoteSkills":[{"source":"nacos","skills":[{"name":"web-research"}]}]}`},
		{"mcpServers", `{"mcpServers":[{"name":"fetch","url":"https://attacker.example/mcp"}]}`},
	} {
		rec := l2UpdateRequest(t, handler, "alpha-dev", tc.body, caller)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(tc.name)) {
			t.Errorf("%s: error should name the field, got: %s", tc.name, rec.Body.String())
		}
	}
}

// TestL2WorkerUpdateFieldPolicyCoversAllRequestFields is the deny-by-default
// pin for the L2 field policy: every field of UpdateWorkerRequest is probed
// with a single-field request; only `skills` may be accepted. If a new field
// is added to the request type without an explicit policy decision in
// checkHumanWorkerUpdate, the probe gets 200 (fail-open) and this test fails.
func TestL2WorkerUpdateFieldPolicyCoversAllRequestFields(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	typ := reflect.TypeOf(UpdateWorkerRequest{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		// Minimal non-zero probe per field kind: strings get a value, slices
		// and pointers get a zero-valued element/object (non-nil).
		var probe string
		switch f.Type.Kind() {
		case reflect.String:
			probe = fmt.Sprintf(`{"%s":"x"}`, name)
		case reflect.Slice, reflect.Array:
			if f.Type.Elem().Kind() == reflect.String {
				probe = fmt.Sprintf(`{"%s":["x"]}`, name)
			} else {
				probe = fmt.Sprintf(`{"%s":[{}]}`, name)
			}
		case reflect.Ptr:
			probe = fmt.Sprintf(`{"%s":{}}`, name)
		default:
			t.Fatalf("field %s: unsupported kind %s for probe", name, f.Type.Kind())
		}
		rec := l2UpdateRequest(t, handler, "alpha-dev", probe, caller)
		if name == "skills" {
			if rec.Code != http.StatusOK {
				t.Errorf("skills: expected 200 (allowed), got %d: %s", rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("field %s: expected 400 (L2 must not be able to write it), got %d: %s — fail-open policy gap", name, rec.Code, rec.Body.String())
			continue
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(name)) {
			t.Errorf("field %s: 400 should name the offending field, got: %s", name, rec.Body.String())
		}
	}
}

// Cross-team L2 update is hidden (404) at the handler boundary, not denied
// (403): a 403 would let a scoped human enumerate workers it cannot see on
// the read path and learn their owning team (W8 probe resistance).
func TestUpdateWorker_L2HumanCrossTeamHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "sunzong", Teams: []string{"beta-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Standalone workers are hidden from L2 readers, so the update path hides
// them too (404, probe-resistant).
func TestUpdateWorker_L2HumanStandaloneWorkerHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "solo-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// L2 humans touching owner-domain fields are rejected with 400 naming the
// offending fields.
func TestUpdateWorker_L2HumanForbiddenFieldsRejected(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"model":"qwen3.8","soul":"override"}`, caller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model")) || !bytes.Contains(rec.Body.Bytes(), []byte("soul")) {
		t.Errorf("error should name offending fields, got: %s", rec.Body.String())
	}
}

// An empty L2 update body is a harmless no-op.
func TestUpdateWorker_L2HumanEmptyBodyNoOp(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "maizong", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{}`, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// L1 admins keep full update rights (regression).
func TestUpdateWorker_AdminFullUpdateUnchanged(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}

	body := `{"model":"qwen3.8","image":"reg.example/qwenpaw:latest","skills":["file-sync"]}`
	rec := l2UpdateRequest(t, handler, "alpha-dev", body, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Team leaders keep in-scope full updates (regression — no L2 gate applies).
func TestUpdateWorker_TeamLeaderInScopeUnchanged(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	body := `{"model":"qwen3.8","state":"Sleeping"}`
	rec := l2UpdateRequest(t, handler, "alpha-dev", body, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An L2 human with no accessibleTeams cannot update any worker. In production
// the middleware rejects the teamless caller first (403); the handler hides
// the out-of-scope worker with 404, same as any other out-of-scope case.
func TestUpdateWorker_L2HumanNoTeamsHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: nil}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}
