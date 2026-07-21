package agentconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateOpenClawConfig_Basic(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "matrix.test:8080",
		MatrixServerURL: "http://matrix.test:8080",
		AIGatewayURL:    "http://aigw.test:8080",
		AdminUser:       "admin",
		DefaultModel:    "qwen3.5-plus",
	})

	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName:  "worker-alice",
		MatrixToken: "tok-matrix-alice",
		GatewayKey:  "key-gateway-alice",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Verify Matrix channel config
	channels := config["channels"].(map[string]interface{})
	matrixCfg := channels["matrix"].(map[string]interface{})
	if matrixCfg["userId"] != "@worker-alice:matrix.test:8080" {
		t.Errorf("userId = %v", matrixCfg["userId"])
	}
	if matrixCfg["accessToken"] != "tok-matrix-alice" {
		t.Errorf("accessToken = %v", matrixCfg["accessToken"])
	}
	if matrixCfg["streaming"] != "partial" {
		t.Errorf("streaming = %v, want partial", matrixCfg["streaming"])
	}
	if matrixCfg["blockStreaming"] != true {
		t.Errorf("blockStreaming = %v, want true", matrixCfg["blockStreaming"])
	}

	// Verify default allowFrom includes manager and admin
	groupAllow := toStringSlice(matrixCfg["groupAllowFrom"])
	if !containsString(groupAllow, "@manager:matrix.test:8080") {
		t.Errorf("groupAllowFrom missing manager: %v", groupAllow)
	}
	if !containsString(groupAllow, "@admin:matrix.test:8080") {
		t.Errorf("groupAllowFrom missing admin: %v", groupAllow)
	}

	// Verify default model in agents.defaults.model.primary
	agents := config["agents"].(map[string]interface{})
	defaults := agents["defaults"].(map[string]interface{})
	modelCfg := defaults["model"].(map[string]interface{})
	if modelCfg["primary"] != "agentteams-gateway/qwen3.5-plus" {
		t.Errorf("agents.defaults.model.primary = %v, want agentteams-gateway/qwen3.5-plus", modelCfg["primary"])
	}
}

func TestGenerateOpenClawConfig_TeamWorker(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "matrix.test:8080",
		MatrixServerURL: "http://matrix.test:8080",
		AIGatewayURL:    "http://aigw.test:8080",
	})

	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName:     "worker-dev-1",
		MatrixToken:    "tok",
		GatewayKey:     "key",
		TeamLeaderName: "team-lead-dev",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}

	var config map[string]interface{}
	json.Unmarshal(data, &config)

	matrixCfg := config["channels"].(map[string]interface{})["matrix"].(map[string]interface{})
	groupAllow := toStringSlice(matrixCfg["groupAllowFrom"])

	if containsString(groupAllow, "@manager:matrix.test:8080") {
		t.Error("team worker should not have manager in groupAllowFrom")
	}
	if !containsString(groupAllow, "@team-lead-dev:matrix.test:8080") {
		t.Errorf("team worker groupAllowFrom should include leader: %v", groupAllow)
	}
}

func TestGenerateOpenClawConfig_ChannelPolicy(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "d",
		MatrixServerURL: "http://m:8080",
		AIGatewayURL:    "http://g:8080",
	})

	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName:  "w1",
		MatrixToken: "tok",
		GatewayKey:  "key",
		ChannelPolicy: &ChannelPolicy{
			GroupAllowExtra: []string{"extra-user"},
			GroupDenyExtra:  []string{"manager"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}

	var config map[string]interface{}
	json.Unmarshal(data, &config)

	matrixCfg := config["channels"].(map[string]interface{})["matrix"].(map[string]interface{})
	groupAllow := toStringSlice(matrixCfg["groupAllowFrom"])

	if containsString(groupAllow, "@manager:d") {
		t.Error("manager should be denied by policy")
	}
	if !containsString(groupAllow, "@extra-user:d") {
		t.Errorf("extra-user should be allowed: %v", groupAllow)
	}
}

func TestGenerateOpenClawConfig_CustomModel(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "d",
		MatrixServerURL: "http://m:8080",
		AIGatewayURL:    "http://g:8080",
		DefaultModel:    "custom-model-x",
	})

	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName:  "w1",
		MatrixToken: "tok",
		GatewayKey:  "key",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}

	var config map[string]interface{}
	json.Unmarshal(data, &config)

	agents := config["agents"].(map[string]interface{})
	defaults := agents["defaults"].(map[string]interface{})
	modelCfg := defaults["model"].(map[string]interface{})
	if modelCfg["primary"] != "agentteams-gateway/custom-model-x" {
		t.Errorf("agents.defaults.model.primary = %v, want agentteams-gateway/custom-model-x", modelCfg["primary"])
	}
}

