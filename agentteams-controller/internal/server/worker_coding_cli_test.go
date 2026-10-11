package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeExecBackend records exec commands and file writes; canned responses
// are provided per test.
type fakeExecBackend struct {
	mu         sync.Mutex
	commands   [][]string
	fileWrites map[string]string
	respond    func(cmd []string) (string, string, int, error)
	writeErr   error
}

func (f *fakeExecBackend) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	f.mu.Lock()
	f.commands = append(f.commands, append([]string{}, command...))
	f.mu.Unlock()
	return f.respond(command)
}

func (f *fakeExecBackend) WriteFile(_ context.Context, _, path, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.fileWrites[path] = content
	return nil
}

func (f *fakeExecBackend) lastCommand() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) == 0 {
		return nil
	}
	return f.commands[len(f.commands)-1]
}

func (f *fakeExecBackend) written(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.fileWrites[path]
	return c, ok
}

func newTestCodingCliHandler(t *testing.T, kubeMode string, exec *fakeExecBackend, store *ossfake.Memory, objs ...runtime.Object) *CodingCliHandler {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithRuntimeObjects(objs...).Build()
	h := NewCodingCliHandler(k8s, "default", kubeMode, nil, nil)
	if store != nil {
		h.audit = audit.NewClient(store)
	}
	if exec != nil {
		h.execBackend = func(context.Context, string) (codingCliExecer, error) { return exec, nil }
	}
	return h
}

func codingCliRequest(method, url, body string, kv ...string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		req.SetPathValue(kv[i], kv[i+1])
	}
	return req
}

