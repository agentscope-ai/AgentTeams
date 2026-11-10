package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTracingCommandsUseCallerToken(t *testing.T) {
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tasks":[],"retrieval_complete":true}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "leader-token")
	root := newRootCommand("agt")
	root.SetArgs([]string{"get", "tasks", "--team", "alpha", "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	root = newRootCommand("agt")
	root.SetArgs([]string{"tracing", "task", "p", "work", "--team", "alpha", "--related"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/tasks", "/api/v1/projects/p/tasks/work/traces"} {
		r := <-requests
		if r.URL.Path != path || r.URL.Query().Get("team") != "alpha" || r.Header.Get("Authorization") != "Bearer leader-token" {
			t.Fatalf("request=%v", r)
		}
		if path != "/api/v1/tasks" && r.URL.Query().Get("includeRelated") != "true" {
			t.Fatal("missing related flag")
		}
	}
}
