package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig/hermes"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig/openclaw"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// memberConfigPath is where resolveConfigPath lands in the test fake
// (QWENPAW_WORKING_DIR=/workspace, openclaw.json present there).
const memberConfigPath = "/workspace/openclaw.json"

// memberConfigInitial is the pre-existing openclaw.json the fake container
// holds; its exact bytes define the initial rev (sha256).
const memberConfigInitial = `{
  "agents": {
    "defaults": {
      "model": {
        "primary": "agentteams-gateway/qwen3-max"
      }
    }
  }
}
`

const memberPluginsJSON = `{"plugins":[{"id":"discord","enabled":true,"status":"loaded","channelIds":["discord"]}]}`

// memberExecBackend is a fake worker-exec backend backed by a virtual
// in-container filesystem: the config manager's sh scripts resolve paths
// against fs, and WriteFile mutates fs so the read-back verify sees the new
// bytes (exactly like the atomic tmp+rename write in the docker backend).
// gwHash emulates the gateway's appliedConfigHash; with gwApplyOnWrite the
// hash moves to the sha256 of freshly written bytes, exactly like the
// gateway's file watch applying a changed openclaw.json.
type memberExecBackend struct {
	fs             map[string]string
	plugins        string
	commands       [][]string
	gwHash         *string
	gwApplyOnWrite bool
	gwErr          error
}

func (b *memberExecBackend) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	b.commands = append(b.commands, append([]string{}, command...))
	switch {
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "QWENPAW_WORKING_DIR"):
		return "/workspace", "", 0, nil
	case len(command) == 3 && command[0] == "sh" && command[1] == "-c" && strings.Contains(command[2], "${HOME}"):
		return "/home/worker", "", 0, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && command[2] == `test -f "$1"`:
		if _, ok := b.fs[command[4]]; ok {
			return "", "", 0, nil
		}
		return "", "no such file", 1, nil
	case len(command) == 5 && command[0] == "sh" && command[1] == "-c" && strings.HasPrefix(command[2], "cat"):
		c, ok := b.fs[command[4]]
		if !ok {
			return "", "cat: no such file", 1, nil
		}
		return c, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "plugins":
		return b.plugins, "", 0, nil
	case len(command) >= 4 && command[0] == "openclaw" && command[1] == "gateway":
		if b.gwErr != nil {
			return "", "gateway unavailable: " + b.gwErr.Error(), 1, nil
		}
		h := "null"
		if b.gwHash != nil {
			h = `"` + *b.gwHash + `"`
		}
		return `{"appliedConfigHash":` + h + `}`, "", 0, nil
	case len(command) == 2 && command[0] == "printenv":
		return "", "not defined", 1, nil
	}
	return "", "unhandled command: " + strings.Join(command, " "), 127, nil
}

func (b *memberExecBackend) WriteFile(_ context.Context, _, path, content string) error {
	b.fs[path] = content
	if b.gwApplyOnWrite {
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		b.gwHash = &h
	}
	return nil
}

func newMemberExec() *memberExecBackend {
	b := &memberExecBackend{
		fs:      map[string]string{memberConfigPath: memberConfigInitial},
		plugins: memberPluginsJSON,
	}
	h := "applied-baseline"
	b.gwHash = &h
	b.gwApplyOnWrite = true
	return b
}

func newTestMemberConfigHandler(t *testing.T, kubeMode string, exec openclaw.ExecBackend, store *ossfake.Memory, objs ...runtime.Object) *MemberConfigHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	reg := runtimeconfig.NewRegistry()
	reg.Register(openclaw.New())
	reg.Register(hermes.New())
	var ac *audit.Client
	if store != nil {
		ac = audit.NewClient(store)
	}
	h := NewMemberConfigHandler(k8s, "default", kubeMode, ac, nil, store, reg, "openclaw")
	if exec != nil {
		h.execBackend = func(context.Context, string) (openclaw.ExecBackend, error) { return exec, nil }
	}
	return h
}

