package matrix

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// RenderAppServiceRegistration builds an AppServiceRegistration from the
// current Matrix config.
//
// Security model — user namespace:
//
//	By default the registration claims the exclusive "@.*:<domain>" user
//	namespace, which means the as_token can impersonate EVERY local user on
//	the homeserver. This is only safe when the homeserver is exclusively
//	HiClaw-managed — i.e. HiClaw provisions the homeserver and every local
//	user on it. That is the only supported deployment mode: the embedded
//	install ships an embedded Tuwunel, and Helm's 00-validate.yaml rejects
//	anything but matrix.provider=tuwunel + matrix.mode=managed.
//
//	DO NOT enable AppService mode against a shared or pre-existing
//	homeserver that also hosts non-HiClaw users. Doing so would let the
//	as_token impersonate those users. Instead set
//	AGENTTEAMS_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX to a restrictive regex
//	(e.g. "@hiclaw-.*:<domain>") that covers only HiClaw-managed localparts,
//	and ensure HiClaw-managed users are created under that prefix.
func RenderAppServiceRegistration(cfg Config) AppServiceRegistration {
	domain := cfg.Domain
	userRegex := cfg.AppServiceUserNamespaceRegex
	if userRegex == "" {
		userRegex = fmt.Sprintf("@.*:%s", domain)
	}
	var pushURL *string
	if cfg.AppServicePushURL != "" {
		pushURL = &cfg.AppServicePushURL
	}
	return AppServiceRegistration{
		ID:              cfg.AppServiceID,
		URL:             pushURL,
		ASToken:         cfg.AppServiceToken,
		HSToken:         cfg.AppServiceHSToken,
		SenderLocalpart: cfg.AppServiceSenderLocalpart,
		RateLimited:     false,
		Namespaces: AppServiceNamespaces{
			Users: []AppServiceNamespace{
				{Exclusive: true, Regex: userRegex},
			},
			Aliases: []AppServiceNamespace{
				{Exclusive: false, Regex: fmt.Sprintf("#hiclaw-.*:%s", domain)},
			},
			Rooms: []AppServiceNamespace{},
		},
	}
}

// RegisterAppService sends the AppService registration YAML to the Tuwunel
// admin bot via the #admins room. It first attempts a smoke test; if the
// existing registration already works with the current token, registration
// is skipped (idempotent across restarts). When the smoke test fails (e.g.,
// token was rotated), it unregisters the old registration first to ensure
// clean state regardless of Tuwunel's overwrite semantics for same-ID
// registrations.
func (c *TuwunelClient) RegisterAppService(ctx context.Context, reg AppServiceRegistration) error {
	// Fast path: current token already works.
	if err := c.AppServiceSmokeTest(ctx); err == nil {
		return nil
	}

	// Slow path: token changed or first registration.
	// Unregister first to ensure clean state regardless of whether Tuwunel
	// overwrites or rejects same-ID registrations with different tokens.
	// Best-effort: ignore errors (registration may not exist yet).
	_ = c.UnregisterAppService(ctx, reg.ID)

	yamlBytes, err := yaml.Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal appservice registration: %w", err)
	}

	command := fmt.Sprintf("!admin appservices register\n```yaml\n%s```", string(yamlBytes))

	if err := c.AdminCommand(ctx, command); err != nil {
		return fmt.Errorf("send appservice registration command: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	return nil
}

// UnregisterAppService removes the AppService registration from the homeserver.
// Uses the admin bot command; works regardless of the current as_token validity
// because admin commands authenticate via admin user login, not as_token.
func (c *TuwunelClient) UnregisterAppService(ctx context.Context, id string) error {
	cmd := fmt.Sprintf("!admin appservices unregister %s", id)
	return c.AdminCommand(ctx, cmd)
}

// AppServiceSmokeTest verifies that the AppService registration is active by
// attempting an AS login as the sender_localpart user. Retries up to 5 times
// with 2-second intervals to account for async admin command processing.
func (c *TuwunelClient) AppServiceSmokeTest(ctx context.Context) error {
	sender := c.config.AppServiceSenderLocalpart
	if sender == "" {
		return fmt.Errorf("appservice smoke test: sender_localpart not configured")
	}

	const maxAttempts = 5
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		token, err := c.LoginAppServiceUser(ctx, sender)
		if err == nil && token != "" {
			return nil
		}
		lastErr = err

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return fmt.Errorf("appservice smoke test failed after %d attempts: %w", maxAttempts, lastErr)
}
