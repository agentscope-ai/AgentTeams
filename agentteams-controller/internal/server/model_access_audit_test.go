package server

import (
	"context"
	"encoding/json"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestModelAccessAuditChangesWithEqualRowCounts(t *testing.T) {
	const beforeJSON = `{"scopes":[{"role":"worker","main":{"entries":[{"provider":"gateway-a","model":"model-a"}]},"spawn":{"inherit":true},"codingCli":[{"cli":"qwen-code","inherit":true}]}]}`
	for _, tc := range []struct {
		name, afterJSON string
		want            int
	}{
		{"provider-and-model", `{"scopes":[{"role":"worker","main":{"entries":[{"provider":"gateway-b","model":"model-b"}]},"spawn":{"inherit":true},"codingCli":[{"cli":"qwen-code","inherit":true}]}]}`, 1},
		{"spawn-inherit", `{"scopes":[{"role":"worker","main":{"entries":[{"provider":"gateway-a","model":"model-a"}]},"spawn":{"inherit":false},"codingCli":[{"cli":"qwen-code","inherit":true}]}]}`, 1},
		{"cli-inherit", `{"scopes":[{"role":"worker","main":{"entries":[{"provider":"gateway-a","model":"model-a"}]},"spawn":{"inherit":true},"codingCli":[{"cli":"qwen-code","inherit":false}]}]}`, 1},
		{"identical-rewrite", beforeJSON, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after v1beta1.ModelAccessSpec
			if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.afterJSON), &after); err != nil {
				t.Fatal(err)
			}
			store := ossfake.NewMemory()
			h := &ResourceHandler{audit: audit.NewClient(store)}
			h.auditModelAccessChange(context.Background(), "alice", &authpkg.CallerIdentity{Username: "admin", Role: authpkg.RoleAdmin}, &before, &after)
			result, err := audit.NewQuery(store).List(context.Background(), audit.QueryOptions{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Events) != tc.want {
				t.Fatalf("durable audit events=%d want=%d", len(result.Events), tc.want)
			}
			if tc.want == 1 && (result.Events[0].Action != "model_access_update" || result.Events[0].Target != "alice") {
				t.Fatalf("unexpected audit event: %+v", result.Events[0])
			}
		})
	}
}
