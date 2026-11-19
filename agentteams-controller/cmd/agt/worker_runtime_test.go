package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerRuntimeHandoffUsesControllerConditionAndStableRequest(t *testing.T) {
	for _, tc := range []struct{ name, etag, id, flag string }{
		{"new", "", "", "If-None-Match"},
		{"retry", `"existing"`, "request-one", "If-Match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/v1/workers/alice/runtime-handoff" || r.Header.Get("Authorization") != "Bearer test" {
					t.Errorf("unexpected route or identity")
				}
				if r.Method == "GET" {
					if tc.etag != "" {
						w.Header().Set("ETag", tc.etag)
						fmt.Fprint(w, `{"handoff":{"id":"request-one","phase":"draining","target_runtime":"hermes"}}`)
					} else {
						fmt.Fprint(w, `{}`)
					}
					return
				}
				condition := tc.etag
				if condition == "" {
					condition = "*"
				}
				if r.Method != "POST" || r.Header.Get(tc.flag) != condition {
					t.Errorf("condition missing %v", r.Header)
				}
				var req struct {
					ID     string `json:"request_id"`
					Target string `json:"target_runtime"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" || req.Target != "hermes" || (tc.id != "" && req.ID != tc.id) {
					t.Errorf("bad request: %+v %v", req, err)
				}
				fmt.Fprint(w, `{"handoff":{"id":"request-one","phase":"draining","target_runtime":"hermes"}}`)
			}))
			defer s.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", s.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "test")
			var out bytes.Buffer
			cmd := workerCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"switch-runtime", "--name", "alice", "--runtime", "hermes"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !strings.Contains(out.String(), "draining") {
				t.Fatalf("calls=%d, output=%s", calls, out.String())
			}
		})
	}
}

func TestWorkerRuntimeStatusOnlyReadsCurrentOperation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("status mutated state")
		}
		fmt.Fprint(w, `{"handoff":{"id":"saved","phase":"recovering","target_runtime":"qwenpaw"},"target_failure":"Container ran out of memory (OOMKilled)."}`)
	}))
	defer s.Close()
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", s.URL)
	for _, format := range [][]string{nil, {"-o", "json"}} {
		var out bytes.Buffer
		cmd := workerCmd()
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"runtime-status", "--name", "alice"}, format...))
		if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "recovering") || !strings.Contains(out.String(), "OOMKilled") {
			t.Fatalf("out=%s err=%v", out.String(), err)
		}
	}
}
