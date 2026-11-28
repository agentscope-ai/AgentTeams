package server

import (
	"encoding/json"
	"net/http"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func TestWorkerAgentApprovalModeUpdate(t *testing.T) {
	h, _ := newL2UpdateRig(t)
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
	for _, mode := range []string{"ask", "allow"} {
		r := l2UpdateRequest(t, h, "alpha-dev", `{"approvalMode":"`+mode+`"}`, admin)
		var response map[string]any
		if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if r.Code != http.StatusOK || response["approvalMode"] != mode {
			t.Fatalf("mode %s: %d %s", mode, r.Code, r.Body.String())
		}
	}
	invalid := l2UpdateRequest(t, h, "alpha-dev", `{"approvalMode":"typo"}`, admin)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode: %d %s", invalid.Code, invalid.Body.String())
	}
	leader := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Teams: []string{"alpha-team"}}
	r := l2UpdateRequest(t, h, "alpha-dev", `{"approvalMode":"allow"}`, leader)
	if r.Code == http.StatusOK {
		t.Fatal("a Team Leader must not grant itself broader tool permissions")
	}
}
