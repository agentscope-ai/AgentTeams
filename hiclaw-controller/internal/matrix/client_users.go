package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (c *TuwunelClient) ensureAdminToken(ctx context.Context) (string, error) {
	if t, ok := c.adminToken.Load().(string); ok && t != "" {
		return t, nil
	}
	token, err := c.Login(ctx, c.config.AdminUser, c.config.AdminPassword)
	if err != nil {
		return "", fmt.Errorf("admin login: %w", err)
	}
	c.adminToken.Store(token)
	return token, nil
}
func (c *TuwunelClient) EnsureUser(ctx context.Context, req EnsureUserRequest) (*UserCredentials, error) {
	password := req.Password
	if password == "" {
		var err error
		password, err = GeneratePassword(16)
		if err != nil {
			return nil, fmt.Errorf("generate password: %w", err)
		}
	}

	// Try registration first
	regBody := map[string]interface{}{
		"username": req.Username,
		"password": password,
		"auth": map[string]string{
			"type":  "m.login.registration_token",
			"token": c.config.RegistrationToken,
		},
	}
	var regResp struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		ErrCode     string `json:"errcode"`
		Error       string `json:"error"`
	}

	statusCode, _, err := c.doJSON(ctx, http.MethodPost,
		"/_matrix/client/v3/register", "", regBody, &regResp)
	if err != nil {
		return nil, fmt.Errorf("register user %s: %w", req.Username, err)
	}

	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return &UserCredentials{
			UserID:      regResp.UserID,
			AccessToken: regResp.AccessToken,
			Password:    password,
			Created:     true,
		}, nil
	}

	// Only fall back to login if the user already exists
	if regResp.ErrCode != "" && regResp.ErrCode != "M_USER_IN_USE" {
		return nil, fmt.Errorf("register user %s: %s (%s)", req.Username, regResp.ErrCode, regResp.Error)
	}

	// Registration failed with M_USER_IN_USE — try login
	token, err := c.Login(ctx, req.Username, password)
	if err == nil {
		return &UserCredentials{
			UserID:      c.UserID(req.Username),
			AccessToken: token,
			Password:    password,
			Created:     false,
		}, nil
	}

	// Orphan recovery: Matrix still has a userid_password entry for
	// this username (either deactivated by a prior delete flow, or the
	// password was rotated out-of-band), so login with our current
	// password fails. Since Tuwunel cannot hard-delete users, we
	// reactivate via the admin bot's reset-password command and retry
	// login.
	userID := c.UserID(req.Username)
	cmd := fmt.Sprintf("!admin users reset-password %s %s", userID, password)
	if adminErr := c.AdminCommand(ctx, cmd); adminErr != nil {
		return nil, fmt.Errorf("user %s exists but login failed (%v) and orphan recovery failed: %w",
			req.Username, err, adminErr)
	}

	const maxAttempts = 5
	baseDelay := c.orphanRetryBaseDelay
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	var lastErr = err
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(baseDelay * time.Duration(attempt)):
		}
		token, lastErr = c.Login(ctx, req.Username, password)
		if lastErr == nil {
			return &UserCredentials{
				UserID:      userID,
				AccessToken: token,
				Password:    password,
				Created:     false,
			}, nil
		}
	}
	return nil, fmt.Errorf("user %s exists, orphan recovery issued but login still failing: %w",
		req.Username, lastErr)
}
func (c *TuwunelClient) Login(ctx context.Context, username, password string) (string, error) {
	body := map[string]interface{}{
		"type": "m.login.password",
		"identifier": map[string]string{
			"type": "m.id.user",
			"user": username,
		},
		"password": password,
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		"/_matrix/client/v3/login", "", body, &resp)
	if err != nil {
		return "", fmt.Errorf("login %s: %w", username, err)
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("login %s: HTTP %d: %s", username, statusCode, truncate(respBody, 500))
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("login %s: empty access token", username)
	}
	return resp.AccessToken, nil
}

