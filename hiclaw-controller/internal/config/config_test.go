package config

import (
	"testing"
)

func TestNormalizeMinIOS3Endpoint(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"http://fs-local.hiclaw.io:9000", "http://fs-local.hiclaw.io:9000"},
		{"http://fs-local.hiclaw.io:8080", "http://fs-local.hiclaw.io:9000"},
		{"http://hiclaw-controller:8080", "http://hiclaw-controller:9000"},
		{"http://example:18080", "http://example:18080"},
	}
	for _, tc := range tests {
		if got := normalizeMinIOS3Endpoint(tc.in); got != tc.want {
			t.Errorf("normalizeMinIOS3Endpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadConfigAppliesManagerSpec(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_SPEC", `{
		"model":"qwen-max",
		"runtime":"copaw",
		"image":"hiclaw/manager:test",
		"resources":{
			"requests":{"cpu":"750m","memory":"1536Mi"},
			"limits":{"cpu":"3","memory":"5Gi"}
		}
	}`)
	t.Setenv("HICLAW_DEFAULT_MODEL", "qwen-default")

	cfg := LoadConfig()

	if cfg.ManagerModel != "qwen-max" {
		t.Fatalf("ManagerModel = %q, want %q", cfg.ManagerModel, "qwen-max")
	}
	if cfg.ManagerRuntime != "copaw" {
		t.Fatalf("ManagerRuntime = %q, want %q", cfg.ManagerRuntime, "copaw")
	}
	if cfg.ManagerImage != "hiclaw/manager:test" {
		t.Fatalf("ManagerImage = %q, want %q", cfg.ManagerImage, "hiclaw/manager:test")
	}
	if cfg.K8sManagerCPURequest != "750m" {
		t.Fatalf("K8sManagerCPURequest = %q, want %q", cfg.K8sManagerCPURequest, "750m")
	}
	if cfg.K8sManagerMemoryRequest != "1536Mi" {
		t.Fatalf("K8sManagerMemoryRequest = %q, want %q", cfg.K8sManagerMemoryRequest, "1536Mi")
	}
	if cfg.K8sManagerCPU != "3" {
		t.Fatalf("K8sManagerCPU = %q, want %q", cfg.K8sManagerCPU, "3")
	}
	if cfg.K8sManagerMemory != "5Gi" {
		t.Fatalf("K8sManagerMemory = %q, want %q", cfg.K8sManagerMemory, "5Gi")
	}
	if cfg.ManagerSpecResources == nil {
		t.Fatal("ManagerSpecResources = nil, want resources from HICLAW_MANAGER_SPEC")
	}
	if cfg.ManagerSpecResources.Requests.CPU != "750m" || cfg.ManagerSpecResources.Limits.Memory != "5Gi" {
		t.Fatalf("ManagerSpecResources = %+v", cfg.ManagerSpecResources)
	}
}

func TestLoadConfigUsesLegacyManagerEnvFallback(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_MODEL", "legacy-model")
	t.Setenv("HICLAW_MANAGER_RUNTIME", "openclaw")
	t.Setenv("HICLAW_MANAGER_IMAGE", "hiclaw/manager:legacy")
	t.Setenv("HICLAW_K8S_MANAGER_CPU", "4")
	t.Setenv("HICLAW_K8S_MANAGER_MEMORY", "6Gi")

	cfg := LoadConfig()

	if cfg.ManagerModel != "legacy-model" {
		t.Fatalf("ManagerModel = %q, want %q", cfg.ManagerModel, "legacy-model")
	}
	if cfg.ManagerRuntime != "openclaw" {
		t.Fatalf("ManagerRuntime = %q, want %q", cfg.ManagerRuntime, "openclaw")
	}
	if cfg.ManagerImage != "hiclaw/manager:legacy" {
		t.Fatalf("ManagerImage = %q, want %q", cfg.ManagerImage, "hiclaw/manager:legacy")
	}
	if cfg.K8sManagerCPU != "4" {
		t.Fatalf("K8sManagerCPU = %q, want %q", cfg.K8sManagerCPU, "4")
	}
	if cfg.K8sManagerMemory != "6Gi" {
		t.Fatalf("K8sManagerMemory = %q, want %q", cfg.K8sManagerMemory, "6Gi")
	}
}

func TestLoadConfigPanicsOnInvalidManagerSpec(t *testing.T) {
	t.Setenv("HICLAW_MANAGER_SPEC", "{")

	defer func() {
		if recover() == nil {
			t.Fatal("LoadConfig() did not panic on invalid HICLAW_MANAGER_SPEC")
		}
	}()

	_ = LoadConfig()
}

func TestLoadConfigPrefersAbstractInfraEnv(t *testing.T) {
	t.Setenv("HICLAW_KUBE_MODE", "incluster")
	t.Setenv("HICLAW_AI_GATEWAY_ADMIN_URL", "http://higress-admin.example.com:8001")
	t.Setenv("HICLAW_FS_BUCKET", "hiclaw-fs")
	t.Setenv("HICLAW_FS_ENDPOINT", "http://fs.example.com:9000")
	t.Setenv("HICLAW_STORAGE_PREFIX", "teams/demo")
	t.Setenv("HICLAW_CONTROLLER_URL", "http://controller.example.com:8090")
	t.Setenv("HICLAW_AI_GATEWAY_URL", "http://aigw.example.com:8080")
	t.Setenv("HICLAW_MATRIX_URL", "http://matrix.example.com:8080")

	cfg := LoadConfig()

	if cfg.HigressBaseURL != "http://higress-admin.example.com:8001" {
		t.Fatalf("HigressBaseURL = %q, want abstract admin URL", cfg.HigressBaseURL)
	}
	if cfg.OSSBucket != "hiclaw-fs" {
		t.Fatalf("OSSBucket = %q, want %q", cfg.OSSBucket, "hiclaw-fs")
	}
	if cfg.WorkerEnv.FSBucket != "hiclaw-fs" {
		t.Fatalf("WorkerEnv.FSBucket = %q, want %q", cfg.WorkerEnv.FSBucket, "hiclaw-fs")
	}
	if cfg.WorkerEnv.FSEndpoint != "http://fs.example.com:9000" {
		t.Fatalf("WorkerEnv.FSEndpoint = %q, want %q", cfg.WorkerEnv.FSEndpoint, "http://fs.example.com:9000")
	}
}

func TestLoadConfigUsesSharedAdminCredentialsForHigress(t *testing.T) {
	t.Setenv("HICLAW_ADMIN_USER", "shared-admin")
	t.Setenv("HICLAW_ADMIN_PASSWORD", "shared-secret")

	cfg := LoadConfig()

	if cfg.HigressAdminUser != "shared-admin" {
		t.Fatalf("HigressAdminUser = %q, want %q", cfg.HigressAdminUser, "shared-admin")
	}
	if cfg.HigressAdminPassword != "shared-secret" {
		t.Fatalf("HigressAdminPassword = %q, want %q", cfg.HigressAdminPassword, "shared-secret")
	}
}

func TestGatewayConfigAllowsDefaultAdminFallbackOnlyInEmbedded(t *testing.T) {
	t.Run("embedded", func(t *testing.T) {
		t.Setenv("HICLAW_KUBE_MODE", "embedded")
		cfg := LoadConfig()
		if !cfg.GatewayConfig().AllowDefaultAdminFallback {
			t.Fatal("expected embedded gateway config to allow default admin fallback")
		}
	})

	t.Run("incluster", func(t *testing.T) {
		t.Setenv("HICLAW_KUBE_MODE", "incluster")
		cfg := LoadConfig()
		if cfg.GatewayConfig().AllowDefaultAdminFallback {
			t.Fatal("expected incluster gateway config to disable default admin fallback")
		}
	})
}

func TestManagerAgentEnvForwardsAbstractInfraEnv(t *testing.T) {
	t.Setenv("HICLAW_KUBE_MODE", "incluster")
	t.Setenv("HICLAW_MINIO_USER", "root")
	t.Setenv("HICLAW_MINIO_PASSWORD", "secret")
	t.Setenv("HICLAW_AI_GATEWAY_ADMIN_URL", "http://higress-admin.example.com:8001")
	t.Setenv("HICLAW_FS_BUCKET", "hiclaw-fs")
	t.Setenv("HICLAW_FS_ENDPOINT", "http://fs.example.com:9000")
	t.Setenv("HICLAW_STORAGE_PREFIX", "teams/demo")
	t.Setenv("HICLAW_AI_GATEWAY_URL", "http://aigw.example.com:8080")
	t.Setenv("HICLAW_MATRIX_URL", "http://matrix.example.com:8080")

	cfg := LoadConfig()
	env := cfg.ManagerAgentEnv()

	for key, want := range map[string]string{
		"HICLAW_AI_GATEWAY_ADMIN_URL": "http://higress-admin.example.com:8001",
		"HICLAW_MATRIX_URL":           "http://matrix.example.com:8080",
		"HICLAW_AI_GATEWAY_URL":       "http://aigw.example.com:8080",
		"HICLAW_FS_ENDPOINT":          "http://fs.example.com:9000",
		"HICLAW_FS_BUCKET":            "hiclaw-fs",
		"HICLAW_STORAGE_PREFIX":       "teams/demo",
		"HICLAW_FS_ACCESS_KEY":        "root",
		"HICLAW_FS_SECRET_KEY":        "secret",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{
		"HIGRESS_BASE_URL",
		"HICLAW_MINIO_ENDPOINT",
		"HICLAW_MINIO_BUCKET",
		"HICLAW_OSS_BUCKET",
		"HICLAW_HIGRESS_ADMIN_USER",
		"HICLAW_HIGRESS_ADMIN_PASSWORD",
	} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in ManagerAgentEnv", legacyKey)
		}
	}
}

