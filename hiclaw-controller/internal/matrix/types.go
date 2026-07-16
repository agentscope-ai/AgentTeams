package matrix

import "crypto/rand"

// Config holds connection parameters for a Matrix homeserver.
type Config struct {
	ServerURL         string // internal Matrix CS API URL, e.g. http://tuwunel:6167
	Domain            string // Matrix domain for user IDs, e.g. matrix-local.agentteams.io:8080
	RegistrationToken string // shared registration secret (m.login.registration_token)
	AdminUser         string // global admin username
	AdminPassword     string // global admin password
	E2EEEnabled       bool   // whether to enable E2EE on new rooms

	// AppService mode configuration. When enabled, the controller acts as a
	// Matrix Application Service, using as_token to register/login users
	// without passwords. Legacy password-based auth is preserved when disabled.
	AppServiceEnabled         bool
	AppServiceID              string // e.g. "agentteams-controller"
	AppServiceToken           string // as_token — never logged or exposed to agents
	AppServiceHSToken         string // hs_token — reserved for future AS HTTP receiver
	AppServiceSenderLocalpart string // e.g. "agentteams-controller"

	// AppServiceUserNamespaceRegex optionally narrows the exclusive Matrix
	// user namespace claimed by the AppService. When empty, the controller
	// claims the broad "@.*:<domain>" namespace, which is ONLY safe when the
	// homeserver is exclusively AgentTeams-managed (the only supported mode —
	// enforced by Helm's matrix.mode=managed and the embedded Tuwunel
	// install). Set this to a restrictive regex (e.g. "@hiclaw-.*:<domain>")
	// when running AppService mode against a shared/existing homeserver so
	// the as_token cannot impersonate non-AgentTeams local users.
	AppServiceUserNamespaceRegex string

	// AppServicePushURL is the controller HTTP endpoint registered with
	// Tuwunel for homeserver → appservice transaction push (mention wakeup).
	// When empty, registration omits url (passwordless-only mode).
	AppServicePushURL string
}

// EnsureUserRequest describes a user to register or log in.
type EnsureUserRequest struct {
	Username string // localpart only, e.g. "alice"
	Password string // if empty, a secure random password is generated
}

// UserCredentials holds the result of a successful EnsureUser call.
type UserCredentials struct {
	UserID      string // full Matrix user ID, e.g. @alice:domain
	AccessToken string
	Password    string // the password used (generated or caller-provided)
	Created     bool   // true if newly registered, false if existing user logged in
}

// StateEvent describes a Matrix state event included in createRoom.initial_state.
type StateEvent struct {
	Type     string                 `json:"type"`
	StateKey string                 `json:"state_key"`
	Content  map[string]interface{} `json:"content"`
}

// CreateRoomRequest describes a new Matrix room.
type CreateRoomRequest struct {
	Name         string         // human-readable room name
	Topic        string         // room topic
	Invite       []string       // user IDs to invite
	PowerLevels  map[string]int // userID → power level override
	CreatorToken string         // access token of the room creator
	E2EE         bool           // add m.room.encryption to initial_state
	InitialState []StateEvent   // Matrix state events to seed via createRoom.initial_state

	// IsDirect marks the room as a direct message (1:1) room.
	IsDirect bool

	// RoomAliasName is the alias localpart (without leading '#' or ':server')
	// that uniquely identifies this room on the homeserver. When non-empty,
	// the request is sent with room_alias_name so Matrix itself guarantees
	// idempotency: repeated CreateRoom calls with the same alias return the
	// existing room (Created=false) instead of creating a duplicate. The
	// alias is the sole source of truth for room identity — callers should
	// not depend on any external K8s/MinIO state to avoid duplicates.
	RoomAliasName string
}

// RoomInfo holds the result of a CreateRoom call.
type RoomInfo struct {
	RoomID  string
	Created bool // true if newly created; false if the alias already existed
}

// RoomMember describes a user's presence in a Matrix room.
// Only members whose Membership is "join" or "invite" are surfaced via
// ListRoomMembers; leave/ban/knock entries are filtered out by the client.
type RoomMember struct {
	UserID     string
	Membership string // "join" | "invite"
}

// AppServiceRegistration describes a Matrix Application Service registration.
// Rendered as YAML and sent to the homeserver via admin command.
type AppServiceRegistration struct {
	ID              string               `yaml:"id"`
	URL             *string              `yaml:"url"` // nil = no push from HS
	ASToken         string               `yaml:"as_token"`
	HSToken         string               `yaml:"hs_token"`
	SenderLocalpart string               `yaml:"sender_localpart"`
	RateLimited     bool                 `yaml:"rate_limited"`
	Namespaces      AppServiceNamespaces `yaml:"namespaces"`
}

// AppServiceNamespaces holds the user/alias/room namespace declarations.
type AppServiceNamespaces struct {
	Users   []AppServiceNamespace `yaml:"users"`
	Aliases []AppServiceNamespace `yaml:"aliases"`
	Rooms   []AppServiceNamespace `yaml:"rooms"`
}

// AppServiceNamespace is a single namespace entry with exclusivity flag.
type AppServiceNamespace struct {
	Exclusive bool   `yaml:"exclusive"`
	Regex     string `yaml:"regex"`
}

// GeneratePassword produces a cryptographically secure random password
// of the given byte length, hex-encoded (output length = 2*n).
func GeneratePassword(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out), nil
}
