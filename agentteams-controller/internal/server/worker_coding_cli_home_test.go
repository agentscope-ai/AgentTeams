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
)

// homeFake models a worker whose $HOME (/root) differs from
// QWENPAW_WORKING_DIR (/app/working), with a small in-memory filesystem so
// settings written through WriteFile can be read back by the shell commands
// the handler issues. Sharing one fs map across instances simulates a
// container recreation: the working dir is the persistent volume.
type homeFake struct {
	mu       sync.Mutex
	fs       map[string]string
	commands [][]string
	home     string
	working  string
}

func newHomeFake(fs map[string]string) *homeFake {
	if fs == nil {
		fs = map[string]string{}
	}
	return &homeFake{fs: fs, home: "/root", working: "/app/working"}
}

func (f *homeFake) Exec(_ context.Context, _ string, command []string, _ time.Duration) (string, string, int, error) {
	f.mu.Lock()
	f.commands = append(f.commands, append([]string{}, command...))
	f.mu.Unlock()
	if len(command) >= 3 && command[0] == "sh" && command[1] == "-c" {
		script := command[2]
		switch {
		case strings.Contains(script, "QWENPAW_WORKING_DIR"):
			return f.working, "", 0, nil
		case strings.Contains(script, `"${HOME:-}"`):
			return f.home, "", 0, nil
		case strings.Contains(script, "test -x "):
			return "", "", 1, nil
		case strings.Contains(script, "test -f "), strings.Contains(script, "test -s "):
			if len(command) < 5 || command[3] != "sh" {
				// A path interpolated into the script is word-split by the
				// real shell; model that failure instead of being lenient.
				return "", "too many arguments", 2, nil
			}
			path := command[len(command)-1]
			if _, ok := f.fs[path]; ok {
				return "", "", 0, nil
			}
			return "", "", 1, nil
		case strings.Contains(script, "cat "):
			if len(command) < 5 || command[3] != "sh" {
				return "", "No such file or directory", 1, nil
			}
			path := command[len(command)-1]
			if data, ok := f.fs[path]; ok {
				return data, "", 0, nil
			}
			return "", "no such file", 1, nil
		}
		return "", "", 1, nil
	}
	if command[0] == "env" {
		return "9.9.9\n", "", 0, nil
	}
	if command[0] == "npm" {
		return "added 1 package\n", "", 0, nil
	}
	if command[0] == "qwen" {
		return "qwen 0.25.0\n", "", 0, nil
	}
	return "", "", 1, nil
}

func (f *homeFake) WriteFile(_ context.Context, _, path, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fs[path] = content
	return nil
}

func (f *homeFake) wroteHome(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if len(c) >= 2 && c[0] == "env" && strings.HasPrefix(c[1], "HOME="+prefix) {
			return true
		}
	}
	return false
}

func homeTestHandler(t *testing.T, exec codingCliExecer) *CodingCliHandler {
	t.Helper()
	return newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
}

func putSettings(t *testing.T, h *CodingCliHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/settings", body, "name", "daily-carol", "cli", "qwen-code")))
	return rec
}

func getSettings(t *testing.T, h *CodingCliHandler) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	h.getCliSettings(rec, adminCaller(codingCliRequest(http.MethodGet, "/settings", "", "name", "daily-carol", "cli", "qwen-code")))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec.Body.String())
}

