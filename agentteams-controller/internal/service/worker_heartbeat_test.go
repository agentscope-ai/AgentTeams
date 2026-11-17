package service

import (
	"context"
	"encoding/json"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

func TestWorkerHeartbeatSurvivesConfigRegeneration(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{AgentConfig: agentconfig.NewGenerator(agentconfig.Config{}), OSS: store, AgentFSDir: t.TempDir()})
	for _, enabled := range []bool{false, true, false, false} {
		for i := 0; i < 2; i++ {
			err := d.DeployWorkerConfig(ctx, WorkerDeployRequest{Name: "alice", Spec: v1beta1.WorkerSpec{Runtime: "openclaw", LlmHeartbeatEnabled: &enabled}, IsUpdate: i > 0})
			if err != nil {
				t.Fatal(err)
			}
			data, err := store.GetObject(ctx, "agents/alice/openclaw.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			hb := cfg["agents"].(map[string]any)["defaults"].(map[string]any)["heartbeat"].(map[string]any)
			want := "0m"
			if enabled {
				want = "30m"
			}
			if hb["every"] != want {
				t.Fatalf("regenerated heartbeat=%v want %s", hb, want)
			}
		}
	}
}

func TestStandardWorkerHeartbeatPreservesOverrideAndInheritedCadence(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name      string
		override  *bool
		inherited *agentconfig.HeartbeatConfig
		want      string
	}{
		{name: "ordinary", want: "0m"},
		{name: "enabled", override: &enabled, want: "30m"},
		{name: "leader", inherited: &agentconfig.HeartbeatConfig{Enabled: true, Every: "7m"}, want: "7m"},
		{name: "disabled leader", override: &disabled, inherited: &agentconfig.HeartbeatConfig{Enabled: true, Every: "7m"}, want: "0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			d := NewDeployer(DeployerConfig{OSS: store})
			if err := d.DeployMemberRuntimeConfig(context.Background(), MemberRuntimeConfigDeployRequest{
				Name: "worker", Runtime: "openclaw", Spec: v1beta1.WorkerSpec{LlmHeartbeatEnabled: tc.override}, Heartbeat: tc.inherited,
			}); err != nil {
				t.Fatal(err)
			}
			body, err := store.GetObject(context.Background(), "agents/worker/runtime/runtime.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := yaml.Unmarshal(body, &record); err != nil {
				t.Fatal(err)
			}
			heartbeat, ok := record["desired"].(map[string]any)["heartbeat"].(map[string]any)
			if !ok || heartbeat["every"] != tc.want || heartbeat["enabled"] != (tc.want != "0m") {
				t.Fatalf("standard heartbeat=%v, want every=%s", heartbeat, tc.want)
			}
		})
	}
}