// EnsureAppServiceUser registers a user via the Matrix Application Service API.
// It uses the as_token as Bearer authentication instead of a registration token.
// If the user already exists (M_USER_IN_USE), it falls back to LoginAppServiceUser.
func (c *TuwunelClient) EnsureAppServiceUser(ctx context.Context, username string) (*UserCredentials, error) {
	regBody := map[string]interface{}{
		"type":     "m.login.application_service",
		"username": username,
	}
	var regResp struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		ErrCode     string `json:"errcode"`
		Error       string `json:"error"`
	}

	logger := log.FromContext(ctx).WithValues("matrixUserID", c.UserID(username), "localpart", username)

	statusCode, _, err := c.doJSONWithASToken(ctx, http.MethodPost,
		"/_matrix/client/v3/register", regBody, &regResp)
	if err != nil {
		logger.Error(err, "AppService register request failed (transport)")
		return nil, fmt.Errorf("AS register user %s: %w", username, err)
	}

	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		logger.Info("AppService registered new Matrix account",
			"httpStatus", statusCode, "registeredUserID", regResp.UserID, "hasAccessToken", regResp.AccessToken != "")
		return &UserCredentials{
			UserID:      regResp.UserID,
			AccessToken: regResp.AccessToken,
			Password:    "",
			Created:     true,
		}, nil
	}

	// User already exists → fall back to AS login
	if regResp.ErrCode == "M_USER_IN_USE" {
		logger.Info("Matrix account already exists; falling back to AppService login", "httpStatus", statusCode)
		token, loginErr := c.LoginAppServiceUser(ctx, username)
		if loginErr != nil {
			if errors.Is(loginErr, ErrAppServiceNotReady) {
				logger.Info("Matrix AppService token not active yet during login fallback; will retry")
				return nil, loginErr
			}
			logger.Error(loginErr, "AppService login failed for existing Matrix account")
			return nil, fmt.Errorf("AS user %s exists but AS login failed: %w", username, loginErr)
		}
		return &UserCredentials{
			UserID:      c.UserID(username),
			AccessToken: token,
			Password:    "",
			Created:     false,
		}, nil
	}

	// Startup race: homeserver does not recognize the as_token yet. This is
	// transient and self-heals once cluster init registers/verifies the
	// AppService, so report it as retryable instead of a hard error.
	if statusCode == http.StatusUnauthorized && regResp.ErrCode == "M_UNKNOWN_TOKEN" {
		logger.Info("Matrix AppService token not active yet; will retry once it is registered/verified",
			"httpStatus", statusCode)
		return nil, fmt.Errorf("AS register user %s: %w", username, ErrAppServiceNotReady)
	}

	logger.Error(nil, "AppService register rejected by homeserver",
		"httpStatus", statusCode, "errcode", regResp.ErrCode, "error", regResp.Error)
	return nil, fmt.Errorf("AS register user %s: %s (%s)", username, regResp.ErrCode, regResp.Error)
}

// LoginAppServiceUser obtains an access token for a user via the Application
// Service login flow. The as_token authenticates the request; no user password
// is needed.
func (c *TuwunelClient) LoginAppServiceUser(ctx context.Context, username string) (string, error) {
	body := map[string]interface{}{
		"type": "m.login.application_service",
		"identifier": map[string]string{
			"type": "m.id.user",
			"user": username,
		},
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		ErrCode     string `json:"errcode"`
		Error       string `json:"error"`
	}

	statusCode, respBody, err := c.doJSONWithASToken(ctx, http.MethodPost,
		"/_matrix/client/v3/login", body, &resp)
	if err != nil {
		return "", fmt.Errorf("AS login %s: %w", username, err)
	}
	if statusCode != http.StatusOK {
		if statusCode == http.StatusUnauthorized && resp.ErrCode == "M_UNKNOWN_TOKEN" {
			return "", fmt.Errorf("AS login %s: %w", username, ErrAppServiceNotReady)
		}
		return "", fmt.Errorf("AS login %s: HTTP %d %s %s: %s",
			username, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("AS login %s: empty access token", username)
	}
	return resp.AccessToken, nil
}

// SetPasswordAsAdmin sets a user's password via the Tuwunel admin bot command.
// This is used in AppService mode to set initial passwords for Human users
// so they can still log in via Element with username/password.
func (c *TuwunelClient) SetPasswordAsAdmin(ctx context.Context, userID, password string) error {
	cmd := fmt.Sprintf("!admin users reset-password %s %s", userID, password)
	return c.AdminCommand(ctx, cmd)
}

// doJSONWithASToken performs an HTTP request authenticated with the AppService
// as_token instead of a user access token. Reuses the same JSON plumbing as
// doJSON but substitutes the Bearer token.
func (c *TuwunelClient) doJSONWithASToken(ctx context.Context, method, path string, reqBody interface{}, respOut interface{}) (int, []byte, error) {
	return c.doJSON(ctx, method, path, c.config.AppServiceToken, reqBody, respOut)
}

// VerifyAccessToken checks whether a user access token is still valid
// by calling GET /_matrix/client/v3/account/whoami.
func (c *TuwunelClient) VerifyAccessToken(ctx context.Context, accessToken string) error {
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		"/_matrix/client/v3/account/whoami", accessToken, nil, nil)
	if err != nil {
		return fmt.Errorf("verify access token: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("verify access token: HTTP %d: %s", statusCode, truncate(respBody, 200))
	}
	return nil
}
func (c *TuwunelClient) SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error {
	path := fmt.Sprintf("/_matrix/client/v3/profile/%s/displayname", url.PathEscape(userID))
	body := map[string]string{"displayname": displayName}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPut, path, accessToken, body, nil)
	if err != nil {
		return fmt.Errorf("set displayName for %s: %w", userID, err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("set displayName for %s: HTTP %d: %s", userID, statusCode, truncate(respBody, 500))
	}
	return nil
}
func (c *TuwunelClient) accessTokenUserID(ctx context.Context, accessToken string) (string, error) {
	var resp struct {
		UserID string `json:"user_id"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		"/_matrix/client/v3/account/whoami", accessToken, nil, &resp)
	if err != nil {
		return "", fmt.Errorf("whoami: %w", err)
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("whoami: HTTP %d: %s", statusCode, truncate(respBody, 200))
	}
	if resp.UserID == "" {
		return "", errors.New("whoami: empty user_id")
	}
	return resp.UserID, nil
}

// ResolveRoomAlias implements Client.ResolveRoomAlias.
