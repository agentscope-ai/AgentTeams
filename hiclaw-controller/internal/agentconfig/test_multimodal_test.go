package agentconfig

import (
	"encoding/json"
	"testing"
)

func TestMultimodal_VisionModel_GetsSupportsFlag(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "test.local",
		MatrixServerURL: "http://m:8008",
		AIGatewayURL:    "http://g:8080",
	})
	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName: "test-mm",
		ModelName:  "qwen3.6-plus",
		GatewayKey: "test-key",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	agents, ok := out["agents"].(map[string]any)
	if !ok {
		t.Fatal("missing agents section")
	}
	defaults, ok := agents["defaults"].(map[string]any)
	if !ok {
		t.Fatal("missing agents.defaults section")
	}

	if mm, ok := defaults["supports_multimodal"]; !ok || mm != true {
		t.Errorf("BUG: qwen3.6-plus missing supports_multimodal: got %v", mm)
	}
	if si, ok := defaults["supports_image"]; !ok || si != true {
		t.Errorf("BUG: qwen3.6-plus missing supports_image: got %v", si)
	}
}

func TestMultimodal_TextOnlyModel_NoFlag(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "test.local",
		MatrixServerURL: "http://m:8008",
		AIGatewayURL:    "http://g:8080",
	})
	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName: "test-txt",
		ModelName:  "deepseek-chat",
		GatewayKey: "test-key",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	defaults := out["agents"].(map[string]any)["defaults"].(map[string]any)

	if mm, ok := defaults["supports_multimodal"]; ok && mm == true {
		t.Errorf("BUG: deepseek-chat should not have supports_multimodal=true: got %v", mm)
	}
	if si, ok := defaults["supports_image"]; ok && si == true {
		t.Errorf("BUG: deepseek-chat should not have supports_image=true: got %v", si)
	}
}
