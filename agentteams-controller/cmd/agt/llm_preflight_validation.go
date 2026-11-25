package main

import (
	"encoding/json"
	"strings"
)

// Installer-only validation, before a Harness exists. Runtime health is Harness-owned.
func validateLLMPreflight(b []byte) string {
	var r struct {
		Error    json.RawMessage `json:"error"`
		BaseResp struct {
			Code int `json:"status_code"`
		} `json:"base_resp"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if len(b) > 65536 || json.Unmarshal(b, &r) != nil {
		return "invalid_response"
	}
	if (len(r.Error) > 0 && string(r.Error) != "null") || r.BaseResp.Code != 0 {
		// Inspect in memory only, never return any provider-controlled string.
		text := strings.ToLower(string(b))
		switch {
		case r.BaseResp.Code == 1008 || strings.Contains(text, "balance") || strings.Contains(text, "quota"):
			return "billing_or_quota"
		case strings.Contains(text, "unauthorized") || strings.Contains(text, "api_key") || strings.Contains(text, "authentication"):
			return "authentication"
		default:
			return "provider_error"
		}
	}
	for _, c := range r.Choices {
		if strings.TrimSpace(c.Message.Content) != "" {
			return "ok"
		}
	}
	return "empty_response"
}
