package config

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Provide AS tokens for all tests so LoadConfig() does not panic.
	os.Setenv("AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN", "test-as-token")
	os.Setenv("AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN", "test-hs-token")
	os.Exit(m.Run())
}

func TestNormalizeMinIOS3Endpoint(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"http://fs-local.agentteams.io:9000", "http://fs-local.agentteams.io:9000"},
		{"http://fs-local.agentteams.io:8080", "http://fs-local.agentteams.io:9000"},
		{"http://agentteams-controller:8080", "http://agentteams-controller:9000"},
		{"http://example:18080", "http://example:18080"},
	}
	for _, tc := range tests {
		if got := normalizeMinIOS3Endpoint(tc.in); got != tc.want {
			t.Errorf("normalizeMinIOS3Endpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoadConfigMetricsBindAddrDefaultsByMode(t *testing.T) {
	t.Run("embedded", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_KUBE_MODE", "embedded")
		cfg := LoadConfig()
		if cfg.MetricsBindAddr != "0" {
			t.Fatalf("MetricsBindAddr = %q, want disabled metrics in embedded mode", cfg.MetricsBindAddr)
		}
	})

	t.Run("incluster", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
		cfg := LoadConfig()
		if cfg.MetricsBindAddr != ":8080" {
			t.Fatalf("MetricsBindAddr = %q, want :8080 in incluster mode", cfg.MetricsBindAddr)
		}
	})
}

func TestLoadConfigMetricsBindAddrPrefersAgentTeamsEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
	t.Setenv("AGENTTEAMS_METRICS_BIND_ADDR", ":19090")

	cfg := LoadConfig()
	if cfg.MetricsBindAddr != ":19090" {
		t.Fatalf("MetricsBindAddr = %q, want AGENTTEAMS_METRICS_BIND_ADDR", cfg.MetricsBindAddr)
	}
}

func TestLoadConfigMetricsBindAddrIgnoresLegacyFallback(t *testing.T) {
	t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
	t.Setenv("HICLAW_METRICS_BIND_ADDR", ":18080")

	cfg := LoadConfig()
	if cfg.MetricsBindAddr != ":8080" {
		t.Fatalf("MetricsBindAddr = %q, want default :8080 without HICLAW fallback", cfg.MetricsBindAddr)
	}
}

func TestSandboxConfigUsesControllerName(t *testing.T) {
	t.Setenv("AGENTTEAMS_CONTROLLER_NAME", "ctl-main")

	cfg := LoadConfig()
	sandboxCfg := cfg.SandboxConfig()
	if sandboxCfg.ControllerName != "ctl-main" {
		t.Fatalf("SandboxConfig.ControllerName = %q, want AGENTTEAMS_CONTROLLER_NAME", sandboxCfg.ControllerName)
	}
}

func TestLoadConfigSandboxPrewarmSize(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{name: "default", want: 1},
		{name: "explicit", env: "3", want: 3},
		{name: "invalid", env: "many", want: 1},
		{name: "zero", env: "0", want: 0},
		{name: "negative", env: "-2", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTTEAMS_SANDBOX_PREWARM_SIZE", tt.env)
			cfg := LoadConfig()
			if cfg.SandboxPrewarmSize != tt.want {
				t.Fatalf("SandboxPrewarmSize=%d, want %d", cfg.SandboxPrewarmSize, tt.want)
			}
			if cfg.SandboxConfig().SandboxPrewarmSize != tt.want {
				t.Fatalf("SandboxConfig().SandboxPrewarmSize=%d, want %d", cfg.SandboxConfig().SandboxPrewarmSize, tt.want)
			}
		})
	}
}

func TestLoadConfigMountAuthEnv(t *testing.T) {
	t.Run("default RRSA", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_MOUNT_AUTH_TYPE", "")
		t.Setenv("AGENTTEAMS_MOUNT_ROLE_NAME", "")
		cfg := LoadConfig()
		if cfg.MountAuthType != "RRSA" {
			t.Fatalf("MountAuthType=%q, want RRSA", cfg.MountAuthType)
		}
		if cfg.MountRoleName != "" {
			t.Fatalf("MountRoleName=%q, want empty by default", cfg.MountRoleName)
		}
	})

	t.Run("explicit values", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_MOUNT_AUTH_TYPE", "AccessKey")
		t.Setenv("AGENTTEAMS_MOUNT_ROLE_NAME", "rrsa-role-a")
		cfg := LoadConfig()
		if cfg.MountAuthType != "AccessKey" {
			t.Fatalf("MountAuthType=%q, want AccessKey", cfg.MountAuthType)
		}
		if cfg.MountRoleName != "rrsa-role-a" {
			t.Fatalf("MountRoleName=%q, want rrsa-role-a", cfg.MountRoleName)
		}
	})
}

