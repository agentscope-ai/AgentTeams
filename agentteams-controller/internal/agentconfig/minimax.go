package agentconfig

import "strings"

const MiniMaxFlash = "MiniMax-M3.1-Flash"

// IsMiniMaxOpenAI identifies only the documented direct OpenAI-compatible routes.
// A gateway URL or a hostname containing "minimax" does not establish support.
func IsMiniMaxOpenAI(provider, baseURL string) bool {
	if provider != "openai-compat" {
		return false
	}
	switch strings.TrimSuffix(baseURL, "/") {
	case "https://api.minimax.io/v1", "https://api.minimax.cn/v1":
		return true
	}
	return false
}

// DefaultModel preserves explicit selections, including legacy and custom IDs.
func DefaultModel(provider, baseURL, selected, fallback string) string {
	if selected != "" {
		return selected
	}
	if IsMiniMaxOpenAI(provider, baseURL) {
		return MiniMaxFlash
	}
	return fallback
}

func (g *Generator) modelNames() []string {
	models := []string{
		"gpt-5.4", "gpt-5.3-codex", "gpt-5-mini", "gpt-5-nano",
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
		"qwen3.6-plus", "qwen3.5-plus", "deepseek-chat", "deepseek-reasoner",
		"kimi-k2.5", "glm-5",
	}
	if IsMiniMaxOpenAI(g.config.LLMProvider, g.config.OpenAIBaseURL) {
		return append([]string{MiniMaxFlash, "MiniMax-M3"}, models...)
	}
	return append(models, "MiniMax-M3", "MiniMax-M2.7")
}
