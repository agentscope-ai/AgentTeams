package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessageUsesCurrentManagerIdentityAndMentions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || !strings.Contains(r.URL.Path, "/rooms/!leader:matrix.local/send/m.room.message/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer manager-matrix" {
			t.Errorf("message used the wrong identity")
		}
		var body struct {
			Body     string `json:"body"`
			Mentions struct {
				UserIDs []string `json:"user_ids"`
			} `json:"m.mentions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Body != "@leader:matrix.local Please coordinate this task." || len(body.Mentions.UserIDs) != 1 || body.Mentions.UserIDs[0] != "@leader:matrix.local" {
			t.Errorf("wrong message %+v", body)
		}
		_, _ = w.Write([]byte(`{"event_id":"$sent"}`))
	}))
	defer server.Close()
	t.Setenv("AGENTTEAMS_AGENT_ROLE", "manager")
	t.Setenv("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "manager-matrix")
	t.Setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "other-worker")
	t.Setenv("AGENTTEAMS_MATRIX_URL", server.URL)
	cmd := messageCmd()
	cmd.SetArgs([]string{"--room", "!leader:matrix.local", "--text", "@leader:matrix.local Please coordinate this task."})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestReadyUsesResourceIdentityForEachRole(t *testing.T) {
	for _, role := range []string{"manager", "worker"} {
		t.Run(role, func(t *testing.T) {
			resource := "workers"
			if role == "manager" {
				resource = "managers"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/"+resource+"/default/ready" {
					t.Errorf("wrong readiness path %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_AGENT_ROLE", role)
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_WORKER_CR_NAME", "default")
			if err := workerReportReadyCmd().Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