func TestLoadConfigAppliesManagerSpec(t *testing.T) {
	t.Setenv("AGENTTEAMS_MANAGER_SPEC", `{
		"model":"qwen-max",
		"runtime":"copaw",
		"image":"hiclaw/manager:test",
		"resources":{
			"requests":{"cpu":"750m","memory":"1536Mi"},
			"limits":{"cpu":"3","memory":"5Gi"}
		}
	}`)
	t.Setenv("AGENTTEAMS_DEFAULT_MODEL", "qwen-default")
	t.Setenv("AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN", "test-as-token-for-unit-tests")
	t.Setenv("AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN", "test-hs-token-for-unit-tests")

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
}

func TestLoadConfigUsesLegacyManagerEnvFallback(t *testing.T) {
	t.Setenv("AGENTTEAMS_MANAGER_MODEL", "legacy-model")
	t.Setenv("AGENTTEAMS_MANAGER_RUNTIME", "openclaw")
	t.Setenv("AGENTTEAMS_MANAGER_IMAGE", "hiclaw/manager:legacy")
	t.Setenv("AGENTTEAMS_K8S_MANAGER_CPU", "4")
	t.Setenv("AGENTTEAMS_K8S_MANAGER_MEMORY", "6Gi")

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

func TestLoadConfigAuthTokenExpirationSeconds(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg := LoadConfig()
		if cfg.AuthTokenExpirationSeconds != 3600 {
			t.Fatalf("AuthTokenExpirationSeconds=%d, want 3600", cfg.AuthTokenExpirationSeconds)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_AUTH_TOKEN_EXPIRATION_SECONDS", "7200")
		cfg := LoadConfig()
		if cfg.AuthTokenExpirationSeconds != 7200 {
			t.Fatalf("AuthTokenExpirationSeconds=%d, want 7200", cfg.AuthTokenExpirationSeconds)
		}
	})

	t.Run("minimum", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_AUTH_TOKEN_EXPIRATION_SECONDS", "300")
		cfg := LoadConfig()
		if cfg.AuthTokenExpirationSeconds != 600 {
			t.Fatalf("AuthTokenExpirationSeconds=%d, want min clamp 600", cfg.AuthTokenExpirationSeconds)
		}
	})
}

func TestLoadConfigPanicsOnInvalidManagerSpec(t *testing.T) {
	t.Setenv("AGENTTEAMS_MANAGER_SPEC", "{")

	defer func() {
		if recover() == nil {
			t.Fatal("LoadConfig() did not panic on invalid AGENTTEAMS_MANAGER_SPEC")
		}
	}()

	_ = LoadConfig()
}

func TestLoadConfigPrefersAbstractInfraEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
	t.Setenv("AGENTTEAMS_AI_GATEWAY_ADMIN_URL", "http://higress-admin.example.com:8001")
	t.Setenv("AGENTTEAMS_FS_BUCKET", "agentteams-fs")
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "http://fs.example.com:9000")
	t.Setenv("AGENTTEAMS_STORAGE_PREFIX", "teams/demo")
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.example.com:8090")
	t.Setenv("AGENTTEAMS_AI_GATEWAY_URL", "http://aigw.example.com:8080")
	t.Setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com:8080")

	cfg := LoadConfig()

	if cfg.HigressBaseURL != "http://higress-admin.example.com:8001" {
		t.Fatalf("HigressBaseURL = %q, want abstract admin URL", cfg.HigressBaseURL)
	}
	if cfg.OSSBucket != "agentteams-fs" {
		t.Fatalf("OSSBucket = %q, want %q", cfg.OSSBucket, "agentteams-fs")
	}
	if cfg.WorkerEnv.FSBucket != "agentteams-fs" {
		t.Fatalf("WorkerEnv.FSBucket = %q, want %q", cfg.WorkerEnv.FSBucket, "agentteams-fs")
	}
	if cfg.WorkerEnv.FSEndpoint != "http://fs.example.com:9000" {
		t.Fatalf("WorkerEnv.FSEndpoint = %q, want %q", cfg.WorkerEnv.FSEndpoint, "http://fs.example.com:9000")
	}
}

