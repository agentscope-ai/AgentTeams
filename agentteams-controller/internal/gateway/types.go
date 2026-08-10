package gateway

// Config holds connection parameters for an AI gateway.
type Config struct {
	ConsoleURL                string // gateway console API, e.g. http://127.0.0.1:8001
	AdminUser                 string // console login username
	AdminPassword             string // console login password
	AllowDefaultAdminFallback bool   // embedded bootstrap only: recover from all-in-one default admin/admin
	DataPlaneURL              string // gateway data plane URL, e.g. http://aigw-local.agentteams.io:8080
}

// ConsumerRequest describes a gateway consumer to create.
type ConsumerRequest struct {
	Name          string // consumer name, e.g. "worker-alice"
	CredentialKey string // API key for key-auth (self-hosted Higress)
	ConsumerID    string // platform-specific consumer ID (cloud APIG, optional)
}

// ConsumerResult holds the result of an EnsureConsumer call.
type ConsumerResult struct {
	Status     string // "created" or "exists"
	APIKey     string // the active API key
	ConsumerID string // platform-specific consumer ID (cloud only)
}

// AIRoute represents an AI route in the gateway.
type AIRoute struct {
	Name             string   `json:"name"`
	AllowedConsumers []string `json:"allowedConsumers,omitempty"`
}

// PortExposeRequest describes a port to expose through the gateway.
type PortExposeRequest struct {
	WorkerName  string // worker identifier
	ServiceHost string // DNS hostname of the service
	Port        int    // port number to expose
	Domain      string // domain name to bind
}

// AIProviderRequest describes an LLM provider to register in the gateway.
type AIProviderRequest struct {
	Name     string // provider name, e.g. "qwen"
	Type     string // provider type, e.g. "qwen", "openai"
	Tokens   []string
	Protocol string                 // e.g. "openai/v1"
	Raw      map[string]interface{} // provider-specific raw config
}

// ModelProviderInfo holds the resolved endpoint info for a named model provider
// discovered via the APIG ListHttpApis API.
type ModelProviderInfo struct {
	HttpApiID   string // APIG httpApiId for consumer authorization
	BasePath    string // e.g. "/v1"
	IntranetURL string // full Intranet URL including protocol and domain (e.g. "http://xxx.internal.xxx.com")
}

// AIRouteRequest describes an AI route skeleton to create.
//
// It intentionally carries no authorization fields: the Initializer is the
// sole creator of the AI route skeleton, while Manager/Worker Reconcilers are
// the sole writers of authConfig.allowedConsumers (via AuthorizeAIRoutes /
// DeauthorizeAIRoutes). This separation prevents the race observed on
// controller restart where the Initializer previously reset allowedConsumers
// to [] and transiently locked out Manager/Workers.
type AIRouteRequest struct {
	Name       string
	PathPrefix string // e.g. "/v1"
	Provider   string // upstream provider name
}
