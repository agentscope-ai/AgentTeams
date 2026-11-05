// Hermes member-config plane, exercised through the real
// MemberConfigHandler: GET projection + credential redaction, PUT write
// with read-back consistency and rev change, stale if-rev 409, unknown
// field 422, restart-key pendingRestart, delegation projection on write,
// and rollback restoring the pre-write snapshot.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig/hermes"
)

// hermesConfigPath is where the fake backend resolves the worker's config:
// HERMES_HOME unset, HOME=/home/worker -> $HOME/.hermes/config.yaml.
const hermesConfigPath = "/home/worker/.hermes/config.yaml"

// hermesConfigInitial is the pre-existing config.yaml the fake container
// holds; its exact bytes define the initial rev (sha256). It carries a
// declared sensitive value (providers.openai.api_key), declared restart
// values (platforms.matrix.enabled + token), and a foreign key that must
// stay in the file but never appear in Values.
const hermesConfigInitial = `_config_version: 51
model:
  default: agentteams-gateway/qwen3-max
  provider: agentteams-gateway
providers:
  openai:
    enabled: true
    api_key: sk-live-secret
platforms:
  matrix:
    enabled: true
    token: matrix-token-secret
foreign_key:
  nested: 1
`

// hermesExecBackend is a fake worker-exec backend backed by a virtual
// in-container filesystem. It speaks exactly the /bin/sh command shapes
// the hermes config manager issues (HERMES_HOME resolution, test -f,
// cat, chmod --reference + mv -f) against fs, and WriteFile stores the
// temp file — the same atomic tmp+rename sequence the docker backend's
// exec path runs.
type hermesExecBackend struct {
	fs       map[string]string
	commands [][]string
	// driftAfterActivate, when set, overwrites the target right after the
	// activation rename — another writer touching the file between
	// activation and the controller's read-back.
	driftAfterActivate string
}

func newHermesExec() *hermesExecBackend {
	return &hermesExecBackend{
		fs: map[string]string{hermesConfigPath: hermesConfigInitial},
	}
}

func (b *hermesExecBackend) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	b.commands = append(b.commands, append([]string{}, command...))
	if len(command) < 3 || command[0] != "/bin/sh" || command[1] != "-c" {
		return "", "unhandled command: " + strings.Join(command, " "), 127, nil
	}
	script := command[2]
	switch {
	case strings.Contains(script, "HERMES_HOME"):
		// printf %s "${HERMES_HOME:-$HOME/.hermes}"
		return "/home/worker/.hermes", "", 0, nil
	case strings.Contains(script, `test -f`):
		if _, ok := b.fs[command[4]]; ok {
			return "", "", 0, nil
		}
		return "", "no such file", 1, nil
	case strings.HasPrefix(script, "cat --"):
		c, ok := b.fs[command[4]]
		if !ok {
			return "", "cat: no such file", 1, nil
		}
		return c, "", 0, nil
	case strings.Contains(script, "mv -f"):
		// activate step: chmod --reference "$2" "$1" || chmod 600; mv -f "$1" "$2"
		tmp, target := command[4], command[5]
		content, ok := b.fs[tmp]
		if !ok {
			return "", "mv: cannot stat " + tmp, 1, nil
		}
		b.fs[target] = content
		delete(b.fs, tmp)
		if b.driftAfterActivate != "" {
			b.fs[target] = b.driftAfterActivate
		}
		return "", "", 0, nil
	}
	return "", "unhandled command: " + strings.Join(command, " "), 127, nil
}

func (b *hermesExecBackend) WriteFile(_ context.Context, _, path, content string) error {
	b.fs[path] = content
	return nil
}

// hermesTeamWithWorkers builds a Team plus Worker CRs pinned to the hermes
// runtime (an empty spec.runtime resolves to the install default, openclaw —
// a hermes worker must say so explicitly).
func hermesTeamWithWorkers(name string, workers ...string) []runtime.Object {
	objs := checkpointTeamWithWorkers(name, workers...)
	for _, o := range objs[1:] {
		if w, ok := o.(*v1beta1.Worker); ok {
			w.Spec.Runtime = "hermes"
		}
	}
	return objs
}

