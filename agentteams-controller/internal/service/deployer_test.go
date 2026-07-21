package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

func TestDeployWorkerConfigSeedsLocalFilesWithoutOverwritingRuntimeState(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentFSDir := filepath.Join(tmp, "agents")
	workerDir := filepath.Join(agentFSDir, "alice")
	if err := os.MkdirAll(filepath.Join(workerDir, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "config", "credagent.json"), []byte(`{"source":"template"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "notes.md"), []byte("template note"), 0644); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/config/credagent.json", []byte(`{"source":"runtime"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, "agents/alice/openclaw.json", []byte(`{"old":true}`)); err != nil {
		t.Fatal(err)
	}

	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  agentFSDir,
	})
	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:        "alice",
		MatrixToken: "matrix-token",
		GatewayKey:  "gateway-key",
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/config/credagent.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"source":"runtime"}` {
		t.Fatalf("credagent.json overwritten: %s", got)
	}

	got, err = store.GetObject(ctx, "agents/alice/notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "template note" {
		t.Fatalf("notes.md not seeded: %s", got)
	}

	got, err = store.GetObject(ctx, "agents/alice/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "gateway-key") {
		t.Fatalf("openclaw.json was not overwritten by controller config: %s", got)
	}

	got, err = store.GetObject(ctx, "agents/alice/.agentteams-keep")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("directory marker should be empty, got %q", got)
	}
}

func TestEnsureTeamStorageCreatesPrefixMarkers(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	if err := deployer.EnsureTeamStorage(ctx, "alpha"); err != nil {
		t.Fatalf("EnsureTeamStorage failed: %v", err)
	}

	for _, key := range []string{
		"teams/alpha/.agentteams-keep",
		"teams/alpha/shared/.agentteams-keep",
		"teams/alpha/shared/tasks/.keep",
		"teams/alpha/shared/projects/.keep",
		"teams/alpha/shared/knowledge/.keep",
	} {
		got, err := store.GetObject(ctx, key)
		if err != nil {
			t.Fatalf("missing %s: %v", key, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s should be empty, got %q", key, got)
		}
	}
}

func TestDeployWorkerConfigInjectsTeamLeaderContext(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentFSDir := filepath.Join(tmp, "agents")
	leaderDir := filepath.Join(agentFSDir, "leader")
	if err := os.MkdirAll(leaderDir, 0755); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  agentFSDir,
	})

	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:           "leader",
		Role:           "team_leader",
		TeamName:       "demo-team",
		TeamRoomID:     "!team:matrix.local",
		LeaderDMRoomID: "!leader:matrix.local",
		MatrixToken:    "matrix-token",
		GatewayKey:     "gateway-key",
		TeamMembers: []RuntimeConfigTeamMember{{
			Name:           "leader",
			RuntimeName:    "leader",
			Role:           "team_leader",
			PersonalRoomID: "!leader:matrix.local",
		}, {
			Name:           "dev",
			RuntimeName:    "dev",
			Role:           "worker",
			PersonalRoomID: "!dev:matrix.local",
		}},
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/leader/AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"hiclaw-team-context-start",
		"demo-team",
		"Upstream",
		"@manager:",
		"dev",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("leader AGENTS.md missing %q:\n%s", want, text)
		}
	}
}

func TestDeployWorkerConfigInlineSoulOverridesPackageSeed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentFSDir := filepath.Join(tmp, "agents")
	workerDir := filepath.Join(agentFSDir, "alice")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "SOUL.md"), []byte("OVERRIDDEN SOUL FROM INLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	store := ossfake.NewMemory()
	if err := store.PutObject(ctx, "agents/alice/SOUL.md", []byte("ORIGINAL SOUL FROM PACKAGE\n")); err != nil {
		t.Fatal(err)
	}

	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  agentFSDir,
	})
	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:        "alice",
		MatrixToken: "matrix-token",
		GatewayKey:  "gateway-key",
		IsUpdate:    true,
		Spec: v1beta1.WorkerSpec{
			Runtime: "hermes",
			Soul:    "OVERRIDDEN SOUL FROM INLINE",
		},
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/SOUL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "OVERRIDDEN SOUL FROM INLINE") {
		t.Fatalf("SOUL.md did not contain inline content: %s", got)
	}
	if strings.Contains(string(got), "ORIGINAL SOUL FROM PACKAGE") {
		t.Fatalf("SOUL.md still contains package seed content: %s", got)
	}
}

func TestDeployWorkerConfigMergesMcporterConfigPreservingExternalMCP(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store := ossfake.NewMemory()
	existing := []byte(`{
  "mcpServers": {
    "github": {
      "url": "https://gw.example.com/mcp-servers/github/mcp",
      "transport": "http",
      "headers": {"Authorization": "Bearer OLD"}
    },
    "stale-gateway": {
      "url": "https://gw.example.com/mcp-servers/stale-gateway/mcp",
      "transport": "http",
      "headers": {"Authorization": "Bearer OLD"}
    },
    "external": {
      "url": "https://external.example.com/mcp",
      "transport": "sse",
      "headers": {"Authorization": "Bearer EXTERNAL"},
      "custom": true
    }
  }
}`)
	if err := store.PutObject(ctx, "agents/alice/config/mcporter.json", existing); err != nil {
		t.Fatal(err)
	}

	deployer := NewDeployer(DeployerConfig{
		AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}),
		OSS:         store,
		AgentFSDir:  filepath.Join(tmp, "agents"),
	})
	err := deployer.DeployWorkerConfig(ctx, WorkerDeployRequest{
		Name:       "alice",
		GatewayKey: "NEW",
		McpServers: []v1beta1.MCPServer{
			{Name: "github", URL: "https://gw.example.com/mcp-servers/github/mcp"},
			{Name: "jira", URL: "https://gw.example.com/mcp-servers/jira/mcp", Transport: "sse"},
		},
	})
	if err != nil {
		t.Fatalf("DeployWorkerConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/alice/config/mcporter.json")
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]map[string]map[string]interface{}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("invalid mcporter JSON: %v", err)
	}
	servers := decoded["mcpServers"]
	if len(servers) != 3 {
		t.Fatalf("servers=%v, want github+jira+external", servers)
	}
	if _, ok := servers["stale-gateway"]; ok {
		t.Fatalf("stale current-gateway MCP should have been removed: %v", servers)
	}
	if servers["github"]["headers"].(map[string]interface{})["Authorization"] != "Bearer NEW" {
		t.Fatalf("github should be overwritten by CR config: %v", servers["github"])
	}
	if servers["jira"]["transport"] != "sse" {
		t.Fatalf("jira transport=%v, want sse", servers["jira"]["transport"])
	}
	if servers["external"]["headers"].(map[string]interface{})["Authorization"] != "Bearer EXTERNAL" {
		t.Fatalf("external MCP should be preserved: %v", servers["external"])
	}
	if servers["external"]["custom"] != true {
		t.Fatalf("external MCP custom fields should be preserved: %v", servers["external"])
	}
}

func TestDeployMemberRuntimeConfigWritesAgentScopedYaml(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			StorageProvider:           "oss",
			StorageBucket:             "agentteams-storage",
			StorageEndpoint:           "https://oss.example.com",
			AIGatewayURL:              "https://aigw.example.com",
			AgentIdentityDataEndpoint: "agentidentitydata.cn-beijing.aliyuncs.com",
		},
	})
	state := "Running"

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:              "worker-cr-a",
		RuntimeName:       "worker-a",
		Runtime:           "qwenpaw",
		Role:              "worker",
		Generation:        12,
		MatrixUserID:      "@worker-a:matrix.local",
		PersonalRoomID:    "!worker-dm:matrix.local",
		TeamName:          "demo-team",
		TeamRoomID:        "!team:matrix.local",
		LeaderName:        "leader",
		LeaderRuntimeName: "leader-runtime",
		LeaderDMRoomID:    "!leader-dm:matrix.local",
		TeamAdminName:     "admin",
		TeamAdminMatrixID: "@admin:matrix.local",
		TeamMembers: []RuntimeConfigTeamMember{{
			Name:           "leader",
			RuntimeName:    "leader-runtime",
			Role:           "team_leader",
			MatrixUserID:   "@leader-runtime:matrix.local",
			PersonalRoomID: "!leader-dm:matrix.local",
		}, {
			Name:           "worker-cr-a",
			RuntimeName:    "worker-a",
			Role:           "worker",
			MatrixUserID:   "@worker-a:matrix.local",
			PersonalRoomID: "!worker-dm:matrix.local",
		}, {
			Name:           "qa",
			RuntimeName:    "qa-runtime",
			Role:           "worker",
			MatrixUserID:   "@qa-runtime:matrix.local",
			PersonalRoomID: "!qa-dm:matrix.local",
		}, {
			Name:         "human-coord",
			Role:         "coordinator",
			MatrixUserID: "@human:matrix.local",
		}},
		Spec: v1beta1.WorkerSpec{
			Model:   "qwen-plus",
			Package: "nacos://registry/ns/dev-worker?version=1.2.0",
			AgentIdentity: &v1beta1.AgentIdentitySpec{
				WorkloadIdentityName: "wi-worker-a",
			},
			CredentialBindings: []v1beta1.CredentialBinding{{
				CredentialRef: v1beta1.CredentialRef{
					TokenVaultName:               "default",
					APIKeyCredentialProviderName: "GITHUB_TOKEN",
				},
				ToolWhitelist: []string{"gh"},
			}},
			McpServers: []v1beta1.MCPServer{{
				Name:      "github",
				URL:       "https://aigw.example.com/mcp-servers/github/mcp",
				Transport: "http",
			}},
			ChannelPolicy: &v1beta1.ChannelPolicySpec{
				GroupAllowExtra: []string{"@leader:matrix.local"},
				DmDenyExtra:     []string{"@blocked:matrix.local"},
			},
			State: &state,
		},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, secret := range []string{"matrix-token", "gateway-key", "storage-secret", "Authorization"} {
		if strings.Contains(text, secret) {
			t.Fatalf("runtime.yaml leaked secret-like content %q:\n%s", secret, text)
		}
	}

	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	if got, want := fmt.Sprint(doc["kind"]), "MemberRuntimeConfig"; got != want {
		t.Fatalf("kind=%q, want %q", got, want)
	}
	member := doc["member"].(map[string]any)
	if got := fmt.Sprint(member["name"]); got != "worker-cr-a" {
		t.Fatalf("member.name=%q", got)
	}
	if got := fmt.Sprint(member["runtimeName"]); got != "worker-a" {
		t.Fatalf("member.runtimeName=%q", got)
	}
	team := doc["team"].(map[string]any)
	members := team["members"].([]any)
	if len(members) != 4 {
		t.Fatalf("team.members len=%d, want 4: %#v", len(members), members)
	}
	memberByName := map[string]map[string]any{}
	for _, item := range members {
		entry := item.(map[string]any)
		memberByName[fmt.Sprint(entry["name"])] = entry
	}
	if got := fmt.Sprint(memberByName["leader"]["runtimeName"]); got != "leader-runtime" {
		t.Fatalf("leader runtimeName=%q", got)
	}
	if got := fmt.Sprint(memberByName["worker-cr-a"]["personalRoomId"]); got != "!worker-dm:matrix.local" {
		t.Fatalf("worker personalRoomId=%q", got)
	}
	if got := fmt.Sprint(memberByName["qa"]["matrixUserId"]); got != "@qa-runtime:matrix.local" {
		t.Fatalf("qa matrixUserId=%q", got)
	}
	if _, ok := memberByName["human-coord"]["runtimeName"]; ok {
		t.Fatalf("human coordinator should not have runtimeName: %#v", memberByName["human-coord"])
	}

	desired := doc["desired"].(map[string]any)
	model := desired["model"].(map[string]any)
	if got := fmt.Sprint(model["model"]); got != "qwen-plus" {
		t.Fatalf("desired.model.model=%q", got)
	}
	if got := fmt.Sprint(model["gatewayUrl"]); got != "https://aigw.example.com" {
		t.Fatalf("desired.model.gatewayUrl=%q", got)
	}
	agentPackage := desired["agentPackage"].(map[string]any)
	if got := fmt.Sprint(agentPackage["ref"]); got != "nacos://registry/ns/dev-worker?version=1.2.0" {
		t.Fatalf("desired.agentPackage.ref=%q", got)
	}
	servers := desired["mcpServers"].([]any)
	if len(servers) != 1 || fmt.Sprint(servers[0].(map[string]any)["name"]) != "github" {
		t.Fatalf("desired.mcpServers=%#v", servers)
	}
	policy := desired["channelPolicy"].(map[string]any)
	if got := fmt.Sprint(policy["dmDenyExtra"]); !strings.Contains(got, "@blocked:matrix.local") {
		t.Fatalf("desired.channelPolicy=%#v", policy)
	}
	if _, ok := desired["outputSanitize"]; ok {
		t.Fatalf("desired.outputSanitize should not be generated: %#v", desired["outputSanitize"])
	}
	if _, ok := desired["agentIdentityData"]; ok {
		t.Fatalf("desired.agentIdentityData should not be generated: %#v", desired["agentIdentityData"])
	}
	if _, ok := doc["agentIdentity"]; ok {
		t.Fatalf("top-level agentIdentity should not be generated: %#v", doc["agentIdentity"])
	}
	if _, ok := doc["credentialBindings"]; ok {
		t.Fatalf("top-level credentialBindings should not be generated: %#v", doc["credentialBindings"])
	}
	agentIdentity := desired["agentIdentity"].(map[string]any)
	if got := fmt.Sprint(agentIdentity["workloadIdentityName"]); got != "wi-worker-a" {
		t.Fatalf("desired.agentIdentity.workloadIdentityName=%q", got)
	}
	bindings := desired["credentialBindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("desired.credentialBindings len=%d", len(bindings))
	}
	bindingRef := bindings[0].(map[string]any)["credentialRef"].(map[string]any)
	if got := fmt.Sprint(bindingRef["tokenVaultName"]); got != "default" {
		t.Fatalf("credentialRef.tokenVaultName=%q", got)
	}
	if got := fmt.Sprint(bindingRef["apiKeyCredentialProviderName"]); got != "GITHUB_TOKEN" {
		t.Fatalf("credentialRef.apiKeyCredentialProviderName=%q", got)
	}
	whitelist := bindings[0].(map[string]any)["toolWhitelist"].([]any)
	if len(whitelist) != 1 || fmt.Sprint(whitelist[0]) != "gh" {
		t.Fatalf("credentialBindings[0].toolWhitelist=%#v", whitelist)
	}

	agentIdentityData := doc["agentIdentityData"].(map[string]any)
	if got := fmt.Sprint(agentIdentityData["endpoint"]); got != "agentidentitydata.cn-beijing.aliyuncs.com" {
		t.Fatalf("agentIdentityData.endpoint=%q", got)
	}
	if _, ok := agentIdentityData["region"]; ok {
		t.Fatalf("agentIdentityData.region should not be generated: %#v", agentIdentityData["region"])
	}

	credentials := doc["credentials"].(map[string]any)
	if got := fmt.Sprint(credentials["gatewayKeyEnv"]); got != "AGENTTEAMS_WORKER_GATEWAY_KEY" {
		t.Fatalf("credentials.gatewayKeyEnv=%q", got)
	}
	if got := fmt.Sprint(credentials["serviceAccountTokenPath"]); got != "/var/run/secrets/agentteams/token" {
		t.Fatalf("credentials.serviceAccountTokenPath=%q", got)
	}
	if _, ok := credentials["agentIdentityDataSTS"]; ok {
		t.Fatalf("credentials.agentIdentityDataSTS should not be generated: %#v", credentials["agentIdentityDataSTS"])
	}
	if _, ok := credentials["purpose"]; ok {
		t.Fatalf("credentials.purpose should not be generated: %#v", credentials["purpose"])
	}
	storage := doc["storage"].(map[string]any)
	if got := fmt.Sprint(storage["memberPrefix"]); got != "agents/worker-a" {
		t.Fatalf("storage.memberPrefix=%q", got)
	}
}

func TestDeployMemberRuntimeConfigUsesRequestGatewayURL(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			AIGatewayURL: "https://global-aigw.example.com",
		},
	})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:         "worker-cr-a",
		RuntimeName:  "worker-a",
		Runtime:      "qwenpaw",
		Role:         "worker",
		Spec:         v1beta1.WorkerSpec{Model: "qwen3.7-max"},
		AIGatewayURL: "https://provider-aigw.example.com/default",
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	desired := doc["desired"].(map[string]any)
	model := desired["model"].(map[string]any)
	if got := fmt.Sprint(model["gatewayUrl"]); got != "https://provider-aigw.example.com/default" {
		t.Fatalf("desired.model.gatewayUrl=%q", got)
	}
}

func TestDeployMemberRuntimeConfigOmitsDesiredModelForNativeConfig(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "edge-worker",
		RuntimeName: "edge-worker",
		Runtime:     "claude-code",
		Spec: v1beta1.WorkerSpec{
			Model:   "native-config",
			Package: "oss://agents/edge/packages/demo.zip",
		},
		GatewayKey:   "gateway-key",
		AIGatewayURL: "https://aigw.example.com",
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/edge-worker/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	desired := doc["desired"].(map[string]any)
	if _, ok := desired["model"]; ok {
		t.Fatalf("desired.model should be omitted for native-config: %#v", desired["model"])
	}
	agentPackage := desired["agentPackage"].(map[string]any)
	if got := fmt.Sprint(agentPackage["ref"]); got != "oss://agents/edge/packages/demo.zip" {
		t.Fatalf("desired.agentPackage.ref=%q", got)
	}
}

func TestDeployMemberRuntimeConfigRejectsCredentialBindingsWithoutWorkloadIdentity(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "worker-cr-a",
		RuntimeName: "worker-a",
		Runtime:     "qwenpaw",
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-plus",
			CredentialBindings: []v1beta1.CredentialBinding{{
				CredentialRef: v1beta1.CredentialRef{
					TokenVaultName:               "default",
					APIKeyCredentialProviderName: "GITHUB_TOKEN",
				},
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "agentIdentity.workloadIdentityName is required") {
		t.Fatalf("expected missing workload identity error, got %v", err)
	}
}

func TestDeployMemberRuntimeConfigOmitsAgentIdentityDataWithoutCredentialBindings(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			AgentIdentityDataEndpoint: "agentidentitydata.cn-beijing.aliyuncs.com",
		},
	})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "worker-a",
		RuntimeName: "worker-a",
		Runtime:     "qwenpaw",
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-plus",
		},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	if _, ok := doc["agentIdentityData"]; ok {
		t.Fatalf("agentIdentityData should not be generated without credentialBindings: %#v", doc["agentIdentityData"])
	}
}

func serviceBoolPtr(v bool) *bool { return &v }

func TestDeployMemberRuntimeConfigProjectsDingTalkChannel(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "worker-cr-a",
		RuntimeName: "worker-a",
		Runtime:     "qwenpaw",
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-plus",
			Channels: &v1beta1.ChannelsSpec{
				DingTalk: &v1beta1.DingTalkChannelSpec{
					Enabled:          serviceBoolPtr(true),
					ClientID:         "demo-client-id",
					ClientSecret:     "test-client-secret",
					RobotCode:        "demo-robot-code",
					ShowThinking:     true,
					ShowToolCalls:    false,
					StreamingEnabled: true,
					MessageType:      "card",
					CardTemplateID:   "card-template-1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	desired := doc["desired"].(map[string]any)
	channels := desired["channels"].(map[string]any)
	dingtalk := channels["dingtalk"].(map[string]any)

	assertRuntimeField := func(key string, want any) {
		t.Helper()
		if got := dingtalk[key]; got != want {
			t.Fatalf("dingtalk.%s=%#v, want %#v; all=%#v", key, got, want, dingtalk)
		}
	}
	assertRuntimeField("enabled", true)
	assertRuntimeField("client_id", "demo-client-id")
	assertRuntimeField("client_secret", "test-client-secret")
	assertRuntimeField("robot_code", "demo-robot-code")
	assertRuntimeField("filter_thinking", false)
	assertRuntimeField("filter_tool_messages", true)
	assertRuntimeField("streaming_enabled", true)
	assertRuntimeField("message_type", "card")
	assertRuntimeField("card_template_id", "card-template-1")
	assertRuntimeField("card_template_key", "content")
	assertRuntimeField("card_auto_layout", false)
}

func TestDeployMemberRuntimeConfigProjectsDingTalkDisabled(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "worker-cr-a",
		RuntimeName: "worker-a",
		Runtime:     "qwenpaw",
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-plus",
			Channels: &v1beta1.ChannelsSpec{
				DingTalk: &v1beta1.DingTalkChannelSpec{Enabled: serviceBoolPtr(false)},
			},
		},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	desired := doc["desired"].(map[string]any)
	channels := desired["channels"].(map[string]any)
	dingtalk := channels["dingtalk"].(map[string]any)
	if got := dingtalk["enabled"]; got != false {
		t.Fatalf("dingtalk.enabled=%#v, want false", got)
	}
	if _, ok := dingtalk["client_secret"]; ok {
		t.Fatalf("disabled DingTalk must not include client_secret: %#v", dingtalk)
	}
}

func TestDeployMemberRuntimeConfigRejectsInvalidEnabledDingTalkChannel(t *testing.T) {
	cases := []struct {
		name     string
		spec     v1beta1.DingTalkChannelSpec
		wantText string
	}{
		{
			name:     "missing client id",
			spec:     v1beta1.DingTalkChannelSpec{Enabled: serviceBoolPtr(true), ClientSecret: "test-client-secret"},
			wantText: "clientId",
		},
		{
			name:     "missing client secret",
			spec:     v1beta1.DingTalkChannelSpec{Enabled: serviceBoolPtr(true), ClientID: "demo-client-id"},
			wantText: "clientSecret",
		},
		{
			name:     "invalid message type",
			spec:     v1beta1.DingTalkChannelSpec{Enabled: serviceBoolPtr(true), ClientID: "demo-client-id", ClientSecret: "test-client-secret", MessageType: "plain"},
			wantText: "messageType",
		},
		{
			name:     "card missing template id",
			spec:     v1beta1.DingTalkChannelSpec{Enabled: serviceBoolPtr(true), ClientID: "demo-client-id", ClientSecret: "test-client-secret", MessageType: "card"},
			wantText: "cardTemplateId",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := ossfake.NewMemory()
			deployer := NewDeployer(DeployerConfig{OSS: store})

			err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
				Name:        "worker-cr-a",
				RuntimeName: "worker-a",
				Runtime:     "qwenpaw",
				Spec: v1beta1.WorkerSpec{
					Model: "qwen-plus",
					Channels: &v1beta1.ChannelsSpec{
						DingTalk: &tc.spec,
					},
				},
			})
			if err == nil {
				t.Fatal("DeployMemberRuntimeConfig succeeded with invalid enabled DingTalk config")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error=%q, want %s validation", err, tc.wantText)
			}
			if strings.Contains(err.Error(), "test-client-secret") {
				t.Fatalf("error leaked clientSecret: %q", err)
			}
			if _, getErr := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml"); !os.IsNotExist(getErr) {
				t.Fatalf("invalid config wrote runtime.yaml, get err=%v", getErr)
			}
		})
	}
}

func TestDeployMemberRuntimeConfigProjectsRemoteManagedLocalFields(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			StorageProvider: "oss",
			StorageBucket:   "agentteams-storage",
			StorageEndpoint: "https://oss.example.com",
			AIGatewayURL:    "https://default-gateway.example.com/default",
		},
	})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:                  "edge-worker-cr",
		RuntimeName:           "claude-local",
		Runtime:               "remote-managed-local",
		Role:                  "standalone",
		Generation:            3,
		MatrixUserID:          "@claude-local:matrix.local",
		PersonalRoomID:        "!worker-dm:matrix.local",
		MatrixAccessToken:     "matrix-access-token",
		GatewayKey:            "gateway-key",
		AIGatewayURL:          "http://aigw.internal/v1/claude",
		SkillRegistryURL:      "nacos://market.agentteams.io:80/public",
		SkillRegistryAuthType: "sts-hiclaw",
		Spec: v1beta1.WorkerSpec{
			Model:   "claude-sonnet-4",
			Package: "oss://agents/claude-local/packages/demo.zip",
		},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/claude-local/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	member := doc["member"].(map[string]any)
	if got := fmt.Sprint(member["runtime"]); got != "remote-managed-local" {
		t.Fatalf("member.runtime=%q", got)
	}
	matrix := doc["matrix"].(map[string]any)
	if got := fmt.Sprint(matrix["accessToken"]); got != "matrix-access-token" {
		t.Fatalf("matrix.accessToken=%q", got)
	}
	desired := doc["desired"].(map[string]any)
	model := desired["model"].(map[string]any)
	if got := fmt.Sprint(model["gatewayUrl"]); got != "http://aigw.internal/v1/claude" {
		t.Fatalf("desired.model.gatewayUrl=%q", got)
	}
	if got := fmt.Sprint(model["gatewayKey"]); got != "gateway-key" {
		t.Fatalf("desired.model.gatewayKey=%q", got)
	}
	skillRegistry := desired["skillRegistry"].(map[string]any)
	if got := fmt.Sprint(skillRegistry["provider"]); got != "nacos" {
		t.Fatalf("desired.skillRegistry.provider=%q", got)
	}
	if got := fmt.Sprint(skillRegistry["url"]); got != "nacos://market.agentteams.io:80/public" {
		t.Fatalf("desired.skillRegistry.url=%q", got)
	}
	if got := fmt.Sprint(skillRegistry["authType"]); got != "sts-hiclaw" {
		t.Fatalf("desired.skillRegistry.authType=%q", got)
	}
}

func TestMergeMemberRuntimeTeamContextPreservesRemoteManagedSecrets(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})
	key := "agents/claude-local/runtime/runtime.yaml"
	if err := store.PutObject(ctx, key, []byte(`
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
metadata:
  generation: 3
  updatedAt: "2026-06-10T12:00:00Z"
