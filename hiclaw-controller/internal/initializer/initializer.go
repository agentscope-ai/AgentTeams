package initializer

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/migration"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Config holds parameters for cluster initialization.
type Config struct {
	ManagerEnabled   bool
	ManagerModel     string
	ManagerRuntime   string
	ManagerImage     string
	ManagerResources *v1beta1.AgentResourceRequirements
	AdminUser        string
	AdminPassword    string
	Namespace        string
	IsEmbedded       bool   // embedded mode: use static service sources for local services
	AgentFSDir       string // local filesystem root for agent workspaces (embedded mode)
	ControllerName   string // HICLAW_CONTROLLER_NAME; stamped as agentteams.io/controller label on created CRs in incluster mode

	// Matrix AppService mode
	AppServiceEnabled         bool
	AppServiceID              string
	AppServiceToken           string
	AppServiceHSToken         string
	AppServiceSenderLocalpart string
	AppServicePushURL         string
	MatrixDomain              string // needed for AS registration YAML

	// Provider selection — drives which initialization steps run.
	GatewayProvider string // "higress" | "ai-gateway"
	StorageProvider string // "minio"   | "oss"

	// Gateway initialization (only consulted when GatewayProvider == "higress")
	LLMProvider                string // e.g. "qwen", "openai"
	LLMAPIKey                  string
	LLMApiURL                  string // provider-specific base URL (optional)
	OpenAIBaseURL              string // custom base URL for openai-compat providers
	AIStreamIdleTimeoutSeconds int
	TuwunelURL                 string // internal Tuwunel URL, e.g. http://tuwunel:6167
	ElementWebURL              string // internal Element Web URL (optional)
}

func (c Config) managesGatewayRoutes() bool {
	return c.GatewayProvider == "" || c.GatewayProvider == "higress"
}

func (c Config) managesStorage() bool {
	return c.StorageProvider == "" || c.StorageProvider == "minio"
}

// Initializer performs one-time cluster bootstrap: waits for infrastructure,
// initializes storage structure, registers the admin account, sets up gateway
// routes, and optionally creates the Manager CR.
type Initializer struct {
	OSS     oss.StorageClient
	Matrix  matrix.Client
	Gateway gateway.Client
	RestCfg *rest.Config
	Config  Config
}

func (i *Initializer) Run(ctx context.Context) error {
	logger := ctrl.Log.WithName("initializer")
	logger.Info("starting cluster initialization")

	if err := i.waitForOSS(ctx); err != nil {
		return fmt.Errorf("OSS not ready: %w", err)
	}
	logger.Info("OSS is ready")

	if err := i.ensureOSSStructure(ctx); err != nil {
		return fmt.Errorf("OSS structure init failed: %w", err)
	}
	logger.Info("OSS directory structure initialized")

	if err := i.waitForMatrix(ctx); err != nil {
		return fmt.Errorf("Matrix not ready: %w", err)
	}
	logger.Info("Matrix is ready")

	if err := i.registerAdmin(ctx); err != nil {
		return fmt.Errorf("admin registration failed: %w", err)
	}
	logger.Info("admin account ready", "user", i.Config.AdminUser)

	// Register Matrix AppService if enabled (must happen after admin is ready)
	if i.Config.AppServiceEnabled {
		if err := i.registerAppService(ctx); err != nil {
			return fmt.Errorf("Matrix AppService registration failed: %w", err)
		}
		logger.Info("Matrix AppService registered and verified",
			"id", i.Config.AppServiceID,
			"sender", i.Config.AppServiceSenderLocalpart)
	}

	if i.Gateway != nil {
		if err := i.waitForGateway(ctx); err != nil {
			return fmt.Errorf("Gateway not ready: %w", err)
		}
		logger.Info("Gateway is ready")

		if i.Config.managesGatewayRoutes() {
			if err := i.initGatewayRoutes(ctx); err != nil {
				return fmt.Errorf("Gateway route init failed: %w", err)
			}
			logger.Info("Gateway routes initialized")
		} else {
			logger.Info("skipping gateway route initialization",
				"provider", i.Config.GatewayProvider,
				"reason", "routes are managed out-of-band by the cloud platform")
		}
	}

	if i.Config.ManagerEnabled {
		if err := i.ensureManagerCR(ctx); err != nil {
			return fmt.Errorf("Manager CR creation failed: %w", err)
		}
		logger.Info("Manager CR ensured", "name", "default")
	}

	// Migrate v1.0.9 registry data to CRs (embedded mode only)
	if i.Config.IsEmbedded {
		migrator := &migration.Migrator{
			OSS:          i.OSS,
			RestCfg:      i.RestCfg,
			Namespace:    i.Config.Namespace,
			DefaultModel: i.Config.ManagerModel,
			AgentFSDir:   i.Config.AgentFSDir,
		}
		if err := migrator.Run(ctx); err != nil {
			logger.Error(err, "registry migration failed (non-fatal, continuing)")
		}
	}

	logger.Info("cluster initialization complete")
	return nil
}

