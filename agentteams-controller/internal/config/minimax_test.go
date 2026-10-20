package config

import "testing"

func TestMiniMaxRoutePropagation(t *testing.T) {
	for _, endpoint := range []string{"https://api.minimax.io/v1", "https://api.minimax.cn/v1/"} {
		for _, selected := range []string{"", "MiniMax-M3", "MiniMax-M2.7", "saved-custom"} {
			t.Setenv("AGENTTEAMS_LLM_PROVIDER", "openai-compat")
			t.Setenv("AGENTTEAMS_OPENAI_BASE_URL", endpoint)
			t.Setenv("AGENTTEAMS_DEFAULT_MODEL", selected)
			cfg := LoadConfig()
			want := selected
			if want == "" {
				want = "MiniMax-M3.1-Flash"
			}
			if cfg.DefaultModel != want {
				t.Fatalf("default=%s want=%s", cfg.DefaultModel, want)
			}
			generated := cfg.AgentConfig()
			if generated.DefaultModelAutomatic != (selected == "") {
				t.Fatal("generator lost automatic default provenance")
			}
			if generated.OpenAIBaseURL != endpoint || generated.LLMProvider != "openai-compat" || generated.DefaultModel != want {
				t.Fatalf("generator route lost: %+v", generated)
			}
			env := cfg.ManagerAgentEnv()
			if (env["AGENTTEAMS_MINIMAX_DEFAULTED"] == "1") != (selected == "") {
				t.Fatal("auto-default marker must not override explicit selection")
			}
			if env["AGENTTEAMS_OPENAI_BASE_URL"] != endpoint || env["AGENTTEAMS_DEFAULT_MODEL"] != want {
				t.Fatal("manager route/default lost")
			}
		}
	}
}
