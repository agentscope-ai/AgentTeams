package service

import (
	"context"
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

// This verifies the shared configuration projection, not native Harness
// startup, message consumption or execution recovery.
func TestWorkerConfigurationSurvivesHarnessChanges(t *testing.T) {
	harnesses := []string{"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"}
	for _, source := range harnesses {
		for _, target := range harnesses {
			t.Run(source+"_to_"+target, func(t *testing.T) {
				ctx := context.Background()
				store := ossfake.NewMemory()
				req := MemberRuntimeConfigDeployRequest{
					Name: "worker-cr-a", RuntimeName: "worker-a", Runtime: source,
					Role: "worker", Generation: 1, TeamName: "team-a",
					MatrixUserID: "@worker-a:matrix.test", PersonalRoomID: "!worker:matrix.test",
					Spec: v1beta1.WorkerSpec{
						Model: "team-model", Identity: "developer", Soul: "Be concise", Agents: "Verify your work",
						Skills:        []string{"github"},
						McpServers:    []v1beta1.MCPServer{{Name: "tools", URL: "https://gateway.test/mcp", Transport: "http"}},
						ChannelPolicy: &v1beta1.ChannelPolicySpec{GroupAllowExtra: []string{"@leader:matrix.test"}},
						AgentIdentity: &v1beta1.AgentIdentitySpec{WorkloadIdentityName: "worker-identity"},
						CredentialBindings: []v1beta1.CredentialBinding{{
							CredentialRef: v1beta1.CredentialRef{TokenVaultName: "default", APIKeyCredentialProviderName: "GITHUB_TOKEN"},
							ToolWhitelist: []string{"gh"},
						}},
					},
				}
				deploy := func() memberRuntimeConfigDocument {
					t.Helper()
					// A fresh Deployer has no source Harness directory or cached
					// native configuration available to reconstruct this Worker.
					if err := NewDeployer(DeployerConfig{OSS: store}).DeployMemberRuntimeConfig(ctx, req); err != nil {
						t.Fatal(err)
					}
					body, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
					if err != nil {
						t.Fatal(err)
					}
					var doc memberRuntimeConfigDocument
					if err := yaml.Unmarshal(body, &doc); err != nil {
						t.Fatal(err)
					}
					return doc
				}
				before := deploy()
				req.Runtime, req.Generation = target, 2
				after := deploy()
				if after.Member.Runtime != target || after.Metadata.Generation != 2 {
					t.Fatal("the target Harness or configuration generation was not projected")
				}
				after.Member.Runtime = before.Member.Runtime
				if !reflect.DeepEqual(before.Member, after.Member) || !reflect.DeepEqual(before.Team, after.Team) ||
					!reflect.DeepEqual(before.Desired, after.Desired) || !reflect.DeepEqual(before.Credentials, after.Credentials) ||
					!reflect.DeepEqual(before.Storage, after.Storage) {
					t.Fatal("changing Harness changed the Worker's identity, settings, credential bindings or storage scope")
				}
				keys, err := store.ListObjects(ctx, "agents/")
				if err != nil || len(keys) != 1 {
					t.Fatalf("configuration must remain in one Worker scope: keys=%v error=%v", keys, err)
				}
			})
		}
	}
}