// waitForOSS polls MinIO/OSS until the bucket is accessible.
//
// For the embedded MinIO (storage.provider == "minio") the bucket is
// created on demand through BucketManager.EnsureBucket. For an
// externally-managed OSS bucket the initializer does not try to create
// or mutate anything — it just polls ListObjects to confirm that the
// controller's credentials grant access to the configured bucket.
func (i *Initializer) waitForOSS(ctx context.Context) error {
	if i.Config.managesStorage() {
		if bm, ok := i.OSS.(oss.BucketManager); ok {
			return retry(ctx, 3*time.Second, 5*time.Minute, func() error {
				return bm.EnsureBucket(ctx)
			})
		}
	}
	return retry(ctx, 3*time.Second, 5*time.Minute, func() error {
		_, err := i.OSS.ListObjects(ctx, "")
		return err
	})
}

func (i *Initializer) ensureOSSStructure(ctx context.Context) error {
	dirs := []string{
		"shared/knowledge/",
		"shared/tasks/",
		"workers/",
		"hiclaw-config/workers/",
		"hiclaw-config/teams/",
		"hiclaw-config/humans/",
		"agents/",
	}
	for _, dir := range dirs {
		if err := i.OSS.PutObject(ctx, dir+".gitkeep", []byte("")); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// waitForMatrix polls the Matrix server until it responds.
func (i *Initializer) waitForMatrix(ctx context.Context) error {
	return retry(ctx, 3*time.Second, 5*time.Minute, func() error {
		_, err := i.Matrix.Login(ctx, "__healthcheck__", "invalid")
		if err != nil && isMatrixConnError(err) {
			return err
		}
		// Any non-connection error (403, 401, etc.) means Matrix is up.
		return nil
	})
}

func (i *Initializer) registerAdmin(ctx context.Context) error {
	_, err := i.Matrix.EnsureUser(ctx, matrix.EnsureUserRequest{
		Username: i.Config.AdminUser,
		Password: i.Config.AdminPassword,
	})
	return err
}

// registerAppService registers the HiClaw controller as a Matrix Application
// Service via the Tuwunel admin bot, then verifies with a smoke test.
func (i *Initializer) registerAppService(ctx context.Context) error {
	cfg := matrix.Config{
		Domain:                    i.Config.MatrixDomain,
		AppServiceID:              i.Config.AppServiceID,
		AppServiceToken:           i.Config.AppServiceToken,
		AppServiceHSToken:         i.Config.AppServiceHSToken,
		AppServiceSenderLocalpart: i.Config.AppServiceSenderLocalpart,
		AppServicePushURL:         i.Config.AppServicePushURL,
	}
	reg := matrix.RenderAppServiceRegistration(cfg)
	if err := i.Matrix.RegisterAppService(ctx, reg); err != nil {
		return fmt.Errorf("register appservice: %w", err)
	}
	if err := i.Matrix.AppServiceSmokeTest(ctx); err != nil {
		return fmt.Errorf("appservice smoke test: %w", err)
	}
	return nil
}

// waitForGateway polls the Higress Console until it responds.
func (i *Initializer) waitForGateway(ctx context.Context) error {
	return retry(ctx, 3*time.Second, 5*time.Minute, func() error {
		return i.Gateway.Healthy(ctx)
	})
}

// initGatewayRoutes registers service sources, LLM provider, AI route, and
// infrastructure routes (Matrix, Element Web) in Higress. All calls are
// idempotent — safe to re-run on controller restart.
func (i *Initializer) initGatewayRoutes(ctx context.Context) error {
	logger := ctrl.Log.WithName("initializer")
	cfg := i.Config

	// 1. Tuwunel service source
	if cfg.TuwunelURL != "" {
		host, port, err := parseHostPort(cfg.TuwunelURL)
		if err != nil {
			return fmt.Errorf("parse Tuwunel URL: %w", err)
		}

		var svcSuffix string
		if cfg.IsEmbedded {
			if err := i.Gateway.EnsureStaticServiceSource(ctx, "tuwunel", host, port); err != nil {
				logger.Error(err, "failed to register Tuwunel static service source (non-fatal)")
			}
			svcSuffix = "static"
		} else {
			if err := i.Gateway.EnsureServiceSource(ctx, "tuwunel", host, port, "http"); err != nil {
				logger.Error(err, "failed to register Tuwunel service source (non-fatal)")
			}
			svcSuffix = "dns"
		}

		// Matrix Homeserver routes (/_matrix/*, /_tuwunel/* → Tuwunel)
		if err := i.Gateway.EnsureRoute(ctx, "matrix-homeserver", nil, "tuwunel."+svcSuffix, port, "/_matrix"); err != nil {
			logger.Error(err, "failed to create Matrix route (non-fatal)")
		}
	}

	// 2. Element Web service source + route
	if cfg.ElementWebURL != "" {
		host, port, err := parseHostPort(cfg.ElementWebURL)
		if err != nil {
			logger.Error(err, "failed to parse Element Web URL (non-fatal)")
		} else {
			var svcSuffix string
			if cfg.IsEmbedded {
				if err := i.Gateway.EnsureStaticServiceSource(ctx, "element-web", host, port); err != nil {
					logger.Error(err, "failed to register Element Web static service source (non-fatal)")
				}
				svcSuffix = "static"
			} else {
				if err := i.Gateway.EnsureServiceSource(ctx, "element-web", host, port, "http"); err != nil {
					logger.Error(err, "failed to register Element Web service source (non-fatal)")
				}
				svcSuffix = "dns"
			}
			if err := i.Gateway.EnsureRoute(ctx, "element-web", nil, "element-web."+svcSuffix, port, "/"); err != nil {
				logger.Error(err, "failed to create Element Web route (non-fatal)")
			}
		}
	}

	// 3. LLM Provider
	if cfg.LLMAPIKey != "" {
		streamIdleTimeout := cfg.AIStreamIdleTimeoutSeconds
		if streamIdleTimeout <= 0 {
			streamIdleTimeout = 900
		}
		if err := i.Gateway.EnsureStreamIdleTimeout(ctx, streamIdleTimeout); err != nil {
			logger.Error(err, "failed to update Higress stream idle timeout (non-fatal)")
		}

		provider := cfg.LLMProvider
		if provider == "" {
			provider = "qwen"
		}

		switch provider {
		case "qwen":
			raw := map[string]interface{}{
				"hiclawMode":           true,
				"qwenEnableSearch":     false,
				"qwenEnableCompatible": true,
				"qwenFileIds":          []interface{}{},
			}
			if err := i.Gateway.EnsureAIProvider(ctx, gateway.AIProviderRequest{
				Name:     "qwen",
				Type:     "qwen",
				Tokens:   []string{cfg.LLMAPIKey},
				Protocol: "openai/v1",
				Raw:      raw,
			}); err != nil {
				logger.Error(err, "failed to create LLM provider (non-fatal)")
			}

		case "openai-compat":
			if cfg.OpenAIBaseURL == "" {
				// No custom base URL — fall back to official OpenAI endpoint
				logger.Info("HICLAW_OPENAI_BASE_URL not set, using official OpenAI endpoint")
				raw := map[string]interface{}{"hiclawMode": true}
				if err := i.Gateway.EnsureAIProvider(ctx, gateway.AIProviderRequest{
					Name:     "openai-compat",
					Type:     "openai",
					Tokens:   []string{cfg.LLMAPIKey},
					Protocol: "openai/v1",
					Raw:      raw,
				}); err != nil {
					logger.Error(err, "failed to create LLM provider (non-fatal)")
				}
			} else {
				// Parse URL to create DNS service source
				host, port, err := parseHostPort(cfg.OpenAIBaseURL)
				if err != nil {
					logger.Error(err, "failed to parse HICLAW_OPENAI_BASE_URL (non-fatal)")
				} else {
					proto := "https"
					if strings.HasPrefix(cfg.OpenAIBaseURL, "http://") {
						proto = "http"
					}
					if err := i.Gateway.EnsureServiceSource(ctx, "openai-compat", host, port, proto); err != nil {
						logger.Error(err, "failed to register openai-compat service source (non-fatal)")
					}
					// Wait for DNS service source to propagate before creating provider
					time.Sleep(2 * time.Second)
					raw := map[string]interface{}{
						"hiclawMode":              true,
						"openaiCustomUrl":         cfg.OpenAIBaseURL,
						"openaiCustomServiceName": "openai-compat.dns",
						"openaiCustomServicePort": port,
					}
					if err := i.Gateway.EnsureAIProvider(ctx, gateway.AIProviderRequest{
						Name:     "openai-compat",
						Type:     "openai",
						Tokens:   []string{cfg.LLMAPIKey},
						Protocol: "openai/v1",
						Raw:      raw,
					}); err != nil {
						logger.Error(err, "failed to create LLM provider (non-fatal)")
					}
				}
			}

		default:
			if cfg.OpenAIBaseURL != "" {
				// Provider name is unrecognized but a custom base URL is provided —
				// set up an openai-compatible provider with the custom endpoint.
				host, port, err := parseHostPort(cfg.OpenAIBaseURL)
				if err != nil {
					logger.Error(err, "failed to parse HICLAW_OPENAI_BASE_URL (non-fatal)")
				} else {
					proto := "https"
					if strings.HasPrefix(cfg.OpenAIBaseURL, "http://") {
						proto = "http"
					}
					if err := i.Gateway.EnsureServiceSource(ctx, provider, host, port, proto); err != nil {
						logger.Error(err, "failed to register service source for provider (non-fatal)")
					}
					time.Sleep(2 * time.Second)
					raw := map[string]interface{}{
						"hiclawMode":              true,
						"openaiCustomUrl":         cfg.OpenAIBaseURL,
						"openaiCustomServiceName": provider + ".dns",
						"openaiCustomServicePort": port,
					}
					if err := i.Gateway.EnsureAIProvider(ctx, gateway.AIProviderRequest{
						Name:     provider,
						Type:     "openai",
						Tokens:   []string{cfg.LLMAPIKey},
						Protocol: "openai/v1",
						Raw:      raw,
					}); err != nil {
						logger.Error(err, "failed to create LLM provider (non-fatal)")
					}
				}
			} else {
				raw := map[string]interface{}{"hiclawMode": true}
				if err := i.Gateway.EnsureAIProvider(ctx, gateway.AIProviderRequest{
					Name:     provider,
					Type:     "openai",
					Tokens:   []string{cfg.LLMAPIKey},
					Protocol: "openai/v1",
					Raw:      raw,
				}); err != nil {
					logger.Error(err, "failed to create LLM provider (non-fatal)")
				}
			}
		}

		// 4. AI Route skeleton — only creates the route if it does not yet
		// exist. We intentionally do NOT pass any authorization data here:
		// authConfig.allowedConsumers is owned exclusively by Manager/Worker
		// Reconcilers (via AuthorizeAIRoutes). This separation of ownership
		// avoids the restart-time race where the Initializer would otherwise
		// re-declare an empty allowedConsumers list and transiently lock out
		// the Manager/Workers.
		if err := i.Gateway.EnsureAIRoute(ctx, gateway.AIRouteRequest{
			Name:       "default-ai-route",
			PathPrefix: "/v1",
			Provider:   provider,
		}); err != nil {
			logger.Error(err, "failed to create AI route (non-fatal)")
		}
	}

	// 5. Remove Higress default landing page
	if err := i.Gateway.DeleteRoute(ctx, "default"); err != nil {
		logger.Error(err, "failed to remove default route (non-fatal)")
	}

	return nil
}

// parseHostPort extracts host and port from a URL like "http://host:port".
func parseHostPort(rawURL string) (string, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, err
	}
	host := u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		if u.Scheme == "https" {
			return host, 443, nil
		}
		return host, 80, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	return host, port, nil
}

func (i *Initializer) ensureManagerCR(ctx context.Context) error {
	logger := ctrl.Log.WithName("initializer")

	dynClient, err := dynamic.NewForConfig(i.RestCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    v1beta1.GroupName,
		Version:  v1beta1.Version,
		Resource: "managers",
	}

	ns := i.Config.Namespace
	name := "default"

	_, err = dynClient.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		logger.Info("Manager CR already exists, skipping creation")
		return nil
	}

	spec := map[string]interface{}{
		"model":   i.Config.ManagerModel,
		"runtime": i.Config.ManagerRuntime,
	}
	if i.Config.ManagerImage != "" {
		spec["image"] = i.Config.ManagerImage
	}
	if i.Config.ManagerResources != nil {
		spec["resources"] = i.Config.ManagerResources
	}

	metadata := map[string]interface{}{
		"name":      name,
		"namespace": ns,
	}
	if i.Config.ControllerName != "" {
		metadata["labels"] = map[string]interface{}{
			v1beta1.LabelController: i.Config.ControllerName,
		}
	}
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": v1beta1.GroupName + "/" + v1beta1.Version,
			"kind":       "Manager",
			"metadata":   metadata,
			"spec":       spec,
		},
	}

	_, err = dynClient.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Manager CR: %w", err)
	}
	return nil
}

// retry calls fn repeatedly until it succeeds or the timeout is reached.
func retry(ctx context.Context, interval, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %v: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// isMatrixConnError returns true if the error indicates a transport-level failure
// (connection refused, DNS error, etc.) as opposed to an HTTP-level response.
func isMatrixConnError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sub := range []string{"connection refused", "no such host", "dial tcp", "i/o timeout", "EOF"} {
		if contains(msg, sub) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
