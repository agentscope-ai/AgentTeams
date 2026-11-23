package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskRoomCommandsUseControllerIdentity(t *testing.T) {
	for _, action := range []string{"archive", "unarchive", "delete", "status"} {
		t.Run(action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, operation := "POST", action
				if action == "status" {
					method, operation = "GET", "archive"
				}
				if r.Method != method || r.URL.Path != "/api/v1/task-rooms/!room:test/"+operation {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer leader-token" {
					t.Error("caller credential was not preserved")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "leader-token")
			cmd := newRootCommand("agt")
			cmd.SetArgs([]string{"task-room", action, "!room:test"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
