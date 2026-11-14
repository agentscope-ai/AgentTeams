package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

func TestQwenPawManagerUsesMemberRuntimeContract(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	err := d.DeployManagerConfig(ctx, ManagerDeployRequest{
		Name: "default",
		Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Model: "qwen-test", Agents: "You manage teams."},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc memberRuntimeConfigDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Member.Name != "default" || doc.Member.RuntimeName != "manager" || doc.Member.Role != "manager" {
		t.Fatalf("manager identity = %+v", doc.Member)
	}
	if doc.Desired.Model.Model != "qwen-test" || doc.Desired.InlineConfig.Agents != "You manage teams." {
		t.Fatalf("manager desired state = %+v", doc.Desired)
	}
	if doc.Credentials.MatrixTokenEnv != "AGENTTEAMS_MANAGER_MATRIX_TOKEN" {
		t.Fatalf("manager credential contract = %+v", doc.Credentials)
	}
}

func TestQwenPawManagerEnvUsesSharedHarness(t *testing.T) {
	b := NewWorkerEnvBuilder(config.WorkerEnvDefaults{})
	for _, runtime := range []string{"openclaw", "qwenpaw", "hermes", "deepseek-harness", "openhuman", "codex"} {
		env := b.BuildManager("default", &ManagerProvisionResult{MatrixToken: "manager-token", GatewayKey: "manager-key"}, v1beta1.ManagerSpec{Runtime: runtime})
		for key, want := range map[string]string{
			"AGENTTEAMS_AGENT_ROLE":          "manager",
			"AGENTTEAMS_WORKER_ROLE":         "manager",
			"AGENTTEAMS_WORKER_NAME":         "manager",
			"AGENTTEAMS_WORKER_CR_NAME":      "default",
			"AGENTTEAMS_WORKER_HOME":         "/root/manager-workspace",
			"AGENTTEAMS_WORKER_MATRIX_TOKEN": "manager-token",
			"AGENTTEAMS_WORKER_GATEWAY_KEY":  "manager-key",
		} {
			if env[key] != want {
				t.Errorf("%s = %q, want %q", key, env[key], want)
			}
		}
	}
}

func TestLegacyManagerSelectorUsesQwenPawContract(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	if err := d.DeployManagerConfig(ctx, ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: "copaw", Model: "test"}}); err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc memberRuntimeConfigDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Member.Runtime != "qwenpaw" || doc.Member.Role != "manager" {
		t.Fatalf("legacy selector = %+v", doc.Member)
	}
}

func TestManagerRuntimeChannelPolicySurvivesReconcile(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	req := ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Model: "qwen-test"}}
	if err := d.DeployManagerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	s := NewManagerConfigStore(ManagerConfigStoreConfig{OSS: store})
	if err := s.UpdateManagerGroupAllowFrom("@new-leader:matrix.local", true); err != nil {
		t.Fatal(err)
	}
	if err := d.DeployManagerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	assertPolicy := func(want int) {
		t.Helper()
		data, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var doc memberRuntimeConfigDocument
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Desired.ChannelPolicy == nil || len(doc.Desired.ChannelPolicy.GroupAllowExtra) != want {
			t.Fatalf("policy = %+v, want %d members", doc.Desired.ChannelPolicy, want)
		}
	}
	assertPolicy(1)
	if err := s.UpdateManagerGroupAllowFrom("@new-leader:matrix.local", false); err != nil {
		t.Fatal(err)
	}
	assertPolicy(0)
}

