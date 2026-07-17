package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrAppServiceNotReady signals that the homeserver rejected an AppService
// call because it does not yet recognize the controller's as_token
// (M_UNKNOWN_TOKEN). This is a transient startup race: the controller's
// AppService registration has not been registered/verified with the
// homeserver yet. Callers should treat it as retryable and requeue quietly
// instead of logging it as a hard error.
var ErrAppServiceNotReady = errors.New("matrix appservice token not active yet")

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

	// SetRoomName updates the human-readable Matrix room name. When userToken
	// is empty, it falls back to the homeserver-admin identity.
	SetRoomName(ctx context.Context, roomID, name, userToken string) error

	// SetRoomState writes a Matrix room state event. When userToken is empty,
	// it falls back to the homeserver-admin identity.
	SetRoomState(ctx context.Context, roomID, eventType, stateKey string, content map[string]interface{}, userToken string) error

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

	// SyncMessages returns Matrix room message events visible to the admin user.
	SyncMessages(ctx context.Context, since string, timeout time.Duration) (*SyncMessagesResult, error)

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
type MessageEvent struct {
	RoomID   string
	EventID  string
	Sender   string
	Mentions []string
}
type SyncMessagesResult struct {
	NextBatch string
	Events    []MessageEvent
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