func TestLoadConfigAgentIdentityDataEndpoint(t *testing.T) {
	t.Run("explicit endpoint wins", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_AGENT_IDENTITY_DATA_ENDPOINT", "agentidentitydata.internal.example.com")
		t.Setenv("AGENTTEAMS_REGION", "cn-beijing")

		cfg := LoadConfig()

		if cfg.AgentIdentityDataEndpoint != "agentidentitydata.internal.example.com" {
			t.Fatalf("AgentIdentityDataEndpoint = %q, want explicit endpoint", cfg.AgentIdentityDataEndpoint)
		}
	})

	t.Run("derives endpoint from region", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_AGENT_IDENTITY_DATA_ENDPOINT", "")
		t.Setenv("AGENTTEAMS_REGION", "cn-beijing")

		cfg := LoadConfig()

		if cfg.AgentIdentityDataEndpoint != "agentidentitydata.cn-beijing.aliyuncs.com" {
			t.Fatalf("AgentIdentityDataEndpoint = %q, want derived endpoint", cfg.AgentIdentityDataEndpoint)
		}
	})

	t.Run("uses default region", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_AGENT_IDENTITY_DATA_ENDPOINT", "")
		t.Setenv("AGENTTEAMS_REGION", "")

		cfg := LoadConfig()

		if cfg.AgentIdentityDataEndpoint != "agentidentitydata.cn-hangzhou.aliyuncs.com" {
			t.Fatalf("AgentIdentityDataEndpoint = %q, want default-region endpoint", cfg.AgentIdentityDataEndpoint)
		}
	})
}

func TestLoadConfigSandboxAgentRuntimeImage(t *testing.T) {
	t.Run("derives image from region", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_REGION", "cn-beijing")
		t.Setenv("AGENTTEAMS_SANDBOX_AGENT_RUNTIME_IMAGE", "")

		cfg := LoadConfig()

		if got, want := cfg.SandboxConfig().AgentRuntimeImage, "registry-cn-beijing-vpc.ack.aliyuncs.com/acs/agent-runtime:v0.0.9"; got != want {
			t.Fatalf("SandboxConfig.AgentRuntimeImage = %q, want %q", got, want)
		}
	})

	t.Run("explicit override wins", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_REGION", "cn-beijing")
		t.Setenv("AGENTTEAMS_SANDBOX_AGENT_RUNTIME_IMAGE", "registry.example.com/custom/agent-runtime:test")

		cfg := LoadConfig()

		if got, want := cfg.SandboxConfig().AgentRuntimeImage, "registry.example.com/custom/agent-runtime:test"; got != want {
			t.Fatalf("SandboxConfig.AgentRuntimeImage = %q, want %q", got, want)
		}
	})
}

func TestMatrixConfigIncludesAppServicePushURL(t *testing.T) {
	cfg := &Config{
		MatrixAppServicePushURL: appServicePushURL("http://controller.example.com:8090/"),
	}

	if got, want := cfg.MatrixConfig().AppServicePushURL, "http://controller.example.com:8090"; got != want {
		t.Fatalf("AppServicePushURL = %q, want %q", got, want)
	}
}

func TestLoadConfigUsesMatrixAppServiceControllerURLOverride(t *testing.T) {
	t.Setenv("AGENTTEAMS_MATRIX_APPSERVICE_CONTROLLER_URL", "http://matrix-facing-controller:8090/")

	cfg := LoadConfig()

	if got, want := cfg.MatrixConfig().AppServicePushURL, "http://matrix-facing-controller:8090"; got != want {
		t.Fatalf("AppServicePushURL = %q, want %q", got, want)
	}
}

func TestLoadConfigUsesSharedAdminCredentialsForHigress(t *testing.T) {
	t.Setenv("AGENTTEAMS_ADMIN_USER", "shared-admin")
	t.Setenv("AGENTTEAMS_ADMIN_PASSWORD", "shared-secret")

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
		t.Setenv("AGENTTEAMS_KUBE_MODE", "embedded")
		cfg := LoadConfig()
		if !cfg.GatewayConfig().AllowDefaultAdminFallback {
			t.Fatal("expected embedded gateway config to allow default admin fallback")
		}
	})

	t.Run("incluster", func(t *testing.T) {
		t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
		cfg := LoadConfig()
		if cfg.GatewayConfig().AllowDefaultAdminFallback {
			t.Fatal("expected incluster gateway config to disable default admin fallback")
		}
	})
}

