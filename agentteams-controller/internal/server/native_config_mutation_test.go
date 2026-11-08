package server

import (
	"context"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestMemberConfigRollback_NativePolicyBeforeMutation(t *testing.T) {
	exec := newMemberExec()
	store := ossfake.NewMemory()
	snapshot := `{"agents":{"defaults":{"model":{"primary":"other/ungranted"}}}}`
	rev := revOfBytes(t, snapshot)
	if err := store.PutObject(context.Background(), "agents/daily-carol/config/previous/"+rev+".json", []byte(snapshot)); err != nil {
		t.Fatal(err)
	}
	h := newTestMemberConfigHandler(t, "embedded", exec, store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	policy := &stubNativeConfigPolicy{err: &NativeConfigPolicyError{Code: "model_not_allowed", Message: "snapshot model not granted"}}
	h.nativeConfigPolicy = policy
	response := httptest.NewRecorder()
	h.rollbackConfig(response, adminCaller(codingCliRequest(http.MethodPost, "/api/members/daily-carol/config/rollback?rev="+rev, "", "name", "daily-carol")))
	if response.Code != 403 || policy.calls != 1 {
		t.Fatalf("rollback=%d policy calls=%d body=%s", response.Code, policy.calls, response.Body)
	}
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatal("policy rejected rollback changed native file")
	}
	if err := store.Stat(context.Background(), "agents/daily-carol/config/previous/"+revOfBytes(t, memberConfigInitial)+".json"); err == nil {
		t.Fatal("rejected rollback created a snapshot")
	}
}

// A complete target document and an unredacted current document are required
// to distinguish an unchanged provider definition from an endpoint change.
type checkingNativePolicy struct {
	t      *testing.T
	called bool
}

func (p *checkingNativePolicy) CheckNativeWrite(ctx context.Context, _ *authpkg.CallerIdentity, _, _ string, next map[string]any, read func(context.Context) (map[string]any, error)) error {
	p.called = true
	current, err := read(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	secret := func(doc map[string]any) any {
		return doc["models"].(map[string]any)["providers"].(map[string]any)["custom"].(map[string]any)["apiKey"]
	}
	if secret(current) != "owned-test-placeholder" || secret(next) != secret(current) {
		p.t.Fatal("policy received projected or incomplete native documents")
	}
	model := next["agents"].(map[string]any)["defaults"].(map[string]any)["model"].(map[string]any)["primary"]
	if model != "other/new-model" {
		p.t.Fatal("policy target is not the actual merged document")
	}
	return &NativeConfigPolicyError{Code: "model_not_allowed", Message: "model not granted"}
}
func TestMemberConfigPolicyUsesExactNativeDocuments(t *testing.T) {
	exec := newMemberExec()
	initial := `{"models":{"providers":{"custom":{"apiKey":"owned-test-placeholder"}}},"agents":{"defaults":{"model":{"primary":"original/old-model"}}}}`
	exec.fs[memberConfigPath] = initial
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(), checkpointTeamWithWorkers("team-a", "daily-carol")...)
	policy := &checkingNativePolicy{t: t}
	h.nativeConfigPolicy = policy
	response := httptest.NewRecorder()
	h.putConfig(response, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", `{"patch":{"agents":{"defaults":{"model":{"primary":"other/new-model"}}}}}`, "name", "daily-carol")))
	if response.Code != 403 || !policy.called || exec.fs[memberConfigPath] != initial {
		t.Fatalf("status=%d policyCalled=%v", response.Code, policy.called)
	}
}
