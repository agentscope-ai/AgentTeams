package agentconfig

// Config holds parameters for generating agent runtime configurations.
type Config struct {
	MatrixDomain    string // Matrix domain for user IDs, e.g. "matrix-local.agentteams.io:8080"
	MatrixServerURL string // Matrix CS API URL for agent connections
	AIGatewayURL    string // AI gateway URL for model API calls
	AdminUser       string // admin username
	DefaultModel    string // default model name
	EmbeddingModel  string // embedding model for memory search (optional)
	Runtime         string // "docker", "k8s", "aliyun"
	E2EEEnabled     bool   // enable Matrix E2EE

	// Model parameter overrides (empty = use defaults from model table)
	ModelContextWindow int
	ModelMaxTokens     int
	ModelVision        *bool // nil = use model default
	ModelReasoning     *bool // nil = use model default

	// CMS observability (optional)
	CMSTracesEnabled  bool
	CMSMetricsEnabled bool
	CMSEndpoint       string
	CMSLicenseKey     string
	CMSProject        string
	CMSWorkspace      string
	CMSServiceName    string
}

// HeartbeatConfig describes the heartbeat settings to embed in openclaw.json.
type HeartbeatConfig struct {
	Enabled bool
	Every   string // e.g. "30m", "1h"
}

// WorkerConfigRequest describes everything needed to generate a worker's config files.
type WorkerConfigRequest struct {
	WorkerName     string           // e.g. "worker-alice"
	MatrixToken    string           // worker's Matrix access token
	GatewayKey     string           // worker's gateway API key
	ModelName      string           // optional: override default model
	AIGatewayURL   string           // per-worker AI Gateway URL override (from modelProvider)
	TeamLeaderName string           // if non-empty, this is a team worker
	ChannelPolicy  *ChannelPolicy   // optional communication policy overrides
	Heartbeat      *HeartbeatConfig // optional: team leader heartbeat settings
}

// ChannelPolicy describes additive/subtractive communication rules.
type ChannelPolicy struct {
	GroupAllowExtra []string `json:"groupAllowExtra,omitempty"`
	GroupDenyExtra  []string `json:"groupDenyExtra,omitempty"`
	DMAllowExtra    []string `json:"dmAllowExtra,omitempty"`
	DMDenyExtra     []string `json:"dmDenyExtra,omitempty"`
}

// ModelSpec describes LLM parameters for a specific model.
type ModelSpec struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"` // e.g. ["text", "image"]
}

// BuiltinMarkers are the delimiters for merge-managed sections in AGENTS.md.
const (
	BuiltinStart  = "<!-- hiclaw-builtin-start -->"
	BuiltinEnd    = "<!-- hiclaw-builtin-end -->"
	BuiltinHeader = `<!-- hiclaw-builtin-start -->
> ⚠️ **DO NOT EDIT** this section. It is managed by AgentTeams and will be automatically
> replaced on upgrade. To customize, add your content **after** the
> ` + "`<!-- hiclaw-builtin-end -->`" + ` marker below.
`
)

// SoulTemplateMarkers are the delimiters for the template-managed section in SOUL.md.
const (
	SoulTemplateStart  = "<!-- agentteams-soul-template-start -->"
	SoulTemplateEnd    = "<!-- agentteams-soul-template-end -->"
	SoulTemplateHeader = `<!-- agentteams-soul-template-start -->
> ⚠️ **DO NOT EDIT** this section. It is managed by AgentTeams and will be automatically
> replaced on upgrade. To customize, add your content **after** the
> ` + "`<!-- agentteams-soul-template-end -->`" + ` marker below.
`
)
