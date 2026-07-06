package config

import (
	"encoding/json"
	"fmt"
	"net/url"
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
	// resource/container prefixes. When false, default agentteams-* prefixes are
	// disabled unless explicit AGENTTEAMS_PROXY_CONTAINER_PREFIX is provided.
	// Set via AGENTTEAMS_RESOURCE_AUTOPREFIX. Default true.
	ResourceAutoPrefix bool

	// Docker proxy (embedded mode only)
	SocketPath      string
	ContainerPrefix string // worker container/pod name prefix; derived from ResourcePrefix when AGENTTEAMS_PROXY_CONTAINER_PREFIX is unset

	// Auth
	AuthAudience               string // SA token audience for TokenReview
	AuthTokenExpirationSeconds int64  // projected SA token TTL; default 3600s, minimum 600s

	// Provider selection (driven by Helm values)
	GatewayProvider string // "higress" | "ai-gateway"
	StorageProvider string // "minio"   | "oss"

	// Higress (self-hosted gateway)
	HigressBaseURL       string
	HigressCookieFile    string
	HigressAdminUser     string
	HigressAdminPassword string

	// Sandbox backend configuration
	SandboxProviderType string // "openkruise" (default)
	SandboxCapabilities string // comma-separated opt-in list, e.g. "hibernate"; empty = all disabled
	SandboxPrewarmSize  int    // AGENTTEAMS_SANDBOX_PREWARM_SIZE; default 1
	MountAuthType       string // AGENTTEAMS_MOUNT_AUTH_TYPE; RRSA (default) or AccessKey
	MountRoleName       string // AGENTTEAMS_MOUNT_ROLE_NAME; required when MountAuthType=RRSA

	// Region (used by AI Gateway / OSS, etc.)
	Region string

	// AgentIdentityDataEndpoint is the data-plane endpoint projected into
	// Worker runtime.yaml for AgentIdentityData SDK clients.
	AgentIdentityDataEndpoint string

	// AI Gateway (Alibaba Cloud APIG) — only used when GatewayProvider == "ai-gateway"
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

	// Manager deployment (Initializer creates the Manager CR if enabled)
	ManagerEnabled          bool
	ManagerModel            string
	ManagerRuntime          string
	ManagerImage            string
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

	// WorkerBackend selects the high-level infrastructure backend ("docker" / "k8s").
	// Read from AGENTTEAMS_WORKER_BACKEND. Controls which backends are registered.
	WorkerBackend string

	// DefaultWorkerBackendRuntime is the cluster-level default backendRuntime
	// when a Worker CR's spec.backendRuntime is not explicitly set.
	// Values: "pod" (default), "sandbox". Only meaningful in incluster mode.
	// Read from AGENTTEAMS_WORKER_BACKEND_RUNTIME.
	DefaultWorkerBackendRuntime string

	// Controller URL (advertised to workers for STS refresh etc.)
	ControllerURL string

	// ControllerMatrixURL is the controller HTTP endpoint registered with
	// the Matrix homeserver (Tuwunel) as the appservice push target. In
	// cross-cluster ("半托管") deployments where workers run in a separate
	// VPC and reach the controller through an externally-routable domain,
	// ControllerURL must point at that external endpoint, but Tuwunel —
	// which lives in the same cluster as the controller — needs an
	// in-cluster Service DNS instead. Sourced from
	// AGENTTEAMS_CONTROLLER_URL_MATRIX. When empty, AppserviceConfig falls
	// back to ControllerURL to preserve single-cluster behavior.
	ControllerMatrixURL string

	// ControllerName identifies this controller instance. When multiple
	// agentteams-controller deployments live in the same namespace (e.g. separate
	// Helm releases), each must use a distinct LeaderElection lease to avoid
	// one instance blocking the other. Sourced from AGENTTEAMS_CONTROLLER_NAME;
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

	// Appservice (global Matrix event push — replaces /sync polling)
	AppserviceEnabled bool   // AGENTTEAMS_APPSERVICE_ENABLED; default true
	AppserviceID      string // AGENTTEAMS_APPSERVICE_ID; default "agentteams-watcher"
	AppserviceASToken string // AGENTTEAMS_APPSERVICE_AS_TOKEN; auto-generated if empty
	AppserviceHSToken string // AGENTTEAMS_APPSERVICE_HS_TOKEN; auto-generated if empty

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

// WorkerEnvDefaults holds environment variable defaults injected into worker containers.
// All values are resolved once at config load time from the controller's own environment.
type WorkerEnvDefaults struct {
	MatrixDomain    string
	FSEndpoint      string
	FSBucket        string
	StoragePrefix   string
	StorageProvider string
	ControllerURL   string
	AIGatewayURL    string
	MatrixURL       string
	AdminUser       string
	Runtime         string // "docker" for embedded, "k8s" for incluster
	YoloMode        bool   // AGENTTEAMS_YOLO=1 — propagated to managers and workers
	MatrixDebug     bool   // AGENTTEAMS_MATRIX_DEBUG=1 — propagated to managers and workers,
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
	Model     string               `json:"model"`
	Runtime   string               `json:"runtime"`
	Image     string               `json:"image"`
	Resources managerSpecResources `json:"resources"`
}

type managerSpecResources struct {
	Requests managerSpecResourceValues `json:"requests"`
	Limits   managerSpecResourceValues `json:"limits"`
}

type managerSpecResourceValues struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

func LoadConfig() *Config {
	kubeMode := envOrDefaultAny("embedded", "AGENTTEAMS_KUBE_MODE")
	metricsBindAddr := envAny("AGENTTEAMS_METRICS_BIND_ADDR")
	if metricsBindAddr == "" {
		if kubeMode == "embedded" {
			metricsBindAddr = "0"
		} else {
			metricsBindAddr = ":8080"
		}
	}

	dataDir := envOrDefaultAny("/data/agentteams-controller", "AGENTTEAMS_DATA_DIR")
	if !filepath.IsAbs(dataDir) {
		if wd, err := os.Getwd(); err == nil {
			dataDir = filepath.Join(wd, dataDir)
		}
	}

	resourceAutoPrefix := envBoolDefaultAny(true, "AGENTTEAMS_RESOURCE_AUTOPREFIX")
	resourcePrefix := ""
	if resourceAutoPrefix {
		resourcePrefix = envOrDefaultAny("agentteams-", "AGENTTEAMS_RESOURCE_PREFIX")
	}
	// ContainerPrefix defaults to "${resourcePrefix}worker-" when auto-prefix
	// is enabled. AGENTTEAMS_PROXY_CONTAINER_PREFIX remains an explicit override.
	containerPrefix := envAny("AGENTTEAMS_PROXY_CONTAINER_PREFIX")
	if containerPrefix == "" && resourceAutoPrefix {
		containerPrefix = resourcePrefix + "worker-"
	}
	region := envOrDefaultAny("cn-hangzhou", "AGENTTEAMS_REGION")

	cfg := &Config{
		KubeMode:        kubeMode,
		DataDir:         dataDir,
		HTTPAddr:        envOrDefaultAny(":8090", "AGENTTEAMS_HTTP_ADDR"),
		MetricsBindAddr: metricsBindAddr,
		ConfigDir:       envOrDefaultAny("/root/hiclaw-fs/agentteams-config", "AGENTTEAMS_CONFIG_DIR"),
		CRDDir:          envOrDefaultAny("/opt/hiclaw/config/crd", "AGENTTEAMS_CRD_DIR"),
		SkillsDir:       envOrDefaultAny("/opt/hiclaw/agent/skills", "AGENTTEAMS_SKILLS_DIR"),

		ResourcePrefix:     resourcePrefix,
		ResourceAutoPrefix: resourceAutoPrefix,

		SocketPath:      envOrDefaultAny("/var/run/docker.sock", "AGENTTEAMS_PROXY_SOCKET"),
		ContainerPrefix: containerPrefix,

		AuthAudience: envOrDefaultAny("agentteams-controller", "AGENTTEAMS_AUTH_AUDIENCE"),
		AuthTokenExpirationSeconds: backend.NormalizeAuthTokenExpirationSeconds(
			int64(envOrDefaultIntAny(int(backend.DefaultAuthTokenExpirationSeconds), "AGENTTEAMS_AUTH_TOKEN_EXPIRATION_SECONDS")),
		),

		GatewayProvider: envOrDefaultAny("higress", "AGENTTEAMS_GATEWAY_PROVIDER"),
		StorageProvider: envOrDefaultAny("minio", "AGENTTEAMS_STORAGE_PROVIDER"),

		CredentialProviderURL: envAny("AGENTTEAMS_CREDENTIAL_PROVIDER_URL"),

		HigressBaseURL:    envOrDefaultAny("http://127.0.0.1:8001", "AGENTTEAMS_AI_GATEWAY_ADMIN_URL"),
		HigressCookieFile: os.Getenv("HIGRESS_COOKIE_FILE"),
		// Higress and Matrix share the same admin credentials.
		HigressAdminUser:     envAny("AGENTTEAMS_ADMIN_USER"),
		HigressAdminPassword: envAny("AGENTTEAMS_ADMIN_PASSWORD"),

		WorkerDepsStorageBucket: firstNonEmpty(
			os.Getenv("AGENTTEAMS_WORKER_DEPS_STORAGE_BUCKET"),
			os.Getenv("AGENTTEAMS_FS_BUCKET"),
			os.Getenv("HICLAW_FS_BUCKET"),
			"agentteams-storage",
		),
		WorkerDepsStorageEndpoint: firstNonEmpty(
			os.Getenv("AGENTTEAMS_WORKER_DEPS_STORAGE_ENDPOINT"),
			os.Getenv("AGENTTEAMS_FS_ENDPOINT"),
			os.Getenv("HICLAW_FS_ENDPOINT"),
		),
		WorkerDepsMountAuthType: envOrDefault("AGENTTEAMS_MOUNT_AUTH_TYPE", "RRSA"),
		WorkerDepsMountRoleName: os.Getenv("AGENTTEAMS_MOUNT_ROLE_NAME"),

		SandboxProviderType: envAny("AGENTTEAMS_SANDBOX_PROVIDER_TYPE"),
		SandboxCapabilities: envAny("AGENTTEAMS_SANDBOX_CAPABILITIES"),
		SandboxPrewarmSize: backend.NormalizeSandboxPrewarmSize(
			envOrDefaultIntAny(backend.DefaultSandboxPrewarmSize, "AGENTTEAMS_SANDBOX_PREWARM_SIZE"),
		),
		MountAuthType: envOrDefaultAny("RRSA", "AGENTTEAMS_MOUNT_AUTH_TYPE"),
		MountRoleName: envAny("AGENTTEAMS_MOUNT_ROLE_NAME"),

		Region:                    region,
		AgentIdentityDataEndpoint: agentIdentityDataEndpoint(region),

		GWGatewayID:  envAny("AGENTTEAMS_GW_GATEWAY_ID"),
		GWModelAPIID: envAny("AGENTTEAMS_GW_MODEL_API_ID"),
		GWEnvID:      envAny("AGENTTEAMS_GW_ENV_ID"),

		OSSBucket: envOrDefaultAny("agentteams-storage", "AGENTTEAMS_FS_BUCKET"),

		K8sNamespace:    envAny("AGENTTEAMS_K8S_NAMESPACE"),
		K8sWorkerCPU:    envOrDefaultAny("1000m", "AGENTTEAMS_K8S_WORKER_CPU"),
		K8sWorkerMemory: envOrDefaultAny("2Gi", "AGENTTEAMS_K8S_WORKER_MEMORY"),

		ManagerEnabled:          envOrDefaultAny("true", "AGENTTEAMS_MANAGER_ENABLED") == "true",
		ManagerModel:            firstNonEmpty(envAny("AGENTTEAMS_MANAGER_MODEL"), envOrDefaultAny("qwen3.6-plus", "AGENTTEAMS_DEFAULT_MODEL")),
		ManagerRuntime:          envOrDefaultAny("openclaw", "AGENTTEAMS_MANAGER_RUNTIME"),
		ManagerImage:            envAny("AGENTTEAMS_MANAGER_IMAGE"),
		DefaultWorkerRuntime:    envAny("AGENTTEAMS_DEFAULT_WORKER_RUNTIME"),
		K8sManagerCPURequest:    envOrDefaultAny("500m", "AGENTTEAMS_K8S_MANAGER_CPU_REQUEST"),
		K8sManagerMemoryRequest: envOrDefaultAny("1Gi", "AGENTTEAMS_K8S_MANAGER_MEMORY_REQUEST"),
		K8sManagerCPU:           envOrDefaultAny("2", "AGENTTEAMS_K8S_MANAGER_CPU"),
		K8sManagerMemory:        envOrDefaultAny("4Gi", "AGENTTEAMS_K8S_MANAGER_MEMORY"),

		WorkerBackend: firstNonEmpty(
			envAny("AGENTTEAMS_WORKER_BACKEND"),
			os.Getenv("AGENTTEAMS_ALIYUN_WORKER_BACKEND"),
		),
		DefaultWorkerBackendRuntime: envOrDefaultAny("pod", "AGENTTEAMS_WORKER_BACKEND_RUNTIME"),

		ControllerURL:       envAny("AGENTTEAMS_CONTROLLER_URL"),
		ControllerMatrixURL: envAny("AGENTTEAMS_CONTROLLER_URL_MATRIX"),
		ControllerName:      envAny("AGENTTEAMS_CONTROLLER_NAME"),

		ManagerWorkspaceDir: envAny("AGENTTEAMS_WORKSPACE_DIR"),
		HostShareDir:        envAny("AGENTTEAMS_HOST_SHARE_DIR"),
		ManagerConsolePort:  envOrDefaultAny("18888", "AGENTTEAMS_PORT_MANAGER_CONSOLE"),
		ManagerPassword:     envAny("AGENTTEAMS_MANAGER_PASSWORD"),
		ManagerGatewayKey:   envAny("AGENTTEAMS_MANAGER_GATEWAY_KEY"),

		MatrixServerURL:         envOrDefaultAny("http://matrix-local.agentteams.io:8080", "AGENTTEAMS_MATRIX_URL"),
		MatrixDomain:            envOrDefaultAny("matrix-local.agentteams.io:8080", "AGENTTEAMS_MATRIX_DOMAIN"),
		MatrixRegistrationToken: envOrDefaultAny(envAny("AGENTTEAMS_REGISTRATION_TOKEN"), "AGENTTEAMS_MATRIX_REGISTRATION_TOKEN"),
		MatrixAdminUser:         envAny("AGENTTEAMS_ADMIN_USER"),
		MatrixAdminPassword:     envAny("AGENTTEAMS_ADMIN_PASSWORD"),
		MatrixE2EE:              envBoolAny("AGENTTEAMS_MATRIX_E2EE"),

		OSSStoragePrefix: envOrDefaultAny("agentteams/agentteams-storage", "AGENTTEAMS_STORAGE_PREFIX"),

		DefaultModel:       envOrDefaultAny("qwen3.6-plus", "AGENTTEAMS_DEFAULT_MODEL"),
		EmbeddingModel:     envAny("AGENTTEAMS_EMBEDDING_MODEL"),
		Runtime:            envOrDefaultAny("docker", "AGENTTEAMS_RUNTIME"),
		ModelContextWindow: envOrDefaultIntAny(0, "AGENTTEAMS_MODEL_CONTEXT_WINDOW"),
		ModelMaxTokens:     envOrDefaultIntAny(0, "AGENTTEAMS_MODEL_MAX_TOKENS"),

		LLMProvider:                envOrDefaultAny("qwen", "AGENTTEAMS_LLM_PROVIDER"),
		LLMAPIKey:                  envAny("AGENTTEAMS_LLM_API_KEY"),
		OpenAIBaseURL:              envAny("AGENTTEAMS_OPENAI_BASE_URL"),
		AIStreamIdleTimeoutSeconds: envOrDefaultIntAny(900, "AGENTTEAMS_AI_STREAM_IDLE_TIMEOUT_SECONDS"),
		ElementWebURL:              envAny("AGENTTEAMS_ELEMENT_WEB_URL"),

		MatrixAppServiceEnabled:            envBoolDefaultAny(true, "AGENTTEAMS_MATRIX_APPSERVICE_ENABLED"),
		MatrixAppServiceID:                 envOrDefaultAny("agentteams-controller", "AGENTTEAMS_MATRIX_APPSERVICE_ID"),
		MatrixAppServiceASToken:            envAny("AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN"),
		MatrixAppServiceHSToken:            envAny("AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN"),
		MatrixAppServiceSenderLocalpart:    envOrDefaultAny("agentteams-controller", "AGENTTEAMS_MATRIX_APPSERVICE_SENDER_LOCALPART"),
		MatrixAppServiceUserNamespaceRegex: envAny("AGENTTEAMS_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX"),

		UserLanguage: envOrDefaultAny("zh", "AGENTTEAMS_LANGUAGE"),
		UserTimezone: envOrDefault("TZ", "Asia/Shanghai"),

		AppserviceEnabled: envBoolDefaultAny(true, "AGENTTEAMS_APPSERVICE_ENABLED"),
		AppserviceID:      envOrDefaultAny("agentteams-watcher", "AGENTTEAMS_APPSERVICE_ID"),
		AppserviceASToken: envAny("AGENTTEAMS_APPSERVICE_AS_TOKEN"),
		AppserviceHSToken: envAny("AGENTTEAMS_APPSERVICE_HS_TOKEN"),

		CMSTracesEnabled:  envBoolAny("AGENTTEAMS_CMS_TRACES_ENABLED"),
		CMSMetricsEnabled: envBoolAny("AGENTTEAMS_CMS_METRICS_ENABLED"),
		CMSEndpoint:       envAny("AGENTTEAMS_CMS_ENDPOINT"),
		CMSLicenseKey:     envAny("AGENTTEAMS_CMS_LICENSE_KEY"),
		CMSProject:        envAny("AGENTTEAMS_CMS_PROJECT"),
		CMSWorkspace:      envAny("AGENTTEAMS_CMS_WORKSPACE"),
		CMSServiceName:    envOrDefaultAny("agentteams-manager", "AGENTTEAMS_CMS_SERVICE_NAME"),

		WorkerEnv: WorkerEnvDefaults{
			MatrixDomain:    envOrDefaultAny("matrix-local.agentteams.io:8080", "AGENTTEAMS_MATRIX_DOMAIN"),
			FSEndpoint:      envAny("AGENTTEAMS_FS_ENDPOINT"),
			FSBucket:        envOrDefaultAny("agentteams-storage", "AGENTTEAMS_FS_BUCKET"),
			StoragePrefix:   envOrDefaultAny("agentteams/agentteams-storage", "AGENTTEAMS_STORAGE_PREFIX"),
			StorageProvider: envOrDefaultAny("minio", "AGENTTEAMS_STORAGE_PROVIDER"),
			ControllerURL:   envAny("AGENTTEAMS_CONTROLLER_URL"),
			AIGatewayURL:    envOrDefaultAny("http://aigw-local.agentteams.io:8080", "AGENTTEAMS_AI_GATEWAY_URL"),
			MatrixURL:       envOrDefaultAny("http://matrix-local.agentteams.io:8080", "AGENTTEAMS_MATRIX_URL"),
			AdminUser:       envAny("AGENTTEAMS_ADMIN_USER"),
			YoloMode:        envBoolAny("AGENTTEAMS_YOLO"),
			MatrixDebug:     envBoolAny("AGENTTEAMS_MATRIX_DEBUG"),

			// CMS observability (propagated from controller env to all workers/managers)
			CMSTracesEnabled:  envBoolAny("AGENTTEAMS_CMS_TRACES_ENABLED"),
			CMSMetricsEnabled: envBoolAny("AGENTTEAMS_CMS_METRICS_ENABLED"),
			CMSEndpoint:       envAny("AGENTTEAMS_CMS_ENDPOINT"),
			CMSLicenseKey:     envAny("AGENTTEAMS_CMS_LICENSE_KEY"),
			CMSProject:        envAny("AGENTTEAMS_CMS_PROJECT"),
			CMSWorkspace:      envAny("AGENTTEAMS_CMS_WORKSPACE"),
			SkillsAPIURL:      envOrDefault("SKILLS_API_URL", envAny("AGENTTEAMS_SKILLS_API_URL")),
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

	if specJSON := envAny("AGENTTEAMS_MANAGER_SPEC"); specJSON != "" {
		if err := applyManagerSpec(cfg, specJSON); err != nil {
			panic(fmt.Sprintf("invalid AGENTTEAMS_MANAGER_SPEC: %v", err))
		}
	}

	// Validate AppService tokens when AS mode is enabled.
	// Tokens must be provided via env vars (set by install script or manually).
	// We do NOT auto-generate at runtime to prevent token drift across restarts.
	if cfg.MatrixAppServiceEnabled {
		if cfg.AppserviceASToken == "" {
			cfg.AppserviceASToken = cfg.MatrixAppServiceASToken
		}
		if cfg.AppserviceHSToken == "" {
			cfg.AppserviceHSToken = cfg.MatrixAppServiceHSToken
		}
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
	return envOrDefaultAny("/data/worker-creds", "AGENTTEAMS_CREDS_DIR")
}

// AgentFSDir returns the local filesystem root for agent workspaces.
func (c *Config) AgentFSDir() string {
	return envOrDefaultAny("/root/hiclaw-fs/agents", "AGENTTEAMS_AGENT_FS_DIR")
}

// WorkerAgentDir returns the source directory for builtin worker agent files.
func (c *Config) WorkerAgentDir() string {
	return envOrDefaultAny("/opt/hiclaw/agent/worker-agent", "AGENTTEAMS_WORKER_AGENT_DIR")
}

// ManagerConfigPath returns the path to the Manager Agent's openclaw.json (embedded mode).
func (c *Config) ManagerConfigPath() string {
	return envOrDefaultAny("/root/openclaw.json", "AGENTTEAMS_MANAGER_CONFIG_PATH")
}

// RegistryPath returns the path to the workers-registry.json (embedded mode).
func (c *Config) RegistryPath() string {
	return envOrDefaultAny("/root/workers-registry.json", "AGENTTEAMS_REGISTRY_PATH")
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
		WorkerImage:          envOrDefaultAny("agentteams/worker-agent:latest", "AGENTTEAMS_WORKER_IMAGE"),
		CopawWorkerImage:     envOrDefaultAny("agentteams/copaw-worker:latest", "AGENTTEAMS_COPAW_WORKER_IMAGE"),
		HermesWorkerImage:    envOrDefaultAny("agentteams/hermes-worker:latest", "AGENTTEAMS_HERMES_WORKER_IMAGE"),
		OpenHumanWorkerImage: envOrDefaultAny("agentteams/openhuman-worker:latest", "AGENTTEAMS_OPENHUMAN_WORKER_IMAGE"),
		QwenPawWorkerImage:   envOrDefaultAny("agentteams/qwenpaw-worker:latest", "AGENTTEAMS_QWENPAW_WORKER_IMAGE"),
		DefaultNetwork:       envOrDefaultAny("agentteams-net", "AGENTTEAMS_DOCKER_NETWORK"),
	}
}

func (c *Config) STSConfig() credentials.STSConfig {
	return credentials.STSConfig{
		OSSBucket:   c.OSSBucket,
		OSSEndpoint: firstNonEmpty(envAny("AGENTTEAMS_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint),
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
		WorkerImage:          envOrDefaultAny("agentteams/worker-agent:latest", "AGENTTEAMS_WORKER_IMAGE"),
		CopawWorkerImage:     envOrDefaultAny("agentteams/copaw-worker:latest", "AGENTTEAMS_COPAW_WORKER_IMAGE"),
		HermesWorkerImage:    envOrDefaultAny("agentteams/hermes-worker:latest", "AGENTTEAMS_HERMES_WORKER_IMAGE"),
		OpenHumanWorkerImage: envOrDefaultAny("agentteams/openhuman-worker:latest", "AGENTTEAMS_OPENHUMAN_WORKER_IMAGE"),
		QwenPawWorkerImage:   envOrDefaultAny("agentteams/qwenpaw-worker:latest", "AGENTTEAMS_QWENPAW_WORKER_IMAGE"),
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
		AgentRuntimeImage:            envOrDefaultAny(sandboxAgentRuntimeImage(c.Region), "AGENTTEAMS_SANDBOX_AGENT_RUNTIME_IMAGE"),
		WorkerImage:                  envOrDefaultAny("agentteams/worker-agent:latest", "AGENTTEAMS_WORKER_IMAGE"),
		CopawWorkerImage:             envOrDefaultAny("agentteams/copaw-worker:latest", "AGENTTEAMS_COPAW_WORKER_IMAGE"),
		HermesWorkerImage:            envOrDefaultAny("agentteams/hermes-worker:latest", "AGENTTEAMS_HERMES_WORKER_IMAGE"),
		OpenHumanWorkerImage:         envOrDefaultAny("agentteams/openhuman-worker:latest", "AGENTTEAMS_OPENHUMAN_WORKER_IMAGE"),
		QwenPawWorkerImage:           envOrDefaultAny("agentteams/qwenpaw-worker:latest", "AGENTTEAMS_QWENPAW_WORKER_IMAGE"),
		WorkerCPU:                    c.K8sWorkerCPU,
		WorkerMemory:                 c.K8sWorkerMemory,
		SandboxPrewarmSize:           c.SandboxPrewarmSize,
		SandboxPrewarmSizeConfigured: true,
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

func envAny(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

func envOrDefaultAny(defaultVal string, keys ...string) string {
	if v := envAny(keys...); v != "" {
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

func envOrDefaultIntAny(defaultVal int, keys ...string) int {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return defaultVal
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}

func envBoolAny(keys ...string) bool {
	for _, key := range keys {
		if envBool(key) {
			return true
		}
	}
	return false
}

func envBoolDefault(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	return v == "1" || v == "true" || v == "True" || v == "TRUE"
}

func envBoolDefaultAny(defaultVal bool, keys ...string) bool {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v == "1" || v == "true" || v == "True" || v == "TRUE"
		}
	}
	return defaultVal
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func agentIdentityDataEndpoint(region string) string {
	return firstNonEmpty(
		envAny("AGENTTEAMS_AGENT_IDENTITY_DATA_ENDPOINT"),
		fmt.Sprintf("agentidentitydata.%s.aliyuncs.com", region),
	)
}

func sandboxAgentRuntimeImage(region string) string {
	if region == "" {
		region = "cn-hangzhou"
	}
	return fmt.Sprintf("registry-%s-vpc.ack.aliyuncs.com/acs/agent-runtime:v0.0.9", region)
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

// AppServicePushURL returns the controller HTTP endpoint registered with
// Tuwunel for mention-wakeup transaction push. Empty when push is disabled.
func (c *Config) AppServicePushURL() string {
	if !c.AppserviceEnabled {
		return ""
	}
	return c.appserviceReachableURL()
}

func (c *Config) appserviceReachableURL() string {
	return firstNonEmpty(c.ControllerMatrixURL, c.ControllerURL, "http://127.0.0.1"+c.HTTPAddr)
}

// AppserviceConfig returns the resolved appservice configuration.
// Tokens are auto-generated when not provided via env.
func (c *Config) AppserviceConfig() matrix.AppserviceConfig {
	// In incluster mode, derive a per-instance Appservice ID from the
	// controller name so multiple controllers sharing the same homeserver
	// don't overwrite each other's registration. Embedded mode only runs
	// a single instance, so the plain default is fine.
	id := c.AppserviceID
	if id == "" {
		id = "agentteams-watcher"
	}
	if c.ControllerName != "" && id == "agentteams-watcher" {
		id = "agentteams-watcher-" + c.ControllerName
	}

	return matrix.AppserviceConfig{
		Enabled: c.AppserviceEnabled,
		ID:      id,
		ASToken: firstNonEmpty(c.AppserviceASToken, c.MatrixAppServiceASToken),
		HSToken: firstNonEmpty(c.AppserviceHSToken, c.MatrixAppServiceHSToken),
		// URL is the controller's own HTTP address reachable from Tuwunel.
		// Priority: ControllerMatrixURL (cross-cluster — Tuwunel-side
		// in-cluster Service DNS) > ControllerURL (single-cluster default,
		// also advertised to workers) > literal HTTPAddr (embedded/local
		// dev). The split exists because in cross-cluster deployments the
		// worker-facing URL must be externally routable while Tuwunel only
		// has the in-cluster route.
		URL: c.appserviceReachableURL(),
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
	accessKey := firstNonEmpty(envAny("AGENTTEAMS_FS_ACCESS_KEY"), envAny("AGENTTEAMS_MINIO_USER"))
	secretKey := firstNonEmpty(envAny("AGENTTEAMS_FS_SECRET_KEY"), envAny("AGENTTEAMS_MINIO_PASSWORD"))
	endpoint := firstNonEmpty(envAny("AGENTTEAMS_FS_ENDPOINT"), c.WorkerEnv.FSEndpoint)
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
	setIfNonEmpty("AGENTTEAMS_FS_ACCESS_KEY", firstNonEmpty(envAny("AGENTTEAMS_FS_ACCESS_KEY"), envAny("AGENTTEAMS_MINIO_USER")))
	setIfNonEmpty("AGENTTEAMS_FS_SECRET_KEY", firstNonEmpty(envAny("AGENTTEAMS_FS_SECRET_KEY"), envAny("AGENTTEAMS_MINIO_PASSWORD")))
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
	aiGatewayURL := envOrDefaultAny("http://aigw-local.agentteams.io:8080", "AGENTTEAMS_AI_GATEWAY_URL")
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