member:
  name: edge-worker-cr
  runtimeName: claude-local
  role: standalone
  runtime: remote-managed-local
  matrixUserId: "@claude-local:matrix.local"
  personalRoomId: "!worker-dm:matrix.local"
matrix:
  accessToken: matrix-access-token
desired:
  model:
    providerId: agentteams-gateway
    model: claude-sonnet-4
    gatewayUrl: http://aigw.internal/v1/claude
    gatewayKey: gateway-key
  agentPackage:
    ref: oss://agents/claude-local/packages/demo.zip
  state: Running
storage:
  provider: oss
  bucket: agentteams-storage
  endpoint: https://oss.example.com
  sharedPrefix: shared
  globalSharedPrefix: shared
  memberPrefix: agents/claude-local
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
  storageAccessKeyEnv: AGENTTEAMS_FS_ACCESS_KEY
  storageSecretKeyEnv: AGENTTEAMS_FS_SECRET_KEY
  serviceAccountTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
`)); err != nil {
		t.Fatal(err)
	}

	err := deployer.MergeMemberRuntimeTeamContext(ctx, MemberRuntimeConfigDeployRequest{
		Name:              "edge-worker-cr",
		RuntimeName:       "claude-local",
		Role:              "worker",
		Generation:        4,
		MatrixUserID:      "@claude-local:matrix.local",
		PersonalRoomID:    "!worker-dm:matrix.local",
		TeamName:          "demo-team",
		TeamRoomID:        "!team:matrix.local",
		LeaderName:        "leader-cr",
		LeaderRuntimeName: "leader-runtime",
		LeaderDMRoomID:    "!leader-dm:matrix.local",
		TeamAdminName:     "admin",
		TeamAdminMatrixID: "@admin:matrix.local",
		TeamMembers: []RuntimeConfigTeamMember{{
			Name:           "leader-cr",
			RuntimeName:    "leader-runtime",
			Role:           "team_leader",
			MatrixUserID:   "@leader-runtime:matrix.local",
			PersonalRoomID: "!leader-dm:matrix.local",
		}, {
			Name:           "edge-worker-cr",
			RuntimeName:    "claude-local",
			Role:           "worker",
			MatrixUserID:   "@claude-local:matrix.local",
			PersonalRoomID: "!worker-dm:matrix.local",
		}},
	})
	if err != nil {
		t.Fatalf("MergeMemberRuntimeTeamContext failed: %v", err)
	}

	got, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	member := doc["member"].(map[string]any)
	if got := fmt.Sprint(member["runtime"]); got != "remote-managed-local" {
		t.Fatalf("member.runtime=%q, want remote-managed-local", got)
	}
	if got := fmt.Sprint(member["role"]); got != "worker" {
		t.Fatalf("member.role=%q, want worker", got)
	}
	matrix := doc["matrix"].(map[string]any)
	if got := fmt.Sprint(matrix["accessToken"]); got != "matrix-access-token" {
		t.Fatalf("matrix.accessToken=%q", got)
	}
	desired := doc["desired"].(map[string]any)
	model := desired["model"].(map[string]any)
	if got := fmt.Sprint(model["gatewayUrl"]); got != "http://aigw.internal/v1/claude" {
		t.Fatalf("desired.model.gatewayUrl=%q", got)
	}
	if got := fmt.Sprint(model["gatewayKey"]); got != "gateway-key" {
		t.Fatalf("desired.model.gatewayKey=%q", got)
	}
	team := doc["team"].(map[string]any)
	if got := fmt.Sprint(team["teamRoomId"]); got != "!team:matrix.local" {
		t.Fatalf("team.teamRoomId=%q", got)
	}
	members := team["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("team.members len=%d, want 2: %#v", len(members), members)
	}
	storage := doc["storage"].(map[string]any)
	if got := fmt.Sprint(storage["sharedPrefix"]); got != "teams/demo-team/shared" {
		t.Fatalf("storage.sharedPrefix=%q", got)
	}
	if got := fmt.Sprint(storage["memberPrefix"]); got != "agents/claude-local" {
		t.Fatalf("storage.memberPrefix=%q", got)
	}
}

func TestDeployMemberRuntimeConfigPreservesExistingTeamContextForStandaloneUpdate(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			StorageProvider: "oss",
			StorageBucket:   "agentteams-storage",
			StorageEndpoint: "https://oss.example.com",
			AIGatewayURL:    "https://aigw.example.com",
		},
	})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:              "leader-cr",
		RuntimeName:       "leader-runtime",
		Runtime:           "qwenpaw",
		Role:              "team_leader",
		Generation:        1,
		MatrixUserID:      "@leader-runtime:matrix.local",
		PersonalRoomID:    "!leader-dm:matrix.local",
		TeamName:          "demo-team",
		TeamRoomID:        "!team:matrix.local",
		LeaderName:        "leader-cr",
		LeaderRuntimeName: "leader-runtime",
		LeaderDMRoomID:    "!leader-dm:matrix.local",
		TeamAdminName:     "admin",
		TeamAdminMatrixID: "@admin:matrix.local",
		TeamMembers: []RuntimeConfigTeamMember{{
			Name:           "leader-cr",
			RuntimeName:    "leader-runtime",
			Role:           "team_leader",
			MatrixUserID:   "@leader-runtime:matrix.local",
			PersonalRoomID: "!leader-dm:matrix.local",
		}, {
			Name:           "worker-cr",
			RuntimeName:    "worker-runtime",
			Role:           "worker",
			MatrixUserID:   "@worker-runtime:matrix.local",
			PersonalRoomID: "!worker-dm:matrix.local",
		}},
		Spec: v1beta1.WorkerSpec{
			Model:   "qwen-plus",
			Package: "oss://packages/leader-v1.tar.gz",
		},
	})
	if err != nil {
		t.Fatalf("initial DeployMemberRuntimeConfig failed: %v", err)
	}

	err = deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:           "leader-cr",
		RuntimeName:    "leader-runtime",
		Runtime:        "qwenpaw",
		Role:           "standalone",
		Generation:     2,
		MatrixUserID:   "@leader-runtime:matrix.local",
		PersonalRoomID: "!leader-dm:matrix.local",
		Spec: v1beta1.WorkerSpec{
			Model:   "qwen-plus",
			Package: "oss://packages/leader-v2.tar.gz",
		},
	})
	if err != nil {
		t.Fatalf("standalone update DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/leader-runtime/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	member := doc["member"].(map[string]any)
	if got := fmt.Sprint(member["role"]); got != "team_leader" {
		t.Fatalf("member.role=%q, want team_leader\n%s", got, text)
	}
	desired := doc["desired"].(map[string]any)
	agentPackage := desired["agentPackage"].(map[string]any)
	if got := fmt.Sprint(agentPackage["ref"]); got != "oss://packages/leader-v2.tar.gz" {
		t.Fatalf("desired.agentPackage.ref=%q, want v2", got)
	}
	team := doc["team"].(map[string]any)
	members := team["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("team.members len=%d, want 2: %#v", len(members), members)
	}
	storage := doc["storage"].(map[string]any)
	if got := fmt.Sprint(storage["sharedPrefix"]); got != "teams/demo-team/shared" {
		t.Fatalf("storage.sharedPrefix=%q, want teams/demo-team/shared", got)
	}
}

func TestDeployMemberRuntimeConfigDropsExistingTeamContextWhenRequested(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{
		OSS: store,
		RuntimeProjection: RuntimeProjectionConfig{
			StorageProvider: "oss",
			StorageBucket:   "agentteams-storage",
			StorageEndpoint: "https://oss.example.com",
		},
	})

	if err := store.PutObject(ctx, "agents/worker-a/runtime/runtime.yaml", []byte(`
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
team:
  name: old-team
  teamRoomId: "!old-team:matrix.local"