func TestLoadConfigDisablesAutoPrefix(t *testing.T) {
	t.Setenv("HICLAW_RESOURCE_AUTOPREFIX", "false")
	t.Setenv("HICLAW_RESOURCE_PREFIX", "hiclaw-")

	cfg := LoadConfig()

	if cfg.ResourceAutoPrefix {
		t.Fatal("ResourceAutoPrefix = true, want false")
	}
	if cfg.ResourcePrefix != "" {
		t.Fatalf("ResourcePrefix = %q, want empty", cfg.ResourcePrefix)
	}
	if cfg.ContainerPrefix != "" {
		t.Fatalf("ContainerPrefix = %q, want empty", cfg.ContainerPrefix)
	}
}

func TestLoadConfigAutoPrefixDisabledKeepsExplicitContainerPrefix(t *testing.T) {
	t.Setenv("HICLAW_RESOURCE_AUTOPREFIX", "false")
	t.Setenv("HICLAW_PROXY_CONTAINER_PREFIX", "custom-worker-")

	cfg := LoadConfig()

	if cfg.ContainerPrefix != "custom-worker-" {
		t.Fatalf("ContainerPrefix = %q, want %q", cfg.ContainerPrefix, "custom-worker-")
	}
}

// --- HiClaw → AgentTeams rename (#861): dual-prefix env regression ---

