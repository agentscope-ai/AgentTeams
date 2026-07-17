package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/credentials"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
)

func (c *Config) Namespace() string {
	if c.K8sNamespace != "" {
		return c.K8sNamespace
	}
	return "default"
}

// HasMinIOAdmin reports whether the local MinIO admin API is available.
func (c *Config) HasMinIOAdmin() bool {
	return c.WorkerEnv.FSEndpoint != ""
}

// CredsDir returns the directory for persisted worker credentials (embedded mode).
func (c *Config) CredsDir() string {
	return envOrDefault("AGENTTEAMS_CREDS_DIR", "/data/worker-creds")
}

// AgentFSDir returns the local filesystem root for agent workspaces.
func (c *Config) AgentFSDir() string {
	return envOrDefault("AGENTTEAMS_AGENT_FS_DIR", "/root/hiclaw-fs/agents")
}

// ManagerStateFile returns the path to the Manager Agent's task-tracking
// state.json (embedded mode), defaulting to "<AgentFSDir>/manager/state.json".
// AGENTTEAMS_MANAGER_STATE_FILE overrides the default for testing/non-standard
// layouts.
func (c *Config) ManagerStateFile() string {
	return envOrDefault("AGENTTEAMS_MANAGER_STATE_FILE", filepath.Join(c.AgentFSDir(), "manager", "state.json"))
}

// WorkerAgentDir returns the source directory for builtin worker agent files.
func (c *Config) WorkerAgentDir() string {
	return envOrDefault("AGENTTEAMS_WORKER_AGENT_DIR", "/opt/hiclaw/agent/worker-agent")
}

// ManagerConfigPath returns the path to the Manager Agent's openclaw.json (embedded mode).
func (c *Config) ManagerConfigPath() string {
	return envOrDefault("AGENTTEAMS_MANAGER_CONFIG_PATH", "/root/openclaw.json")
}

// RegistryPath returns the path to the workers-registry.json (embedded mode).
func (c *Config) RegistryPath() string {
	return envOrDefault("AGENTTEAMS_REGISTRY_PATH", "/root/workers-registry.json")
}

