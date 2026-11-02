package agentconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

// providerPrefixOf splits a "provider/model" reference into its provider id.
func providerPrefixOf(t *testing.T, ref string) string {
	t.Helper()
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("model reference %q is not in provider/model form", ref)
	}
	return parts[0]
}

// generateTestConfig renders and parses an openclaw.json for the given request.
func generateTestConfig(t *testing.T, cfg Config, req WorkerConfigRequest) map[string]interface{} {
	t.Helper()
	g := NewGenerator(cfg)
	data, err := g.GenerateOpenClawConfig(req)
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("unmarshal openclaw.json: %v", err)
	}
	return config
}

// providerKeysOf returns the models.providers block ids of a generated config.
func providerKeysOf(t *testing.T, config map[string]interface{}) []string {
	t.Helper()
	providers, ok := config["models"].(map[string]interface{})["providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("models.providers missing or malformed: %v", config["models"])
	}
	keys := make([]string, 0, len(providers))
	for k := range providers {
		keys = append(keys, k)
	}
	return keys
}

// TestOpenClawProvider_Invariant pins the provider-block id to the single
// gatewayProviderID constant: models.providers must expose exactly that key,
// and every model reference (primary, subagents.model) must carry a provider
// prefix that resolves to a declared block. Catches any future change that
// re-points the "provider/model" prefix at an undeclared provider.
func TestOpenClawProvider_Invariant(t *testing.T) {
	assertResolvable := func(t *testing.T, config map[string]interface{}, ref string) {
		t.Helper()
		prefix := providerPrefixOf(t, ref)
		keys := providerKeysOf(t, config)
		if !containsString(keys, prefix) {
			t.Errorf("model reference %q has provider prefix %q, not in models.providers keys %v", ref, prefix, keys)
		}
	}

	cases := []struct {
		name     string
		model    string
		subagent string
	}{
		{"default model", "", ""},
		{"custom model", "custom-model-x", ""},
		{"builtin subagent", "qwen3.5-plus", "qwen3.5-flash"},
		{"custom subagent with provider prefix", "custom-model-x", "agentteams-gateway/custom-sub-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := generateTestConfig(t, Config{
				MatrixDomain:    "d",
				MatrixServerURL: "m:8080",
				AIGatewayURL:    "g:8080",
			}, WorkerConfigRequest{
				WorkerName:        "w1",
				MatrixToken:       "tok",
				GatewayKey:        "key",
				ModelName:         tc.model,
				SubagentModelName: tc.subagent,
			})

			keys := providerKeysOf(t, config)
			if len(keys) != 1 || keys[0] != gatewayProviderID {
				t.Fatalf("models.providers keys = %v, want exactly [%s]", keys, gatewayProviderID)
			}

			defaults := config["agents"].(map[string]interface{})["defaults"].(map[string]interface{})
			primary, ok := defaults["model"].(map[string]interface{})["primary"].(string)
			if !ok {
				t.Fatalf("agents.defaults.model.primary missing or not a string: %v", defaults["model"])
			}
			assertResolvable(t, config, primary)

			subagents, ok := defaults["subagents"].(map[string]interface{})
			if !ok {
				t.Fatalf("agents.defaults.subagents missing: %v", defaults["subagents"])
			}
			if sub, ok := subagents["model"].(string); ok {
				assertResolvable(t, config, sub)
			}
		})
	}
}

// TestOpenClawProvider_SubagentModelRegistered covers the former gap: a custom
// subagent model (not built-in, not equal to the primary) was written to
// subagents.model without ever being declared in the provider catalog or the
// alias table. It must now appear in both.
func TestOpenClawProvider_SubagentModelRegistered(t *testing.T) {
	const subagentModel = "some-custom-subagent-model"
	config := generateTestConfig(t, Config{
		MatrixDomain:    "d",
		MatrixServerURL: "m:8080",
		AIGatewayURL:    "g:8080",
		DefaultModel:    "qwen3.5-plus",
	}, WorkerConfigRequest{
		WorkerName:        "w1",
		MatrixToken:       "tok",
		GatewayKey:        "key",
		ModelName:         "qwen3.5-plus",
		SubagentModelName: subagentModel,
	})

	defaults := config["agents"].(map[string]interface{})["defaults"].(map[string]interface{})
	subagents, ok := defaults["subagents"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults.subagents missing: %v", defaults["subagents"])
	}
	if got := subagents["model"]; got != gatewayProviderID+"/"+subagentModel {
		t.Errorf("subagents.model = %v, want %s/%s", got, gatewayProviderID, subagentModel)
	}

	// Catalog: the model is declared exactly once.
	providers := config["models"].(map[string]interface{})["providers"].(map[string]interface{})
	gw, ok := providers[gatewayProviderID].(map[string]interface{})
	if !ok {
		t.Fatalf("models.providers[%s] missing", gatewayProviderID)
	}
	catalog, ok := gw["models"].([]interface{})
	if !ok {
		t.Fatalf("models.providers[%s].models malformed: %v", gatewayProviderID, gw["models"])
	}
	found := 0
	for _, entry := range catalog {
		if spec, ok := entry.(map[string]interface{}); ok && spec["id"] == subagentModel {
			found++
		}
	}
	if found != 1 {
		t.Errorf("catalog contains %d entries for %q, want 1", found, subagentModel)
	}

	// Alias table: the provider-qualified key is present.
	aliases, ok := defaults["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults.models missing: %v", defaults["models"])
	}
	alias, present := aliases[gatewayProviderID+"/"+subagentModel]
	if !present {
		t.Errorf("agents.defaults.models missing alias key %s/%s", gatewayProviderID, subagentModel)
	} else if got, _ := alias.(map[string]interface{})["alias"]; got != subagentModel {
		t.Errorf("alias value = %v, want %q", got, subagentModel)
	}
}

