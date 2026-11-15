package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHeartbeatCLIExplicitFalse(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(map[bool]string{true: "create", false: "update"}[create], func(t *testing.T) {
			var body map[string]any
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				io.WriteString(w, `{"name":"alice","phase":"Pending"}`)
			}))
			defer s.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", s.URL)
			cmd := updateWorkerCmd()
			args := []string{"--name", "alice", "--llm-heartbeat-enabled=false"}
			if create {
				cmd = createWorkerCmd()
				args = append(args, "--runtime", "openclaw", "--no-wait")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if value, ok := body["llmHeartbeatEnabled"]; !ok || value != false {
				t.Fatalf("explicit false lost: %#v", body)
			}
		})
	}
}