func TestMemberConfigHermes_GetProjectsSurfaceAndRedacts(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	rec := httptest.NewRecorder()
	h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec.Body.String())
	if body["runtime"] != "hermes" {
		t.Fatalf("runtime=%v, want hermes", body["runtime"])
	}
	if body["rev"] != revOfBytes(t, hermesConfigInitial) {
		t.Fatalf("rev=%v, want the sha256 of the on-disk bytes", body["rev"])
	}
	if body["applied"] != "applied" {
		t.Fatalf("applied=%v, want applied with no desired state", body["applied"])
	}
	values, _ := body["values"].(map[string]interface{})
	model, _ := values["model"].(map[string]interface{})
	if model["default"] != "agentteams-gateway/qwen3-max" {
		t.Fatalf("model.default=%v", model)
	}
	openai, _ := values["providers"].(map[string]interface{})["openai"].(map[string]interface{})
	if openai["api_key"] != "***" {
		t.Fatalf("providers.openai.api_key=%v, want the fixed redaction placeholder", openai["api_key"])
	}
	if openai["enabled"] != true {
		t.Fatalf("providers.openai.enabled=%v", openai["enabled"])
	}
	matrix, _ := values["platforms"].(map[string]interface{})["matrix"].(map[string]interface{})
	if matrix["token"] != "***" {
		t.Fatalf("platforms.matrix.token=%v, want redacted", matrix["token"])
	}
	if matrix["enabled"] != true {
		t.Fatalf("platforms.matrix.enabled=%v", matrix["enabled"])
	}
	if _, leaked := values["foreign_key"]; leaked {
		t.Fatalf("foreign_key must not appear in Values (it stays in the file): %v", values)
	}
}

