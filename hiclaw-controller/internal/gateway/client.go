package gateway

import "context"

// Client abstracts AI gateway operations (consumer management, route authorization).
// Implementations: HigressClient (self-hosted), future APigClient (Alibaba Cloud).
type Client interface {
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

	// ExposePort creates gateway resources to expose a worker port.
	ExposePort(ctx context.Context, req PortExposeRequest) error

	// UnexposePort removes gateway resources for a worker port.
	UnexposePort(ctx context.Context, req PortExposeRequest) error

	// --- Infrastructure initialization (used by Initializer) ---

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

	// ResolveModelProvider looks up a named APIG Model API (HttpApi) and returns
	// its basePath, Intranet subdomain URL, and httpApiId. Only meaningful for
	// the ai-gateway provider; Higress returns ErrUnsupportedOp.
	ResolveModelProvider(ctx context.Context, name string) (*ModelProviderInfo, error)

	// Healthy returns nil if the gateway console is reachable and authenticated.
	Healthy(ctx context.Context) error
}