func decodeJSON(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

// auditActionPresent reports whether any durable audit line in store
// carries the given action (same object key contract as production).
func auditActionPresent(t *testing.T, store *ossfake.Memory, action string) bool {
	t.Helper()
	for _, line := range readAuditLines(t, store) {
		var ev struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Action == action {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

func TestCodingCliList_ProbesAllCLIs(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		switch cmd[0] {
		case "qwen":
			return "0.25.0\n", "", 0, nil
		case "opencode":
			return "opencode: command not found\n", "stderr", 127, nil
		case "sh":
			// test -s auth file / test -f settings → absent
			return "", "", 1, nil
		}
		return "", "", 127, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listCLIs(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli", "", "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	qc, _ := doc["qwen-code"].(map[string]interface{})
	if qc["installed"] != true || qc["version"] != "0.25.0" {
		t.Fatalf("qwen-code probe: %v", qc)
	}
	oc, _ := doc["opencode"].(map[string]interface{})
	if oc["installed"] != false {
		t.Fatalf("opencode probe: %v", oc)
	}
}

func TestCodingCliList_AuthFromSettings(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		switch cmd[0] {
		case "qwen":
			return "qwen 0.25.0\n", "", 0, nil
		case "sh":
			script := cmd[2]
			switch {
			case strings.Contains(script, "test -f"):
				return "", "", 0, nil // settings file exists
			case strings.Contains(script, "cat "):
				return `{"security":{"auth":{"apiKey":"sk-live-123"}}}`, "", 0, nil
			}
			return "", "", 1, nil
		}
		return "", "", 127, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listCLIs(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli", "", "name", "daily-carol")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	qc, _ := doc["qwen-code"].(map[string]interface{})
	auth, _ := qc["auth"].(map[string]interface{})
	if auth["configured"] != true || auth["method"] != "settings" {
		t.Fatalf("auth probe: %v", auth)
	}
}

// ---------------------------------------------------------------------------
// Settings read / write
// ---------------------------------------------------------------------------

func TestCodingCliSettings_RedactsRawSecret(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" && len(cmd) > 2 && strings.Contains(cmd[2], "cat ") {
			return `{"provider":"openai","api_key":"sk-super-secret","model":"gpt"}`, "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getCliSettings(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", "", "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), "sk-super-secret") {
		t.Fatalf("raw secret leaked: %s", rec.Body.String())
	}
	set, _ := doc["settings"].(map[string]interface{})
	red, _ := set["api_key"].(map[string]interface{})
	if red["redacted"] != true || red["has_value"] != true {
		t.Fatalf("redaction: %v", set["api_key"])
	}
	if set["provider"] != "openai" {
		t.Fatalf("non-secret value lost: %v", set)
	}
}

func TestCodingCliSettings_AbsentFile(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		return "", "cat: no such file", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getCliSettings(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", "", "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	if doc["exists"] != false {
		t.Fatalf("want exists=false: %v", doc)
	}
}

func TestCodingCliPutSettings_MergesAndWritesAtomically(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" {
			switch {
			case strings.Contains(cmd[2], "printf %s"):
				return "/root", "", 0, nil // $HOME
			case strings.Contains(cmd[2], "cat "):
				return `{"a":1,"nested":{"keep":"x"}}`, "", 0, nil
			}
		}
		return "", "", 1, nil
	}
	store := ossfake.NewMemory()
	h := newTestCodingCliHandler(t, "embedded", exec, store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", `{"b":2,"nested":{"new":"y"}}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	content, ok := exec.written("/root/.qwen/settings.json")
	if !ok {
		t.Fatalf("WriteFile not called; writes=%v", exec.fileWrites)
	}
	merged := decodeJSON(t, content)
	if merged["a"] != float64(1) || merged["b"] != float64(2) {
		t.Fatalf("merge flat: %v", merged)
	}
	nested, _ := merged["nested"].(map[string]interface{})
	if nested["keep"] != "x" || nested["new"] != "y" {
		t.Fatalf("merge nested: %v", nested)
	}
	if !auditActionPresent(t, store, "coding_cli_write") {
		t.Fatalf("audit event missing")
	}
}

func TestCodingCliPutSettings_SentinelKeepsExisting(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" {
			switch {
			case strings.Contains(cmd[2], "printf %s"):
				return "/root", "", 0, nil
			case strings.Contains(cmd[2], "cat "):
				return `{"security":{"auth":{"apiKey":"sk-existing"}}}`, "", 0, nil
			}
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	body := `{"security":{"auth":{"apiKey":"***"}},"model":"gpt-4"}`
	h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", body, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	content, ok := exec.written("/root/.qwen/settings.json")
	if !ok {
		t.Fatalf("WriteFile not called")
	}
	merged := decodeJSON(t, content)
	sec, _ := merged["security"].(map[string]interface{})
	auth, _ := sec["auth"].(map[string]interface{})
	if auth["apiKey"] != "sk-existing" {
		t.Fatalf("sentinel should keep existing: %v", auth)
	}
	if merged["model"] != "gpt-4" {
		t.Fatalf("other keys should merge: %v", merged)
	}
}

func TestCodingCliPutSettings_SentinelNoExisting_400(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		return "", "", 1, nil // no settings file
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	body := `{"security":{"auth":{"apiKey":"***"}}}`
	h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", body, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
	}
	if len(exec.fileWrites) != 0 {
		t.Fatalf("no write should happen: %v", exec.fileWrites)
	}
}

// ---------------------------------------------------------------------------
// Install
// ---------------------------------------------------------------------------

func TestCodingCliStartInstall_AcceptsAndCompletes(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			return "added 1 package in 42s\n", "", 0, nil
		}
		return "", "", 1, nil
	}
	store := ossfake.NewMemory()
	h := newTestCodingCliHandler(t, "embedded", exec, store, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"0.25.0"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	taskID, _ := doc["task_id"].(string)
	if taskID == "" {
		t.Fatalf("no task id: %s", rec.Body.String())
	}

	// Wait for the async exec to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusRec := httptest.NewRecorder()
		h.getInstallStatus(statusRec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-carol", "cli", "qwen-code", "task_id", taskID)))
		if statusRec.Code == http.StatusOK {
			sdoc := decodeJSON(t, statusRec.Body.String())
			if sdoc["state"] != "running" {
				if sdoc["state"] != "done" || sdoc["exit_code"] != float64(0) {
					t.Fatalf("task state: %v", sdoc)
				}
				break
			}
		} else {
			t.Fatalf("status probe: %d %s", statusRec.Code, statusRec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The npm command must carry the pinned package@version.
	var sawNpm bool
	exec.mu.Lock()
	for _, c := range exec.commands {
		if c[0] == "npm" {
			sawNpm = true
			if c[len(c)-1] != "@qwen-code/qwen-code@0.25.0" {
				t.Fatalf("npm argv: %v", c)
			}
		}
	}
	exec.mu.Unlock()
	if !sawNpm {
		t.Fatalf("npm install never executed: %v", exec.commands)
	}
	if !auditActionPresent(t, store, "coding_cli_install") {
		t.Fatalf("install audit missing")
	}
}

// A non-zero npm exit must land as state=failed carrying the exit code and a
// stderr tail, so a caller polling the task can surface the real reason
// instead of a bare "done" or a hung "running".
func TestCodingCliStartInstall_NpmNonZeroExit_Failed(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			return "", "npm ERR! network timeout", 1, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"0.25.0"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	taskID, _ := decodeJSON(t, rec.Body.String())["task_id"].(string)
	if taskID == "" {
		t.Fatalf("no task id: %s", rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	var sdoc map[string]interface{}
	for time.Now().Before(deadline) {
		statusRec := httptest.NewRecorder()
		h.getInstallStatus(statusRec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-carol", "cli", "qwen-code", "task_id", taskID)))
		if statusRec.Code == http.StatusOK {
			sdoc = decodeJSON(t, statusRec.Body.String())
			if sdoc["state"] == "failed" {
				break
			}
		} else {
			t.Fatalf("status probe: %d %s", statusRec.Code, statusRec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sdoc == nil {
		t.Fatalf("task never reached a terminal state")
	}
	if sdoc["state"] != "failed" {
		t.Fatalf("state=%v want failed: %v", sdoc["state"], sdoc)
	}
	if sdoc["exit_code"] != float64(1) {
		t.Fatalf("exit_code=%v want 1: %v", sdoc["exit_code"], sdoc)
	}
	if tail, _ := sdoc["stderr_tail"].(string); !strings.Contains(tail, "npm ERR!") {
		t.Fatalf("stderr_tail=%v want to carry npm stderr: %v", sdoc["stderr_tail"], sdoc)
	}
}

// An exec-level error (transport/docker failure, not a non-zero exit) must
// also land as state=failed, not hang as "running".
func TestCodingCliStartInstall_ExecError_Failed(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			return "", "", 0, context.DeadlineExceeded
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	taskID, _ := decodeJSON(t, rec.Body.String())["task_id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	var sdoc map[string]interface{}
	for time.Now().Before(deadline) {
		statusRec := httptest.NewRecorder()
		h.getInstallStatus(statusRec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-carol", "cli", "qwen-code", "task_id", taskID)))
		if statusRec.Code == http.StatusOK {
			sdoc = decodeJSON(t, statusRec.Body.String())
			if sdoc["state"] == "failed" {
				break
			}
		} else {
			t.Fatalf("status probe: %d %s", statusRec.Code, statusRec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sdoc == nil {
		t.Fatalf("task never reached a terminal state")
	}
	if sdoc["state"] != "failed" {
		t.Fatalf("state=%v want failed: %v", sdoc["state"], sdoc)
	}
}

// The uninstall action drives npm uninstall -g (no version tag) and lands
// done on a clean exit — the same async contract as install, minus version.
func TestCodingCliStartInstall_UninstallCompletes(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			return "removed 1 package in 3s\n", "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"uninstall"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	taskID, _ := decodeJSON(t, rec.Body.String())["task_id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusRec := httptest.NewRecorder()
		h.getInstallStatus(statusRec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-carol", "cli", "qwen-code", "task_id", taskID)))
		if statusRec.Code == http.StatusOK {
			sdoc := decodeJSON(t, statusRec.Body.String())
			if sdoc["state"] != "running" {
				if sdoc["state"] != "done" || sdoc["exit_code"] != float64(0) {
					t.Fatalf("task state: %v", sdoc)
				}
				break
			}
		} else {
			t.Fatalf("status probe: %d %s", statusRec.Code, statusRec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var sawUninstall bool
	exec.mu.Lock()
	for _, c := range exec.commands {
		if c[0] == "npm" && len(c) >= 4 && c[1] == "uninstall" && c[2] == "-g" && c[3] == "@qwen-code/qwen-code" {
			sawUninstall = true
		}
	}
	exec.mu.Unlock()
	if !sawUninstall {
		t.Fatalf("npm uninstall -g never executed: %v", exec.commands)
	}
}

func TestCodingCliStartInstall_BusyConflict(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	release := make(chan struct{})
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			<-release // block until the test releases
			return "", "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first install: %d %s", rec.Code, rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	h.startInstall(rec2, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second install: %d want 409: %s", rec2.Code, rec2.Body.String())
	}
	close(release)
}

// The busy gate is per-(worker, cli), not per-CLI globally: a blocked install
// on worker A must not reject a same-CLI install on worker B. Regression for
// the cross-worker false-conflict that the global per-CLI gate caused.
func TestCodingCliStartInstall_BusyIsPerWorker(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	release := make(chan struct{})
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			<-release // block until the test releases
			return "", "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol", "daily-dave")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("worker A install: %d %s", rec.Code, rec.Body.String())
	}
	// Same CLI on a DIFFERENT worker: must be accepted, not a 409.
	rec2 := httptest.NewRecorder()
	h.startInstall(rec2, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-dave/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-dave", "cli", "qwen-code")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("worker B install: %d want 202 (different worker is not busy): %s", rec2.Code, rec2.Body.String())
	}
	close(release)
}

// An install task belongs to exactly one worker. Querying a task under a
// DIFFERENT worker must 404 (no cross-worker existence oracle); under the
// owning worker it resolves. Regression for the global task-map scope leak.
func TestCodingCliInstallStatus_CrossWorker_404(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	release := make(chan struct{})
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "npm" {
			<-release // keep the install running so the task is visible
			return "", "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol", "daily-dave")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"latest"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("worker A install: %d %s", rec.Code, rec.Body.String())
	}
	taskID := decodeJSON(t, rec.Body.String())["task_id"].(string)

	// Same task id under a different worker -> 404 (no leak).
	recB := httptest.NewRecorder()
	h.getInstallStatus(recB, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-dave/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-dave", "cli", "qwen-code", "task_id", taskID)))
	if recB.Code != http.StatusNotFound {
		t.Fatalf("cross-worker status: %d want 404: %s", recB.Code, recB.Body.String())
	}
	// Under the owning worker -> 200.
	recA := httptest.NewRecorder()
	h.getInstallStatus(recA, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/"+taskID, "", "name", "daily-carol", "cli", "qwen-code", "task_id", taskID)))
	if recA.Code != http.StatusOK {
		t.Fatalf("same-worker status: %d want 200: %s", recA.Code, recA.Body.String())
	}
	close(release)
}

func TestCodingCliStartInstall_InvalidVersion_400(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) { return "", "", 1, nil }
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"install","version":"0.25.0; rm -rf /"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCodingCliStartInstall_UnknownAction_400(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) { return "", "", 1, nil }
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.startInstall(rec, adminCaller(codingCliRequest(http.MethodPost, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install", `{"action":"reboot"}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Scope / validation
// ---------------------------------------------------------------------------

func TestCodingCli_KubeMode_503(t *testing.T) {
	h := newTestCodingCliHandler(t, "kube", nil, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.listCLIs(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli", "", "name", "daily-carol")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

func TestCodingCli_LeaderReadOnly_403(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) { return "", "", 1, nil }
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	req := codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", `{}`, "name", "daily-carol", "cli", "qwen-code")
	rec := httptest.NewRecorder()
	leader := withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "team-a"})
	h.putCliSettings(rec, leader)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403: %s", rec.Code, rec.Body.String())
	}
}

func TestCodingCli_UnknownCLI_400(t *testing.T) {
	h := newTestCodingCliHandler(t, "embedded", nil, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getCliSettings(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/fish-cli/settings", "", "name", "daily-carol", "cli", "fish-cli")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestCodingCli_TaskID_Invalid_400(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}}
	exec.respond = func(cmd []string) (string, string, int, error) { return "", "", 1, nil }
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.getInstallStatus(rec, adminCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/daily-carol/coding-cli/qwen-code/install/../../etc", "", "name", "daily-carol", "cli", "qwen-code", "task_id", "../../etc")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestCodingCli_WriteFileFailure_502(t *testing.T) {
	exec := &fakeExecBackend{fileWrites: map[string]string{}, writeErr: context.DeadlineExceeded}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" && len(cmd) > 2 && strings.Contains(cmd[2], "printf %s") {
			return "/root", "", 0, nil
		}
		return "", "", 1, nil
	}
	h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
	rec := httptest.NewRecorder()
	h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/daily-carol/coding-cli/qwen-code/settings", `{"a":1}`, "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502: %s", rec.Code, rec.Body.String())
	}
}
