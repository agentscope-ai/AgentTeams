package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

func TestGetManagersJSONPreservesReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, list := range []bool{false, true} {
			t.Run("ready="+strconv.FormatBool(ready)+"/list="+strconv.FormatBool(list), func(t *testing.T) {
				item := map[string]any{"name": "default", "ready": ready}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if list {
						json.NewEncoder(w).Encode(map[string]any{"managers": []any{item}, "total": 1})
					} else {
						json.NewEncoder(w).Encode(item)
					}
				}))
				defer srv.Close()
				t.Setenv("AGENTTEAMS_CONTROLLER_URL", srv.URL)
				t.Setenv("AGENTTEAMS_AUTH_TOKEN", "synthetic-token")

				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				original := os.Stdout
				os.Stdout = w
				defer func() { os.Stdout = original }()
				cmd := getManagersCmd()
				args := []string{"-o", "json"}
				if !list {
					args = append([]string{"default"}, args...)
				}
				cmd.SetArgs(args)
				err = cmd.Execute()
				w.Close()
				os.Stdout = original
				if err != nil {
					t.Fatal(err)
				}
				out, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]any
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
				if list {
					result = result["managers"].([]any)[0].(map[string]any)
				}
				if value, ok := result["ready"].(bool); !ok || value != ready {
					t.Fatalf("ready was lost or changed: %s", out)
				}
			})
		}
	}
}