func TestLoadConfigReadsAgentTeamsPrefixedEnv(t *testing.T) {
	// String + bool + int values, mixing categories that go through
	// envOrDefault / envBool / envOrDefaultInt / direct envcompat.Lookup.
	t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
	t.Setenv("AGENTTEAMS_FS_BUCKET", "new-bucket")
	t.Setenv("AGENTTEAMS_LLM_API_KEY", "at-key")
	t.Setenv("AGENTTEAMS_LLM_PROVIDER", "anthropic")
	t.Setenv("AGENTTEAMS_MATRIX_E2EE", "1")
	t.Setenv("AGENTTEAMS_CMS_TRACES_ENABLED", "true")
	t.Setenv("AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS", "1200")

	cfg := LoadConfig()

	if cfg.KubeMode != "incluster" {
		t.Errorf("KubeMode = %q, want incluster (from AGENTTEAMS_KUBE_MODE)", cfg.KubeMode)
	}
	if cfg.OSSBucket != "new-bucket" {
		t.Errorf("OSSBucket = %q, want new-bucket (from AGENTTEAMS_FS_BUCKET)", cfg.OSSBucket)
	}
	if cfg.LLMAPIKey != "at-key" {
		t.Errorf("LLMAPIKey = %q, want at-key (from AGENTTEAMS_LLM_API_KEY)", cfg.LLMAPIKey)
	}
	if cfg.LLMProvider != "anthropic" {
		t.Errorf("LLMProvider = %q, want anthropic (from AGENTTEAMS_LLM_PROVIDER)", cfg.LLMProvider)
	}
	if !cfg.MatrixE2EE {
		t.Errorf("MatrixE2EE = false, want true (from AGENTTEAMS_MATRIX_E2EE)")
	}
	if !cfg.CMSTracesEnabled {
		t.Errorf("CMSTracesEnabled = false, want true (from AGENTTEAMS_CMS_TRACES_ENABLED)")
	}
	if cfg.AIStreamIdleTimeoutSeconds != 1200 {
		t.Errorf("AIStreamIdleTimeoutSeconds = %d, want 1200 (from AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS)", cfg.AIStreamIdleTimeoutSeconds)
	}
}

