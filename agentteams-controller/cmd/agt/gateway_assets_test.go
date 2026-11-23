package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGatewayCLIUsesSharedAssetAPIAndWorkerProbe(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(paths) == 1 && (body["action"] != "save" || body["input"] == nil) {
			t.Error("missing asset input")
		}
		if len(paths) == 2 && (body["kind"] != "mcp" || body["server"] != "docs") {
			t.Error("incorrect Worker probe")
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	file := filepath.Join(t.TempDir(), "mcp.yaml")
	_ = os.WriteFile(file, []byte("name: docs\nconnection:\n  kind: proxy\n"), 0600)
	for _, args := range [][]string{{"gateway", "mcps", "save", "-f", file}, {"gateway", "mcps", "verify", "docs", "--worker", "alice"}} {
		cmd := newRootCommand("agt")
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) != 2 || paths[0] != "/api/v1/gateway/assets/mcps" || paths[1] != "/api/v1/workers/alice/gateway-probe" {
		t.Fatal(paths)
	}
}
