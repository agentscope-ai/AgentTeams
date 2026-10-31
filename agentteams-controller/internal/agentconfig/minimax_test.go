package agentconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMiniMaxEndpointCatalog(t *testing.T) {
	for _, endpoint := range []string{"https://api.minimax.io/v1", "https://api.minimax.cn/v1/"} {
		for _, selected := range []string{"", "MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "custom-model"} {
			t.Run(endpoint+"/"+selected, func(t *testing.T) {
				g := NewGenerator(Config{LLMProvider: "openai-compat", OpenAIBaseURL: endpoint, DefaultModel: selected})
				data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{GatewayKey: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				var cfg struct {
					Models struct {
						Providers map[string]struct {
							Models []ModelSpec `json:"models"`
						} `json:"providers"`
					} `json:"models"`
					Agents struct {
						Defaults struct {
							Model struct {
								Primary string `json:"primary"`
							} `json:"model"`
							Models map[string]interface{} `json:"models"`
						} `json:"defaults"`
					} `json:"agents"`
				}
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				want := selected
				if want == "" {
					want = MiniMaxFlash
				}
				if cfg.Agents.Defaults.Model.Primary != "agentteams-gateway/"+want {
					t.Fatalf("primary: %s", cfg.Agents.Defaults.Model.Primary)
				}
				models := cfg.Models.Providers["agentteams-gateway"].Models
				if models[0].ID != MiniMaxFlash || models[1].ID != "MiniMax-M3" {
					t.Fatalf("catalog order: %+v", models)
				}
				if models[0].ContextWindow != 1000000 || models[0].MaxTokens != 524288 || models[0].Compat["supportsReasoningEffort"] != false {
					t.Fatalf("Flash metadata: %+v", models[0])
				}
				if _, ok := cfg.Agents.Defaults.Models["agentteams-gateway/"+want]; !ok {
					t.Fatal("selected alias lost")
				}
				for _, model := range models {
					if model.ID == "MiniMax-M2.7" && selected != model.ID {
						t.Fatal("ordinary legacy choice leaked")
					}
				}
			})
		}
	}
	for _, endpoint := range []string{"https://coding.dashscope.aliyuncs.com/v1", "https://api.minimax.io.evil.example/v1", "https://custom.example/v1", "https://api.minimax.io/anthropic", "http://api.minimax.io/v1", "https://api.minimax.io/v1?custom=1"} {
		if IsMiniMaxOpenAI("openai-compat", endpoint) {
			t.Fatalf("incorrect endpoint recognition: %s", endpoint)
		}
		g := NewGenerator(Config{LLMProvider: "openai-compat", OpenAIBaseURL: endpoint})
		if g.config.DefaultModel != "qwen3.6-plus" {
			t.Fatal("unrelated default changed")
		}
		for _, model := range g.allModelSpecs("custom") {
			if model.ID == MiniMaxFlash {
				t.Fatal("Flash advertised on unrelated route")
			}
		}
	}
	g := NewGenerator(Config{LLMProvider: "openai-compat", OpenAIBaseURL: "https://api.minimax.io/v1"})
	for _, selected := range []string{"MiniMax-M2.7", "agentteams-gateway/MiniMax-M2.5", "saved-custom"} {
		data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{ModelName: selected, Runtime: "openclaw"})
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]interface{}
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		defaults := cfg["agents"].(map[string]interface{})["defaults"].(map[string]interface{})
		want := "agentteams-gateway/" + strings.TrimPrefix(selected, "agentteams-gateway/")
		if defaults["model"].(map[string]interface{})["primary"] != want {
			t.Fatal("explicit worker selection lost")
		}
		if _, ok := defaults["models"].(map[string]interface{})[want]; !ok {
			t.Fatal("explicit worker alias lost")
		}
	}
	if IsMiniMaxOpenAI("qwen", "https://api.minimax.io/v1") {
		t.Fatal("provider ignored")
	}
}