func revOfBytes(t *testing.T, raw string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func memberConfigDoc(t *testing.T, content string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("written content is not JSON: %v\n%s", err, content)
	}
	return doc
}

// ---------------------------------------------------------------------------
// Surface
// ---------------------------------------------------------------------------

func TestMemberConfigSurface_ImmutableCacheAndETag(t *testing.T) {
	h := newTestMemberConfigHandler(t, "embedded", nil, nil)
	rec := httptest.NewRecorder()
	h.getSurface(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/runtimes/openclaw/surface", "", "runtime", "openclaw")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control=%q, want the immutable strong-cache header", got)
	}
	sv := openclaw.New().Surface()
	wantETag := "\"" + sv.Runtime + "-" + sv.RuntimeVersion + "-" + sv.SurfaceVersion + "\""
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag=%q, want %q", got, wantETag)
	}
	body := decodeJSON(t, rec.Body.String())
	if body["runtime"] != "openclaw" {
		t.Fatalf("runtime=%v, want openclaw", body["runtime"])
	}
	if faces, _ := body["faces"].([]interface{}); len(faces) == 0 {
		t.Fatalf("surface faces empty: %s", rec.Body.String())
	}

	// Second GET with If-None-Match -> 304 with an empty body.
	req := codingCliRequest(http.MethodGet, "/api/runtimes/openclaw/surface", "", "runtime", "openclaw")
	req.Header.Set("If-None-Match", wantETag)
	rec2 := httptest.NewRecorder()
	h.getSurface(rec2, adminCaller(req))
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s, want 304", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 must carry an empty body, got %d bytes", rec2.Body.Len())
	}
}

func TestMemberConfigSurface_UnknownRuntime404(t *testing.T) {
	h := newTestMemberConfigHandler(t, "embedded", nil, nil)
	rec := httptest.NewRecorder()
	h.getSurface(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/runtimes/unknown-runtime/surface", "", "runtime", "unknown-runtime")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec.Body.String())
	if body["runtime"] != "unknown-runtime" {
		t.Fatalf("runtime=%v, want unknown-runtime", body["runtime"])
	}
	found := map[string]bool{}
	for _, s := range body["supported"].([]interface{}) {
		found[s.(string)] = true
	}
	if !found["openclaw"] || !found["hermes"] {
		t.Fatalf("supported=%v must list both compiled-in runtimes (openclaw + hermes)", body["supported"])
	}
}

func TestMemberConfigSurface_Hermes200(t *testing.T) {
	h := newTestMemberConfigHandler(t, "embedded", nil, nil)
	rec := httptest.NewRecorder()
	h.getSurface(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/runtimes/hermes/surface", "", "runtime", "hermes")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control=%q, want the immutable strong-cache header", got)
	}
	sv := hermes.New().Surface()
	wantETag := "\"" + sv.Runtime + "-" + sv.RuntimeVersion + "-" + sv.SurfaceVersion + "\""
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag=%q, want %q", got, wantETag)
	}
	body := decodeJSON(t, rec.Body.String())
	if body["runtime"] != "hermes" {
		t.Fatalf("runtime=%v, want hermes", body["runtime"])
	}
	if faces, _ := body["faces"].([]interface{}); len(faces) == 0 {
		t.Fatalf("surface faces empty: %s", rec.Body.String())
	}

	// Second GET with If-None-Match -> 304 with an empty body.
	req := codingCliRequest(http.MethodGet, "/api/runtimes/hermes/surface", "", "runtime", "hermes")
	req.Header.Set("If-None-Match", wantETag)
	rec2 := httptest.NewRecorder()
	h.getSurface(rec2, adminCaller(req))
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s, want 304", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 must carry an empty body, got %d bytes", rec2.Body.Len())
	}
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