func TestGenerateOpenClawConfig_WithEmbedding(t *testing.T) {
	g := NewGenerator(Config{
		MatrixDomain:    "d",
		MatrixServerURL: "http://m:8080",
		AIGatewayURL:    "http://g:8080",
		EmbeddingModel:  "text-embedding-v3",
	})

	data, err := g.GenerateOpenClawConfig(WorkerConfigRequest{
		WorkerName:  "w1",
		MatrixToken: "tok",
		GatewayKey:  "key-embed",
	})
	if err != nil {
		t.Fatalf("GenerateOpenClawConfig: %v", err)
	}

	var config map[string]interface{}
	json.Unmarshal(data, &config)

	agents := config["agents"].(map[string]interface{})
	defaults := agents["defaults"].(map[string]interface{})
	memSearch, ok := defaults["memorySearch"].(map[string]interface{})
	if !ok {
		t.Fatal("memorySearch not found in agents.defaults")
	}
	if memSearch["model"] != "text-embedding-v3" {
		t.Errorf("memorySearch.model = %v", memSearch["model"])
	}
}

func TestMergeBuiltinSection_NewFile(t *testing.T) {
	source := "# Worker Agent\n\nYou are a worker.\n"
	result := MergeBuiltinSection("", source)

	if !strings.Contains(result, BuiltinStart) {
		t.Error("result should contain builtin start marker")
	}
	if !strings.Contains(result, BuiltinEnd) {
		t.Error("result should contain builtin end marker")
	}
	if !strings.Contains(result, "You are a worker.") {
		t.Error("result should contain source content")
	}
}

func TestMergeBuiltinSection_UpdatePreservesUserContent(t *testing.T) {
	existing := BuiltinHeader + "\nOld content\n\n" + BuiltinEnd + "\n\nMy custom rules\n"
	newSource := "# Updated\n\nNew builtin content\n"

	result := MergeBuiltinSection(existing, newSource)

	if !strings.Contains(result, "New builtin content") {
		t.Error("result should contain updated builtin content")
	}
	if strings.Contains(result, "Old content") {
		t.Error("result should not contain old builtin content")
	}
	if !strings.Contains(result, "My custom rules") {
		t.Error("result should preserve user content")
	}
}

func TestMergeBuiltinSection_LegacyFile(t *testing.T) {
	legacy := "# Old content without markers\nSome instructions\n"
	source := "# New builtin\nNew content\n"

	result := MergeBuiltinSection(legacy, source)

	if !strings.Contains(result, BuiltinStart) {
		t.Error("result should add markers to legacy file")
	}
	if !strings.Contains(result, "New content") {
		t.Error("result should contain new builtin content")
	}
}

func TestExtractFrontmatter(t *testing.T) {
	content := "---\ntitle: Test\n---\n# Content\nBody text\n"
	fm, body := ExtractFrontmatter(content)

	if !strings.Contains(fm, "title: Test") {
		t.Errorf("frontmatter = %q", fm)
	}
	if !strings.Contains(body, "# Content") {
		t.Errorf("body = %q", body)
	}
}

func TestExtractFrontmatter_NoFrontmatter(t *testing.T) {
	content := "# Just a heading\nBody text\n"
	fm, body := ExtractFrontmatter(content)

	if fm != "" {
		t.Errorf("expected empty frontmatter, got %q", fm)
	}
	if body != content {
		t.Errorf("body should equal original content")
	}
}

func TestDefaultModelSpec(t *testing.T) {
	spec := defaultModelSpec("claude-opus-4-6")
	if spec.ContextWindow != 1000000 {
		t.Errorf("claude-opus-4-6 ctx = %d, want 1000000", spec.ContextWindow)
	}
	if spec.MaxTokens != 128000 {
		t.Errorf("claude-opus-4-6 max = %d, want 128000", spec.MaxTokens)
	}
	if len(spec.Input) != 2 || spec.Input[1] != "image" {
		t.Errorf("claude-opus-4-6 should have vision: %v", spec.Input)
	}

	unknown := defaultModelSpec("unknown-model-xyz")
	if unknown.ContextWindow != 150000 {
		t.Errorf("unknown model ctx = %d, want 150000", unknown.ContextWindow)
	}
}

func TestMergeSoulTemplate_NewFile(t *testing.T) {
	rendered := "# leader - Team Leader\n\nYou are the Team Leader of `my-team`.\n"
	result := MergeSoulTemplate("", rendered)

	if !strings.Contains(result, SoulTemplateStart) {
		t.Error("result should contain soul template start marker")
	}
	if !strings.Contains(result, SoulTemplateEnd) {
		t.Error("result should contain soul template end marker")
	}
	if !strings.Contains(result, "Team Leader") {
		t.Error("result should contain rendered template content")
	}
}

