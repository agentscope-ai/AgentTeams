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
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
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
	return NewResourceHandler(k8sClient, "default", nil, "", nil), worker
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
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

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

// remoteSkills (arbitrary external registries whose source URIs may embed
// tokens) is gated on the external_sources capability: default L2 is
// rejected, a granted L2 human is allowed (#1220 §9).
func TestUpdateWorker_L2HumanRemoteSkillsRequiresCapability(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	body := `{"remoteSkills":[{"source":"nacos","skills":[{"name":"web-research"}]}]}`

	defaults := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "scoped-user", Teams: []string{"alpha-team"}}
	rec := l2UpdateRequest(t, handler, "alpha-dev", body, defaults)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("default L2: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("remoteSkills")) {
		t.Errorf("error should name the field, got: %s", rec.Body.String())
	}

	granted := &authpkg.CallerIdentity{
		Role:         authpkg.RoleHuman,
		Username:     "scoped-user",
		Teams:        []string{"alpha-team"},
		Capabilities: []string{string(authpkg.CapabilityExternalSources)},
	}
	rec = l2UpdateRequest(t, handler, "alpha-dev", body, granted)
	if rec.Code != http.StatusOK {
		t.Fatalf("granted L2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// mcpServers is writable for scoped roles (#1220 §7, part 2): the gateway
// credential is attached at generation time only to trusted-gateway entries,
// so an external URL no longer receives the key.
func TestUpdateWorker_L2HumanMcpServersAllowed(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev",
		`{"mcpServers":[{"name":"fetch","url":"https://gw.example.com/mcp-servers/fetch/mcp"}]}`, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted-gateway URL: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.McpServers) != 1 || resp.McpServers[0].Name != "fetch" {
		t.Errorf("mcpServers not applied, got %v", resp.McpServers)
	}

	// An external endpoint is equally writable: trust decides credential
	// attachment at generation time, not update acceptance.
	rec = l2UpdateRequest(t, handler, "alpha-dev",
		`{"mcpServers":[{"name":"ext","url":"https://external.example/mcp"}]}`, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("external URL: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestL2WorkerUpdateFieldPolicyCoversAllRequestFields is the deny-by-default
// pin for the scoped field policy: every field of UpdateWorkerRequest is
// probed with a single-field request; only `skills` and `mcpServers` may be
// accepted (a granted L2 may additionally write remoteSkills — covered in
// TestUpdateWorker_L2HumanRemoteSkillsRequiresCapability). If a new field is
// added to the request type without an explicit policy decision in
// checkScopedWorkerUpdate, the probe gets 200 (fail-open) and this test fails.
func TestL2WorkerUpdateFieldPolicyCoversAllRequestFields(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
	allowed := map[string]bool{"skills": true, "mcpServers": true}

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
		if allowed[name] {
			if rec.Code != http.StatusOK {
				t.Errorf("%s: expected 200 (allowed), got %d: %s", name, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("field %s: expected 400 (a scoped caller must not be able to write it), got %d: %s — fail-open policy gap", name, rec.Code, rec.Body.String())
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
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"beta-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Standalone workers are hidden from L2 readers, so the update path hides
// them too (404, probe-resistant).
func TestUpdateWorker_L2HumanStandaloneWorkerHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "solo-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// L2 humans touching owner-domain fields are rejected with 400 naming the
// offending fields.
func TestUpdateWorker_L2HumanForbiddenFieldsRejected(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

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
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

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

// B2 (#1220 §5): team leaders are scoped to the SAME non-sensitive
// whitelist as L2 humans. The old behavior (leader full-field update,
// including model/image/env) is the bearer-leak residual path this closes.
func TestUpdateWorker_TeamLeaderInScopeScopedFieldsAllowed(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	body := `{"skills":["file-sync"],"mcpServers":[{"name":"fetch","url":"https://gw.example.com/mcp"}]}`
	rec := l2UpdateRequest(t, handler, "alpha-dev", body, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for scoped fields, got %d: %s", rec.Code, rec.Body.String())
	}
}

// B2 red proof: a leader writing owner-domain fields (model, state) was 200
// before this change and must be 400 now.
func TestUpdateWorker_TeamLeaderOwnerDomainRejected(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	for _, body := range []string{
		`{"model":"qwen3.8"}`,
		`{"state":"Sleeping"}`,
		`{"image":"reg.example/qwenpaw:latest"}`,
	} {
		rec := l2UpdateRequest(t, handler, "alpha-dev", body, caller)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", body, rec.Code, rec.Body.String())
		}
	}
}

// A production leader identity carries no capabilities (SA identities never
// hold Capabilities — the only write site is the admin human-update API,
// which targets Human CRs; see capability.go), so the leader's remoteSkills
// set is structurally empty and the gate rejects it.
func TestUpdateWorker_TeamLeaderRemoteSkillsForbidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	rec := l2UpdateRequest(t, handler, "alpha-dev",
		`{"remoteSkills":[{"source":"nacos","skills":[{"name":"web-research"}]}]}`, caller)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("remoteSkills")) {
		t.Errorf("error should name the field, got: %s", rec.Body.String())
	}
}

// Cross-team leader updates are hidden (404, probe-resistant), same as L2.
func TestUpdateWorker_TeamLeaderCrossTeamHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta-team"}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Standalone workers are hidden from leader updates too (they are not
// members of any team, so the team-membership lookup fails).
func TestUpdateWorker_TeamLeaderStandaloneWorkerHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha-team"}

	rec := l2UpdateRequest(t, handler, "solo-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An L2 human with no accessibleTeams cannot update any worker. In production
// the middleware rejects the teamless caller first (403); the handler hides
// the out-of-scope worker with 404, same as any other out-of-scope case.
func TestUpdateWorker_L2HumanNoTeamsHidden(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "carol", Teams: nil}

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"skills":["file-sync"]}`, caller)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestScopedMcpServerUpdateFlowsThroughRuntimeConfigToConsumerGates verifies
// the full chain this PR newly opens: a scoped (L2) worker update carrying
// mcpServers -> the persisted worker spec -> the runtime desired config the
// native runtime consumes -> the consumer-side credential gate (mcporter
// generation). The final hop — building the native MCP client from the
// runtime desired config — is pinned by the qwenpaw test_update.py
// regressions (trusted/external endpoints through client creation and
// update), which travel with this PR's stack base.
func TestScopedMcpServerUpdateFlowsThroughRuntimeConfigToConsumerGates(t *testing.T) {
	handler, _ := newL2UpdateRig(t)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "scoped-user", Teams: []string{"alpha-team"}}

	rec := l2UpdateRequest(t, handler, "alpha-dev",
		`{"mcpServers":[`+
			`{"name":"docs","url":"https://gw.example.com/mcp-servers/docs/mcp"},`+
			`{"name":"ext","url":"https://external.example/mcp"}]}`, caller)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped mcpServers update: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updated := &v1beta1.Worker{}
	if err := handler.client.Get(context.Background(),
		types.NamespacedName{Name: "alpha-dev", Namespace: "default"}, updated); err != nil {
		t.Fatalf("read worker back: %v", err)
	}
	if len(updated.Spec.McpServers) != 2 {
		t.Fatalf("worker spec mcpServers=%v, want the 2 updated entries", updated.Spec.McpServers)
	}

	// Runtime desired config: the reconciler projects the same spec into
	// runtime.yaml. Both entries must reach the desired config and the
	// document must carry no credential material (attachment happens at the
	// consumer, gated by host trust).
	store := ossfake.NewMemory()
	deployer := service.NewDeployer(service.DeployerConfig{
		OSS: store,
		RuntimeProjection: service.RuntimeProjectionConfig{
			StorageProvider: "oss",
			StorageBucket:   "agentteams-storage",
			StorageEndpoint: "https://oss.example.com",
			AIGatewayURL:    "https://gw.example.com",
		},
	})
	if err := deployer.DeployMemberRuntimeConfig(context.Background(),
		service.MemberRuntimeConfigDeployRequest{
			Name:        "alpha-dev",
			RuntimeName: "alpha-dev",
			Runtime:     "qwenpaw",
			Role:        "worker",
			Generation:  1,
			Spec:        updated.Spec,
		}); err != nil {
		t.Fatalf("DeployMemberRuntimeConfig: %v", err)
	}
	runtimeYAML, err := store.GetObject(context.Background(), "agents/alpha-dev/runtime/runtime.yaml")
	if err != nil {
		t.Fatalf("runtime.yaml missing: %v", err)
	}
	for _, secret := range []string{"Authorization", "Bearer ", "gateway-key"} {
		if strings.Contains(string(runtimeYAML), secret) {
			t.Fatalf("runtime.yaml leaked credential material %q:\n%s", secret, runtimeYAML)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(runtimeYAML, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, runtimeYAML)
	}
	desired, _ := doc["desired"].(map[string]any)
	entries, _ := desired["mcpServers"].([]any)
	names := map[string]bool{}
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		names[fmt.Sprint(m["name"])] = true
	}
	if len(entries) != 2 || !names["docs"] || !names["ext"] {
		t.Fatalf("desired.mcpServers=%v, want both updated entries", entries)
	}

	// Consumer gate (mcporter generation): the trusted-gateway entry carries
	// the credential, the external entry must not.
	gen := agentconfig.NewGenerator(agentconfig.Config{AIGatewayURL: "https://gw.example.com"})
	mcporterJSON, err := gen.GenerateMcporterConfig("gateway-key", updated.Spec.McpServers)
	if err != nil {
		t.Fatalf("GenerateMcporterConfig: %v", err)
	}
	var decoded struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcporterJSON, &decoded); err != nil {
		t.Fatalf("mcporter config is invalid JSON: %v\n%s", err, mcporterJSON)
	}
	docs := decoded.Servers["docs"]
	headers, _ := docs["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer gateway-key" {
		t.Fatalf("trusted entry should carry the gateway credential: %v", docs)
	}
	if ext := decoded.Servers["ext"]; ext != nil {
		if _, ok := ext["headers"]; ok {
			t.Fatalf("external entry must not receive the gateway credential: %v", ext)
		}
	}
}