// ManagerResources returns the resource requirements for the Manager Pod.
func (c *Config) ManagerResources() *backend.ResourceRequirements {
	return &backend.ResourceRequirements{
		CPURequest:    c.K8sManagerCPURequest,
		CPULimit:      c.K8sManagerCPU,
		MemoryRequest: c.K8sManagerMemoryRequest,
		MemoryLimit:   c.K8sManagerMemory,
	}
}
func (c *Config) DockerConfig() backend.DockerConfig {
	return backend.DockerConfig{
		SocketPath:           c.SocketPath,
		WorkerImage:          envOrDefault("AGENTTEAMS_WORKER_IMAGE", "agentteams/agentteams-worker:latest"),
		CopawWorkerImage:     envOrDefault("AGENTTEAMS_COPAW_WORKER_IMAGE", "agentteams/agentteams-copaw-worker:latest"),
		HermesWorkerImage:    envOrDefault("AGENTTEAMS_HERMES_WORKER_IMAGE", "agentteams/agentteams-hermes-worker:latest"),
		OpenHumanWorkerImage: envOrDefault("AGENTTEAMS_OPENHUMAN_WORKER_IMAGE", "agentteams/agentteams-openhuman-worker:latest"),
		QwenPawWorkerImage:   envOrDefault("AGENTTEAMS_QWENPAW_WORKER_IMAGE", "agentteams/agentteams-qwenpaw-worker:latest"),
		DefaultNetwork:       envOrDefault("AGENTTEAMS_DOCKER_NETWORK", "agentteams-net"),
	}
}
func (c *Config) STSConfig() credentials.STSConfig {
	return credentials.STSConfig{
		OSSBucket:   c.OSSBucket,
		OSSEndpoint: firstNonEmpty(os.Getenv("AGENTTEAMS_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint),
	}
}

// AIGatewayConfig returns the gateway.AIGatewayConfig used when
// GatewayProvider == "ai-gateway".
func (c *Config) AIGatewayConfig() gateway.AIGatewayConfig {
	return gateway.AIGatewayConfig{
		Region:     c.Region,
		Endpoint:   c.GWEndpoint,
		GatewayID:  c.GWGatewayID,
		ModelAPIID: c.GWModelAPIID,
		EnvID:      c.GWEnvID,
	}
}

// UsesAIGateway reports whether the controller should wire the AI Gateway
// (APIG) implementation of gateway.Client.
func (c *Config) UsesAIGateway() bool {
	return c.GatewayProvider == "ai-gateway"
}

// UsesExternalOSS reports whether the controller should talk to Alibaba
// Cloud OSS (existing bucket) instead of an embedded MinIO.
func (c *Config) UsesExternalOSS() bool {
	return c.StorageProvider == "oss"
}
func (c *Config) K8sConfig() backend.K8sConfig {
	return backend.K8sConfig{
		Namespace:            c.K8sNamespace,
		WorkerImage:          envOrDefault("AGENTTEAMS_WORKER_IMAGE", "agentteams/agentteams-worker:latest"),
		CopawWorkerImage:     envOrDefault("AGENTTEAMS_COPAW_WORKER_IMAGE", "agentteams/agentteams-copaw-worker:latest"),
		HermesWorkerImage:    envOrDefault("AGENTTEAMS_HERMES_WORKER_IMAGE", "agentteams/agentteams-hermes-worker:latest"),
		OpenHumanWorkerImage: envOrDefault("AGENTTEAMS_OPENHUMAN_WORKER_IMAGE", "agentteams/agentteams-openhuman-worker:latest"),
		QwenPawWorkerImage:   envOrDefault("AGENTTEAMS_QWENPAW_WORKER_IMAGE", "agentteams/agentteams-qwenpaw-worker:latest"),
		WorkerCPU:            c.K8sWorkerCPU,
		WorkerMemory:         c.K8sWorkerMemory,
		ControllerName:       c.ControllerName,
		ResourcePrefix:       c.ResourcePrefix,
	}
}
func (c *Config) SandboxConfig() backend.SandboxConfig {
	return backend.SandboxConfig{
		Namespace:                    c.K8sNamespace,
		ProviderType:                 c.SandboxProviderType,
		AgentRuntimeImage:            os.Getenv("AGENTTEAMS_SANDBOX_AGENT_RUNTIME_IMAGE"),
		WorkerImage:                  envOrDefault("AGENTTEAMS_WORKER_IMAGE", "agentteams/agentteams-worker:latest"),
		CopawWorkerImage:             envOrDefault("AGENTTEAMS_COPAW_WORKER_IMAGE", "agentteams/agentteams-copaw-worker:latest"),
		HermesWorkerImage:            envOrDefault("AGENTTEAMS_HERMES_WORKER_IMAGE", "agentteams/agentteams-hermes-worker:latest"),
		OpenHumanWorkerImage:         envOrDefault("AGENTTEAMS_OPENHUMAN_WORKER_IMAGE", "agentteams/agentteams-openhuman-worker:latest"),
		QwenPawWorkerImage:           envOrDefault("AGENTTEAMS_QWENPAW_WORKER_IMAGE", "agentteams/agentteams-qwenpaw-worker:latest"),
		WorkerCPU:                    c.K8sWorkerCPU,
		WorkerMemory:                 c.K8sWorkerMemory,
		SandboxPrewarmSize:           c.SandboxPrewarmSize,
		SandboxPrewarmSizeConfigured: c.SandboxPrewarmSizeConfigured,
		ControllerName:               c.ControllerName,
		ResourcePrefix:               c.ResourcePrefix,
	}
}
func (c *Config) MatrixConfig() matrix.Config {
	return matrix.Config{
		ServerURL:                    c.MatrixServerURL,
		Domain:                       c.MatrixDomain,
		RegistrationToken:            c.MatrixRegistrationToken,
		AdminUser:                    c.MatrixAdminUser,
		AdminPassword:                c.MatrixAdminPassword,
		E2EEEnabled:                  c.MatrixE2EE,
		AppServiceEnabled:            c.MatrixAppServiceEnabled,
		AppServiceID:                 c.MatrixAppServiceID,
		AppServiceToken:              c.MatrixAppServiceASToken,
		AppServiceHSToken:            c.MatrixAppServiceHSToken,
		AppServiceSenderLocalpart:    c.MatrixAppServiceSenderLocalpart,
		AppServiceUserNamespaceRegex: c.MatrixAppServiceUserNamespaceRegex,
		AppServicePushURL:            c.MatrixAppServicePushURL,
	}
}
func appServicePushURL(controllerURL string) string {
	controllerURL = strings.TrimRight(strings.TrimSpace(controllerURL), "/")
	if controllerURL == "" {
		return ""
	}
	return controllerURL
}
func (c *Config) GatewayConfig() gateway.Config {
	return gateway.Config{
		ConsoleURL:                c.HigressBaseURL,
		AdminUser:                 c.HigressAdminUser,
		AdminPassword:             c.HigressAdminPassword,
		AllowDefaultAdminFallback: c.KubeMode == "embedded",
		DataPlaneURL:              c.WorkerEnv.AIGatewayURL,
	}
}
func (c *Config) OSSConfig() oss.Config {
	accessKey := firstNonEmpty(os.Getenv("AGENTTEAMS_FS_ACCESS_KEY"), os.Getenv("AGENTTEAMS_MINIO_USER"))
	secretKey := firstNonEmpty(os.Getenv("AGENTTEAMS_FS_SECRET_KEY"), os.Getenv("AGENTTEAMS_MINIO_PASSWORD"))
	endpoint := firstNonEmpty(os.Getenv("AGENTTEAMS_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint)
	return oss.Config{
		StoragePrefix: c.OSSStoragePrefix,
		Bucket:        c.OSSBucket,
		Endpoint:      normalizeMinIOS3Endpoint(endpoint),
		AccessKey:     accessKey,
		SecretKey:     secretKey,
	}
}

// ManagerAgentEnv returns environment variables that a standalone Manager Agent
// container needs to connect to the infrastructure services in the embedded
// controller container. These are passed via DockerBackend.Create.
func (c *Config) ManagerAgentEnv() map[string]string {
	env := map[string]string{}
	setIfNonEmpty := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	setIfNonEmpty("AGENTTEAMS_MINIO_USER", os.Getenv("AGENTTEAMS_MINIO_USER"))
	setIfNonEmpty("AGENTTEAMS_MINIO_PASSWORD", os.Getenv("AGENTTEAMS_MINIO_PASSWORD"))
	setIfNonEmpty("AGENTTEAMS_ADMIN_USER", c.MatrixAdminUser)
	setIfNonEmpty("AGENTTEAMS_ADMIN_PASSWORD", c.MatrixAdminPassword)
	setIfNonEmpty("AGENTTEAMS_REGISTRATION_TOKEN", c.MatrixRegistrationToken)
	setIfNonEmpty("AGENTTEAMS_AI_GATEWAY_ADMIN_URL", c.HigressBaseURL)
	setIfNonEmpty("AGENTTEAMS_MATRIX_URL", c.WorkerEnv.MatrixURL)
	setIfNonEmpty("AGENTTEAMS_AI_GATEWAY_URL", c.WorkerEnv.AIGatewayURL)
	setIfNonEmpty("AGENTTEAMS_FS_ENDPOINT", c.WorkerEnv.FSEndpoint)
	setIfNonEmpty("AGENTTEAMS_FS_BUCKET", c.WorkerEnv.FSBucket)
	setIfNonEmpty("AGENTTEAMS_FS_ACCESS_KEY", firstNonEmpty(os.Getenv("AGENTTEAMS_FS_ACCESS_KEY"), os.Getenv("AGENTTEAMS_MINIO_USER")))
	setIfNonEmpty("AGENTTEAMS_FS_SECRET_KEY", firstNonEmpty(os.Getenv("AGENTTEAMS_FS_SECRET_KEY"), os.Getenv("AGENTTEAMS_MINIO_PASSWORD")))
	setIfNonEmpty("AGENTTEAMS_STORAGE_PREFIX", c.OSSStoragePrefix)
	setIfNonEmpty("AGENTTEAMS_MATRIX_DOMAIN", c.MatrixDomain)
	setIfNonEmpty("AGENTTEAMS_DEFAULT_MODEL", c.DefaultModel)
	setIfNonEmpty("AGENTTEAMS_EMBEDDING_MODEL", c.EmbeddingModel)
	setIfNonEmpty("AGENTTEAMS_LLM_PROVIDER", c.LLMProvider)
	setIfNonEmpty("AGENTTEAMS_LLM_API_KEY", c.LLMAPIKey)
	if c.AIStreamIdleTimeoutSeconds > 0 {
		env["AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS"] = strconv.Itoa(c.AIStreamIdleTimeoutSeconds)
	}
	setIfNonEmpty("AGENTTEAMS_ELEMENT_WEB_URL", c.ElementWebURL)
	if c.MatrixE2EE {
		env["AGENTTEAMS_MATRIX_E2EE"] = "1"
	}
	if c.WorkerEnv.MatrixDebug {
		env["AGENTTEAMS_MATRIX_DEBUG"] = "1"
	}
	if c.CMSTracesEnabled {
		env["AGENTTEAMS_CMS_TRACES_ENABLED"] = "1"
	}
	if c.CMSMetricsEnabled {
		env["AGENTTEAMS_CMS_METRICS_ENABLED"] = "1"
	}
	setIfNonEmpty("AGENTTEAMS_CMS_ENDPOINT", c.CMSEndpoint)
	setIfNonEmpty("AGENTTEAMS_CMS_LICENSE_KEY", c.CMSLicenseKey)
	setIfNonEmpty("AGENTTEAMS_CMS_PROJECT", c.CMSProject)
	setIfNonEmpty("AGENTTEAMS_CMS_WORKSPACE", c.CMSWorkspace)
	setIfNonEmpty("AGENTTEAMS_CMS_SERVICE_NAME", c.CMSServiceName)
	return env
}
func (c *Config) AgentConfig() agentconfig.Config {
	// Use WorkerEnv URLs (host-replaced in embedded mode) since openclaw.json
	// is consumed by worker containers, not the controller itself.
	matrixURL := c.MatrixServerURL
	aiGatewayURL := envOrDefault("AGENTTEAMS_AI_GATEWAY_URL", "http://aigw-local.agentteams.io:8080")
	if c.KubeMode == "embedded" {
		if c.WorkerEnv.MatrixURL != "" {
			matrixURL = c.WorkerEnv.MatrixURL
		}
		if c.WorkerEnv.AIGatewayURL != "" {
			aiGatewayURL = c.WorkerEnv.AIGatewayURL
		}
	}
	return agentconfig.Config{
		MatrixDomain:       c.MatrixDomain,
		MatrixServerURL:    matrixURL,
		AIGatewayURL:       aiGatewayURL,
		AdminUser:          c.MatrixAdminUser,
		DefaultModel:       c.DefaultModel,
		EmbeddingModel:     c.EmbeddingModel,
		Runtime:            c.Runtime,
		E2EEEnabled:        c.MatrixE2EE,
		ModelContextWindow: c.ModelContextWindow,
		ModelMaxTokens:     c.ModelMaxTokens,
		CMSTracesEnabled:   c.CMSTracesEnabled,
		CMSMetricsEnabled:  c.CMSMetricsEnabled,
		CMSEndpoint:        c.CMSEndpoint,
		CMSLicenseKey:      c.CMSLicenseKey,
		CMSProject:         c.CMSProject,
		CMSWorkspace:       c.CMSWorkspace,
		CMSServiceName:     c.CMSServiceName,
	}
}
