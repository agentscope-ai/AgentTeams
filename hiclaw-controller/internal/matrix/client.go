package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Client abstracts Matrix homeserver operations.
// Implementations: TuwunelClient (current), future SynapseClient.
type Client interface {
	// EnsureUser registers a user or logs in if the account already exists.
	// Returns credentials regardless of whether the user was newly created.
	EnsureUser(ctx context.Context, req EnsureUserRequest) (*UserCredentials, error)

	// CreateRoom creates a new Matrix room with the given configuration.
	// When req.RoomAliasName is non-empty the call is idempotent: if a room
	// with that alias already exists on the homeserver, the existing RoomID
	// is resolved and returned with Created=false. Callers SHOULD always
	// populate RoomAliasName for controller-managed rooms to avoid duplicate
	// creation caused by K8s informer cache lag or concurrent reconciles.
	CreateRoom(ctx context.Context, req CreateRoomRequest) (*RoomInfo, error)

	// ResolveRoomAlias looks up the RoomID a Matrix alias currently points
	// to. Returns (roomID, true, nil) on hit, ("", false, nil) when the
	// alias does not exist (M_NOT_FOUND), and ("", false, err) on any
	// other error. The alias argument MUST be the full form
	// "#localpart:server".
	ResolveRoomAlias(ctx context.Context, alias string) (string, bool, error)

	// DeleteRoomAlias removes a Matrix alias so a future CreateRoom with the
	// same localpart starts fresh. Idempotent: a missing alias returns nil.
	// The alias argument MUST be the full form "#localpart:server".
	DeleteRoomAlias(ctx context.Context, alias string) error

	// JoinRoom makes the user identified by token join the given room.
	JoinRoom(ctx context.Context, roomID, userToken string) error

	// LeaveRoom makes the user identified by token leave the given room.
	LeaveRoom(ctx context.Context, roomID, userToken string) error

	// SendMessage sends a plain-text message to a room.
	SendMessage(ctx context.Context, roomID, token, body string) error

	// SendMessageAsAdmin sends a plain-text message to a room using the
	// homeserver-admin user identity. Used by the controller to inject
	// system-level prompts (e.g. the first-boot Manager onboarding
	// welcome) into rooms where it does not own the recipient's token.
	// Mirrors the AdminCommand pattern: ensures the admin token is
	// cached, then delegates to SendMessage.
	SendMessageAsAdmin(ctx context.Context, roomID, body string) error

	// Login obtains an access token for an existing user.
	Login(ctx context.Context, username, password string) (string, error)

	// SetDisplayName updates a user's profile displayname.
	SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error

	// AdminCommand sends a `!admin ...` text message to the tuwunel admin
	// bot room (#admins:<domain>). Fire-and-forget: delivery of the
	// message is confirmed but execution of the admin action is not.
	AdminCommand(ctx context.Context, command string) error

	// ListJoinedRooms returns the list of room IDs the user identified
	// by userToken is currently joined to.
	ListJoinedRooms(ctx context.Context, userToken string) ([]string, error)

	// ListRoomMembers returns users currently in the room whose membership
	// is "join" or "invite". leave/ban/knock entries are filtered out.
	// Uses an admin access token internally.
	ListRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error)

	// ListRoomMembersWithToken is the same operation using the supplied
	// access token. The token's user must be allowed to read room state.
	ListRoomMembersWithToken(ctx context.Context, roomID, userToken string) ([]RoomMember, error)

	// InviteToRoom invites userID to roomID using an admin access token.
	// Idempotent: returns nil if the user is already joined/invited.
	InviteToRoom(ctx context.Context, roomID, userID string) error

	// InviteToRoomWithToken invites userID to roomID using the supplied token.
	// The token's user must already be joined in the room.
	InviteToRoomWithToken(ctx context.Context, roomID, userID, inviterToken string) error

	// KickFromRoom removes userID from roomID using an admin access token.
	// Idempotent: returns nil if the user is not currently in the room.
	KickFromRoom(ctx context.Context, roomID, userID, reason string) error

	// KickFromRoomWithToken removes userID from roomID using the supplied token.
	// The token's user must be joined and have enough power in the room.
	KickFromRoomWithToken(ctx context.Context, roomID, userID, reason, kickerToken string) error

	// UserID builds a full Matrix user ID from a localpart.
	UserID(localpart string) string

	// EnsureAppServiceUser registers a user via the Application Service API.
	// Uses as_token authentication instead of registration_token.
	// Returns credentials with empty Password. If the user already exists,
	// falls back to LoginAppServiceUser.
	EnsureAppServiceUser(ctx context.Context, username string) (*UserCredentials, error)

	// LoginAppServiceUser obtains an access token for a user via the
	// Application Service login flow (m.login.application_service).
	// The as_token is used as Bearer authentication; no password needed.
	LoginAppServiceUser(ctx context.Context, username string) (string, error)

	// SetPasswordAsAdmin sets a user's password via the Tuwunel admin bot.
	// Used to set initial passwords for Human users in AppService mode so
	// they can still log in via Element.
	SetPasswordAsAdmin(ctx context.Context, userID, password string) error

	// RegisterAppService registers an Application Service with the homeserver
	// via the admin bot command. Includes smoke-test-first idempotency and
	// unregister-before-register fallback for safe token rotation.
	RegisterAppService(ctx context.Context, reg AppServiceRegistration) error

	// UnregisterAppService removes an Application Service registration by ID.
	// Uses admin bot command; does not require a valid as_token.
	UnregisterAppService(ctx context.Context, id string) error

	// AppServiceSmokeTest verifies that a previously registered AppService
	// is active by attempting an AS login as the sender_localpart user.
	AppServiceSmokeTest(ctx context.Context) error

	// VerifyAccessToken checks whether a user access token is still valid
	// by calling GET /_matrix/client/v3/account/whoami. Returns nil if valid.
	VerifyAccessToken(ctx context.Context, accessToken string) error
}