func TestMemberConfigGet_StateShape(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec.Body.String())
	if body["runtime"] != "openclaw" {
		t.Fatalf("runtime=%v", body["runtime"])
	}
	if body["rev"] != revOfBytes(t, memberConfigInitial) {
		t.Fatalf("rev=%v, want the sha256 of the on-disk bytes", body["rev"])
	}
	if body["applied"] != "applied" {
		t.Fatalf("applied=%v, want \"applied\" when there is no controller desired state", body["applied"])
	}
	if pr, _ := body["pendingRestart"].([]interface{}); len(pr) != 0 {
		t.Fatalf("pendingRestart=%v, want an empty list", body["pendingRestart"])
	}
	if _, ok := body["values"].(map[string]interface{}); !ok {
		t.Fatalf("values missing or not an object: %s", rec.Body.String())
	}
}

func TestMemberConfigGet_UnknownRuntime409(t *testing.T) {
	objs := checkpointTeamWithWorkers("team-a", "qp-worker")
	objs[1].(*v1beta1.Worker).Spec.Runtime = "qwenpaw"
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(), objs...)
	rec := httptest.NewRecorder()
	h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/qp-worker/config", "", "name", "qp-worker")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec.Body.String())
	if body["error"] != "runtime_unknown" || body["runtime"] != "qwenpaw" {
		t.Fatalf("body=%s, want runtime_unknown for qwenpaw", rec.Body.String())
	}
	found := map[string]bool{}
	for _, s := range body["supported"].([]interface{}) {
		found[s.(string)] = true
	}
	if !found["openclaw"] || !found["hermes"] {
		t.Fatalf("supported=%v must contain openclaw and hermes", body["supported"])
	}
}

