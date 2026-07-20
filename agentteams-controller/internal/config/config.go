package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credentials"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type Config struct {
	// Controller core
	KubeMode        string // "embedded" or "incluster"
	DataDir         string
	HTTPAddr        string
	MetricsBindAddr string
	ConfigDir       string
	CRDDir          string
	SkillsDir       string

	// ResourcePrefix is the tenant-level prefix used to derive Pod/SA/label/
	// session names created by this controller. Default "hiclaw-". Set via
	// AGENTTEAMS_RESOURCE_PREFIX to isolate multiple AgentTeams instances that share
	// a K8s namespace (different Helm releases). Downstream names are all
	// derived from this value — see internal/auth.ResourcePrefix for the
	// full list (worker/manager pods, ServiceAccounts, "app" labels, STS
	// session names). Intentionally does NOT cover OPENCLAW_MDNS_HOSTNAME,
	// CMS service name, or install-script hardcoded names.
	ResourcePrefix string
	// ResourceAutoPrefix controls whether controller should auto-derive
	// resource/container prefixes. When false, default hiclaw-* prefixes are
	// disabled unless explicit AGENTTEAMS_PROXY_CONTAINER_PREFIX is provided.
	// Set via AGENTTEAMS_RESOURCE_AUTOPREFIX. Default true.
	ResourceAutoPrefix bool

	// Docker proxy (embedded mode only)
	SocketPath      string
	ContainerPrefix string // worker container/pod name prefix; derived from ResourcePrefix when AGENTTEAMS_PROXY_CONTAINER_PREFIX is unset

	// Auth
	AuthAudience               string // SA token audience for TokenReview
	AuthTokenExpirationSeconds int64

	// Provider selection (driven by Helm values)
	GatewayProvider string // "higress" | "ai-gateway"
	StorageProvider string // "minio"   | "oss"

	// Higress (self-hosted gateway)
	HigressBaseURL       string
	HigressCookieFile    string
	HigressAdminUser     string
	HigressAdminPassword string

	// Worker backend selection
	WorkerBackend        string
	WorkerBackendRuntime string

	// Region (used by AI Gateway / OSS, etc.)
	Region string

	// AI Gateway (Alibaba Cloud APIG) — only used when GatewayProvider == "ai-gateway"
	GWEndpoint   string
	GWGatewayID  string
	GWModelAPIID string
	GWEnvID      string

	// Object storage bucket (shared by minio and oss backends)
	OSSBucket string

	WorkerDepsStorageBucket   string
	WorkerDepsStorageEndpoint string
	WorkerDepsMountAuthType   string
	WorkerDepsMountRoleName   string

	// Credential provider sidecar (hiclaw-credential-provider) used by the
	// controller to obtain STS tokens for its own cloud SDK clients (APIG,
	// OSS) and for downstream worker credential issuance. Empty when the
	// sidecar is not deployed (e.g. self-hosted higress+minio stack).
	CredentialProviderURL string

	// Kubernetes Backend
	K8sNamespace    string
	K8sWorkerCPU    string
	K8sWorkerMemory string

	// Legacy sandbox backend knobs. The open-source controller does not
	// register the OpenKruise sandbox backend.
	SandboxProviderType          string
	SandboxCapabilities          string
	SandboxPrewarmSize           int
	SandboxPrewarmSizeConfigured bool

	// Manager deployment (Initializer creates the Manager CR if enabled)
	ManagerEnabled          bool
	ManagerModel            string
	ManagerRuntime          string
	ManagerImage            string
	ManagerSpecResources    *v1beta1.AgentResourceRequirements
	K8sManagerCPURequest    string
	K8sManagerMemoryRequest string
	K8sManagerCPU           string
	K8sManagerMemory        string

	// DefaultWorkerRuntime is applied by the Worker reconciler when a Worker
	// CR has spec.runtime unset, before falling back to "openclaw". Sourced
	// from AGENTTEAMS_DEFAULT_WORKER_RUNTIME at install time. Manager pods use
	// ManagerRuntime instead, since Backend.Create is shared between both
	// and only the caller knows which env var applies.
	DefaultWorkerRuntime string

	// Controller URL (advertised to workers for STS refresh etc.)
	ControllerURL string

	// ControllerName identifies this controller instance. When multiple
	// agentteams-controller deployments live in the same namespace (e.g. separate
	// Helm releases), each must use a distinct LeaderElection lease to avoid
	// one instance blocking the other. Sourced from AGENTTEAMS_CONTROLLER_NAME;
	// if empty, leader election falls back to the legacy global lease name.
	ControllerName string

	// Embedded-mode Manager Agent container mounts (host paths, read from env)
	ManagerWorkspaceDir string // e.g. ~/agentteams-manager — mounted as /root/manager-workspace
	HostShareDir        string // e.g. ~/ — mounted as /host-share
	ManagerConsolePort  string // host port for manager console (default: 18888)

	// Pre-generated Manager secrets (from install script env)
	ManagerPassword   string // Matrix password for manager user
	ManagerGatewayKey string // Gateway API key for manager consumer

	// Matrix server
	MatrixServerURL         string
	MatrixDomain            string
	MatrixRegistrationToken string
	MatrixAdminUser         string
	MatrixAdminPassword     string
	MatrixE2EE              bool

	// Matrix AppService mode
	MatrixAppServiceEnabled            bool
	MatrixAppServiceID                 string
	MatrixAppServiceASToken            string
	MatrixAppServiceHSToken            string
	MatrixAppServiceSenderLocalpart    string
	MatrixAppServiceUserNamespaceRegex string
	MatrixAppServicePushURL            string

	// Auto-generation tracking (not exported to env / child containers)
	MatrixAppServiceASTokenAutoGenerated bool `json:"-"`
	MatrixAppServiceHSTokenAutoGenerated bool `json:"-"`

	// Object storage (embedded MinIO)
	OSSStoragePrefix string

	// AI model
	DefaultModel       string
	EmbeddingModel     string
	Runtime            string
	ModelContextWindow int
	ModelMaxTokens     int

	// LLM provider (for Gateway initialization)
	LLMProvider                string
	LLMAPIKey                  string
	OpenAIBaseURL              string // AGENTTEAMS_OPENAI_BASE_URL — custom base URL for openai-compat providers
	AIStreamIdleTimeoutSeconds int    // AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS

	// Element Web URL (for Gateway route initialization)
	ElementWebURL string

	// Locale used to render the first-boot Manager onboarding prompt
	// (welcome message). Sourced from the install-time AGENTTEAMS_LANGUAGE
	// (zh / en) and TZ env vars that the install script forwards into
	// the controller container. Both are advisory hints — the controller
	// only embeds them as plain text in the welcome prompt; the agent
	// itself decides how to interpret them when greeting the admin.
	UserLanguage string
	UserTimezone string

	// CMS observability
	CMSTracesEnabled  bool
	CMSMetricsEnabled bool
	CMSEndpoint       string
	CMSLicenseKey     string
	CMSProject        string
	CMSWorkspace      string
	CMSServiceName    string

	// Pre-resolved worker environment defaults (passed to worker containers)
	WorkerEnv WorkerEnvDefaults
}

