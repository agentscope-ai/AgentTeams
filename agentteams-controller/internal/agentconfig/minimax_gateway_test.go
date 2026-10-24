package agentconfig

import (
	"encoding/json"
	"testing"
)

func TestMiniMaxGatewayOverridePreservesDefaultProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, configured, requested, gateway, want string
		automatic                                  bool
	}{
		{name: "normal default", want: MiniMaxFlash},
		{name: "same gateway slash", gateway: "http://official-gateway/", want: MiniMaxFlash},
		{name: "custom gateway default", gateway: "https://custom.example", want: "qwen3.6-plus"},
		{name: "loaded automatic default", configured: MiniMaxFlash, automatic: true, gateway: "https://custom.example", want: "qwen3.6-plus"},
		{name: "explicit configured Flash", configured: MiniMaxFlash, gateway: "https://custom.example", want: MiniMaxFlash},
		{name: "explicit configured legacy", configured: "MiniMax-M2.7-highspeed", gateway: "https://custom.example", want: "MiniMax-M2.7-highspeed"},
		{name: "explicit worker Flash", requested: MiniMaxFlash, gateway: "https://custom.example", want: MiniMaxFlash},
		{name: "explicit worker custom", requested: "saved-custom", gateway: "https://custom.example", want: "saved-custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGenerator(Config{LLMProvider: "openai-compat", OpenAIBaseURL: "https://api.minimax.io/v1", AIGatewayURL: "http://official-gateway", DefaultModel: tc.configured, DefaultModelAutomatic: tc.automatic})
			data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{AIGatewayURL: tc.gateway, ModelName: tc.requested, Runtime: "openclaw"})
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Agents struct {
					Defaults struct{ Model struct{ Primary string } }
				}
				Models struct {
					Providers map[string]struct {
						BaseURL string `json:"baseUrl"`
						Models  []ModelSpec
					}
				}
			}
			if err = json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Agents.Defaults.Model.Primary != "agentteams-gateway/"+tc.want {
				t.Fatalf("primary=%s want=%s", cfg.Agents.Defaults.Model.Primary, tc.want)
			}
			if tc.gateway == "https://custom.example" {
				if cfg.Models.Providers["agentteams-gateway"].BaseURL != "https://custom.example/v1" {
					t.Fatal("gateway overwritten")
				}
				for _, spec := range cfg.Models.Providers["agentteams-gateway"].Models {
					if spec.ID == MiniMaxFlash && (tc.want != MiniMaxFlash || spec.Compat != nil) {
						t.Fatalf("official Flash policy leaked: %+v", spec)
					}
				}
			}
			// Per-worker changes must not mutate the reusable generator.
			if g.config.OpenAIBaseURL != "https://api.minimax.io/v1" {
				t.Fatal("generator provenance mutated")
			}
		})
	}
}
