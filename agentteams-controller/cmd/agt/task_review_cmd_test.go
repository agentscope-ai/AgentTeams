package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskReviewCommandsSendOneExactCandidateDecision(t *testing.T) {
	for _, action := range []string{"accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var input map[string]string
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/t1/"+action || r.URL.Query().Get("team") != "alpha" || json.NewDecoder(r.Body).Decode(&input) != nil || len(input) != 2 || input["submissionId"] != "candidate-1" || input["note"] != "Independent business evidence" {
					t.Errorf("unexpected review request: %s %s %+v", r.Method, r.URL, input)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"review":{"id":"review-1","submissionId":"candidate-1"},"mainProjection":"pending"}`))
			}))
			defer server.Close()
			t.Setenv("AGENTTEAMS_CONTROLLER_URL", server.URL)
			t.Setenv("AGENTTEAMS_AUTH_TOKEN", "fixture")
			cmd := newRootCommand("agt")
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"task", action, "t1", "--submission", "candidate-1", "--note", "Independent business evidence", "--team", "alpha"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var result struct {
				MainProjection string `json:"mainProjection"`
			}
			if json.Unmarshal(output.Bytes(), &result) != nil || result.MainProjection != "pending" || calls != 1 {
				t.Fatalf("receipt lost: %d %s", calls, output.String())
			}
		})
	}
}

func TestTaskReviewCommandsRejectIncompleteDecisionsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"task", "accept", "t1", "--note", "Evidence"},
		{"task", "reject", "t1", "--submission", "candidate"},
		{"task", "accept", "../t1", "--submission", "candidate", "--note", "Evidence"},
		{"task", "accept", "t1", "--submission", "candidate", "--note", "   "},
	} {
		cmd := newRootCommand("agt")
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid decision accepted: %v", args)
		}
	}
}