// WorkerEnvDefaults holds environment variable defaults injected into worker and manager containers.
// All values are resolved once at config load time from the controller's own environment.
type WorkerEnvDefaults struct {
	MatrixDomain         string
	FSEndpoint           string
	FSBucket             string
	StoragePrefix        string
	ControllerURL        string
	AIGatewayURL         string
	MatrixURL            string
	AdminUser            string
	Runtime              string // "docker" for embedded, "k8s" for incluster
	DefaultWorkerRuntime string
	YoloMode             bool // AGENTTEAMS_YOLO=1 — propagated to managers and workers
	MatrixDebug          bool // AGENTTEAMS_MATRIX_DEBUG=1 — propagated to managers and workers,
	// translated to OPENCLAW_MATRIX_DEBUG=1 by the container entrypoints to
	// enable structured INFO-level traces in the openclaw matrix plugin.

	// CMS observability (propagated to all workers and managers)
	CMSTracesEnabled  bool
	CMSMetricsEnabled bool
	CMSEndpoint       string
	CMSLicenseKey     string
	CMSProject        string
	CMSWorkspace      string

	// SkillsAPIURL is propagated to workers as SKILLS_API_URL.
	// Sourced from SKILLS_API_URL, falling back to AGENTTEAMS_SKILLS_API_URL.
	SkillsAPIURL string

	// NacosAuthType is propagated to workers as NACOS_AUTH_TYPE.
	// Sourced from NACOS_AUTH_TYPE.
	// Typical value: "sts-hiclaw".
	NacosAuthType string
}

type managerSpecEnv struct {
	Model     string                            `json:"model"`
	Runtime   string                            `json:"runtime"`
	Image     string                            `json:"image"`
	Resources v1beta1.AgentResourceRequirements `json:"resources"`
}

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
		ConfigDir:       envOrDefault("AGENTTEAMS_CONFIG_DIR", "/root/agentteams-fs/agentteams-config"),
		CRDDir:          envOrDefault("AGENTTEAMS_CRD_DIR", "/opt/agentteams/config/crd"),
		SkillsDir:       envOrDefault("AGENTTEAMS_SKILLS_DIR", "/opt/agentteams/agent/skills"),

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
	return envOrDefault("AGENTTEAMS_AGENT_FS_DIR", "/root/agentteams-fs/agents")
}

// WorkerAgentDir returns the source directory for builtin worker agent files.
func (c *Config) WorkerAgentDir() string {
	return envOrDefault("AGENTTEAMS_WORKER_AGENT_DIR", "/opt/agentteams/agent/worker-agent")
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
		WorkerCPU:                    c.K8sWorkerCPU,
		WorkerMemory:                 c.K8sWorkerMemory,
		SandboxPrewarmSize:           c.SandboxPrewarmSize,
		SandboxPrewarmSizeConfigured: c.SandboxPrewarmSizeConfigured,
		ControllerName:               c.ControllerName,
		ResourcePrefix:               c.ResourcePrefix,
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// generateRandomHex returns a cryptographically random hex string of n bytes (2n hex chars).
func generateRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
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
