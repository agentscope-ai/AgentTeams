package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- PR-B1: grant-based enforcement on the write paths ---

// grantRigWith builds a ResourceHandler over:
//   - team alpha-team (leader alpha-lead, worker alpha-dev, Model qwen3.5-plus)
//   - human CR "alice" (accessibleTeams [alpha-team]) whose worker-scope grant
//     admits exactly (gw, granted-m) on main, with spawn inherit=spawnInherit.
func grantRigWith(t *testing.T, spawnInherit bool) *ResourceHandler {
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
	grant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role:  "worker",
		Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "granted-m"}}},
		Spawn: &v1beta1.ModelAccessSurface{Inherit: maTestBool(spawnInherit)},
	}}}
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "alice",
			AccessibleTeams: []string{"alpha-team"},
			ModelAccess:     grant,
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, worker, alice).Build()
	return NewResourceHandler(k8s, "default", nil, "", nil)
}

func grantRig(t *testing.T) *ResourceHandler { return grantRigWith(t, false) }

func grantCaller() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}
}

func adminCallerID() *authpkg.CallerIdentity {
	return &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
}

// A value-changing model write is checked against the caller's own grant
// set: an in-table (provider, model) pair is accepted, an out-of-table one
// is rejected with model_not_allowed.
func TestModelGrant_HumanModelUpdateGrantGate(t *testing.T) {
	handler := grantRig(t)

	rec := l2UpdateRequest(t, handler, "alpha-dev",
		`{"model":"granted-m","modelProvider":"gw"}`, grantCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("in-table: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = l2UpdateRequest(t, handler, "alpha-dev",
		`{"model":"not-granted"}`, grantCaller())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-table: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model_not_allowed")) {
		t.Errorf("out-of-table: error should carry model_not_allowed, got: %s", rec.Body.String())
	}
}

// A provider without a model is never a valid update (pair rule).
func TestModelGrant_HumanProviderOnlyRequiresPair(t *testing.T) {
	handler := grantRig(t)
	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"modelProvider":"gw"}`, grantCaller())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model_pair_required")) {
		t.Errorf("error should carry model_pair_required, got: %s", rec.Body.String())
	}
}

// Changing only the provider (same model name) is still a pair write: the
// resulting (provider, model) must be admitted. The current value is pinned to
// the default gateway provider, so "same model, evil provider" cannot sneak in
// through the empty-provider wildcard.
func TestModelGrant_HumanProviderChangeChecksPair(t *testing.T) {
	handler := grantRig(t)

	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"model":"qwen3.5-plus","modelProvider":"evil"}`, grantCaller())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("provider swap: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model_not_allowed")) {
		t.Errorf("provider swap: error should carry model_not_allowed, got: %s", rec.Body.String())
	}

	// Naming the implicit default provider with the current model is a no-op
	// pair and stays allowed.
	rec = l2UpdateRequest(t, handler, "alpha-dev", `{"model":"qwen3.5-plus","modelProvider":"agentteams-gateway"}`, grantCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("default provider: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Clearing subagentModel (returning to inheritance) needs the spawn inherit
// grant: inherit=false rejects with inherit_not_granted, inherit=true allows.
func TestModelGrant_HumanSubagentClearNeedsSpawnInherit(t *testing.T) {
	for _, spawnInherit := range []bool{false, true} {
		handler := grantRigWith(t, spawnInherit)

		// L1 (no scoped caller) sets a non-empty current subagent model so
		// the clear below is a real value change.
		rec := l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":"sub-x"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("spawnInherit=%v: L1 set: expected 200, got %d: %s", spawnInherit, rec.Code, rec.Body.String())
		}

		rec = l2UpdateRequest(t, handler, "alpha-dev", `{"subagentModel":""}`, grantCaller())
		if spawnInherit {
			if rec.Code != http.StatusOK {
				t.Fatalf("spawnInherit=true: expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("spawnInherit=false: expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("inherit_not_granted")) {
			t.Errorf("spawnInherit=false: error should carry inherit_not_granted, got: %s", rec.Body.String())
		}
	}
}

// Writing the CURRENT value is always allowed, even with no grant admitting
// it ("lock ≠ unavailable": the current value is part of the effective set).
func TestModelGrant_HumanSameValueAlwaysAllowed(t *testing.T) {
	handler := grantRig(t)
	// The rig's worker runs qwen3.5-plus; alice's grant table does not
	// admit it. Rewriting the identical value must still pass.
	rec := l2UpdateRequest(t, handler, "alpha-dev", `{"model":"qwen3.5-plus"}`, grantCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("same-value write: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An L1 create carries the chosen model into the implicit grant annotation;
// a later L1 model change appends the new pair (dedup keeps one row per pair).
func TestModelGrant_L1CreateAndUpdateAppendImplicitGrant(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{
			{Name: "alpha-dev", Role: "worker"},
		}},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	h := NewResourceHandler(k8s, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers",
		bytes.NewReader([]byte(`{"name":"new-dev","model":"start-m","modelProvider":"gw"}`)))
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), adminCallerID()))
	rec := httptest.NewRecorder()
	h.CreateWorker(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var w v1beta1.Worker
	ctx := context.Background()
	key := client.ObjectKey{Name: "new-dev", Namespace: "default"}
	if err := h.client.Get(ctx, key, &w); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	grants := readModelGrants(&w)
	if len(grants) != 1 || grants[0] != entry("gw", "start-m") {
		t.Fatalf("create annotation = %v, want the created pair", grants)
	}

	// L1 changes the model: the new pair is appended, the old one stays.
	rec = l2UpdateRequest(t, h, "new-dev", `{"model":"next-m"}`, adminCallerID())
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := h.client.Get(ctx, key, &w); err != nil {
		t.Fatalf("get worker after update: %v", err)
	}
	grants = readModelGrants(&w)
	if len(grants) != 2 || grants[0] != entry("gw", "start-m") || grants[1] != entry("gw", "next-m") {
		t.Fatalf("update annotation = %v, want both pairs in order", grants)
	}

	// Re-writing the current value appends nothing (dedupe).
	rec = l2UpdateRequest(t, h, "new-dev", `{"model":"next-m"}`, adminCallerID())
	if rec.Code != http.StatusOK {
		t.Fatalf("same-value update: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := h.client.Get(ctx, key, &w); err != nil {
		t.Fatalf("get worker again: %v", err)
	}
	if got := readModelGrants(&w); len(got) != 2 {
		t.Fatalf("same-value update must not grow the list: %v", got)
	}
}

// --- coding-CLI side (PR-B1 §2.3) ---

// cliGrantRig builds a CodingCliHandler over team-a / daily-carol with a
// human "luo" whose worker-scope grant covers qwen-code with the given
// entries/inherit. The fake backend answers $HOME=/root and a settings file
// of {"model":"cur-m"}.
func cliGrantRig(t *testing.T, cli v1beta1.ModelAccessCli) (*CodingCliHandler, *fakeExecBackend) {
	t.Helper()
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "default"},
		Spec: v1beta1.TeamSpec{WorkerMembers: []v1beta1.TeamWorkerRef{
			{Name: "daily-carol", Role: "worker"},
		}},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-carol", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	human := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "luo", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			AccessibleTeams: []string{"team-a"},
			ModelAccess: &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
				Role:      "worker",
				CodingCli: []v1beta1.ModelAccessCli{cli},
			}}},
		},
	}
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" && len(cmd) > 2 {
			switch {
			case strings.Contains(cmd[2], "printf %s"):
				return "/root", "", 0, nil // $HOME
			case strings.Contains(cmd[2], "cat "):
				return `{"model":"cur-m"}`, "", 0, nil
			}
		}
		return "", "", 1, nil
	}
	objs := []runtime.Object{team, worker, human}
	return newTestCodingCliHandler(t, "embedded", exec, nil, objs...), exec
}

