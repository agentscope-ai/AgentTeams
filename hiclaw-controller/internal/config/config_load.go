package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
)

func LoadConfig() *Config {
	kubeMode := envOrDefault("AGENTTEAMS_KUBE_MODE", "embedded")
	metricsBindAddr := os.Getenv("AGENTTEAMS_METRICS_BIND_ADDR")
	if metricsBindAddr == "" {
		if kubeMode == "embedded" {
			metricsBindAddr = "0"
		} else {
			metricsBindAddr = ":8080"
		}
	}

	dataDir := envOrDefault("AGENTTEAMS_DATA_DIR", "/data/agentteams-controller")
	if !filepath.IsAbs(dataDir) {
		if wd, err := os.Getwd(); err == nil {
			dataDir = filepath.Join(wd, dataDir)
		}
	}

	resourceAutoPrefix := envBoolDefault("AGENTTEAMS_RESOURCE_AUTOPREFIX", true)
	resourcePrefix := ""
	if resourceAutoPrefix {
		resourcePrefix = envOrDefault("AGENTTEAMS_RESOURCE_PREFIX", "agentteams-")
	}
	// ContainerPrefix defaults to "${resourcePrefix}worker-" when auto-prefix
	// is enabled. AGENTTEAMS_PROXY_CONTAINER_PREFIX remains an explicit override.
	containerPrefix := os.Getenv("AGENTTEAMS_PROXY_CONTAINER_PREFIX")
	if containerPrefix == "" && resourceAutoPrefix {
		containerPrefix = resourcePrefix + "worker-"
	}

	cfg := &Config{
		KubeMode:        kubeMode,
		DataDir:         dataDir,
		HTTPAddr:        envOrDefault("AGENTTEAMS_HTTP_ADDR", ":8090"),
		MetricsBindAddr: metricsBindAddr,
		ConfigDir:       envOrDefault("AGENTTEAMS_CONFIG_DIR", "/root/hiclaw-fs/agentteams-config"),
		CRDDir:          envOrDefault("AGENTTEAMS_CRD_DIR", "/opt/hiclaw/config/crd"),
		SkillsDir:       envOrDefault("AGENTTEAMS_SKILLS_DIR", "/opt/hiclaw/agent/skills"),

		ResourcePrefix:     resourcePrefix,
		ResourceAutoPrefix: resourceAutoPrefix,

		SocketPath:      envOrDefault("AGENTTEAMS_PROXY_SOCKET", "/var/run/docker.sock"),
		ContainerPrefix: containerPrefix,

		AuthAudience: firstNonEmpty(
			os.Getenv("AGENTTEAMS_AUTH_AUDIENCE"),
			envOrDefault("AGENTTEAMS_AUTH_AUDIENCE", "agentteams-controller"),
		),
		AuthTokenExpirationSeconds: int64(envOrDefaultInt("AGENTTEAMS_AUTH_TOKEN_EXPIRATION_SECONDS", int(backend.DefaultAuthTokenExpirationSeconds))),

		GatewayProvider: envOrDefault("AGENTTEAMS_GATEWAY_PROVIDER", "higress"),
		StorageProvider: envOrDefault("AGENTTEAMS_STORAGE_PROVIDER", "minio"),

		CredentialProviderURL: os.Getenv("AGENTTEAMS_CREDENTIAL_PROVIDER_URL"),

		HigressBaseURL:    envOrDefault("AGENTTEAMS_AI_GATEWAY_ADMIN_URL", "http://127.0.0.1:8001"),
		HigressCookieFile: os.Getenv("HIGRESS_COOKIE_FILE"),
		// Higress and Matrix share the same admin credentials.
		HigressAdminUser:     os.Getenv("AGENTTEAMS_ADMIN_USER"),
		HigressAdminPassword: os.Getenv("AGENTTEAMS_ADMIN_PASSWORD"),

		WorkerBackend: firstNonEmpty(
			os.Getenv("AGENTTEAMS_WORKER_BACKEND"),
			os.Getenv("AGENTTEAMS_ALIYUN_WORKER_BACKEND"),
		),
		WorkerBackendRuntime: os.Getenv("AGENTTEAMS_WORKER_BACKEND_RUNTIME"),

		Region: envOrDefault("AGENTTEAMS_REGION", "cn-hangzhou"),

		GWEndpoint:   os.Getenv("AGENTTEAMS_APIG_ENDPOINT"),
		GWGatewayID:  os.Getenv("AGENTTEAMS_GW_GATEWAY_ID"),
		GWModelAPIID: os.Getenv("AGENTTEAMS_GW_MODEL_API_ID"),
		GWEnvID:      os.Getenv("AGENTTEAMS_GW_ENV_ID"),

		OSSBucket: envOrDefault("AGENTTEAMS_FS_BUCKET", "agentteams-storage"),
		WorkerDepsStorageBucket: firstNonEmpty(
			os.Getenv("AGENTTEAMS_WORKER_DEPS_STORAGE_BUCKET"),
			os.Getenv("AGENTTEAMS_FS_BUCKET"),
			os.Getenv("AGENTTEAMS_FS_BUCKET"),
			"agentteams-storage",
		),
		WorkerDepsStorageEndpoint: firstNonEmpty(
			os.Getenv("AGENTTEAMS_WORKER_DEPS_STORAGE_ENDPOINT"),
			os.Getenv("AGENTTEAMS_FS_ENDPOINT"),
			os.Getenv("AGENTTEAMS_FS_ENDPOINT"),
		),
		WorkerDepsMountAuthType: envOrDefault("AGENTTEAMS_MOUNT_AUTH_TYPE", "RRSA"),
		WorkerDepsMountRoleName: os.Getenv("AGENTTEAMS_MOUNT_ROLE_NAME"),

		K8sNamespace:    os.Getenv("AGENTTEAMS_K8S_NAMESPACE"),
		K8sWorkerCPU:    envOrDefault("AGENTTEAMS_K8S_WORKER_CPU", "1000m"),
		K8sWorkerMemory: envOrDefault("AGENTTEAMS_K8S_WORKER_MEMORY", "2Gi"),

		SandboxProviderType:          envOrDefault("AGENTTEAMS_SANDBOX_PROVIDER_TYPE", "openkruise"),
		SandboxCapabilities:          os.Getenv("AGENTTEAMS_SANDBOX_CAPABILITIES"),
		SandboxPrewarmSize:           envOrDefaultInt("AGENTTEAMS_SANDBOX_PREWARM_SIZE", backend.DefaultSandboxPrewarmSize),
		SandboxPrewarmSizeConfigured: os.Getenv("AGENTTEAMS_SANDBOX_PREWARM_SIZE") != "",

		ManagerEnabled:          envOrDefault("AGENTTEAMS_MANAGER_ENABLED", "true") == "true",
		ManagerModel:            firstNonEmpty(os.Getenv("AGENTTEAMS_MANAGER_MODEL"), envOrDefault("AGENTTEAMS_DEFAULT_MODEL", "qwen3.6-plus")),
		ManagerRuntime:          envOrDefault("AGENTTEAMS_MANAGER_RUNTIME", "openclaw"),
		ManagerImage:            os.Getenv("AGENTTEAMS_MANAGER_IMAGE"),
		DefaultWorkerRuntime:    os.Getenv("AGENTTEAMS_DEFAULT_WORKER_RUNTIME"),
		K8sManagerCPURequest:    envOrDefault("AGENTTEAMS_K8S_MANAGER_CPU_REQUEST", "500m"),
		K8sManagerMemoryRequest: envOrDefault("AGENTTEAMS_K8S_MANAGER_MEMORY_REQUEST", "1Gi"),
		K8sManagerCPU:           envOrDefault("AGENTTEAMS_K8S_MANAGER_CPU", "2"),
		K8sManagerMemory:        envOrDefault("AGENTTEAMS_K8S_MANAGER_MEMORY", "4Gi"),

		ControllerURL:  os.Getenv("AGENTTEAMS_CONTROLLER_URL"),
		ControllerName: os.Getenv("AGENTTEAMS_CONTROLLER_NAME"),

		ManagerWorkspaceDir: os.Getenv("AGENTTEAMS_WORKSPACE_DIR"),
		HostShareDir:        os.Getenv("AGENTTEAMS_HOST_SHARE_DIR"),
		ManagerConsolePort:  envOrDefault("AGENTTEAMS_PORT_MANAGER_CONSOLE", "18888"),
		ManagerPassword:     os.Getenv("AGENTTEAMS_MANAGER_PASSWORD"),
		ManagerGatewayKey:   os.Getenv("AGENTTEAMS_MANAGER_GATEWAY_KEY"),

		MatrixServerURL:         envOrDefault("AGENTTEAMS_MATRIX_URL", "http://matrix-local.agentteams.io:8080"),
		MatrixDomain:            envOrDefault("AGENTTEAMS_MATRIX_DOMAIN", "matrix-local.agentteams.io:8080"),
		MatrixRegistrationToken: envOrDefault("AGENTTEAMS_MATRIX_REGISTRATION_TOKEN", os.Getenv("AGENTTEAMS_REGISTRATION_TOKEN")),
		MatrixAdminUser:         os.Getenv("AGENTTEAMS_ADMIN_USER"),
		MatrixAdminPassword:     os.Getenv("AGENTTEAMS_ADMIN_PASSWORD"),
		MatrixE2EE:              os.Getenv("AGENTTEAMS_MATRIX_E2EE") == "1" || os.Getenv("AGENTTEAMS_MATRIX_E2EE") == "true",

		MatrixAppServiceEnabled:            os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_ENABLED") != "0" && os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_ENABLED") != "false",
		MatrixAppServiceID:                 envOrDefault("AGENTTEAMS_MATRIX_APPSERVICE_ID", "agentteams-controller"),
		MatrixAppServiceASToken:            os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN"),
		MatrixAppServiceHSToken:            os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN"),
		MatrixAppServiceSenderLocalpart:    envOrDefault("AGENTTEAMS_MATRIX_APPSERVICE_SENDER_LOCALPART", "agentteams-controller"),
		MatrixAppServiceUserNamespaceRegex: os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX"),

		OSSStoragePrefix: envOrDefault("AGENTTEAMS_STORAGE_PREFIX", "agentteams/agentteams-storage"),

		DefaultModel:       envOrDefault("AGENTTEAMS_DEFAULT_MODEL", "qwen3.6-plus"),
		EmbeddingModel:     os.Getenv("AGENTTEAMS_EMBEDDING_MODEL"),
		Runtime:            envOrDefault("AGENTTEAMS_RUNTIME", "docker"),
		ModelContextWindow: envOrDefaultInt("AGENTTEAMS_MODEL_CONTEXT_WINDOW", 0),
		ModelMaxTokens:     envOrDefaultInt("AGENTTEAMS_MODEL_MAX_TOKENS", 0),

		LLMProvider:                envOrDefault("AGENTTEAMS_LLM_PROVIDER", "qwen"),
		LLMAPIKey:                  os.Getenv("AGENTTEAMS_LLM_API_KEY"),
		OpenAIBaseURL:              os.Getenv("AGENTTEAMS_OPENAI_BASE_URL"),
		AIStreamIdleTimeoutSeconds: envOrDefaultInt("AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS", 900),
		ElementWebURL:              os.Getenv("AGENTTEAMS_ELEMENT_WEB_URL"),

		UserLanguage: envOrDefault("AGENTTEAMS_LANGUAGE", "zh"),
		UserTimezone: envOrDefault("TZ", "Asia/Shanghai"),

		CMSTracesEnabled:  envBool("AGENTTEAMS_CMS_TRACES_ENABLED"),
		CMSMetricsEnabled: envBool("AGENTTEAMS_CMS_METRICS_ENABLED"),
		CMSEndpoint:       os.Getenv("AGENTTEAMS_CMS_ENDPOINT"),
		CMSLicenseKey:     os.Getenv("AGENTTEAMS_CMS_LICENSE_KEY"),
		CMSProject:        os.Getenv("AGENTTEAMS_CMS_PROJECT"),
		CMSWorkspace:      os.Getenv("AGENTTEAMS_CMS_WORKSPACE"),
		CMSServiceName:    envOrDefault("AGENTTEAMS_CMS_SERVICE_NAME", "agentteams-manager"),
		SoloOperator:      envBool("AGENTTEAMS_SOLO_OPERATOR"),

		WorkerEnv: WorkerEnvDefaults{
			MatrixDomain:         envOrDefault("AGENTTEAMS_MATRIX_DOMAIN", "matrix-local.agentteams.io:8080"),
			FSEndpoint:           os.Getenv("AGENTTEAMS_FS_ENDPOINT"),
			FSBucket:             envOrDefault("AGENTTEAMS_FS_BUCKET", "agentteams-storage"),
			StoragePrefix:        envOrDefault("AGENTTEAMS_STORAGE_PREFIX", "agentteams/agentteams-storage"),
			ControllerURL:        os.Getenv("AGENTTEAMS_CONTROLLER_URL"),
			AIGatewayURL:         envOrDefault("AGENTTEAMS_AI_GATEWAY_URL", "http://aigw-local.agentteams.io:8080"),
			MatrixURL:            envOrDefault("AGENTTEAMS_MATRIX_URL", "http://matrix-local.agentteams.io:8080"),
			AdminUser:            os.Getenv("AGENTTEAMS_ADMIN_USER"),
			DefaultWorkerRuntime: os.Getenv("AGENTTEAMS_DEFAULT_WORKER_RUNTIME"),
			YoloMode:             envBool("AGENTTEAMS_YOLO"),
			MatrixDebug:          envBool("AGENTTEAMS_MATRIX_DEBUG"),

			// CMS observability (propagated from controller env to all workers/managers)
			CMSTracesEnabled:  envBool("AGENTTEAMS_CMS_TRACES_ENABLED"),
			CMSMetricsEnabled: envBool("AGENTTEAMS_CMS_METRICS_ENABLED"),
			CMSEndpoint:       os.Getenv("AGENTTEAMS_CMS_ENDPOINT"),
			CMSLicenseKey:     os.Getenv("AGENTTEAMS_CMS_LICENSE_KEY"),
			CMSProject:        os.Getenv("AGENTTEAMS_CMS_PROJECT"),
			CMSWorkspace:      os.Getenv("AGENTTEAMS_CMS_WORKSPACE"),
			SkillsAPIURL:      envOrDefault("SKILLS_API_URL", os.Getenv("AGENTTEAMS_SKILLS_API_URL")),
			NacosAuthType:     os.Getenv("NACOS_AUTH_TYPE"),
		},
	}

	// In embedded mode, services (Tuwunel, MinIO) run inside the controller container.
	// The controller itself uses 127.0.0.1, but child containers (Manager, Workers) must
	// reach them via the controller's Docker network hostname.
	if cfg.KubeMode == "embedded" {
		if ctrlHost := extractHost(cfg.WorkerEnv.ControllerURL); ctrlHost != "" {
			cfg.WorkerEnv.MatrixURL = replaceHost(cfg.WorkerEnv.MatrixURL, ctrlHost)
			cfg.WorkerEnv.FSEndpoint = replaceHost(cfg.WorkerEnv.FSEndpoint, ctrlHost)
			cfg.WorkerEnv.AIGatewayURL = replaceHost(cfg.WorkerEnv.AIGatewayURL, ctrlHost)
		}
	}
	// S3/MinIO API is never on the Higress HTTP gateway port (8080). Misconfigured
	// AGENTTEAMS_FS_DOMAIN:8080 URLs are rewritten to the MinIO object port.
	cfg.WorkerEnv.FSEndpoint = normalizeMinIOS3Endpoint(cfg.WorkerEnv.FSEndpoint)

	if specJSON := os.Getenv("AGENTTEAMS_MANAGER_SPEC"); specJSON != "" {
		if err := applyManagerSpec(cfg, specJSON); err != nil {
			panic(fmt.Sprintf("invalid AGENTTEAMS_MANAGER_SPEC: %v", err))
		}
	}

	// Validate AppService tokens when AS mode is enabled.
	// Tokens must be provided via env vars (set by install script or manually).
	// We do NOT auto-generate at runtime to prevent token drift across restarts.
	if cfg.MatrixAppServiceEnabled {
		matrixControllerURL := firstNonEmpty(os.Getenv("AGENTTEAMS_MATRIX_APPSERVICE_CONTROLLER_URL"), cfg.ControllerURL)
		cfg.MatrixAppServicePushURL = appServicePushURL(matrixControllerURL)
		if cfg.MatrixAppServiceASToken == "" {
			panic("AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN is required when AppService mode is enabled; run install script or set env var")
		}
		if cfg.MatrixAppServiceHSToken == "" {
			panic("AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN is required when AppService mode is enabled; run install script or set env var")
		}
	}

	return cfg
}

