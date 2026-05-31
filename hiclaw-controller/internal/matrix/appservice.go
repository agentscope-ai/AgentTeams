package matrix

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// RenderAppServiceRegistration builds an AppServiceRegistration from the
// current Matrix config. The registration covers all local users (exclusive)
// and HiClaw-managed room aliases.
func RenderAppServiceRegistration(cfg Config) AppServiceRegistration {
	domain := cfg.Domain
	return AppServiceRegistration{
		ID:              cfg.AppServiceID,
		URL:             nil, // Phase 1: no push from homeserver
		ASToken:         cfg.AppServiceToken,
		HSToken:         cfg.AppServiceHSToken,
		SenderLocalpart: cfg.AppServiceSenderLocalpart,
		RateLimited:     false,
		Namespaces: AppServiceNamespaces{
			Users: []AppServiceNamespace{
				{Exclusive: true, Regex: fmt.Sprintf("@.*:%s", domain)},
			},
			Aliases: []AppServiceNamespace{
				{Exclusive: true, Regex: fmt.Sprintf("#hiclaw-.*:%s", domain)},
			},
			Rooms: []AppServiceNamespace{},
		},
	}
}

// RegisterAppService sends the AppService registration YAML to the Tuwunel
// admin bot via the #admins room. Processing is asynchronous; call
// AppServiceSmokeTest afterwards to verify the registration took effect.
func (c *TuwunelClient) RegisterAppService(ctx context.Context, reg AppServiceRegistration) error {
	yamlBytes, err := yaml.Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal appservice registration: %w", err)
	}

	// Format: !admin appservices register followed by fenced YAML block
	command := fmt.Sprintf("!admin appservices register\n```yaml\n%s```", string(yamlBytes))

	if err := c.AdminCommand(ctx, command); err != nil {
		return fmt.Errorf("send appservice registration command: %w", err)
	}

	// Give the admin bot time to process the command
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	return nil
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