func TestManagerHarnessSwitchPreservesActiveCommunicationPolicy(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	s := NewManagerConfigStore(ManagerConfigStoreConfig{OSS: store})
	legacy := []byte(`{"channels":{"matrix":{"groupAllowFrom":["@leader:matrix.local"]}}}`)
	if err := s.PutManagerConfig(legacy); err != nil {
		t.Fatal(err)
	}
	d := NewDeployer(DeployerConfig{OSS: store, ManagerConfig: s, AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "matrix.local"})})
	if err := d.DeployManagerConfig(ctx, ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Model: "test"}}); err != nil {
		t.Fatal(err)
	}
	data, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc memberRuntimeConfigDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Desired.ChannelPolicy.GroupAllowExtra) != 1 || doc.Desired.ChannelPolicy.GroupAllowExtra[0] != "@leader:matrix.local" {
		t.Fatalf("lost legacy policy: %+v", doc.Desired.ChannelPolicy)
	}
	if err := s.UpdateManagerGroupAllowFrom("@new-leader:matrix.local", true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateManagerGroupAllowFrom("@leader:matrix.local", false); err != nil {
		t.Fatal(err)
	}
	if err := s.PutManagerConfig([]byte(`{"channels":{"matrix":{"groupAllowFrom":["@admin:matrix.local"]}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml"); err != nil {
		t.Fatal("Manager lost the shared role contract", err)
	}
	data, err = store.GetObject(ctx, "agents/manager/openclaw.json")
	if err != nil {
		t.Fatal(err)
	}
	var active map[string]interface{}
	if err := json.Unmarshal(data, &active); err != nil {
		t.Fatal(err)
	}
	ids := extractStringSlice(active["channels"].(map[string]interface{})["matrix"].(map[string]interface{})["groupAllowFrom"])
	if len(ids) != 2 || ids[0] != "@admin:matrix.local" || ids[1] != "@new-leader:matrix.local" {
		t.Fatalf("switch restored stale membership: %v", ids)
	}
	doc.Member.Runtime = "openclaw"
	if err := s.PutManagerRuntimeConfig(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateManagerGroupAllowFrom("@leader:matrix.local", false); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRoleProjectionAcrossHarnesses(t *testing.T) {
	for _, runtime := range []string{"openclaw", "hermes", "openhuman", "deepseek-harness", "qwenpaw", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			ctx := context.Background()
			store := ossfake.NewMemory()
			if err := store.PutObject(ctx, "agents/manager/AGENTS.md", []byte("LEGACY_MANAGER_RUNTIME_CLI")); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "manager-agent"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "manager-agent/AGENTS.md"), []byte("You are the TeamHarness Manager."), 0644); err != nil {
				t.Fatal(err)
			}
			d := NewDeployer(DeployerConfig{OSS: store, WorkerAgentDir: filepath.Join(root, "worker-agent"), AgentConfig: agentconfig.NewGenerator(agentconfig.Config{MatrixDomain: "matrix.local"})})
			if err := d.DeployManagerConfig(ctx, ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: runtime, Model: "test"}, RoomID: "!admin:matrix.local", MatrixUserID: "@manager:matrix.local"}); err != nil {
				t.Fatal(err)
			}
			data, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var doc memberRuntimeConfigDocument
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Member.Role != "manager" || doc.Member.Runtime != runtime || doc.Member.Name != "default" || doc.Member.PersonalRoomID != "!admin:matrix.local" {
				t.Fatalf("wrong role projection %+v", doc.Member)
			}
			if runtime == "openclaw" || runtime == "hermes" || runtime == "openhuman" {
				prompt, err := store.GetObject(ctx, "agents/manager/AGENTS.md")
				if err != nil || !strings.Contains(string(prompt), "TeamHarness Manager") || strings.Contains(string(prompt), "Standalone Worker") || strings.Contains(string(prompt), "LEGACY_MANAGER_RUNTIME_CLI") {
					t.Fatalf("wrong native role prompt: %s %v", prompt, err)
				}
				if err := store.PutObject(ctx, "agents/manager/AGENTS.md", append(prompt, []byte("\nUSER_CUSTOM_PROMPT")...)); err != nil {
					t.Fatal(err)
				}
				if err := d.DeployManagerConfig(ctx, ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: runtime, Model: "test"}}); err != nil {
					t.Fatal(err)
				}
				prompt, _ = store.GetObject(ctx, "agents/manager/AGENTS.md")
				if !strings.Contains(string(prompt), "USER_CUSTOM_PROMPT") {
					t.Fatal("Manager lost custom prompt after replacing builtins")
				}
			}
		})
	}
}
