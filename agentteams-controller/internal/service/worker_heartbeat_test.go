package service

import (
	"context"
	"encoding/json"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
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