// TuwunelClient implements Client for Tuwunel (conduwuit) homeservers.
type TuwunelClient struct {
	config      Config
	http        *http.Client
	adminToken  atomic.Value // cached admin access token (string)
	adminRoomID atomic.Value // cached admin room ID (string), resolved from #admins:<domain>

	// orphanRetryBaseDelay is the base backoff between Login retries
	// after issuing an admin reset-password command. Exposed as a field
	// (not a const) so tests can collapse the delay.
	orphanRetryBaseDelay time.Duration
}

// NewTuwunelClient creates a Matrix client for a Tuwunel homeserver.
func NewTuwunelClient(cfg Config, httpClient *http.Client) *TuwunelClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &TuwunelClient{
		config:               cfg,
		http:                 httpClient,
		orphanRetryBaseDelay: 500 * time.Millisecond,
	}
}

func (c *TuwunelClient) UserID(localpart string) string {
	return fmt.Sprintf("@%s:%s", localpart, c.config.Domain)
}

// ensureAdminToken obtains and caches an admin access token via Login.
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

	statusCode, _, err := c.doJSONWithASToken(ctx, http.MethodPost,
		"/_matrix/client/v3/register", regBody, &regResp)
	if err != nil {
		return nil, fmt.Errorf("AS register user %s: %w", username, err)
	}

	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return &UserCredentials{
			UserID:      regResp.UserID,
			AccessToken: regResp.AccessToken,
			Password:    "",
			Created:     true,
		}, nil
	}

	// User already exists → fall back to AS login
	if regResp.ErrCode == "M_USER_IN_USE" {
		token, loginErr := c.LoginAppServiceUser(ctx, username)
		if loginErr != nil {
			return nil, fmt.Errorf("AS user %s exists but AS login failed: %w", username, loginErr)
		}
		return &UserCredentials{
			UserID:      c.UserID(username),
			AccessToken: token,
			Password:    "",
			Created:     false,
		}, nil
	}

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

