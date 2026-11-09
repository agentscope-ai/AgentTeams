package service

import (
	"context"
	"encoding/json"
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
	env := b.BuildManager("default", &ManagerProvisionResult{}, v1beta1.ManagerSpec{Runtime: "qwenpaw"})
	for key, want := range map[string]string{
		"AGENTTEAMS_AGENT_ROLE":     "manager",
		"AGENTTEAMS_WORKER_ROLE":    "manager",
		"AGENTTEAMS_WORKER_NAME":    "manager",
		"AGENTTEAMS_WORKER_CR_NAME": "default",
		"AGENTTEAMS_WORKER_HOME":    "/root/manager-workspace",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
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
	if _, err := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml"); err == nil {
		t.Fatal("legacy runtime kept stale Harness projection")
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
	if err := s.UpdateManagerGroupAllowFrom("@leader:matrix.local", false); err != nil {
		t.Fatal(err)
	}
}
