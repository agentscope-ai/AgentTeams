package config

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
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

	// SoloOperator, when true, tailors the first-boot experience for a
	// single human running HiClaw alone rather than a multi-person org:
	// the Manager welcome prompt skips the 4-question identity interview
	// (renderManagerWelcomeBodySolo), every Team's PeerMentions is forced
	// to true regardless of Team.Spec.PeerMentions (there is only one
	// human to loop in, so cross-mentions can't leak to strangers), and
	// the sole Human created via the HTTP API defaults to Admin
	// (PermissionLevel=1) when the request omits one. Sourced from
	// AGENTTEAMS_SOLO_OPERATOR. Default false (unchanged multi-user behavior).
	SoloOperator bool
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
	CMSServiceName    string

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