func TestMergeSoulTemplate_PreservesPackageContent(t *testing.T) {
	packageSoul := "# Custom Leader Identity\n\nYou are a specialized leader.\n\n## Domain Knowledge\n\nYou know about finance.\n"
	rendered := "# leader - Team Leader\n\nYou are the Team Leader of `my-team`.\n"

	result := MergeSoulTemplate(packageSoul, rendered)

	if !strings.Contains(result, "Team Leader") {
		t.Error("result should contain template content")
	}
	if !strings.Contains(result, "Custom Leader Identity") {
		t.Error("result should preserve package content")
	}
	if !strings.Contains(result, "Domain Knowledge") {
		t.Error("result should preserve all package sections")
	}
}

func TestMergeSoulTemplate_UpdateReplacesTemplateSection(t *testing.T) {
	existing := SoulTemplateHeader + "\nOld template content\n\n" + SoulTemplateEnd + "\n\n# Package Content\nKeep this.\n"
	rendered := "# Updated Template\n\nNew team info.\n"

	result := MergeSoulTemplate(existing, rendered)

	if !strings.Contains(result, "New team info") {
		t.Error("result should contain updated template content")
	}
	if strings.Contains(result, "Old template content") {
		t.Error("result should not contain old template content")
	}
	if !strings.Contains(result, "Package Content") {
		t.Error("result should preserve content outside markers")
	}
}

func TestInjectChannelPolicy_FromExisting(t *testing.T) {
	existing := []byte(`{
  "channels": {
    "matrix": {
      "groupAllowFrom": ["@manager:m.test", "@admin:m.test"],
      "dm": {"allowFrom": ["@manager:m.test", "@admin:m.test"]}
    }
  }
}`)

	out := InjectChannelPolicy(
		existing,
		[]string{"@leader:m.test", "@admin:m.test", "@dev:m.test"},
		[]string{"@leader:m.test", "@admin:m.test"},
	)

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	matrix := got["channels"].(map[string]interface{})["matrix"].(map[string]interface{})
	gaf := matrix["groupAllowFrom"].([]interface{})
	if len(gaf) != 3 || gaf[0] != "@leader:m.test" || gaf[1] != "@admin:m.test" || gaf[2] != "@dev:m.test" {
		t.Errorf("groupAllowFrom = %v, want [@leader:m.test @admin:m.test @dev:m.test]", gaf)
	}
	dm := matrix["dm"].(map[string]interface{})
	daf := dm["allowFrom"].([]interface{})
	if len(daf) != 2 || daf[0] != "@leader:m.test" || daf[1] != "@admin:m.test" {
		t.Errorf("dm.allowFrom = %v, want [@leader:m.test @admin:m.test]", daf)
	}
}

func TestInjectChannelPolicy_FromEmpty(t *testing.T) {
	out := InjectChannelPolicy(
		nil,
		[]string{"@leader:m.test", "@admin:m.test", "@leader:m.test"},
		[]string{"@admin:m.test"},
	)

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	matrix := got["channels"].(map[string]interface{})["matrix"].(map[string]interface{})
	if gaf := matrix["groupAllowFrom"].([]interface{}); len(gaf) != 2 {
		t.Errorf("expected groupAllowFrom of length 2, got %v", gaf)
	}
	daf, ok := matrix["dm"].(map[string]interface{})["allowFrom"].([]interface{})
	if !ok {
		t.Fatalf("expected dm.allowFrom to be set")
	}
	if len(daf) != 1 || daf[0] != "@admin:m.test" {
		t.Errorf("dm.allowFrom = %v, want [@admin:m.test]", daf)
	}
}

func TestInjectChannelPolicy_EmptyInputsAreNoop(t *testing.T) {
	existing := []byte(`{"channels":{"matrix":{"groupAllowFrom":["@a:x","@b:x"]}}}`)

	if got := InjectChannelPolicy(existing, nil, []string{"@admin:m.test"}); string(got) != string(existing) {
		t.Errorf("empty group allow should noop, got %s", string(got))
	}
	if got := InjectChannelPolicy(existing, []string{"@leader:m.test"}, nil); string(got) != string(existing) {
		t.Errorf("empty dm allow should noop, got %s", string(got))
	}
}

func TestInjectChannelPolicy_PreservesUnrelatedFields(t *testing.T) {
	existing := []byte(`{
  "agents": {"defaults": {"model": "qwen-plus"}},
  "channels": {
    "matrix": {
      "homeserver": "http://m.test",
      "groupAllowFrom": ["@old:m.test"]
    }
  },
  "extras": {"foo": "bar"}
}`)
	out := InjectChannelPolicy(existing, []string{"@leader:m.test"}, []string{"@admin:m.test"})

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if model := got["agents"].(map[string]interface{})["defaults"].(map[string]interface{})["model"]; model != "qwen-plus" {
		t.Errorf("agents.defaults.model lost: %v", model)
	}
	matrix := got["channels"].(map[string]interface{})["matrix"].(map[string]interface{})
	if hs := matrix["homeserver"]; hs != "http://m.test" {
		t.Errorf("channels.matrix.homeserver lost: %v", hs)
	}
	if extras := got["extras"].(map[string]interface{})["foo"]; extras != "bar" {
		t.Errorf("extras.foo lost: %v", extras)
	}
}