// TestOpenClawProvider_SubagentModelBuiltinNoDuplicate: a subagent model that
// is already in the built-in list must stay a single catalog entry.
func TestOpenClawProvider_SubagentModelBuiltinNoDuplicate(t *testing.T) {
	config := generateTestConfig(t, Config{
		MatrixDomain:    "d",
		MatrixServerURL: "m:8080",
		AIGatewayURL:    "g:8080",
		DefaultModel:    "qwen3.5-plus",
	}, WorkerConfigRequest{
		WorkerName:        "w1",
		MatrixToken:       "tok",
		GatewayKey:        "key",
		ModelName:         "qwen3.5-plus",
		SubagentModelName: "qwen3.5-plus",
	})

	providers := config["models"].(map[string]interface{})["providers"].(map[string]interface{})
	gw, ok := providers[gatewayProviderID].(map[string]interface{})
	if !ok {
		t.Fatalf("models.providers[%s] missing", gatewayProviderID)
	}
	catalog, ok := gw["models"].([]interface{})
	if !ok {
		t.Fatalf("models.providers[%s].models malformed: %v", gatewayProviderID, gw["models"])
	}
	found := 0
	for _, entry := range catalog {
		if spec, ok := entry.(map[string]interface{}); ok && spec["id"] == "qwen3.5-plus" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("catalog contains %d entries for builtin subagent model qwen3.5-plus, want 1", found)
	}
}

// TestOpenClawProvider_SubagentModelUnset: with SubagentModelName empty the
// output is unchanged — no subagents.model key, and catalog/aliases hold
// exactly the 16 built-ins plus the (custom) primary model.
func TestOpenClawProvider_SubagentModelUnset(t *testing.T) {
	config := generateTestConfig(t, Config{
		MatrixDomain:    "d",
		MatrixServerURL: "m:8080",
		AIGatewayURL:    "g:8080",
		DefaultModel:    "custom-model-x",
	}, WorkerConfigRequest{
		WorkerName:  "w1",
		MatrixToken: "tok",
		GatewayKey:  "key",
	})

	defaults := config["agents"].(map[string]interface{})["defaults"].(map[string]interface{})
	subagents, ok := defaults["subagents"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults.subagents missing: %v", defaults["subagents"])
	}
	if _, present := subagents["model"]; present {
		t.Errorf("agents.defaults.subagents.model = %v, want absent when SubagentModelName is empty", subagents["model"])
	}
	if subagents["maxConcurrent"] != float64(8) {
		t.Errorf("subagents.maxConcurrent = %v, want 8", subagents["maxConcurrent"])
	}

	providers := config["models"].(map[string]interface{})["providers"].(map[string]interface{})
	gw, ok := providers[gatewayProviderID].(map[string]interface{})
	if !ok {
		t.Fatalf("models.providers[%s] missing", gatewayProviderID)
	}
	catalog, ok := gw["models"].([]interface{})
	if !ok {
		t.Fatalf("models.providers[%s].models malformed: %v", gatewayProviderID, gw["models"])
	}
	if len(catalog) != 17 {
		t.Errorf("catalog has %d models, want 17 (16 builtins + custom primary)", len(catalog))
	}
	primaryFound := false
	for _, entry := range catalog {
		if spec, ok := entry.(map[string]interface{}); ok && spec["id"] == "custom-model-x" {
			primaryFound = true
		}
	}
	if !primaryFound {
		t.Error("catalog missing custom primary model custom-model-x")
	}

	aliases, ok := defaults["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults.models missing: %v", defaults["models"])
	}
	if len(aliases) != 17 {
		t.Errorf("agents.defaults.models has %d aliases, want 17 (16 builtins + custom primary)", len(aliases))
	}
}
