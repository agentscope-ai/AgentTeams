package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateHumanPermissionLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  int
	}{
		{name: "default worker scope", want: 3},
		{name: "admin", flags: []string{"--permission-level", "1"}, want: 1},
		{name: "team", flags: []string{"--permission-level", "2"}, want: 2},
		{name: "worker", flags: []string{"--permission-level", "3"}, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				PermissionLevel int `json:"permissionLevel"`
			}
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/humans" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"name":"alice"}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			cmd := createHumanCmd()
			cmd.SetArgs(append([]string{"--name", "alice", "--display-name", "Alice"}, tc.flags...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called || got.PermissionLevel != tc.want {
				t.Fatalf("request sent=%v, permissionLevel=%d, want %d", called, got.PermissionLevel, tc.want)
			}
		})
	}
}

func TestCreateHumanRejectsInvalidPermissionBeforeRequest(t *testing.T) {
	for _, level := range []string{"-1", "0", "4", "50", "100"} {
		t.Run(level, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			cmd := createHumanCmd()
			cmd.SetArgs([]string{"--name", "alice", "--display-name", "Alice", "--permission-level", level})
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected permission-level validation error")
			}
			if called {
				t.Fatal("invalid permission level reached the Controller")
			}
		})
	}
}
