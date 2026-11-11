package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTracingSessionAndDetailJSON(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","observations":[{"name":"terminal","level":"WARNING","statusMessage":"retry recovered"}]}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "caller")
	for _, args := range [][]string{{"tracing", "session", "agent:session", "-o", "json"}, {"tracing", "show", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "-o", "json"}, {"tracing", "show", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "-o", "table"}} {
		root := newRootCommand("agt")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if args[1] == "session" && (got.URL.Query().Get("session") != "agent:session" || got.URL.Query().Has("since")) {
			t.Fatal("session query lost all-time semantics")
		}
		if args[1] == "show" && got.URL.Path != "/api/v1/traces/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("wrong detail path: %s", got.URL.Path)
		}
		if args[len(args)-1] == "json" && !json.Valid(out.Bytes()) {
			t.Fatalf("invalid JSON: %s", out.String())
		}
		if args[len(args)-1] == "table" && !bytes.Contains(out.Bytes(), []byte("retry recovered")) {
			t.Fatalf("missing observation: %s", out.String())
		}
	}
}

func TestTracingRelativeSince(t *testing.T) {
	before := time.Now().Add(-time.Hour)
	parsed, err := traceQueryTime("1h", true)
	if err != nil || parsed.Before(before) || parsed.After(time.Now().Add(-time.Hour)) {
		t.Fatalf("invalid relative time: %s %v", parsed, err)
	}
}

func TestGeneralTracingQueriesUseCallerAndDimensions(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"abc","userId":"worker","name":"turn","sessionId":"session"}],"meta":{"page":1,"totalPages":2,"totalItems":12}}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
	t.Setenv("AGENTTEAMS_AUTH_TOKEN", "caller-token")
	for _, args := range [][]string{
		{"tracing", "--agent", "worker"},
		{"tracing", "worker", "worker"},
		{"tracing", "agent", "worker"},
		{"tracing", "list", "--worker", "worker"},
	} {
		root := newRootCommand("agt")
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetArgs(append(args, "--team", "alpha", "--session", "session", "--task", "task", "--project", "project", "--since", "2026-11-11T08:00:00+08:00", "--until", "2026-11-11T10:00:00+08:00", "--limit", "10"))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		q := got.URL.Query()
		if got.Header.Get("Authorization") != "Bearer caller-token" || got.URL.Path != "/api/v1/traces" || q.Get("worker") != "worker" || q.Get("team") != "alpha" || q.Get("session") != "session" || q.Get("task") != "task" || q.Get("project") != "project" || q.Get("limit") != "10" || q.Get("since") != "2026-11-11T00:00:00Z" {
			t.Fatalf("wrong request: %v", got)
		}
		if !bytes.Contains(output.Bytes(), []byte("worker")) || !bytes.Contains(output.Bytes(), []byte("12")) {
			t.Fatalf("missing table/pagination: %s", output.String())
		}
	}
}

func TestGeneralTracingRejectsInvalidOptionsBeforeRequest(t *testing.T) {
	for _, args := range [][]string{
		{"--since", "yesterday"}, {"--since", "-1h"}, {"--until", "2026-11-11T08:00:00"},
		{"--since", "2026-11-12T00:00:00Z", "--until", "2026-11-11T00:00:00Z"},
		{"--limit", "0"}, {"--page", "0"}, {"-o", "xml"}, {"--agent", "one", "--worker", "two"},
	} {
		root := newRootCommand("agt")
		root.SilenceUsage, root.SilenceErrors = true, true
		root.SetArgs(append([]string{"tracing"}, args...))
		if err := root.Execute(); err == nil {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
}