func (c *TuwunelClient) CreateRoom(ctx context.Context, req CreateRoomRequest) (*RoomInfo, error) {
	token := req.CreatorToken
	if token == "" {
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("create room %q: %w", req.Name, err)
		}
	}

	body := map[string]interface{}{
		"name":      req.Name,
		"topic":     req.Topic,
		"invite":    req.Invite,
		"preset":    "trusted_private_chat",
		"is_direct": req.IsDirect,
	}

	if req.RoomAliasName != "" {
		body["room_alias_name"] = req.RoomAliasName
	}

	if len(req.PowerLevels) > 0 {
		body["power_level_content_override"] = map[string]interface{}{
			"users": req.PowerLevels,
		}
	}

	if req.E2EE {
		body["initial_state"] = []map[string]interface{}{
			{
				"type":      "m.room.encryption",
				"state_key": "",
				"content": map[string]string{
					"algorithm": "m.megolm.v1.aes-sha2",
				},
			},
		}
	}

	var resp struct {
		RoomID  string `json:"room_id"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		"/_matrix/client/v3/createRoom", token, body, &resp)
	if err != nil {
		return nil, fmt.Errorf("create room %q: %w", req.Name, err)
	}

	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		if resp.RoomID == "" {
			return nil, fmt.Errorf("create room %q: empty room_id in response", req.Name)
		}
		return &RoomInfo{RoomID: resp.RoomID, Created: true}, nil
	}

	// Alias already claimed by a prior reconcile: resolve it and treat as
	// idempotent success. This is the sole path that turns informer-cache
	// lag / concurrent reconciles into a no-op instead of a duplicate room.
	if req.RoomAliasName != "" && resp.ErrCode == "M_ROOM_IN_USE" {
		alias := roomAliasFullFor(c.config.Domain, req.RoomAliasName)
		existingID, found, resolveErr := c.ResolveRoomAlias(ctx, alias)
		if resolveErr != nil {
			return nil, fmt.Errorf("create room %q: alias %s in use, resolve failed: %w",
				req.Name, alias, resolveErr)
		}
		if !found {
			return nil, fmt.Errorf("create room %q: alias %s reported in use but resolve returned not found",
				req.Name, alias)
		}
		return &RoomInfo{RoomID: existingID, Created: false}, nil
	}

	return nil, fmt.Errorf("create room %q: HTTP %d %s %s: %s",
		req.Name, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

// ResolveRoomAlias implements Client.ResolveRoomAlias.
func (c *TuwunelClient) ResolveRoomAlias(ctx context.Context, alias string) (string, bool, error) {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve alias %s: %w", alias, err)
	}

	var resp struct {
		RoomID  string `json:"room_id"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		"/_matrix/client/v3/directory/room/"+encodeAlias(alias),
		token, nil, &resp)
	if err != nil {
		return "", false, fmt.Errorf("resolve alias %s: %w", alias, err)
	}
	if statusCode == http.StatusOK {
		if resp.RoomID == "" {
			return "", false, fmt.Errorf("resolve alias %s: empty room_id in response", alias)
		}
		return resp.RoomID, true, nil
	}
	if statusCode == http.StatusNotFound || resp.ErrCode == "M_NOT_FOUND" {
		return "", false, nil
	}
	return "", false, fmt.Errorf("resolve alias %s: HTTP %d %s %s: %s",
		alias, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

// DeleteRoomAlias implements Client.DeleteRoomAlias.
func (c *TuwunelClient) DeleteRoomAlias(ctx context.Context, alias string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("delete alias %s: %w", alias, err)
	}

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodDelete,
		"/_matrix/client/v3/directory/room/"+encodeAlias(alias),
		token, nil, &resp)
	if err != nil {
		return fmt.Errorf("delete alias %s: %w", alias, err)
	}
	if statusCode == http.StatusOK {
		return nil
	}
	if statusCode == http.StatusNotFound || resp.ErrCode == "M_NOT_FOUND" {
		return nil
	}
	return fmt.Errorf("delete alias %s: HTTP %d %s %s: %s",
		alias, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

func (c *TuwunelClient) JoinRoom(ctx context.Context, roomID, userToken string) error {
	encodedRoom := encodeRoomID(roomID)
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", encodedRoom),
		userToken, map[string]interface{}{}, nil)
	if err != nil {
		return fmt.Errorf("join room %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("join room %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}

func (c *TuwunelClient) LeaveRoom(ctx context.Context, roomID, userToken string) error {
	token := userToken
	if token == "" {
		var err error
		token, err = c.ensureAdminToken(ctx)
		if err != nil {
			return fmt.Errorf("leave room %s: %w", roomID, err)
		}
	}
	encodedRoom := encodeRoomID(roomID)
	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/leave", encodedRoom),
		token, map[string]interface{}{}, nil)
	if err != nil {
		return fmt.Errorf("leave room %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("leave room %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}

func (c *TuwunelClient) SendMessage(ctx context.Context, roomID, token, body string) error {
	encodedRoom := encodeRoomID(roomID)
	txnID := fmt.Sprintf("hc-%d", txnCounter.Add(1))
	msg := map[string]string{
		"msgtype": "m.text",
		"body":    body,
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s", encodedRoom, txnID),
		token, msg, nil)
	if err != nil {
		return fmt.Errorf("send message to %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("send message to %s: HTTP %d: %s", roomID, statusCode, truncate(respBody, 500))
	}
	return nil
}

// ensureAdminRoomID resolves the Tuwunel admin room via the well-known
// alias "#admins:<domain>" and caches the result for the lifetime of the
// client. Controller restart re-resolves.
func (c *TuwunelClient) ensureAdminRoomID(ctx context.Context) (string, error) {
	if r, ok := c.adminRoomID.Load().(string); ok && r != "" {
		return r, nil
	}
	alias := fmt.Sprintf("#admins:%s", c.config.Domain)
	path := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)

	var resp struct {
		RoomID string `json:"room_id"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet, path, "", nil, &resp)
	if err != nil {
		return "", fmt.Errorf("resolve admin room alias %s: %w", alias, err)
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("resolve admin room alias %s: HTTP %d: %s", alias, statusCode, truncate(respBody, 500))
	}
	if resp.RoomID == "" {
		return "", fmt.Errorf("resolve admin room alias %s: empty room_id", alias)
	}
	c.adminRoomID.Store(resp.RoomID)
	return resp.RoomID, nil
}

// SendMessageAsAdmin sends body to roomID using the cached admin token.
// Errors from token acquisition and message send are wrapped to identify
// the failing stage. Used by the controller for system-level prompts that
// must originate from the admin identity (e.g. Manager onboarding welcome).
func (c *TuwunelClient) SendMessageAsAdmin(ctx context.Context, roomID, body string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("send admin message: %w", err)
	}
	if err := c.SendMessage(ctx, roomID, token, body); err != nil {
		return fmt.Errorf("send admin message: %w", err)
	}
	return nil
}

// AdminCommand sends a command message to the Tuwunel admin bot room as
// the admin user. The bot parses messages starting with "!admin" in the
// admin room. Processing is asynchronous; this call is fire-and-forget.
func (c *TuwunelClient) AdminCommand(ctx context.Context, command string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	roomID, err := c.ensureAdminRoomID(ctx)
	if err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	if err := c.SendMessage(ctx, roomID, token, command); err != nil {
		return fmt.Errorf("admin command: %w", err)
	}
	return nil
}

func (c *TuwunelClient) ListRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error) {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("list members %s: %w", roomID, err)
	}
	return c.ListRoomMembersWithToken(ctx, roomID, token)
}

func (c *TuwunelClient) ListRoomMembersWithToken(ctx context.Context, roomID, userToken string) ([]RoomMember, error) {
	if userToken == "" {
		return nil, fmt.Errorf("list members %s: empty user token", roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	var resp struct {
		Chunk []struct {
			StateKey string `json:"state_key"`
			Content  struct {
				Membership string `json:"membership"`
			} `json:"content"`
		} `json:"chunk"`
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/members", encodedRoom),
		userToken, nil, &resp)
	if err != nil {
		return nil, fmt.Errorf("list members %s: %w", roomID, err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("list members %s: HTTP %d %s %s: %s",
			roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
	}

	members := make([]RoomMember, 0, len(resp.Chunk))
	for _, ev := range resp.Chunk {
		if ev.StateKey == "" {
			continue
		}
		if ev.Content.Membership != "join" && ev.Content.Membership != "invite" {
			continue
		}
		members = append(members, RoomMember{
			UserID:     ev.StateKey,
			Membership: ev.Content.Membership,
		})
	}
	return members, nil
}

func (c *TuwunelClient) InviteToRoom(ctx context.Context, roomID, userID string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("invite %s to %s: %w", userID, roomID, err)
	}
	return c.InviteToRoomWithToken(ctx, roomID, userID, token)
}

func (c *TuwunelClient) InviteToRoomWithToken(ctx context.Context, roomID, userID, inviterToken string) error {
	if inviterToken == "" {
		return fmt.Errorf("invite %s to %s: empty inviter token", userID, roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", encodedRoom),
		inviterToken, map[string]string{"user_id": userID}, &resp)
	if err != nil {
		return fmt.Errorf("invite %s to %s: %w", userID, roomID, err)
	}
	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return nil
	}
	// Idempotent: user already in the room.
	if statusCode == http.StatusForbidden && resp.ErrCode == "M_FORBIDDEN" {
		lower := strings.ToLower(resp.Error)
		if strings.Contains(lower, "already in") || strings.Contains(lower, "already a member") {
			return nil
		}
	}
	return fmt.Errorf("invite %s to %s: HTTP %d %s %s: %s",
		userID, roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

func (c *TuwunelClient) KickFromRoom(ctx context.Context, roomID, userID, reason string) error {
	token, err := c.ensureAdminToken(ctx)
	if err != nil {
		return fmt.Errorf("kick %s from %s: %w", userID, roomID, err)
	}
	return c.KickFromRoomWithToken(ctx, roomID, userID, reason, token)
}

func (c *TuwunelClient) KickFromRoomWithToken(ctx context.Context, roomID, userID, reason, kickerToken string) error {
	if kickerToken == "" {
		return fmt.Errorf("kick %s from %s: empty kicker token", userID, roomID)
	}
	encodedRoom := encodeRoomID(roomID)

	body := map[string]string{"user_id": userID}
	if reason != "" {
		body["reason"] = reason
	}

	var resp struct {
		ErrCode string `json:"errcode"`
		Error   string `json:"error"`
	}

	statusCode, respBody, err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/kick", encodedRoom),
		kickerToken, body, &resp)
	if err != nil {
		return fmt.Errorf("kick %s from %s: %w", userID, roomID, err)
	}
	if statusCode == http.StatusOK || statusCode == http.StatusCreated {
		return nil
	}
	// Idempotent: user not in the room (or already left).
	if statusCode == http.StatusNotFound {
		return nil
	}
	if statusCode == http.StatusForbidden && resp.ErrCode == "M_FORBIDDEN" {
		lower := strings.ToLower(resp.Error)
		if strings.Contains(lower, "not in") || strings.Contains(lower, "not a member") ||
			strings.Contains(lower, "cannot kick") {
			return nil
		}
	}
	return fmt.Errorf("kick %s from %s: HTTP %d %s %s: %s",
		userID, roomID, statusCode, resp.ErrCode, resp.Error, truncate(respBody, 500))
}

// ListJoinedRooms returns the room IDs joined by the user identified by
// the given access token.
func (c *TuwunelClient) ListJoinedRooms(ctx context.Context, userToken string) ([]string, error) {
	var resp struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	statusCode, respBody, err := c.doJSON(ctx, http.MethodGet,
		"/_matrix/client/v3/joined_rooms", userToken, nil, &resp)
	if err != nil {
		return nil, fmt.Errorf("list joined rooms: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("list joined rooms: HTTP %d: %s", statusCode, truncate(respBody, 500))
	}
	return resp.JoinedRooms, nil
}

// doJSON performs an HTTP request with JSON body/response.
// Returns the HTTP status code, the raw response body, and any transport/decode error.
// If respOut is nil, the response body is not decoded (but still read and returned).
// The raw body is always returned (possibly nil) so callers can include it in
// diagnostic error messages even when respOut is set.
func (c *TuwunelClient) doJSON(ctx context.Context, method, path, token string, reqBody interface{}, respOut interface{}) (int, []byte, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := strings.TrimRight(c.config.ServerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	// Clear cached admin token on auth failure so next call re-authenticates
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.adminToken.Store("")
	}

	respBody, _ := io.ReadAll(resp.Body)

	if respOut != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, respOut); err != nil {
			return resp.StatusCode, respBody, fmt.Errorf("decode response: %w (body: %s)", err, truncate(respBody, 200))
		}
	}

	return resp.StatusCode, respBody, nil
}

// encodeRoomID percent-encodes the "!" in room IDs for URL paths.
func encodeRoomID(roomID string) string {
	return strings.ReplaceAll(roomID, "!", "%21")
}

// roomAliasFullFor builds the full Matrix alias "#localpart:server" from a
// localpart. Exposed at package level so the service layer can synthesize
// the same alias format used by the client when calling ResolveRoomAlias /
// DeleteRoomAlias.
func roomAliasFullFor(domain, localpart string) string {
	return "#" + localpart + ":" + domain
}

// encodeAlias percent-encodes the "#" and ":" characters used by Matrix room
// aliases for safe inclusion in URL paths.
func encodeAlias(alias string) string {
	s := strings.ReplaceAll(alias, "#", "%23")
	s = strings.ReplaceAll(s, ":", "%3A")
	return s
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// txnCounter provides unique transaction IDs for Matrix event sends.
var txnCounter atomic.Int64
