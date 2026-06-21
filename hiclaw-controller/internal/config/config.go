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

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/credentials"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
)

type Config struct {
	// Controller core
	KubeMode  string // "embedded" or "incluster"
	DataDir   string
	HTTPAddr  string
	ConfigDir string
	CRDDir    string
	SkillsDir string

	// ResourcePrefix is the tenant-level prefix used to derive Pod/SA/label/
	// session names created by this controller. Default "hiclaw-". Set via
	// HICLAW_RESOURCE_PREFIX to isolate multiple HiClaw instances that share
	// a K8s namespace (different Helm releases). Downstream names are all
	// derived from this value — see internal/auth.ResourcePrefix for the
	// full list (worker/manager pods, ServiceAccounts, "app" labels, STS
	// session names). Intentionally does NOT cover OPENCLAW_MDNS_HOSTNAME,
	// CMS service name, or install-script hardcoded names.
	ResourcePrefix string
	// ResourceAutoPrefix controls whether controller should auto-derive
	// resource/container prefixes. When false, default hiclaw-* prefixes are
	// disabled unless explicit HICLAW_PROXY_CONTAINER_PREFIX is provided.
	// Set via HICLAW_RESOURCE_AUTOPREFIX. Default true.
	ResourceAutoPrefix bool

	// Docker proxy (embedded mode only)
	SocketPath      string
	ContainerPrefix string // worker container/pod name prefix; derived from ResourcePrefix when HICLAW_PROXY_CONTAINER_PREFIX is unset

	// Auth
	AuthAudience string // SA token audience for TokenReview

	// Provider selection (driven by Helm values)
	GatewayProvider string // "higress" | "ai-gateway"
	StorageProvider string // "minio"   | "oss"

	// Higress (self-hosted gateway)
	HigressBaseURL       string
	HigressCookieFile    string
	HigressAdminUser     string
	HigressAdminPassword string

	// Worker backend selection
	WorkerBackend string

	// Region (used by AI Gateway / OSS, etc.)
	Region string

	// AI Gateway (Alibaba Cloud APIG) — only used when GatewayProvider == "ai-gateway"
	GWGatewayID  string
	GWModelAPIID string
	GWEnvID      string

	// Object storage bucket (shared by minio and oss backends)
	OSSBucket string

	// Credential provider sidecar (hiclaw-credential-provider) used by the
	// controller to obtain STS tokens for its own cloud SDK clients (APIG,
	// OSS) and for downstream worker credential issuance. Empty when the
	// sidecar is not deployed (e.g. self-hosted higress+minio stack).
	CredentialProviderURL string

	// Kubernetes Backend
	K8sNamespace    string
	K8sWorkerCPU    string
	K8sWorkerMemory string

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
	// from HICLAW_DEFAULT_WORKER_RUNTIME at install time. Manager pods use
	// ManagerRuntime instead, since Backend.Create is shared between both
	// and only the caller knows which env var applies.
	DefaultWorkerRuntime string

	// Controller URL (advertised to workers for STS refresh etc.)
	ControllerURL string

	// ControllerName identifies this controller instance. When multiple
	// hiclaw-controller deployments live in the same namespace (e.g. separate
	// Helm releases), each must use a distinct LeaderElection lease to avoid
	// one instance blocking the other. Sourced from HICLAW_CONTROLLER_NAME;
	// if empty, leader election falls back to the legacy global lease name.
	ControllerName string

	// Embedded-mode Manager Agent container mounts (host paths, read from env)
	ManagerWorkspaceDir string // e.g. ~/hiclaw-manager — mounted as /root/manager-workspace
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
	OpenAIBaseURL              string // HICLAW_OPENAI_BASE_URL — custom base URL for openai-compat providers
	AIStreamIdleTimeoutSeconds int    // HICLAW_AI_STREAM_IDLE_TIMEOUT_SECONDS

	// Element Web URL (for Gateway route initialization)
	ElementWebURL string

	// Locale used to render the first-boot Manager onboarding prompt
	// (welcome message). Sourced from the install-time HICLAW_LANGUAGE
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
	YoloMode             bool // HICLAW_YOLO=1 — propagated to managers and workers
	MatrixDebug          bool // HICLAW_MATRIX_DEBUG=1 — propagated to managers and workers,
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
	// Sourced from SKILLS_API_URL, falling back to HICLAW_SKILLS_API_URL.
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
	dataDir := envOrDefault("HICLAW_DATA_DIR", "/data/hiclaw-controller")
	if !filepath.IsAbs(dataDir) {
		if wd, err := os.Getwd(); err == nil {
			dataDir = filepath.Join(wd, dataDir)
		}
	}

	resourceAutoPrefix := envBoolDefault("HICLAW_RESOURCE_AUTOPREFIX", true)
	resourcePrefix := ""
	if resourceAutoPrefix {
		resourcePrefix = envOrDefault("HICLAW_RESOURCE_PREFIX", "hiclaw-")
	}
	// ContainerPrefix defaults to "${resourcePrefix}worker-" when auto-prefix
	// is enabled. HICLAW_PROXY_CONTAINER_PREFIX remains an explicit override.
	containerPrefix := os.Getenv("HICLAW_PROXY_CONTAINER_PREFIX")
	if containerPrefix == "" && resourceAutoPrefix {
		containerPrefix = resourcePrefix + "worker-"
	}

	cfg := &Config{
		KubeMode:  envOrDefault("HICLAW_KUBE_MODE", "embedded"),
		DataDir:   dataDir,
		HTTPAddr:  envOrDefault("HICLAW_HTTP_ADDR", ":8090"),
		ConfigDir: envOrDefault("HICLAW_CONFIG_DIR", "/root/hiclaw-fs/hiclaw-config"),
		CRDDir:    envOrDefault("HICLAW_CRD_DIR", "/opt/hiclaw/config/crd"),
		SkillsDir: envOrDefault("HICLAW_SKILLS_DIR", "/opt/hiclaw/agent/skills"),

		ResourcePrefix:     resourcePrefix,
		ResourceAutoPrefix: resourceAutoPrefix,

		SocketPath:      envOrDefault("HICLAW_PROXY_SOCKET", "/var/run/docker.sock"),
		ContainerPrefix: containerPrefix,

		AuthAudience: envOrDefault("HICLAW_AUTH_AUDIENCE", "hiclaw-controller"),

		GatewayProvider: envOrDefault("HICLAW_GATEWAY_PROVIDER", "higress"),
		StorageProvider: envOrDefault("HICLAW_STORAGE_PROVIDER", "minio"),

		CredentialProviderURL: os.Getenv("HICLAW_CREDENTIAL_PROVIDER_URL"),

		HigressBaseURL:    envOrDefault("HICLAW_AI_GATEWAY_ADMIN_URL", "http://127.0.0.1:8001"),
		HigressCookieFile: os.Getenv("HIGRESS_COOKIE_FILE"),
		// Higress and Matrix share the same admin credentials.
		HigressAdminUser:     os.Getenv("HICLAW_ADMIN_USER"),
		HigressAdminPassword: os.Getenv("HICLAW_ADMIN_PASSWORD"),

		WorkerBackend: firstNonEmpty(
			os.Getenv("HICLAW_WORKER_BACKEND"),
			os.Getenv("HICLAW_ALIYUN_WORKER_BACKEND"),
		),

		Region: envOrDefault("HICLAW_REGION", "cn-hangzhou"),

		GWGatewayID:  os.Getenv("HICLAW_GW_GATEWAY_ID"),
		GWModelAPIID: os.Getenv("HICLAW_GW_MODEL_API_ID"),
		GWEnvID:      os.Getenv("HICLAW_GW_ENV_ID"),

		OSSBucket: envOrDefault("HICLAW_FS_BUCKET", "hiclaw-storage"),

		K8sNamespace:    os.Getenv("HICLAW_K8S_NAMESPACE"),
		K8sWorkerCPU:    envOrDefault("HICLAW_K8S_WORKER_CPU", "1000m"),
		K8sWorkerMemory: envOrDefault("HICLAW_K8S_WORKER_MEMORY", "2Gi"),

		ManagerEnabled:          envOrDefault("HICLAW_MANAGER_ENABLED", "true") == "true",
		ManagerModel:            firstNonEmpty(os.Getenv("HICLAW_MANAGER_MODEL"), envOrDefault("HICLAW_DEFAULT_MODEL", "qwen3.6-plus")),
		ManagerRuntime:          envOrDefault("HICLAW_MANAGER_RUNTIME", "openclaw"),
		ManagerImage:            os.Getenv("HICLAW_MANAGER_IMAGE"),
		DefaultWorkerRuntime:    os.Getenv("HICLAW_DEFAULT_WORKER_RUNTIME"),
		K8sManagerCPURequest:    envOrDefault("HICLAW_K8S_MANAGER_CPU_REQUEST", "500m"),
		K8sManagerMemoryRequest: envOrDefault("HICLAW_K8S_MANAGER_MEMORY_REQUEST", "1Gi"),
		K8sManagerCPU:           envOrDefault("HICLAW_K8S_MANAGER_CPU", "2"),
		K8sManagerMemory:        envOrDefault("HICLAW_K8S_MANAGER_MEMORY", "4Gi"),

		ControllerURL:  os.Getenv("HICLAW_CONTROLLER_URL"),
		ControllerName: os.Getenv("HICLAW_CONTROLLER_NAME"),

		ManagerWorkspaceDir: os.Getenv("HICLAW_WORKSPACE_DIR"),
		HostShareDir:        os.Getenv("HICLAW_HOST_SHARE_DIR"),
		ManagerConsolePort:  envOrDefault("HICLAW_PORT_MANAGER_CONSOLE", "18888"),
		ManagerPassword:     os.Getenv("HICLAW_MANAGER_PASSWORD"),
		ManagerGatewayKey:   os.Getenv("HICLAW_MANAGER_GATEWAY_KEY"),

		MatrixServerURL:         envOrDefault("HICLAW_MATRIX_URL", "http://matrix-local.hiclaw.io:8080"),
		MatrixDomain:            envOrDefault("HICLAW_MATRIX_DOMAIN", "matrix-local.hiclaw.io:8080"),
		MatrixRegistrationToken: envOrDefault("HICLAW_MATRIX_REGISTRATION_TOKEN", os.Getenv("HICLAW_REGISTRATION_TOKEN")),
		MatrixAdminUser:         os.Getenv("HICLAW_ADMIN_USER"),
		MatrixAdminPassword:     os.Getenv("HICLAW_ADMIN_PASSWORD"),
		MatrixE2EE:              os.Getenv("HICLAW_MATRIX_E2EE") == "1" || os.Getenv("HICLAW_MATRIX_E2EE") == "true",

		MatrixAppServiceEnabled:            os.Getenv("HICLAW_MATRIX_APPSERVICE_ENABLED") != "0" && os.Getenv("HICLAW_MATRIX_APPSERVICE_ENABLED") != "false",
		MatrixAppServiceID:                 envOrDefault("HICLAW_MATRIX_APPSERVICE_ID", "hiclaw-controller"),
		MatrixAppServiceASToken:            os.Getenv("HICLAW_MATRIX_APPSERVICE_AS_TOKEN"),
		MatrixAppServiceHSToken:            os.Getenv("HICLAW_MATRIX_APPSERVICE_HS_TOKEN"),
		MatrixAppServiceSenderLocalpart:    envOrDefault("HICLAW_MATRIX_APPSERVICE_SENDER_LOCALPART", "hiclaw-controller"),
		MatrixAppServiceUserNamespaceRegex: os.Getenv("HICLAW_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX"),

		OSSStoragePrefix: envOrDefault("HICLAW_STORAGE_PREFIX", "hiclaw/hiclaw-storage"),

		DefaultModel:       envOrDefault("HICLAW_DEFAULT_MODEL", "qwen3.6-plus"),
		EmbeddingModel:     os.Getenv("HICLAW_EMBEDDING_MODEL"),
		Runtime:            envOrDefault("HICLAW_RUNTIME", "docker"),
		ModelContextWindow: envOrDefaultInt("HICLAW_MODEL_CONTEXT_WINDOW", 0),
		ModelMaxTokens:     envOrDefaultInt("HICLAW_MODEL_MAX_TOKENS", 0),

		LLMProvider:                envOrDefault("HICLAW_LLM_PROVIDER", "qwen"),
		LLMAPIKey:                  os.Getenv("HICLAW_LLM_API_KEY"),
		OpenAIBaseURL:              os.Getenv("HICLAW_OPENAI_BASE_URL"),
		AIStreamIdleTimeoutSeconds: envOrDefaultInt("HICLAW_AI_STREAM_IDLE_TIMEOUT_SECONDS", 900),
		ElementWebURL:              os.Getenv("HICLAW_ELEMENT_WEB_URL"),

		UserLanguage: envOrDefault("HICLAW_LANGUAGE", "zh"),
		UserTimezone: envOrDefault("TZ", "Asia/Shanghai"),

		CMSTracesEnabled:  envBool("HICLAW_CMS_TRACES_ENABLED"),
		CMSMetricsEnabled: envBool("HICLAW_CMS_METRICS_ENABLED"),
		CMSEndpoint:       os.Getenv("HICLAW_CMS_ENDPOINT"),
		CMSLicenseKey:     os.Getenv("HICLAW_CMS_LICENSE_KEY"),
		CMSProject:        os.Getenv("HICLAW_CMS_PROJECT"),
		CMSWorkspace:      os.Getenv("HICLAW_CMS_WORKSPACE"),
		CMSServiceName:    envOrDefault("HICLAW_CMS_SERVICE_NAME", "hiclaw-manager"),

		WorkerEnv: WorkerEnvDefaults{
			MatrixDomain:         envOrDefault("HICLAW_MATRIX_DOMAIN", "matrix-local.hiclaw.io:8080"),
			FSEndpoint:           os.Getenv("HICLAW_FS_ENDPOINT"),
			FSBucket:             envOrDefault("HICLAW_FS_BUCKET", "hiclaw-storage"),
			StoragePrefix:        envOrDefault("HICLAW_STORAGE_PREFIX", "hiclaw/hiclaw-storage"),
			ControllerURL:        os.Getenv("HICLAW_CONTROLLER_URL"),
			AIGatewayURL:         envOrDefault("HICLAW_AI_GATEWAY_URL", "http://aigw-local.hiclaw.io:8080"),
			MatrixURL:            envOrDefault("HICLAW_MATRIX_URL", "http://matrix-local.hiclaw.io:8080"),
			AdminUser:            os.Getenv("HICLAW_ADMIN_USER"),
			DefaultWorkerRuntime: os.Getenv("HICLAW_DEFAULT_WORKER_RUNTIME"),
			YoloMode:             envBool("HICLAW_YOLO"),
			MatrixDebug:          envBool("HICLAW_MATRIX_DEBUG"),

			// CMS observability (propagated from controller env to all workers/managers)
			CMSTracesEnabled:  envBool("HICLAW_CMS_TRACES_ENABLED"),
			CMSMetricsEnabled: envBool("HICLAW_CMS_METRICS_ENABLED"),
			CMSEndpoint:       os.Getenv("HICLAW_CMS_ENDPOINT"),
			CMSLicenseKey:     os.Getenv("HICLAW_CMS_LICENSE_KEY"),
			CMSProject:        os.Getenv("HICLAW_CMS_PROJECT"),
			CMSWorkspace:      os.Getenv("HICLAW_CMS_WORKSPACE"),
			SkillsAPIURL:      envOrDefault("SKILLS_API_URL", os.Getenv("HICLAW_SKILLS_API_URL")),
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
	// HICLAW_FS_DOMAIN:8080 URLs are rewritten to the MinIO object port.
	cfg.WorkerEnv.FSEndpoint = normalizeMinIOS3Endpoint(cfg.WorkerEnv.FSEndpoint)

	if specJSON := os.Getenv("HICLAW_MANAGER_SPEC"); specJSON != "" {
		if err := applyManagerSpec(cfg, specJSON); err != nil {
			panic(fmt.Sprintf("invalid HICLAW_MANAGER_SPEC: %v", err))
		}
	}

	// Validate AppService tokens when AS mode is enabled.
	// Tokens must be provided via env vars (set by install script or manually).
	// We do NOT auto-generate at runtime to prevent token drift across restarts.
	if cfg.MatrixAppServiceEnabled {
		if cfg.MatrixAppServiceASToken == "" {
			panic("HICLAW_MATRIX_APPSERVICE_AS_TOKEN is required when AppService mode is enabled; run install script or set env var")
		}
		if cfg.MatrixAppServiceHSToken == "" {
			panic("HICLAW_MATRIX_APPSERVICE_HS_TOKEN is required when AppService mode is enabled; run install script or set env var")
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
	return envOrDefault("HICLAW_CREDS_DIR", "/data/worker-creds")
}

// AgentFSDir returns the local filesystem root for agent workspaces.
func (c *Config) AgentFSDir() string {
	return envOrDefault("HICLAW_AGENT_FS_DIR", "/root/hiclaw-fs/agents")
}

// WorkerAgentDir returns the source directory for builtin worker agent files.
func (c *Config) WorkerAgentDir() string {
	return envOrDefault("HICLAW_WORKER_AGENT_DIR", "/opt/hiclaw/agent/worker-agent")
}

// ManagerConfigPath returns the path to the Manager Agent's openclaw.json (embedded mode).
func (c *Config) ManagerConfigPath() string {
	return envOrDefault("HICLAW_MANAGER_CONFIG_PATH", "/root/openclaw.json")
}

// RegistryPath returns the path to the workers-registry.json (embedded mode).
func (c *Config) RegistryPath() string {
	return envOrDefault("HICLAW_REGISTRY_PATH", "/root/workers-registry.json")
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
		WorkerImage:          envOrDefault("HICLAW_WORKER_IMAGE", "hiclaw/worker-agent:latest"),
		CopawWorkerImage:     envOrDefault("HICLAW_COPAW_WORKER_IMAGE", "hiclaw/copaw-worker:latest"),
		HermesWorkerImage:    envOrDefault("HICLAW_HERMES_WORKER_IMAGE", "hiclaw/hermes-worker:latest"),
		OpenHumanWorkerImage: envOrDefault("HICLAW_OPENHUMAN_WORKER_IMAGE", "hiclaw/openhuman-worker:latest"),
		DefaultNetwork:       envOrDefault("HICLAW_DOCKER_NETWORK", "hiclaw-net"),
	}
}

func (c *Config) STSConfig() credentials.STSConfig {
	return credentials.STSConfig{
		OSSBucket:   c.OSSBucket,
		OSSEndpoint: firstNonEmpty(os.Getenv("HICLAW_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint),
	}
}

// AIGatewayConfig returns the gateway.AIGatewayConfig used when
// GatewayProvider == "ai-gateway".
func (c *Config) AIGatewayConfig() gateway.AIGatewayConfig {
	return gateway.AIGatewayConfig{
		Region:     c.Region,
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
		WorkerImage:          envOrDefault("HICLAW_WORKER_IMAGE", "hiclaw/worker-agent:latest"),
		CopawWorkerImage:     envOrDefault("HICLAW_COPAW_WORKER_IMAGE", "hiclaw/copaw-worker:latest"),
		HermesWorkerImage:    envOrDefault("HICLAW_HERMES_WORKER_IMAGE", "hiclaw/hermes-worker:latest"),
		OpenHumanWorkerImage: envOrDefault("HICLAW_OPENHUMAN_WORKER_IMAGE", "hiclaw/openhuman-worker:latest"),
		WorkerCPU:            c.K8sWorkerCPU,
		WorkerMemory:         c.K8sWorkerMemory,
		ControllerName:       c.ControllerName,
		ResourcePrefix:       c.ResourcePrefix,
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

// extractHost returns the hostname from a URL (e.g. "http://hiclaw-controller:8090" → "hiclaw-controller").
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
// is served on the object store port (9000 in HiClaw), not the Higress HTTP
// gateway (8080). A URL like http://fs-local.hiclaw.io:8080 breaks mc silently.
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
	}
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
	accessKey := firstNonEmpty(os.Getenv("HICLAW_FS_ACCESS_KEY"), os.Getenv("HICLAW_MINIO_USER"))
	secretKey := firstNonEmpty(os.Getenv("HICLAW_FS_SECRET_KEY"), os.Getenv("HICLAW_MINIO_PASSWORD"))
	endpoint := firstNonEmpty(os.Getenv("HICLAW_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint)
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
	setIfNonEmpty("HICLAW_MINIO_USER", os.Getenv("HICLAW_MINIO_USER"))
	setIfNonEmpty("HICLAW_MINIO_PASSWORD", os.Getenv("HICLAW_MINIO_PASSWORD"))
	setIfNonEmpty("HICLAW_ADMIN_USER", c.MatrixAdminUser)
	setIfNonEmpty("HICLAW_ADMIN_PASSWORD", c.MatrixAdminPassword)
	setIfNonEmpty("HICLAW_REGISTRATION_TOKEN", c.MatrixRegistrationToken)
	setIfNonEmpty("HICLAW_AI_GATEWAY_ADMIN_URL", c.HigressBaseURL)
	setIfNonEmpty("HICLAW_MATRIX_URL", c.WorkerEnv.MatrixURL)
	setIfNonEmpty("HICLAW_AI_GATEWAY_URL", c.WorkerEnv.AIGatewayURL)
	setIfNonEmpty("HICLAW_FS_ENDPOINT", c.WorkerEnv.FSEndpoint)
	setIfNonEmpty("HICLAW_FS_BUCKET", c.WorkerEnv.FSBucket)
	setIfNonEmpty("HICLAW_FS_ACCESS_KEY", firstNonEmpty(os.Getenv("HICLAW_FS_ACCESS_KEY"), os.Getenv("HICLAW_MINIO_USER")))
	setIfNonEmpty("HICLAW_FS_SECRET_KEY", firstNonEmpty(os.Getenv("HICLAW_FS_SECRET_KEY"), os.Getenv("HICLAW_MINIO_PASSWORD")))
	setIfNonEmpty("HICLAW_STORAGE_PREFIX", c.OSSStoragePrefix)
	setIfNonEmpty("HICLAW_MATRIX_DOMAIN", c.MatrixDomain)
	setIfNonEmpty("HICLAW_DEFAULT_MODEL", c.DefaultModel)
	setIfNonEmpty("HICLAW_EMBEDDING_MODEL", c.EmbeddingModel)
	setIfNonEmpty("HICLAW_LLM_PROVIDER", c.LLMProvider)
	setIfNonEmpty("HICLAW_LLM_API_KEY", c.LLMAPIKey)
	if c.AIStreamIdleTimeoutSeconds > 0 {
		env["HICLAW_AI_STREAM_IDLE_TIMEOUT_SECONDS"] = strconv.Itoa(c.AIStreamIdleTimeoutSeconds)
	}
	setIfNonEmpty("HICLAW_ELEMENT_WEB_URL", c.ElementWebURL)
	if c.MatrixE2EE {
		env["HICLAW_MATRIX_E2EE"] = "1"
	}
	if c.WorkerEnv.MatrixDebug {
		env["HICLAW_MATRIX_DEBUG"] = "1"
	}
	if c.CMSTracesEnabled {
		env["HICLAW_CMS_TRACES_ENABLED"] = "1"
	}
	if c.CMSMetricsEnabled {
		env["HICLAW_CMS_METRICS_ENABLED"] = "1"
	}
	setIfNonEmpty("HICLAW_CMS_ENDPOINT", c.CMSEndpoint)
	setIfNonEmpty("HICLAW_CMS_LICENSE_KEY", c.CMSLicenseKey)
	setIfNonEmpty("HICLAW_CMS_PROJECT", c.CMSProject)
	setIfNonEmpty("HICLAW_CMS_WORKSPACE", c.CMSWorkspace)
	setIfNonEmpty("HICLAW_CMS_SERVICE_NAME", c.CMSServiceName)
	return env
}

func (c *Config) AgentConfig() agentconfig.Config {
	// Use WorkerEnv URLs (host-replaced in embedded mode) since openclaw.json
	// is consumed by worker containers, not the controller itself.
	matrixURL := c.MatrixServerURL
	aiGatewayURL := envOrDefault("HICLAW_AI_GATEWAY_URL", "http://aigw-local.hiclaw.io:8080")
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
