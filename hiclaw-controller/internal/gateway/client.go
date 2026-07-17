package gateway

import "context"

// ConsumerClient manages gateway consumer credentials and AI-route authorization.
type ConsumerClient interface {
	// EnsureConsumer creates a consumer or returns existing.
	// Idempotent: repeated calls with the same name are safe.
	EnsureConsumer(ctx context.Context, req ConsumerRequest) (*ConsumerResult, error)

	// DeleteConsumer removes a consumer by name. No-op if not found.
	DeleteConsumer(ctx context.Context, name string) error

	// AuthorizeAIRoutes adds the consumer to AI routes' allowedConsumers.
	// Handles 409 conflict with retry logic. When modelAPIID is non-empty,
	// the consumer is authorized only on that provider route and removed
	// from other AI routes.
	AuthorizeAIRoutes(ctx context.Context, consumerName string, modelAPIID string) error

	// DeauthorizeAIRoutes removes the consumer from AI routes' allowedConsumers.
	// When modelAPIID is non-empty, only that provider route is modified.
	DeauthorizeAIRoutes(ctx context.Context, consumerName string, modelAPIID string) error
}

// PortExposeClient manages per-worker port exposure through the gateway.
type PortExposeClient interface {
	// ExposePort creates gateway resources to expose a worker port.
	ExposePort(ctx context.Context, req PortExposeRequest) error

	// UnexposePort removes gateway resources for a worker port.
	UnexposePort(ctx context.Context, req PortExposeRequest) error
}

// InfrastructureClient manages gateway bootstrap resources (routes, providers, service sources).
// On ai-gateway provider deployments these operations return ErrUnsupportedOp.
type InfrastructureClient interface {
	// EnsureServiceSource registers a DNS-type service source.
	EnsureServiceSource(ctx context.Context, name, domain string, port int, protocol string) error

	// EnsureStaticServiceSource registers a static (fixed IP:port) service source.
	EnsureStaticServiceSource(ctx context.Context, name, address string, port int) error

	// EnsureRoute creates a route mapping domains to a backend service.
	// pathPrefix is the URL prefix to match (e.g. "/" or "/_matrix").
	EnsureRoute(ctx context.Context, name string, domains []string, serviceName string, port int, pathPrefix string) error

	// DeleteRoute removes a route by name. No-op if not found.
	DeleteRoute(ctx context.Context, name string) error

	// EnsureAIProvider creates an LLM provider configuration.
	EnsureAIProvider(ctx context.Context, req AIProviderRequest) error

	// EnsureStreamIdleTimeout updates the gateway stream idle timeout used by
	// long-running LLM streaming responses.
	EnsureStreamIdleTimeout(ctx context.Context, seconds int) error

	// EnsureAIRoute creates an AI route with consumer auth.
	EnsureAIRoute(ctx context.Context, req AIRouteRequest) error
}

// ModelProviderClient resolves cloud model-provider metadata.
type ModelProviderClient interface {
	// ResolveModelProvider looks up a named APIG Model API (HttpApi) and returns
	// its basePath, Intranet subdomain URL, and httpApiId. Only meaningful for
	// the ai-gateway provider; Higress returns ErrUnsupportedOp.
	ResolveModelProvider(ctx context.Context, name string) (*ModelProviderInfo, error)
}

// HealthClient reports gateway control-plane reachability.
type HealthClient interface {
	// Healthy returns nil if the gateway console is reachable and authenticated.
	Healthy(ctx context.Context) error
}

// MCPAdminClient lists MCP servers registered in the gateway console.
type MCPAdminClient interface {
	// ListMCPServers returns MCP server inventory for health probes.
	// Returns ErrUnsupportedOp on providers without a console MCP API.
	ListMCPServers(ctx context.Context) ([]MCPServerInfo, error)
}

// MCPServerInfo describes one MCP server entry from the gateway console.
type MCPServerInfo struct {
	Name             string
	AllowedConsumers []string
}

// Client abstracts AI gateway operations (consumer management, route authorization).
// Implementations: HigressClient (self-hosted), AIGatewayClient (Alibaba Cloud).
//
// Phase C10.9: split into focused interfaces for compile-time clarity; Client
// remains the union type wired through Provisioner and Initializer.
type Client interface {
	ConsumerClient
	PortExposeClient
	InfrastructureClient
	ModelProviderClient
	HealthClient
	MCPAdminClient
}