func TestMemberConfigGet_AppliedButInactive(t *testing.T) {
	exec := newMemberExec()
	// The discord plugin exists but is not enabled: a write touching
	// channels.discord is applied to the file (the gateway reloads) yet
	// inert at runtime — applied-but-inactive (design §8.4).
	exec.plugins = `{"plugins":[{"id":"discord","enabled":false,"status":"loaded","channelIds":["discord"]}]}`
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	body := `{"patch":{"channels":{"discord":{"enabled":true}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["applied"] != "applied-but-inactive" {
		t.Fatalf("applied=%v, want applied-but-inactive", res["applied"])
	}
	if in, _ := res["inactive"].([]interface{}); len(in) != 1 || in[0] != "discord" {
		t.Fatalf("inactive=%v, want [discord]", res["inactive"])
	}
	// The GET projection agrees.
	rec2 := httptest.NewRecorder()
	h.getConfig(rec2, adminCaller(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body2 := decodeJSON(t, rec2.Body.String())
	if body2["applied"] != "applied-but-inactive" {
		t.Fatalf("GET applied=%v, want applied-but-inactive", body2["applied"])
	}
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

func TestMemberConfigPut_DeclaredAndOpaqueLeaves(t *testing.T) {
	exec := newMemberExec()
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// channels.discord.enabled: exact wildcard-declared leaf (boolean).
	// agents.defaults.subagents.model: opaque leaf below the declared
	// object agents.defaults.subagents — passes through unvalidated.
	body := `{"patch":{"channels":{"discord":{"enabled":true}},"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/qwen3.5-flash"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["runtime"] != "openclaw" || res["rev"] == "" {
		t.Fatalf("response=%s", rec.Body.String())
	}
	if res["applied"] != "applied" {
		t.Fatalf("applied=%v, want \"applied\" (the fake gateway reloads on write)", res["applied"])
	}
	doc := memberConfigDoc(t, exec.fs[memberConfigPath])
	discord, _ := doc["channels"].(map[string]any)["discord"].(map[string]any)
	if discord["enabled"] != true {
		t.Fatalf("channels.discord.enabled not written: %v", doc["channels"])
	}
	subagents, _ := doc["agents"].(map[string]any)["defaults"].(map[string]any)["subagents"].(map[string]any)
	if subagents["model"] != "agentteams-gateway/qwen3.5-flash" {
		t.Fatalf("agents.defaults.subagents.model not written: %v", subagents)
	}
	// Untouched declared state survives the merge.
	primary, _ := doc["agents"].(map[string]any)["defaults"].(map[string]any)["model"].(map[string]any)["primary"].(string)
	if primary != "agentteams-gateway/qwen3-max" {
		t.Fatalf("agents.defaults.model.primary clobbered: %q", primary)
	}
	// The pre-write snapshot landed in object storage under the old rev.
	snapKey, _ := res["snapshot"].(string)
	wantSnap := "agents/daily-carol/config/previous/" + revOfBytes(t, memberConfigInitial) + ".json"
	if snapKey != wantSnap {
		t.Fatalf("snapshot=%q, want %q", snapKey, wantSnap)
	}
	if _, err := store.GetObject(context.Background(), wantSnap); err != nil {
		t.Fatalf("snapshot not stored: %v", err)
	}
	if !auditActionPresent(t, store, "member_config_write") {
		t.Fatalf("write was not audit-logged")
	}
}

func TestMemberConfigPut_UnknownField422(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// agents.defaults has no declared object at the root, so a bogus leaf
	// under it is unknown (not opaque).
	body := `{"patch":{"agents":{"defaults":{"bogusKey":1}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "unknown_field" {
		t.Fatalf("error=%v, want unknown_field", res["error"])
	}
	fields := res["fields"].([]interface{})
	if len(fields) != 1 || fields[0] != "agents.defaults.bogusKey" {
		t.Fatalf("fields=%v", fields)
	}
	// A 422 write must not touch the file.
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatalf("file modified by a rejected write")
	}

	// The root channels object is allowlist-only: an undeclared channel id
	// is unknown too.
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config",
		`{"patch":{"channels":{"totally-new-channel":{"bar":1}}}}`, "name", "daily-carol")))
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422 for undeclared channel id", rec2.Code, rec2.Body.String())
	}
	if res2 := decodeJSON(t, rec2.Body.String()); res2["error"] != "unknown_field" {
		t.Fatalf("error=%v, want unknown_field", res2["error"])
	}
}

func TestMemberConfigPut_SensitiveOpaqueRefused(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// agents.defaults.sandbox is a declared SENSITIVE object: nested writes
	// through the opaque path would bypass the ref-only rule, so they are
	// refused 422.
	body := `{"patch":{"agents":{"defaults":{"sandbox":{"workspace":"/tmp"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "invalid_value" || res["key"] != "agents.defaults.sandbox.workspace" {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatalf("file modified by a rejected write")
	}
}

func TestMemberConfigPut_StaleIfRev409(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	wrong := strings.Repeat("0", 64)
	body := `{"patch":{"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/qwen3.5-flash"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut,
		"/api/members/daily-carol/config?if-rev="+wrong, body, "name", "daily-carol")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "stale_rev" {
		t.Fatalf("error=%v, want stale_rev", res["error"])
	}
	if res["current"] != revOfBytes(t, memberConfigInitial) {
		t.Fatalf("current=%v, want the live rev", res["current"])
	}
	if res["expected"] != wrong {
		t.Fatalf("expected=%v, want the submitted if-rev", res["expected"])
	}
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatalf("file modified by a stale write")
	}
}

func TestMemberConfigPut_MissingPatch400(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config",
		`{}`, "name", "daily-carol")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestMemberConfigPut_RestartKeysListed(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	// gateway.port is declared with applies=restart: it is written and
	// reported in pendingRestart as a key list (design §4.3), not a bool.
	body := `{"patch":{"gateway":{"port":19000}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	pr, _ := res["pendingRestart"].([]interface{})
	if len(pr) != 1 || pr[0] != "gateway.port" {
		t.Fatalf("pendingRestart=%v, want [gateway.port]", res["pendingRestart"])
	}
	doc := memberConfigDoc(t, exec.fs[memberConfigPath])
	gw, _ := doc["gateway"].(map[string]any)
	if gw["port"] != 19000.0 {
		t.Fatalf("gateway.port not written: %v", doc["gateway"])
	}
}

func TestMemberConfigPut_GatewayDownStillWritesPending(t *testing.T) {
	exec := newMemberExec()
	exec.gwErr = errors.New("gateway not running")
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	body := `{"patch":{"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/qwen3.5-flash"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s (the file write must succeed even with the gateway down)", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["applied"] != "pending" {
		t.Fatalf("applied=%v, want pending (an unverifiable write never reports applied)", res["applied"])
	}
	// The file was written and verified.
	if exec.fs[memberConfigPath] == memberConfigInitial {
		t.Fatalf("file not written despite the 200")
	}
	// The GET projection is fail-closed too: baseline unknown -> pending.
	rec2 := httptest.NewRecorder()
	h.getConfig(rec2, adminCaller(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body2 := decodeJSON(t, rec2.Body.String())
	if body2["applied"] != "pending" {
		t.Fatalf("GET applied=%v, want pending", body2["applied"])
	}
}

// stubNativeConfigPolicy is the test double for the native-config policy
// seam: it records that it was consulted and returns a fixed error.
type stubNativeConfigPolicy struct {
	err   error
	calls int
}

func (s *stubNativeConfigPolicy) CheckNativeWrite(_ context.Context, _ *authpkg.CallerIdentity, _, _ string,
	_ map[string]any, _ func(context.Context) (map[string]any, error)) error {
	s.calls++
	return s.err
}

func TestMemberConfigPut_NativeConfigPolicySeam(t *testing.T) {
	// A wired policy hook refuses a definition-plane write: the PUT is a
	// 403 carrying the policy's machine-readable code, and the file is
	// untouched (the hook runs before mgr.Write).
	exec := newMemberExec()
	stub := &stubNativeConfigPolicy{err: &NativeConfigPolicyError{
		Code:    "native_config_definition_l1_only",
		Message: "provider definition keys are L1-only",
	}}
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	h.nativeConfigPolicy = stub
	body := `{"patch":{"models":{"providers":{"custom":{"baseUrl":"http://gw"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403 from the policy hook", rec.Code, rec.Body.String())
	}
	res := decodeJSON(t, rec.Body.String())
	if res["error"] != "native_config_definition_l1_only" {
		t.Fatalf("error=%v, want the policy code", res["error"])
	}
	if stub.calls != 1 {
		t.Fatalf("hook calls=%d, want 1", stub.calls)
	}
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatalf("file modified by a policy-refused write")
	}

	// With no policy wired (nil), behaviour is unchanged: the standard
	// success path from the existing write tests still goes through.
	exec2 := newMemberExec()
	h2 := newTestMemberConfigHandler(t, "embedded", exec2, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	body2 := `{"patch":{"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/qwen3.5-flash"}}}}}`
	rec2 := httptest.NewRecorder()
	h2.putConfig(rec2, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body2, "name", "daily-carol")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("nil-hook PUT status=%d body=%s, want 200 (current behaviour)", rec2.Code, rec2.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestMemberConfigRollback_RestoresSnapshot(t *testing.T) {
	exec := newMemberExec()
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	body := `{"patch":{"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/qwen3.5-flash"}}}}}`
	rec := httptest.NewRecorder()
	h.putConfig(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config", body, "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	preRev := revOfBytes(t, memberConfigInitial)
	rec2 := httptest.NewRecorder()
	h.rollbackConfig(rec2, adminCaller(codingCliRequest(http.MethodPost,
		"/api/members/daily-carol/config/rollback?rev="+preRev, "", "name", "daily-carol")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	res := decodeJSON(t, rec2.Body.String())
	if res["rev"] != preRev {
		t.Fatalf("rev=%v, want the rolled-back rev", res["rev"])
	}
	if exec.fs[memberConfigPath] != memberConfigInitial {
		t.Fatalf("file not restored to the snapshot bytes")
	}
	if !auditActionPresent(t, store, "member_config_rollback") {
		t.Fatalf("rollback was not audit-logged")
	}
}

func TestMemberConfigRollback_RevShape400(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	for _, rev := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("A", 64)} {
		rec := httptest.NewRecorder()
		h.rollbackConfig(rec, adminCaller(codingCliRequest(http.MethodPost,
			"/api/members/daily-carol/config/rollback?rev="+rev, "", "name", "daily-carol")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("rev=%q status=%d body=%s, want 400", rev, rec.Code, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Scope / permission discipline
// ---------------------------------------------------------------------------

func TestMemberConfig_KubeModeUniform503(t *testing.T) {
	h := newTestMemberConfigHandler(t, "k8s", nil, nil,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET status=%d, want uniform 503 in kube mode", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, adminCaller(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config",
		`{"patch":{}}`, "name", "daily-carol")))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT status=%d, want uniform 503 in kube mode", rec2.Code)
	}
}

func TestMemberConfig_InvalidWorkerName400(t *testing.T) {
	h := newTestMemberConfigHandler(t, "embedded", nil, nil)
	for _, name := range []string{"", "Daily-Carol", "../etc", "a b"} {
		rec := httptest.NewRecorder()
		h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/x/config", "", "name", name)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status=%d, want 400", name, rec.Code)
		}
	}
}

func TestMemberConfig_UnknownWorker404(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory())
	rec := httptest.NewRecorder()
	h.getConfig(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/members/ghost-worker/config", "", "name", "ghost-worker")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown worker", rec.Code)
	}
}

func TestMemberConfig_TeamLeaderReadOnly(t *testing.T) {
	exec := newMemberExec()
	store := ossfake.NewMemory()
	h := newTestMemberConfigHandler(t, "embedded", exec, store,
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	leader := func(r *http.Request) *http.Request {
		return withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead", Team: "team-a"})
	}
	// GET is allowed for the in-scope leader.
	rec := httptest.NewRecorder()
	h.getConfig(rec, leader(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("leader GET status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	// PUT is refused 403 (read-only role on this surface).
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, leader(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config",
		`{"patch":{"agents":{"defaults":{"subagents":{"model":"agentteams-gateway/x"}}}}}`, "name", "daily-carol")))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("leader PUT status=%d body=%s, want 403", rec2.Code, rec2.Body.String())
	}
	// Rollback is refused the same way.
	rec3 := httptest.NewRecorder()
	h.rollbackConfig(rec3, leader(codingCliRequest(http.MethodPost,
		"/api/members/daily-carol/config/rollback?rev="+strings.Repeat("0", 64), "", "name", "daily-carol")))
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("leader rollback status=%d body=%s, want 403", rec3.Code, rec3.Body.String())
	}
}

func TestMemberConfig_CrossTeam404(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("team-a", "daily-carol")...)
	outsider := func(r *http.Request) *http.Request {
		return withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"team-b"}})
	}
	rec := httptest.NewRecorder()
	h.getConfig(rec, outsider(codingCliRequest(http.MethodGet, "/api/members/daily-carol/config", "", "name", "daily-carol")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team GET status=%d, want 404 (W8: never 403)", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.putConfig(rec2, outsider(codingCliRequest(http.MethodPut, "/api/members/daily-carol/config",
		`{"patch":{}}`, "name", "daily-carol")))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cross-team PUT status=%d, want 404 (W8)", rec2.Code)
	}
}

func TestMemberConfig_L2HumanInScopeAllowed(t *testing.T) {
	exec := newMemberExec()
	h := newTestMemberConfigHandler(t, "embedded", exec, ossfake.NewMemory(),
		checkpointTeamWithWorkers("market-team", "market-writer")...)
	req := withCaller(codingCliRequest(http.MethodGet, "/api/members/market-writer/config", "", "name", "market-writer"),
		&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"market-team"}})
	rec := httptest.NewRecorder()
	h.getConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-scope L2 human GET status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}
