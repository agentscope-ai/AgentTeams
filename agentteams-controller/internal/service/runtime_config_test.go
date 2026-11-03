package service

import (
	"context"
	"fmt"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

// TestMemberRuntimeConfigProjectsProvider pins the provider projection in
// the runtime.yaml document: spec.modelProvider names the gateway provider
// for both the primary and the subagent model; an empty value keeps the
// built-in agentteams-gateway provider.
func TestMemberRuntimeConfigProjectsProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		want     string
	}{
		{name: "custom provider", provider: "custom-gw", want: "custom-gw"},
		{name: "empty falls back to built-in", provider: "", want: "agentteams-gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := ossfake.NewMemory()
			deployer := NewDeployer(DeployerConfig{OSS: store})

			err := deployer.DeployMemberRuntimeConfig(ctx, MemberRuntimeConfigDeployRequest{
				Name:          "worker-cr-a",
				RuntimeName:   "worker-a",
				Runtime:       "qwenpaw",
				Role:          "worker",
				Spec:          v1beta1.WorkerSpec{Model: "qwen3.6-plus", ModelProvider: tc.provider},
				SubagentModel: "qwen3.6-flash",
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
			if got := fmt.Sprint(model["providerId"]); got != tc.want {
				t.Fatalf("desired.model.providerId=%q, want %q", got, tc.want)
			}
			sub := desired["subagentModel"].(map[string]any)
			if got := fmt.Sprint(sub["providerId"]); got != tc.want {
				t.Fatalf("desired.subagentModel.providerId=%q, want %q", got, tc.want)
			}
		})
	}
}