func TestLoadConfigPrefersAgentTeamsOverHiclaw(t *testing.T) {
	// When both prefixes are set, AGENTTEAMS_ wins for every category.
	t.Setenv("HICLAW_LLM_API_KEY", "old-key")
	t.Setenv("AGENTTEAMS_LLM_API_KEY", "new-key")
	t.Setenv("HICLAW_DEFAULT_MODEL", "old-model")
	t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "new-model")
	t.Setenv("HICLAW_YOLO", "1")
	t.Setenv("AGENTTEAMS_YOLO", "0")
	t.Setenv("HICLAW_MODEL_MAX_TOKENS", "1000")
	t.Setenv("AGENTTEAMS_MODEL_MAX_TOKENS", "4096")

	cfg := LoadConfig()

	if cfg.LLMAPIKey != "new-key" {
		t.Errorf("LLMAPIKey = %q, want new-key (AGENTTEAMS_ should win)", cfg.LLMAPIKey)
	}
	if cfg.DefaultModel != "new-model" {
		t.Errorf("DefaultModel = %q, want new-model", cfg.DefaultModel)
	}
	if cfg.WorkerEnv.YoloMode {
		t.Errorf("WorkerEnv.YoloMode = true, want false (AGENTTEAMS_YOLO=0 should win over HICLAW_YOLO=1)")
	}
	if cfg.ModelMaxTokens != 4096 {
		t.Errorf("ModelMaxTokens = %d, want 4096", cfg.ModelMaxTokens)
	}
}

func TestLoadConfigFallsBackToHiclawWhenAgentTeamsUnset(t *testing.T) {
	// Only legacy prefix set — must still flow through to Config.
	t.Setenv("HICLAW_LLM_API_KEY", "legacy-key")
	t.Setenv("HICLAW_FS_BUCKET", "legacy-bucket")
	t.Setenv("HICLAW_AI_STREAM_IDLE_TIMEOUT_SECONDS", "300")

	cfg := LoadConfig()

	if cfg.LLMAPIKey != "legacy-key" {
		t.Errorf("LLMAPIKey = %q, want legacy-key (HICLAW_ fallback)", cfg.LLMAPIKey)
	}
	if cfg.OSSBucket != "legacy-bucket" {
		t.Errorf("OSSBucket = %q, want legacy-bucket", cfg.OSSBucket)
	}
	if cfg.AIStreamIdleTimeoutSeconds != 300 {
		t.Errorf("AIStreamIdleTimeoutSeconds = %d, want 300", cfg.AIStreamIdleTimeoutSeconds)
	}
}
