package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
)

func TestCodingCliQoderSettingsDirectory(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"default", "/root/.qoder/settings.json"},
		{"custom", "/tmp/custom-qoder/settings.json"},
		{"special-characters", "/tmp/qoder settings 'quoted' $HOME $(printf wrong);literal~/settings.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecBackend{fileWrites: map[string]string{tc.path: `{"keep":true,"api_key":"synthetic-existing"}`}}
			exec.respond = func(cmd []string) (string, string, int, error) {
				if cmd[0] == "sh" && strings.Contains(cmd[2], "QODER_CONFIG_DIR") {
					return tc.path, "", 0, nil
				}
				if len(cmd) == 3 && cmd[0] == "cat" && cmd[1] == "--" && cmd[2] == tc.path {
					if data, ok := exec.written(tc.path); ok {
						return data, "", 0, nil
					}
				}
				return "", "not found", 1, nil
			}
			h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				rec := httptest.NewRecorder()
				req := adminCaller(codingCliRequest(method, "/settings", `{"model":"updated","api_key":"***"}`, "name", "daily-carol", "cli", "qoder"))
				if method == http.MethodGet {
					h.getCliSettings(rec, req)
				} else {
					h.putCliSettings(rec, req)
				}
				assertQoderSettingsResponse(t, rec, tc.path)
			}
			written, _ := exec.written(tc.path)
			doc := decodeJSON(t, written)
			if doc["keep"] != true || doc["model"] != "updated" || doc["api_key"] != "synthetic-existing" {
				t.Fatalf("update did not preserve/merge the effective settings: %s", written)
			}
			delete(exec.fileWrites, tc.path)
			rec := httptest.NewRecorder()
			h.getCliSettings(rec, adminCaller(codingCliRequest(http.MethodGet, "/settings", "", "name", "daily-carol", "cli", "qoder")))
			if rec.Code != http.StatusOK || decodeJSON(t, rec.Body.String())["exists"] != false || decodeJSON(t, rec.Body.String())["path"] != tc.path {
				t.Fatalf("missing-file response has the wrong path/state: %s", rec.Body.String())
			}
		})
	}
}

func assertQoderSettingsResponse(t *testing.T, rec *httptest.ResponseRecorder, path string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	doc := decodeJSON(t, rec.Body.String())
	settings, _ := doc["settings"].(map[string]interface{})
	if doc["path"] != path || doc["exists"] != true || settings["keep"] != true {
		t.Fatalf("response missed the effective settings: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "synthetic-existing") {
		t.Fatal("response exposed the synthetic credential")
	}
}

func TestCodingCliQoderSettingsPathFailureDoesNotWrite(t *testing.T) {
	for _, failure := range []struct {
		name string
		exit int
		err  error
	}{
		{"transport", 0, errors.New("test transport failure")},
		{"command", 1, nil},
	} {
		t.Run(failure.name, func(t *testing.T) {
			exec := &fakeExecBackend{fileWrites: map[string]string{}}
			exec.respond = func([]string) (string, string, int, error) { return "", "", failure.exit, failure.err }
			h := newTestCodingCliHandler(t, "embedded", exec, nil, checkpointTeamWithWorkers("team-a", "daily-carol")...)
			rec := httptest.NewRecorder()
			h.putCliSettings(rec, adminCaller(codingCliRequest(http.MethodPut, "/settings", `{"model":"updated"}`, "name", "daily-carol", "cli", "qoder")))
			if rec.Code != http.StatusBadGateway || len(exec.fileWrites) != 0 {
				t.Fatalf("path failure must not write: status=%d writes=%v", rec.Code, exec.fileWrites)
			}
		})
	}
}

// Opt in only with a disposable Worker container and an explicit expected path.
// No Qoder account or model call is needed to validate the settings API.
func TestCodingCliQoderSettingsDockerRoundTrip(t *testing.T) {
	socket := os.Getenv("CODING_CLI_TEST_DOCKER_SOCKET")
	worker := os.Getenv("CODING_CLI_TEST_DOCKER_CONTAINER")
	path := os.Getenv("CODING_CLI_TEST_QODER_SETTINGS_PATH")
	if socket == "" || worker == "" || path == "" {
		t.Skip("requires an isolated Docker fixture and CODING_CLI_TEST_QODER_SETTINGS_PATH")
	}
	eb := backend.NewDockerBackend(backend.DockerConfig{SocketPath: socket}, "")
	ctx := context.Background()
	if err := eb.WriteFile(ctx, worker, path, `{"keep":true,"api_key":"synthetic-existing"}`); err != nil {
		t.Fatal(err)
	}
	h := newTestCodingCliHandler(t, "embedded", nil, nil, checkpointTeamWithWorkers("team-a", worker)...)
	h.execBackend = func(context.Context, string) (codingCliExecer, error) { return eb, nil }
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		req := adminCaller(codingCliRequest(method, "/settings", `{"model":"updated","api_key":"***"}`, "name", worker, "cli", "qoder"))
		if method == http.MethodGet {
			h.getCliSettings(rec, req)
		} else {
			h.putCliSettings(rec, req)
		}
		assertQoderSettingsResponse(t, rec, path)
	}
	out, _, exit, err := eb.Exec(ctx, worker, []string{"cat", "--", path}, codingCliExecTimeout)
	if err != nil || exit != 0 {
		t.Fatalf("readback exit=%d err=%v", exit, err)
	}
	doc := decodeJSON(t, out)
	if doc["keep"] != true || doc["model"] != "updated" || doc["api_key"] != "synthetic-existing" {
		t.Fatalf("effective settings were not updated: %s", out)
	}
}