func cliHumanCaller(req *http.Request) *http.Request {
	return withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "luo", Teams: []string{"team-a"}})
}

func cliPut(t *testing.T, h *CodingCliHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", body,
		"name", "daily-carol", "cli", "qwen-code")
	rec := httptest.NewRecorder()
	h.putCliSettings(rec, cliHumanCaller(req))
	return rec
}

// A value-changing model write is grant-gated on the merged value: an
// admitted model is written, an unadmitted one is rejected (no write).
func TestModelGrant_CLI_HumanModelUpdateGrantGate(t *testing.T) {
	h, exec := cliGrantRig(t, v1beta1.ModelAccessCli{
		CLI:     "qwen-code",
		Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "cli-m1"}},
	})

	rec := cliPut(t, h, `{"model":"cli-m1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-table: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	content, ok := exec.written("/root/.qwen/settings.json")
	if !ok {
		t.Fatalf("in-table: settings not written")
	}
	if !strings.Contains(content, `"model": "cli-m1"`) {
		t.Errorf("written settings = %s, want model cli-m1", content)
	}

	rec = cliPut(t, h, `{"model":"evil-m"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-table: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("model_not_allowed")) {
		t.Errorf("out-of-table: error should carry model_not_allowed, got: %s", rec.Body.String())
	}
}

// Provider-defining keys are L1-only for humans: rejected before any exec,
// so the worker container is never poked for a write that cannot proceed.
func TestModelGrant_CLI_HumanProviderKeyL1Only(t *testing.T) {
	h, exec := cliGrantRig(t, v1beta1.ModelAccessCli{CLI: "qwen-code"})

	for _, body := range []string{
		`{"provider":{"baseUrl":"https://evil.example"}}`,
		`{"env":{"OPENAI_BASE_URL":"https://evil.example"}}`,
	} {
		rec := cliPut(t, h, body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d: %s", body, rec.Code, rec.Body.String())
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("provider_config_l1_only")) {
			t.Errorf("%s: error should carry provider_config_l1_only, got: %s", body, rec.Body.String())
		}
	}
	if len(exec.commands) != 0 {
		t.Fatalf("provider-key rejection must not exec into the worker: %v", exec.commands)
	}
	if len(exec.fileWrites) != 0 {
		t.Fatalf("provider-key rejection must not write: %v", exec.fileWrites)
	}
}

// Clearing the CLI model ("") needs the CLI grant's inherit flag; a patch
// that does not change the model skips the grant check entirely.
func TestModelGrant_CLI_HumanClearAndNonModelPatch(t *testing.T) {
	h, _ := cliGrantRig(t, v1beta1.ModelAccessCli{CLI: "qwen-code"})

	rec := cliPut(t, h, `{"model":""}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("clear without inherit: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("inherit_not_granted")) {
		t.Errorf("clear: error should carry inherit_not_granted, got: %s", rec.Body.String())
	}

	// Rewriting the current model is always allowed (same value).
	rec = cliPut(t, h, `{"model":"cur-m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-value model: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// A non-model patch bypasses the grant check even though the file's
	// current model is not in the (empty) grant table.
	rec = cliPut(t, h, `{"foo":"bar"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-model patch: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