func TestManagerAgentEnvForwardsAbstractInfraEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_KUBE_MODE", "incluster")
	t.Setenv("AGENTTEAMS_MINIO_USER", "root")
	t.Setenv("AGENTTEAMS_MINIO_PASSWORD", "secret")
	t.Setenv("AGENTTEAMS_AI_GATEWAY_ADMIN_URL", "http://higress-admin.example.com:8001")
	t.Setenv("AGENTTEAMS_FS_BUCKET", "agentteams-fs")
	t.Setenv("AGENTTEAMS_FS_ENDPOINT", "http://fs.example.com:9000")
	t.Setenv("AGENTTEAMS_STORAGE_PREFIX", "teams/demo")
	t.Setenv("AGENTTEAMS_AI_GATEWAY_URL", "http://aigw.example.com:8080")
	t.Setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com:8080")

	cfg := LoadConfig()
	env := cfg.ManagerAgentEnv()

	for key, want := range map[string]string{
		"AGENTTEAMS_AI_GATEWAY_ADMIN_URL": "http://higress-admin.example.com:8001",
		"AGENTTEAMS_MATRIX_URL":           "http://matrix.example.com:8080",
		"AGENTTEAMS_AI_GATEWAY_URL":       "http://aigw.example.com:8080",
		"AGENTTEAMS_FS_ENDPOINT":          "http://fs.example.com:9000",
		"AGENTTEAMS_FS_BUCKET":            "agentteams-fs",
		"AGENTTEAMS_STORAGE_PREFIX":       "teams/demo",
		"AGENTTEAMS_FS_ACCESS_KEY":        "root",
		"AGENTTEAMS_FS_SECRET_KEY":        "secret",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for _, legacyKey := range []string{
		"HIGRESS_BASE_URL",
		"AGENTTEAMS_MINIO_ENDPOINT",
		"AGENTTEAMS_MINIO_BUCKET",
		"AGENTTEAMS_OSS_BUCKET",
		"AGENTTEAMS_HIGRESS_ADMIN_USER",
		"AGENTTEAMS_HIGRESS_ADMIN_PASSWORD",
	} {
		if _, ok := env[legacyKey]; ok {
			t.Fatalf("unexpected legacy env %s in ManagerAgentEnv", legacyKey)
		}
	}
}

func TestAppserviceConfigPrefersMatrixURL(t *testing.T) {
	// Cross-cluster ("半托管") setup: AGENTTEAMS_CONTROLLER_URL is the
	// externally-routable domain advertised to remote Workers, while
	// AGENTTEAMS_CONTROLLER_URL_MATRIX is the in-cluster Service DNS Tuwunel
	// uses to push appservice transactions. AppserviceConfig().URL must
	// pick the Matrix-side value, not the worker-facing one.
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.external.example.com")
	t.Setenv("AGENTTEAMS_CONTROLLER_URL_MATRIX", "http://controller.hiclaw.svc.cluster.local:8090")

	cfg := LoadConfig()
	as := cfg.AppserviceConfig()

	if got, want := as.URL, "http://controller.hiclaw.svc.cluster.local:8090"; got != want {
		t.Fatalf("AppserviceConfig().URL = %q, want %q (matrix-specific URL must win)", got, want)
	}
}

func TestAppserviceConfigFallsBackToControllerURL(t *testing.T) {
	// Single-cluster default: only AGENTTEAMS_CONTROLLER_URL is set.
	// AppserviceConfig().URL must fall back to it so existing deployments
	// keep working without setting the new variable.
	t.Setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.hiclaw.svc.cluster.local:8090")

	cfg := LoadConfig()
	as := cfg.AppserviceConfig()

	if got, want := as.URL, "http://controller.hiclaw.svc.cluster.local:8090"; got != want {
		t.Fatalf("AppserviceConfig().URL = %q, want %q (must fall back to ControllerURL)", got, want)
	}
}

func TestAppserviceConfigFallsBackToHTTPAddrWhenNoURLSet(t *testing.T) {
	// Neither URL is set (embedded / local dev): AppserviceConfig().URL
	// must fall back to the literal HTTPAddr so the controller can still
	// register with a colocated homeserver.
	t.Setenv("AGENTTEAMS_HTTP_ADDR", ":18090")

	cfg := LoadConfig()
	as := cfg.AppserviceConfig()

	if got, want := as.URL, "http://127.0.0.1:18090"; got != want {
		t.Fatalf("AppserviceConfig().URL = %q, want %q (must fall back to 127.0.0.1+HTTPAddr)", got, want)
	}
}

func TestLoadConfigDisablesAutoPrefix(t *testing.T) {
	t.Setenv("AGENTTEAMS_RESOURCE_AUTOPREFIX", "false")
	t.Setenv("AGENTTEAMS_RESOURCE_PREFIX", "hiclaw-")

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
	t.Setenv("AGENTTEAMS_RESOURCE_AUTOPREFIX", "false")
	t.Setenv("AGENTTEAMS_PROXY_CONTAINER_PREFIX", "custom-worker-")

	cfg := LoadConfig()

	if cfg.ContainerPrefix != "custom-worker-" {
		t.Fatalf("ContainerPrefix = %q, want %q", cfg.ContainerPrefix, "custom-worker-")
	}
}