member:
  runtimeName: worker-a
  role: worker
desired:
  state: Running
storage:
  sharedPrefix: teams/old-team/shared
  globalSharedPrefix: shared
  memberPrefix: agents/worker-a
credentials: {}
`)); err != nil {
		t.Fatal(err)
	}

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:            "worker-a",
		RuntimeName:     "worker-a",
		Runtime:         "qwenpaw",
		Role:            "standalone",
		DropTeamContext: true,
		Spec:            v1beta1.WorkerSpec{Model: "qwen-plus"},
	})
	if err != nil {
		t.Fatalf("DeployMemberRuntimeConfig failed: %v", err)
	}

	got, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("runtime.yaml is invalid YAML: %v\n%s", err, got)
	}
	if _, ok := doc["team"]; ok {
		t.Fatalf("runtime.yaml still has team context:\n%s", got)
	}
	member := doc["member"].(map[string]any)
	if got := fmt.Sprint(member["role"]); got != "standalone" {
		t.Fatalf("member.role=%q, want standalone", got)
	}
	storage := doc["storage"].(map[string]any)
	if got := fmt.Sprint(storage["sharedPrefix"]); got != "shared" {
		t.Fatalf("storage.sharedPrefix=%q, want shared", got)
	}
}

func TestCleanupOSSDataDeletesAgentAndRuntimeConfig(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	if err := store.PutObject(ctx, "agents/worker-a/AGENTS.md", []byte("agent")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, "agents/worker-a/runtime/runtime.yaml", []byte("runtime")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(ctx, "agents/other/runtime/runtime.yaml", []byte("other")); err != nil {
		t.Fatal(err)
	}

	if err := deployer.CleanupOSSData(ctx, "worker-a"); err != nil {
		t.Fatalf("CleanupOSSData: %v", err)
	}
	if _, err := store.GetObject(ctx, "agents/worker-a/AGENTS.md"); !os.IsNotExist(err) {
		t.Fatalf("agents/worker-a/AGENTS.md err=%v, want not exist", err)
	}
	if _, err := store.GetObject(ctx, "agents/worker-a/runtime/runtime.yaml"); !os.IsNotExist(err) {
		t.Fatalf("worker runtime.yaml err=%v, want not exist", err)
	}
	if _, err := store.GetObject(ctx, "agents/other/runtime/runtime.yaml"); err != nil {
		t.Fatalf("other runtime.yaml should remain: %v", err)
	}
}

func TestDeployMemberRuntimeConfigRequiresOSSClient(t *testing.T) {
	ctx := context.Background()
	deployer := NewDeployer(DeployerConfig{})

	err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
		Name:        "worker-a",
		RuntimeName: "worker-a",
		Runtime:     "qwenpaw",
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-plus",
		},
	})
	if err == nil {
		t.Fatal("DeployMemberRuntimeConfig succeeded without OSS client")
	}
	if !strings.Contains(err.Error(), "OSS") {
		t.Fatalf("error %q does not mention OSS", err)
	}
}

func TestPrepareWorkerDepsWritesObjectStorageLayout(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	deployer := NewDeployer(DeployerConfig{OSS: store})

	err := deployer.PrepareWorkerDeps(ctx, WorkerDepsPrepareRequest{
		WorkerName:   "alice",
		TokenSubPath: "instances/alice/token",
		EnvSubPath:   "instances/alice/env",
		DataSubPath:  "instances/alice/data",
		Token:        "sa-token",
		UseToken:     true,
		UseEnv:       true,
		Env: map[string]string{
			"AGENTTEAMS_AUTH_TOKEN_FILE": "/var/run/secrets/agentteams/token",
			"HOME":                       "/workspace",
			"INVALID-KEY":                "ignored",
			"QUOTE":                      "it's ok",
		},
	})
	if err != nil {
		t.Fatalf("PrepareWorkerDeps: %v", err)
	}

	wantKeys := []string{
		"instances/alice/data/.agentteams-keep",
		"instances/alice/env/env",
		"instances/alice/token/token",
	}
	gotKeys, err := store.ListObjects(ctx, "instances/alice/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotKeys, "\n") != strings.Join(wantKeys, "\n") {
		t.Fatalf("worker deps keys=%v, want %v", gotKeys, wantKeys)
	}
	for _, key := range wantKeys {
		if err := store.Stat(ctx, key); err != nil {
			t.Fatalf("missing %s: %v", key, err)
		}
	}
	token, err := store.GetObject(ctx, "instances/alice/token/token")
	if err != nil {
		t.Fatal(err)
	}
	if string(token) != "sa-token" {
		t.Fatalf("token=%q", token)
	}
	env, err := store.GetObject(ctx, "instances/alice/env/env")
	if err != nil {
		t.Fatal(err)
	}
	text := string(env)
	for _, want := range []string{
		"export AGENTTEAMS_AUTH_TOKEN_FILE='/var/run/secrets/agentteams/token'\n",
		"export HOME='/workspace'\n",
		"export QUOTE='it'\\''s ok'\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("env file missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "INVALID-KEY") {
		t.Fatalf("env file should ignore invalid env keys:\n%s", text)
	}
}
