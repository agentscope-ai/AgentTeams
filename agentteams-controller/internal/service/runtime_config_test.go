package service

import (
	"context"
	"fmt"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/observability"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

func TestMCPApprovalPolicyReachesRuntimeOnUpdate(t *testing.T) {
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	req := MemberRuntimeConfigDeployRequest{Name: "worker", Runtime: "codex", Spec: v1beta1.WorkerSpec{
		McpServers: []v1beta1.MCPServer{{Name: "docs", URL: "https://gateway/docs"}},
	}}
	for _, mode := range []string{"ask", "allow", "deny"} {
		req.Spec.McpServers[0].Approvals = &v1beta1.MCPApprovals{Tools: "*", Mode: mode}
		if err := d.DeployMemberRuntimeConfig(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		raw, err := store.GetObject(t.Context(), "agents/worker/runtime/runtime.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var doc memberRuntimeConfigDocument
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		got := doc.Desired.MCPServers[0].Approvals
		if got == nil || *got != *req.Spec.McpServers[0].Approvals {
			t.Fatalf("wanted %s for all tools, got %+v", mode, got)
		}
	}
}

func TestWorkerRuntimeConfigVersionTracksConfigurationInsteadOfRestartGeneration(t *testing.T) {
	for _, harness := range []string{"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"} {
		t.Run(harness, func(t *testing.T) {
			store := ossfake.NewMemory()
			d := NewDeployer(DeployerConfig{OSS: store})
			req := MemberRuntimeConfigDeployRequest{Name: "worker", Runtime: harness, Generation: 5, Spec: v1beta1.WorkerSpec{Model: "model-a"}}
			publish := func(want int64) {
				t.Helper()
				if err := d.DeployMemberRuntimeConfig(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				got, err := WorkerRuntimeConfigGeneration(t.Context(), store, "worker", harness)
				if err != nil || got != want {
					t.Fatalf("configuration generation = %d, want %d: %v", got, want, err)
				}
			}
			publish(5)
			req.Generation = 6 // Restart changes the Worker CR, not its projected settings.
			running := "Running"
			req.Spec.State = &running // The lifecycle API materializes the existing default.
			publish(5)
			req.Spec.Model = "model-b"
			publish(6)
			req.Generation = 7
			publish(6)
			req.Generation, req.Spec.Model = 2, "model-c" // A different owner's counter cannot regress it.
			publish(7)
		})
	}
}

func TestRuntimeTeamMergeVersionsOnlyChangedConfiguration(t *testing.T) {
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	req := MemberRuntimeConfigDeployRequest{Name: "worker", Runtime: "hermes", Role: "worker", Generation: 5}
	if err := d.DeployMemberRuntimeConfig(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.Generation = 6
	for _, want := range []int64{5, 6, 6} {
		if want == 6 {
			req.TeamName = "team-a"
		}
		if err := d.MergeMemberRuntimeTeamContext(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		got, err := WorkerRuntimeConfigGeneration(t.Context(), store, "worker", "hermes")
		if err != nil || got != want {
			t.Fatalf("team configuration generation %d, want %d: %v", got, want, err)
		}
	}
}

func TestManagerRuntimeVersionTracksPolicyInsteadOfCRGeneration(t *testing.T) {
	store := ossfake.NewMemory()
	l := NewManagerConfigStore(ManagerConfigStoreConfig{OSS: store, ManagerName: "manager", MatrixDomain: "test"})
	doc := memberRuntimeConfigDocument{APIVersion: "agentteams.io/v1beta1", Kind: memberRuntimeConfigKind,
		Metadata: memberRuntimeConfigMetadata{Generation: 5}, Member: memberRuntimeConfigMember{RuntimeName: "manager", Runtime: "hermes"},
		Storage: memberRuntimeConfigStorage{MemberPrefix: "agents/manager"}}
	if err := l.PutManagerRuntimeConfig(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	doc.Metadata.Generation = 6
	if err := l.PutManagerRuntimeConfig(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	got, _ := WorkerRuntimeConfigGeneration(t.Context(), store, "manager", "hermes")
	if got != 5 {
		t.Fatalf("unchanged Manager config changed version: %d", got)
	}
	if err := l.UpdateManagerGroupAllowFrom("@leader:test", true); err != nil {
		t.Fatal(err)
	}
	got, _ = WorkerRuntimeConfigGeneration(t.Context(), store, "manager", "hermes")
	if got != 6 {
		t.Fatalf("changed communication policy did not change version: %d", got)
	}
}

func TestMemberRuntimeConfigPublishesCommonObservabilityForEveryHarness(t *testing.T) {
	for _, harness := range []string{"codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"} {
		for _, enabled := range []bool{true, false} {
			store := ossfake.NewMemory()
			d := NewDeployer(DeployerConfig{OSS: store})
			settings := observability.RuntimeSettings(map[string]any{"revision": "r1", "enabled": enabled, "captureContent": false, "collectorEndpoint": "http://collector:4318"})
			if err := d.DeployMemberRuntimeConfig(t.Context(), MemberRuntimeConfigDeployRequest{Name: "worker", Runtime: harness, Observability: settings}); err != nil {
				t.Fatal(err)
			}
			data, err := store.GetObject(t.Context(), "agents/worker/runtime/runtime.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var doc memberRuntimeConfigDocument
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if got := doc.Desired.Observability; got == nil || *got != *settings {
				t.Fatalf("%s: common record lost tracing settings", harness)
			}
			if doc.Credentials.TraceHeadersEnv != "OTEL_EXPORTER_OTLP_HEADERS" {
				t.Fatal("missing standard ingestion credential binding")
			}
		}
	}
}

// TestMemberRuntimeConfigProjectsProvider pins the runtime invariant:
// spec.modelProvider is the gateway provider name and must not become the
// runtime providerId. Both custom and empty gateway providers project the
// constant agentteams-gateway runtime provider.
func TestMemberRuntimeConfigProjectsProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		want     string
	}{
		{name: "custom gateway provider", provider: "custom-gw", want: "agentteams-gateway"},
		{name: "gateway provider colliding with qwenpaw builtin", provider: "deepseek", want: "agentteams-gateway"},
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