// The distinct-home round trip: PUT writes under QWENPAW_WORKING_DIR and the
// following GET / auth probe read the very same file.
func TestCodingCliHome_DistinctHomeRoundTrip(t *testing.T) {
	exec := newHomeFake(nil)
	h := homeTestHandler(t, exec)

	rec := putSettings(t, h, `{"model":"m1","security":{"auth":{"apiKey":"sk-1"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := exec.fs["/app/working/.qwen/settings.json"]; !ok {
		t.Fatalf("settings written outside the working dir: %v", exec.fs)
	}

	doc := getSettings(t, h)
	if doc["exists"] != true {
		t.Fatalf("GET exists=%v body=%v", doc["exists"], doc)
	}
	settings, _ := doc["settings"].(map[string]interface{})
	if settings["model"] != "m1" {
		t.Fatalf("GET lost the written model: %v", settings)
	}

	listRec := httptest.NewRecorder()
	h.listCLIs(listRec, adminCaller(codingCliRequest(http.MethodGet, "/coding-cli", "", "name", "daily-carol")))
	qc, _ := decodeJSON(t, listRec.Body.String())["qwen-code"].(map[string]interface{})
	auth, _ := qc["auth"].(map[string]interface{})
	if auth["configured"] != true || auth["method"] != "settings" {
		t.Fatalf("auth probe after PUT: %v", auth)
	}
	// The CLI probe must run with the same effective home.
	if !exec.wroteHome("/app/working") {
		t.Fatalf("CLI probe did not run with HOME=/app/working: %v", exec.commands)
	}
}

func TestCodingCliHome_SentinelKeepsSecretAndFields(t *testing.T) {
	exec := newHomeFake(nil)
	h := homeTestHandler(t, exec)
	if rec := putSettings(t, h, `{"model":"m1","security":{"auth":{"apiKey":"sk-1"}}}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putSettings(t, h, `{"security":{"auth":{"apiKey":"***"}},"model":"m2"}`); rec.Code != http.StatusOK {
		t.Fatalf("sentinel PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putSettings(t, h, `{"tools":{"x":1}}`); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(exec.fs["/app/working/.qwen/settings.json"]), &doc); err != nil {
		t.Fatalf("stored settings are not JSON: %v", err)
	}
	sec, _ := doc["security"].(map[string]interface{})
	auth, _ := sec["auth"].(map[string]interface{})
	if auth["apiKey"] != "sk-1" {
		t.Fatalf("sentinel did not keep the secret: %v", doc)
	}
	if doc["model"] != "m2" {
		t.Fatalf("an unrelated PUT dropped the model field: %v", doc)
	}
	tools, _ := doc["tools"].(map[string]interface{})
	if tools["x"] != float64(1) {
		t.Fatalf("unrelated PUT did not land: %v", doc)
	}
}

// A container recreation keeps the settings because they live on the working
// dir (the new backend instance shares the same persistent fs).
func TestCodingCliHome_SurvivesRecreation(t *testing.T) {
	fs := map[string]string{}
	first := newHomeFake(fs)
	h1 := homeTestHandler(t, first)
	if rec := putSettings(t, h1, `{"model":"persisted","security":{"auth":{"apiKey":"sk-1"}}}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	h2 := homeTestHandler(t, newHomeFake(fs))
	doc := getSettings(t, h2)
	if doc["exists"] != true {
		t.Fatalf("settings vanished after recreation: %v", doc)
	}
	settings, _ := doc["settings"].(map[string]interface{})
	if settings["model"] != "persisted" {
		t.Fatalf("GET after recreation = %v", settings)
	}
}

// Credentials that only exist under the legacy $HOME location still count as
// configured, and the first update migrates them to the working dir.
func TestCodingCliHome_LegacyHomeCredential(t *testing.T) {
	fs := map[string]string{
		"/root/.qwen/settings.json": `{"security":{"auth":{"apiKey":"sk-legacy"}},"model":"old"}`,
	}
	exec := newHomeFake(fs)
	h := homeTestHandler(t, exec)

	listRec := httptest.NewRecorder()
	h.listCLIs(listRec, adminCaller(codingCliRequest(http.MethodGet, "/coding-cli", "", "name", "daily-carol")))
	qc, _ := decodeJSON(t, listRec.Body.String())["qwen-code"].(map[string]interface{})
	auth, _ := qc["auth"].(map[string]interface{})
	if auth["configured"] != true {
		t.Fatalf("legacy $HOME credential not detected: %v", auth)
	}

	if rec := putSettings(t, h, `{"model":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	migrated := fs["/app/working/.qwen/settings.json"]
	if migrated == "" {
		t.Fatalf("settings were not migrated to the working dir: %v", fs)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(migrated), &doc); err != nil {
		t.Fatalf("migrated settings are not JSON: %v", err)
	}
	sec, _ := doc["security"].(map[string]interface{})
	authDoc, _ := sec["auth"].(map[string]interface{})
	if authDoc["apiKey"] != "sk-legacy" || doc["model"] != "new" {
		t.Fatalf("migration lost fields: %v", doc)
	}
}

// A persisted path containing a space must survive the shell round trip: the
// handler hands the path to `sh -c` as a positional argument, so `test -f`,
// `cat` and the auth probe never re-parse it. Regression: with `/tmp/working
// dir`, PUT returned 200 but the following GET reported exists:false and the
// auth probe configured:false, and the next unrelated PUT overwrote the
// credential.
func TestCodingCliHome_PathWithSpace(t *testing.T) {
	exec := newHomeFake(nil)
	exec.working = "/tmp/working dir"
	h := homeTestHandler(t, exec)
	const stored = "/tmp/working dir/.qwen/settings.json"

	rec := putSettings(t, h, `{"model":"m1","security":{"auth":{"apiKey":"sk-spaced"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := exec.fs[stored]; !ok {
		t.Fatalf("settings written outside the spaced home: %v", exec.fs)
	}

	doc := getSettings(t, h)
	if doc["exists"] != true {
		t.Fatalf("GET exists=%v after a successful PUT into a spaced path", doc["exists"])
	}
	settings, _ := doc["settings"].(map[string]interface{})
	if settings["model"] != "m1" {
		t.Fatalf("GET lost the written model: %v", settings)
	}
	security, _ := settings["security"].(map[string]interface{})
	authDoc, _ := security["auth"].(map[string]interface{})
	apiKey, _ := authDoc["apiKey"].(map[string]interface{})
	if apiKey["redacted"] != true || apiKey["has_value"] != true {
		t.Fatalf("response must redact the credential, got %v", authDoc["apiKey"])
	}

	listRec := httptest.NewRecorder()
	h.listCLIs(listRec, adminCaller(codingCliRequest(http.MethodGet, "/coding-cli", "", "name", "daily-carol")))
	qc, _ := decodeJSON(t, listRec.Body.String())["qwen-code"].(map[string]interface{})
	auth, _ := qc["auth"].(map[string]interface{})
	if auth["configured"] != true || auth["method"] != "settings" {
		t.Fatalf("auth probe with a spaced home: %v", auth)
	}

	// The sentinel keeps the stored credential and unrelated fields, and the
	// read that backs the merge must not fall over on the spaced path.
	if rec := putSettings(t, h, `{"security":{"auth":{"apiKey":"***"}},"env":{"KEEP":"1"}}`); rec.Code != http.StatusOK {
		t.Fatalf("sentinel PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	var merged map[string]interface{}
	if err := json.Unmarshal([]byte(exec.fs[stored]), &merged); err != nil {
		t.Fatalf("stored settings are not JSON: %v (%q)", err, exec.fs[stored])
	}
	sec, _ := merged["security"].(map[string]interface{})
	a, _ := sec["auth"].(map[string]interface{})
	if a["apiKey"] != "sk-spaced" {
		t.Fatalf("sentinel overwrote the credential: %v", merged)
	}
	if merged["model"] != "m1" {
		t.Fatalf("merge dropped an unrelated field: %v", merged)
	}
	env, _ := merged["env"].(map[string]interface{})
	if env["KEEP"] != "1" {
		t.Fatalf("merge dropped the new field: %v", merged)
	}
}

// Extra auth files (opencode auth.json) are probed with `test -s` on the same
// persisted home; a spaced path must not turn a configured CLI into
// configured:false.
func TestCodingCliHome_ExtraAuthFileWithSpace(t *testing.T) {
	exec := newHomeFake(map[string]string{
		"/tmp/working dir/.local/share/opencode/auth.json": `{"synthetic":{"type":"api","key":"synthetic"}}`,
	})
	exec.working = "/tmp/working dir"
	h := homeTestHandler(t, exec)

	listRec := httptest.NewRecorder()
	h.listCLIs(listRec, adminCaller(codingCliRequest(http.MethodGet, "/coding-cli", "", "name", "daily-carol")))
	oc, _ := decodeJSON(t, listRec.Body.String())["opencode"].(map[string]interface{})
	auth, _ := oc["auth"].(map[string]interface{})
	if auth["configured"] != true || auth["method"] != "auth-file" {
		t.Fatalf("extra auth file under a spaced home: %v", auth)
	}
	// Settings read/write on the same spaced home still round-trips.
	putRec := httptest.NewRecorder()
	h.putCliSettings(putRec, adminCaller(codingCliRequest(http.MethodPut, "/settings", `{"model":"oc-1"}`, "name", "daily-carol", "cli", "opencode")))
	if putRec.Code != http.StatusOK {
		t.Fatalf("opencode PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	getRec := httptest.NewRecorder()
	h.getCliSettings(getRec, adminCaller(codingCliRequest(http.MethodGet, "/settings", "", "name", "daily-carol", "cli", "opencode")))
	doc := decodeJSON(t, getRec.Body.String())
	if doc["exists"] != true {
		t.Fatalf("opencode GET exists=%v body=%v", doc["exists"], doc)
	}
}
