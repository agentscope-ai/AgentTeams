package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
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
				wantGeneration := int64(2)
				if source == target {
					wantGeneration = before.Metadata.Generation // Identical recreation is not a config change.
				}
				if after.Member.Runtime != target || after.Metadata.Generation != wantGeneration {
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

func TestWorkerConfigurationCarriesResolvedMatrixPolicyWithoutNativeSecrets(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	// The Controller already resolved role/Human allowlists when publishing
	// this Worker-level compatibility projection. Do not recompute policy in
	// each Harness, and do not carry native device credentials into the record.
	compatibility := []byte(`{"channels":{"matrix":{"homeserver":"https://matrix.test","userId":"@worker-a:matrix.test","accessToken":"old-device-secret","deviceId":"old-device","encryption":true,"dm":{"policy":"allowlist","allowFrom":["@admin:matrix.test"]},"groupPolicy":"allowlist","groupAllowFrom":["@leader:matrix.test"],"requireMention":true}},"messages":{"groupChat":{"historyLimit":24}}}`)
	if err := store.PutObject(ctx, "agents/worker-a/openclaw.json", compatibility); err != nil {
		t.Fatal(err)
	}
	err := NewDeployer(DeployerConfig{OSS: store}).DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name: "worker-cr-a", RuntimeName: "worker-a", Runtime: "hermes", Role: "worker",
		MatrixUserID: "@worker-a:matrix.test", PersonalRoomID: "!worker:matrix.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Matrix struct {
			Transport map[string]any `json:"transport"`
		} `json:"matrix"`
	}
	if err := yaml.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	policy := record.Matrix.Transport
	if policy["homeserver"] != "https://matrix.test" || policy["groupPolicy"] != "allowlist" || policy["encryption"] != true || policy["historyLimit"] != float64(24) {
		t.Fatalf("missing Worker-owned Matrix transport: %v", policy)
	}
	got, _ := json.Marshal(policy["groupAllowFrom"])
	if string(got) != `["@leader:matrix.test"]` {
		t.Fatalf("resolved group allowlist changed: %s", got)
	}
	if strings.Contains(string(body), "old-device") || strings.Contains(string(body), "accessToken") {
		t.Fatal("Worker transport projection copied native device credentials")
	}
}