// Namespace returns the effective K8s namespace, defaulting to "default".
func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
func envOrDefaultInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}
func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}
func envBoolDefault(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func applyManagerSpec(cfg *Config, specJSON string) error {
	var spec managerSpecEnv
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return err
	}

	if spec.Model != "" {
		cfg.ManagerModel = spec.Model
	}
	if spec.Runtime != "" {
		cfg.ManagerRuntime = spec.Runtime
	}
	if spec.Image != "" {
		cfg.ManagerImage = spec.Image
	}
	if !agentResourcesEmpty(spec.Resources) {
		resources := spec.Resources
		cfg.ManagerSpecResources = &resources
	}
	if spec.Resources.Requests.CPU != "" {
		cfg.K8sManagerCPURequest = spec.Resources.Requests.CPU
	}
	if spec.Resources.Requests.Memory != "" {
		cfg.K8sManagerMemoryRequest = spec.Resources.Requests.Memory
	}
	if spec.Resources.Limits.CPU != "" {
		cfg.K8sManagerCPU = spec.Resources.Limits.CPU
	}
	if spec.Resources.Limits.Memory != "" {
		cfg.K8sManagerMemory = spec.Resources.Limits.Memory
	}

	return nil
}
func agentResourcesEmpty(r v1beta1.AgentResourceRequirements) bool {
	return r.Requests.CPU == "" &&
		r.Requests.Memory == "" &&
		r.Limits.CPU == "" &&
		r.Limits.Memory == ""
}

// extractHost returns the hostname from a URL (e.g. "http://agentteams-controller:8090" → "agentteams-controller").
func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// replaceHost replaces the hostname in a URL while preserving scheme, port, and path.
func replaceHost(rawURL, newHost string) string {
	if rawURL == "" || newHost == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Port() != "" {
		u.Host = newHost + ":" + u.Port()
	} else {
		u.Host = newHost
	}
	return u.String()
}

// normalizeMinIOS3Endpoint rewrites a common misconfiguration: the S3/MinIO API
// is served on the object store port (9000 in AgentTeams), not the Higress HTTP
// gateway (8080). A URL like http://fs-local.agentteams.io:8080 breaks mc silently.
func normalizeMinIOS3Endpoint(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Port() != "8080" {
		return raw
	}
	hostname := u.Hostname()
	if hostname == "" {
		return raw
	}
	u.Host = hostname + ":9000"
	return u.String()
}
