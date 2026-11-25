package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Probe an upstream connection separately from a saved Worker's gateway probe.
// Stored credentials are read and used entirely inside Controller.
func (h *gatewayAssets) probeModel(parent context.Context, req gatewayAssetRequest) (any, error) {
	var config struct {
		Model        string `json:"model"`
		Type         string `json:"type"`
		Protocol     string `json:"protocol"`
		Token        string `json:"token"`
		BaseURL      string `json:"baseUrl"`
		ProviderName string `json:"providerName"`
	}
	if len(req.Input) > 0 && json.Unmarshal(req.Input, &config) != nil {
		return nil, assetError(400, "invalid model probe input")
	}
	if config.ProviderName == "" {
		config.ProviderName = req.Name
	}
	if config.Model == "" {
		config.Model = req.Model
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, assetError(400, "model is required")
	}
	if config.ProviderName != "" {
		if !validModelName(config.ProviderName) {
			return nil, assetError(400, "invalid model provider name")
		}
		value, err := h.ai(parent, "GET", "providers", config.ProviderName, nil)
		if err != nil {
			return nil, err
		}
		provider := object(value)
		if provider == nil {
			return nil, assetError(404, "model provider not found")
		}
		raw := object(provider["rawConfigs"])
		config.Type, config.Protocol, config.BaseURL = textField(provider["type"]), textField(provider["protocol"]), textField(raw["openaiCustomUrl"])
		if config.BaseURL != "" && raw["openaiCustomServicePort"] != nil {
			if u, err := url.Parse(config.BaseURL); err == nil && u.Port() == "" {
				u.Host = u.Hostname() + ":" + fmt.Sprint(raw["openaiCustomServicePort"])
				config.BaseURL = u.String()
			}
		}
		tokens, _ := provider["tokens"].([]any)
		if len(tokens) == 0 {
			tokens, _ = raw["apiTokens"].([]any)
		}
		if len(tokens) > 0 {
			config.Token = textField(tokens[0])
		}
		if target := textField(object(raw["modelMapping"])[config.Model]); target != "" {
			config.Model = target
		}
	}
	if config.Type == "" {
		config.Type = "openai"
	}
	nativeClaude := config.Type == "claude" && (config.BaseURL == "" || config.Protocol == "original")
	nativeGemini := config.Type == "gemini" && config.Protocol == "original"
	if config.Protocol == "original" && !nativeClaude && !nativeGemini && config.Type != "openai" {
		return nil, assetError(400, "this native model protocol cannot be probed; use an OpenAI-compatible endpoint")
	}
	if config.BaseURL == "" {
		config.BaseURL = map[string]string{"openai": "https://api.openai.com/v1", "qwen": "https://dashscope.aliyuncs.com/compatible-mode/v1", "deepseek": "https://api.deepseek.com/v1", "moonshot": "https://api.moonshot.cn/v1", "zhipuai": "https://open.bigmodel.cn/api/paas/v4", "minimax": "https://api.minimaxi.com/v1", "claude": "https://api.anthropic.com/v1", "gemini": "https://generativelanguage.googleapis.com/v1beta/openai", "doubao": "https://ark.cn-beijing.volces.com/api/v3", "mistral": "https://api.mistral.ai/v1", "github": "https://models.inference.ai.azure.com"}[config.Type]
		if nativeGemini {
			config.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
		}
	}
	u, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || config.Token == "" {
		return nil, assetError(400, "valid model endpoint and credential are required")
	}
	policy := environmentModelProbePolicy()
	if h.resources != nil {
		policy, _, err = h.resources.readModelProbePolicy(parent)
		if err != nil {
			return nil, assetError(503, "cannot read model probe address configuration")
		}
	}
	if !policy.hostAllowed(u.Hostname()) {
		return nil, assetError(400, "model probe endpoint is outside the configured network allowlist")
	}
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + config.Token}
	payload := map[string]any{"model": config.Model, "messages": []any{map[string]string{"role": "user", "content": "Reply with only one word: OK"}}, "stream": false}
	endpoint := u.String() + "/chat/completions"
	if nativeClaude {
		endpoint = u.String() + "/messages"
		delete(headers, "Authorization")
		headers["x-api-key"], headers["anthropic-version"] = config.Token, "2023-06-01"
		payload["max_tokens"] = 256
	} else if nativeGemini {
		endpoint = u.String() + "/models/" + url.PathEscape(strings.TrimPrefix(config.Model, "models/")) + ":generateContent"
		delete(headers, "Authorization")
		headers["x-goog-api-key"] = config.Token
		payload = map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "Reply with only one word: OK"}}}}, "generationConfig": map[string]int{"maxOutputTokens": 256}}
	} else {
		if config.Type == "azure" {
			delete(headers, "Authorization")
			headers["api-key"] = config.Token
		}
		budget := "max_tokens"
		if regexp.MustCompile(`^(gpt-[56]|o[134](-|$)|MiniMax-)`).MatchString(config.Model) {
			budget = "max_completion_tokens"
		}
		payload[budget] = 256
		if config.Model == "MiniMax-M3" {
			payload["thinking"] = map[string]string{"type": "disabled"}
		}
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, assetError(400, "invalid model endpoint")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	started := time.Now()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(request)
	success := false
	message := "model upstream connection failed or timed out"
	if err == nil {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		value := decodeObject(raw)
		if res.StatusCode >= 200 && res.StatusCode < 300 && value["error"] == nil && value["success"] != false {
			if items, ok := value["choices"].([]any); ok && len(items) > 0 {
				m := object(object(items[0])["message"])
				success = hasModelText(m["content"]) || hasModelText(m["reasoning_content"])
			}
			success = success || hasModelText(value["content"])
			if candidates, ok := value["candidates"].([]any); ok && len(candidates) > 0 {
				success = success || hasModelText(object(object(candidates[0])["content"])["parts"])
			}
			message = "model responded without generated content"
			if success {
				message = "model upstream generated content; Worker gateway authorization is not verified"
			}
		} else {
			message = fmt.Sprintf("model upstream returned HTTP %d; check endpoint, model, credential and quota", res.StatusCode)
		}
	}
	return map[string]any{"success": success, "message": message, "model": config.Model, "latencyMs": time.Since(started).Milliseconds(), "scope": "upstream"}, nil
}

func hasModelText(value any) bool {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	if parts, ok := value.([]any); ok {
		for _, part := range parts {
			if strings.TrimSpace(textField(object(part)["text"])) != "" {
				return true
			}
		}
	}
	return false
}