func TestMemberConfigHermes_PutWritesAndReadsBack(t *testing.T) {
	exec := newHermesExec()
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	rev0 := revOfBytes(t, hermesConfigInitial)
	body := `{"patch":{"model":{"default":"agentteams-gateway/qwen3.5-flash"}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["runtime"] != "hermes" {
		t.Fatalf("runtime=%v, want hermes", res["runtime"])
	}
	if res["rev"] == "" || res["rev"] == rev0 {
		t.Fatalf("rev must change after a write (got %v, old %v)", res["rev"], rev0)
	}
	if res["applied"] != "applied" {
		t.Fatalf("applied=%v, want applied (the read-back matched)", res["applied"])
	}
	// Read-back consistent: the YAML now carries the new value and the
	// untouched values survive the merge.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(exec.fs[hermesConfigPath]), &doc); err != nil {
		t.Fatalf("written file is not YAML: %v", err)
	}
	model, _ := doc["model"].(map[string]any)
	if model["default"] != "agentteams-gateway/qwen3.5-flash" {
		t.Fatalf("model.default=%v", model)
	}
	if doc["_config_version"] != 51 {
		t.Fatalf("_config_version=%v, want the untouched value to survive", doc["_config_version"])
	}
	// The GET projection agrees with the on-disk state (same rev).
	rec2 := httptest.NewRecorder()
	h.getConfig(rec2, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body2 := decodeJSON(t, rec2.Body.String())
	if body2["rev"] != res["rev"] {
		t.Fatalf("GET rev=%v, want the PUT's rev %v", body2["rev"], res["rev"])
	}
	if body2["applied"] != "applied" {
		t.Fatalf("GET applied=%v, want applied (desired rev matches disk)", body2["applied"])
	}
	// The pre-write snapshot landed in object storage under the old rev
	// with the exact pre-write bytes.
	snapKey, _ := res["snapshot"].(string)
	wantSnap := "agents/hermes-worker/config/previous/" + rev0 + ".json"
	if snapKey != wantSnap {
		t.Fatalf("snapshot=%q, want %q", snapKey, wantSnap)
	}
	if raw, err := store.GetObject(context.Background(), wantSnap); err != nil || string(raw) != hermesConfigInitial {
		t.Fatalf("snapshot missing or wrong: %v", err)
	}
	if !auditActionPresent(t, store, "member_config_write") {
		t.Fatalf("write was not audit-logged")
	}
}

func TestMemberConfigHermes_PutStaleIfRev409(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	wrong := strings.Repeat("0", 64)
	body := `{"patch":{"model":{"default":"agentteams-gateway/qwen3.5-flash"}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut,
		"/api/members/hermes-worker/config?if-rev="+wrong, body, "name", "hermes-worker")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "stale_rev" {
		t.Fatalf("error=%v, want stale_rev", res["error"])
	}
	if res["current"] != revOfBytes(t, hermesConfigInitial) {
		t.Fatalf("current=%v, want the live rev", res["current"])
	}
	if res["expected"] != wrong {
		t.Fatalf("expected=%v, want the submitted if-rev", res["expected"])
	}
	if exec.fs[hermesConfigPath] != hermesConfigInitial {
		t.Fatalf("file modified by a stale write")
	}
}

func TestMemberConfigHermes_PutUnknownField422(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	body := `{"patch":{"bogus":{"x":1}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "unknown_field" {
		t.Fatalf("error=%v, want unknown_field", res["error"])
	}
	fields, _ := res["fields"].([]interface{})
	if len(fields) != 1 || fields[0] != "bogus.x" {
		t.Fatalf("fields=%v, want [bogus.x]", res["fields"])
	}
	if exec.fs[hermesConfigPath] != hermesConfigInitial {
		t.Fatalf("file modified by a rejected write")
	}
	// A declared key with a wrong-typed value is 422 invalid_value.
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config",
		`{"patch":{"_config_version":"not-an-int"}}`, "name", "hermes-worker")))
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec2.Code, rec2.Body.String())
	}
	res2 := decodeJSON(t, rec2.Body.String())
	if res2["error"] != "invalid_value" || res2["key"] != "_config_version" {
		t.Fatalf("res=%v, want invalid_value for _config_version", res2)
	}
}

func TestMemberConfigHermes_PutRestartKeysPendingRestart(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	// streaming.enabled is declared with applies=restart: it is written
	// and reported in pendingRestart as a key list (design §4.3).
	body := `{"patch":{"streaming":{"enabled":true}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	pr, _ := res["pendingRestart"].([]interface{})
	if len(pr) != 1 || pr[0] != "streaming.enabled" {
		t.Fatalf("pendingRestart=%v, want [streaming.enabled]", res["pendingRestart"])
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(exec.fs[hermesConfigPath]), &doc); err != nil {
		t.Fatalf("written file is not YAML: %v", err)
	}
	streaming, _ := doc["streaming"].(map[string]any)
	if streaming["enabled"] != true {
		t.Fatalf("streaming.enabled not written: %v", doc["streaming"])
	}
}

func TestMemberConfigHermes_PutSubagentModelProjectsDelegation(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	// A subagent-model patch projects through ProjectDelegation: the YAML
	// delegation keys must equal the projection's output exactly.
	body := `{"patch":{"delegation":{"model":"m1","provider":"p1","fallback_providers":["f1","f2"]}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(exec.fs[hermesConfigPath]), &doc); err != nil {
		t.Fatalf("written file is not YAML: %v", err)
	}
	del, _ := doc["delegation"].(map[string]any)
	want := hermes.ProjectDelegation(hermes.SubagentModel{Model: "m1", Provider: "p1", Fallback: []string{"f1", "f2"}})
	for _, full := range []string{"delegation.model", "delegation.provider", "delegation.fallback_providers"} {
		short := strings.TrimPrefix(full, "delegation.")
		got, present := del[short]
		wantV, wantPresent := want[full]
		if present != wantPresent {
			t.Fatalf("delegation.%s present=%v, want %v (projection %v)", short, present, wantPresent, want)
		}
		if wantPresent && !yamlEq(got, wantV) {
			t.Fatalf("delegation.%s=%v, want %v", short, got, wantV)
		}
	}
	// An omitted subagent-model field must DELETE its delegation key
	// (omission preserves Hermes' inheritance semantics) while keeping
	// the others.
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config",
		`{"patch":{"delegation":{"model":""}}}`, "name", "hermes-worker")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var doc2 map[string]any
	if err := yaml.Unmarshal([]byte(exec.fs[hermesConfigPath]), &doc2); err != nil {
		t.Fatalf("written file is not YAML: %v", err)
	}
	del2, _ := doc2["delegation"].(map[string]any)
	if _, present := del2["model"]; present {
		t.Fatalf("delegation.model must be deleted when the subagent model is cleared: %v", del2)
	}
}

// yamlEq compares two decoded-YAML values (a []string written by the
// projection decodes back as []interface{}).
func yamlEq(a, b any) bool {
	switch av := a.(type) {
	case []interface{}:
		bv, ok := b.([]string)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// driftAfterActivation is the config another writer leaves behind between
// the controller's activation and its read-back: valid YAML, a different
// model, and not what was requested.
const hermesConfigDrifted = `_config_version: 51
model:
  default: agentteams-gateway/other-model
  provider: agentteams-gateway
providers:
  openai:
    enabled: true
    api_key: sk-live-secret
platforms:
  matrix:
    enabled: true
    token: matrix-token-secret
foreign_key:
  nested: 1
`

// A drifted read-back must keep the write pending on every later GET: the
// desired state records the content the controller meant to write, not the
// hash it observed, so a file that still differs is never reported applied.
func TestMemberConfigHermes_PutDriftAfterActivationStaysPending(t *testing.T) {
	exec := newHermesExec()
	exec.driftAfterActivate = hermesConfigDrifted
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	body := `{"patch":{"model":{"default":"agentteams-gateway/qwen3.5-flash"}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["applied"] != "pending" {
		t.Fatalf("PUT applied=%v, want pending (the read-back saw drift)", res["applied"])
	}
	// The regression: the drifted bytes are the file's rev, so a desired
	// state saved from the observation would compare equal here.
	rec2 := httptest.NewRecorder()
	h.getConfig(rec2, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	got := decodeJSON(t, rec2.Body.String())
	if got["applied"] != "pending" {
		t.Fatalf("GET applied=%v, want pending while the file differs from the request", got["applied"])
	}
	// Settling: a later write with no drift applies and stays applied.
	exec.driftAfterActivate = ""
	rec3 := httptest.NewRecorder()
	h.putConfig(rec3, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec3.Code != http.StatusOK {
		t.Fatalf("settle PUT status=%d body=%s", rec3.Code, rec3.Body.String())
	}
	if applied := decodeJSON(t, rec3.Body.String())["applied"]; applied != "applied" {
		t.Fatalf("settle PUT applied=%v, want applied", applied)
	}
	rec4 := httptest.NewRecorder()
	h.getConfig(rec4, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	if applied := decodeJSON(t, rec4.Body.String())["applied"]; applied != "applied" {
		t.Fatalf("GET after settle applied=%v, want applied", applied)
	}
}

// The rollback path has the same shape: the snapshot is the intended
// content, so drift after activation keeps every later GET pending.
func TestMemberConfigHermes_RollbackDriftAfterActivationStaysPending(t *testing.T) {
	exec := newHermesExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	body := `{"patch":{"model":{"default":"agentteams-gateway/qwen3.5-flash"}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	preRev := revOfBytes(t, hermesConfigInitial)
	exec.driftAfterActivate = hermesConfigDrifted
	rec2 := httptest.NewRecorder()
	h.rollbackConfig(rec2, adminCaller(codingCliRequest(http.MethodPost,
		"/api/members/hermes-worker/config/rollback?rev="+preRev, "", "name", "hermes-worker")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if applied := decodeJSON(t, rec2.Body.String())["applied"]; applied != "pending" {
		t.Fatalf("rollback applied=%v, want pending (the read-back saw drift)", applied)
	}
	rec3 := httptest.NewRecorder()
	h.getConfig(rec3, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	got := decodeJSON(t, rec3.Body.String())
	if got["applied"] != "pending" {
		t.Fatalf("GET applied=%v, want pending while the file differs from the snapshot", got["applied"])
	}
	// Restoring the snapshot bytes settles it.
	exec.driftAfterActivate = ""
	exec.fs[hermesConfigPath] = hermesConfigInitial
	rec4 := httptest.NewRecorder()
	h.getConfig(rec4, adminCaller(codingCliRequest(http.MethodGet, "/api/members/hermes-worker/config", "", "name", "hermes-worker")))
	if applied := decodeJSON(t, rec4.Body.String())["applied"]; applied != "applied" {
		t.Fatalf("GET applied=%v, want applied once the file matches the snapshot", applied)
	}
}

func TestMemberConfigHermes_RollbackRestoresSnapshot(t *testing.T) {
	exec := newHermesExec()
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		hermesTeamWithWorkers("team-a", "hermes-worker")...)
	body := `{"patch":{"model":{"default":"agentteams-gateway/qwen3.5-flash"}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/hermes-worker/config", body, "name", "hermes-worker")))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	preRev := revOfBytes(t, hermesConfigInitial)
	rec2 := httptest.NewRecorder()
	h.rollbackConfig(rec2, adminCaller(codingCliRequest(http.MethodPost,
		"/api/members/hermes-worker/config/rollback?rev="+preRev, "", "name", "hermes-worker")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	res := decodeJSON(t, rec2.Body.String())
	if res["rev"] != preRev {
		t.Fatalf("rev=%v, want the rolled-back rev", res["rev"])
	}
	if res["applied"] != "applied" {
		t.Fatalf("applied=%v, want applied (the read-back matched)", res["applied"])
	}
	if exec.fs[hermesConfigPath] != hermesConfigInitial {
		t.Fatalf("file not restored to the snapshot bytes")
	}
	if !auditActionPresent(t, store, "member_config_rollback") {
		t.Fatalf("rollback was not audit-logged")
	}
}
