package service

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
)

// WorkerProvisioner defines the provisioning operations used by WorkerReconciler
// and TeamReconciler. Implemented by *Provisioner; extracted for testability.
type WorkerProvisioner interface {
	ProvisionWorker(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResult, error)
	DeprovisionWorker(ctx context.Context, req WorkerDeprovisionRequest) error
	RefreshCredentials(ctx context.Context, workerName string) (*RefreshResult, error)
	RefreshWorkerCredentials(ctx context.Context, credentialName, workerName string) (*RefreshResult, error)
	EnsureWorkerGatewayAuth(ctx context.Context, workerName, gatewayKey string) error
	ReconcileExpose(ctx context.Context, workerName string, desired []v1beta1.ExposePort, current []v1beta1.ExposedPortStatus) ([]v1beta1.ExposedPortStatus, error)
	EnsureServiceAccount(ctx context.Context, workerName string) error
	DeleteServiceAccount(ctx context.Context, workerName string) error
	EnsureRemoteServiceAccount(ctx context.Context, workerName, clusterID, namespace string) error
	DeleteRemoteServiceAccount(ctx context.Context, workerName, clusterID, namespace string) error
	DeleteCredentials(ctx context.Context, workerName string) error
	DeleteWorkerCredentials(ctx context.Context, credentialName string) error
	RequestSAToken(ctx context.Context, workerName string) (string, error)
	// LeaveAllWorkerRooms logs in as the worker (using stored credentials,
	// or resetting the password via admin if they are stale) and makes
	// the worker leave every room it is currently joined to.
	LeaveAllWorkerRooms(ctx context.Context, workerName string) error
	// DeleteWorkerRoom fires an admin "!admin rooms delete-room" command
	// for the given room. Best-effort; the actual deletion runs
	// asynchronously inside tuwunel.
	DeleteWorkerRoom(ctx context.Context, roomID string) error
	MatrixUserID(name string) string
	LoginAsHuman(ctx context.Context, username, password string) (string, error)
	ProvisionTeamRooms(ctx context.Context, req TeamRoomRequest) (*TeamRoomResult, error)
	DeleteTeamRoomAliases(ctx context.Context, teamName, leaderName string) error
	DeleteWorkerRoomAlias(ctx context.Context, workerName string) error
	MatrixAppServiceEnabled() bool
}

// WorkerDeployer defines the deployment operations used by WorkerReconciler
// and TeamReconciler. Implemented by *Deployer; extracted for testability.
type WorkerDeployer interface {
	DeployPackage(ctx context.Context, name, uri string, isUpdate bool) error
	WriteInlineConfigs(name string, spec v1beta1.WorkerSpec) error
	DeployWorkerConfig(ctx context.Context, req WorkerDeployRequest) error
	PushOnDemandSkills(ctx context.Context, workerName string, skills []string, remoteSkills []v1beta1.RemoteSkillSource) error
	CleanupOSSData(ctx context.Context, workerName string) error
	InjectCoordinationContext(ctx context.Context, req CoordinationDeployRequest) error
	InjectWorkerCoordination(ctx context.Context, req WorkerCoordinationRequest) error
	InjectHeartbeatConfig(ctx context.Context, req InjectHeartbeatRequest) error
	EnsureTeamStorage(ctx context.Context, teamName string) error
	MaterializeSandboxWorkerDeps(ctx context.Context, req SandboxWorkerDepsRequest) error
}

// WorkerEnvBuilderI defines env map construction for worker containers.
// Implemented by *WorkerEnvBuilder; extracted for testability.
type WorkerEnvBuilderI interface {
	Build(workerName string, prov *WorkerProvisionResult) map[string]string
}

// ManagerProvisioner defines the provisioning operations used by ManagerReconciler.
// Implemented by *Provisioner; extracted for testability.
//
// NOTE: RefreshCredentials is included because the current handleUpdate calls it
// (likely a bug — should be RefreshManagerCredentials). Phase 2 reconciler rewrite
// will unify to RefreshManagerCredentials only.
type ManagerProvisioner interface {
	ProvisionManager(ctx context.Context, req ManagerProvisionRequest) (*ManagerProvisionResult, error)
	DeprovisionManager(ctx context.Context, name string) error
	RefreshCredentials(ctx context.Context, name string) (*RefreshResult, error)
	RefreshManagerCredentials(ctx context.Context, managerName string) (*RefreshResult, error)
	EnsureManagerGatewayAuth(ctx context.Context, managerName, gatewayKey string) error
	EnsureManagerServiceAccount(ctx context.Context, managerName string) error
	DeleteManagerServiceAccount(ctx context.Context, managerName string) error
	DeleteCredentials(ctx context.Context, name string) error
	RequestManagerSAToken(ctx context.Context, managerName string) (string, error)
	// LeaveAllManagerRooms logs in as the manager and makes it leave every
	// room it is currently joined to. See LeaveAllWorkerRooms.
	LeaveAllManagerRooms(ctx context.Context, managerName string) error
	// DeleteManagerRoom fires an admin "!admin rooms delete-room" command
	// for the given room. See DeleteWorkerRoom.
	DeleteManagerRoom(ctx context.Context, roomID string) error
	DeleteManagerRoomAlias(ctx context.Context, managerName string) error
	// IsManagerJoinedDM returns true when the Manager's Matrix user has
	// already joined the given Admin DM room. Used by reconcileManagerWelcome
	// as one of two *side-effect-free* gates before claiming the WelcomeSent
	// slot (the other being IsManagerLLMAuthReady). Sending the welcome
	// before the manager has joined would land the prompt in the room's
	// historical timeline, which OpenClaw / hermes / copaw drop during
	// their first-boot catch-up sync.
	IsManagerJoinedDM(ctx context.Context, roomID string) (bool, error)
	// IsManagerLLMAuthReady returns true when Higress's WASM key-auth
	// filter has finished syncing the manager's consumer credential into
	// its in-memory config — i.e. when a request bearing the manager's
	// gateway key would currently pass the AI route's auth check. The
	// filter activation is asynchronous and takes ~40-45s on first install
	// (the legacy `start-manager-agent.sh` papered over this with a
	// `sleep 45` after Higress setup). Joining the DM room (~10s) is
	// strictly faster than auth propagation (~45s), so reconcileManagerWelcome
	// MUST gate on both signals — sending after only the join check would
	// deliver a prompt the manager receives but cannot reply to (its first
	// /v1/chat/completions call 401s) and the onboarding turn is silently
	// lost.
	IsManagerLLMAuthReady(ctx context.Context, gatewayKey string) (bool, error)
	// SendManagerWelcomeMessage renders and posts the first-boot onboarding
	// prompt as the homeserver admin into the given DM room. Pure side
	// effect, no readiness checks — caller must guarantee the manager has
	// joined the room AND the gateway has propagated its auth, AND that it
	// has won the WelcomeSent claim race.
	SendManagerWelcomeMessage(ctx context.Context, req ManagerWelcomeRequest) error
}

// ManagerDeployer defines the deployment operations used by ManagerReconciler.
// Implemented by *Deployer; extracted for testability.
type ManagerDeployer interface {
	DeployPackage(ctx context.Context, name, uri string, isUpdate bool) error
	DeployManagerConfig(ctx context.Context, req ManagerDeployRequest) error
	PushOnDemandSkills(ctx context.Context, name string, skills []string, remoteSkills []v1beta1.RemoteSkillSource) error
	CleanupOSSData(ctx context.Context, name string) error
}

// ManagerEnvBuilderI defines env map construction for manager containers.
// Implemented by *WorkerEnvBuilder; extracted for testability.
type ManagerEnvBuilderI interface {
	BuildManager(managerName string, prov *ManagerProvisionResult, spec v1beta1.ManagerSpec) map[string]string
}

// HumanProvisioner defines the Matrix-level operations HumanReconciler needs.
// Implemented by *Provisioner; extracted for testability so the reconciler
// can be driven against a mock without a live Matrix homeserver.
//
// Surface intentionally narrow: Humans have no gateway consumer, no MinIO
// account, no container, no backend pod — just a Matrix user plus a set of
// room memberships. Keeping the interface focused on those concerns avoids
// pulling the heavier Worker/Manager credential + registry machinery into
// the Human path.
type HumanProvisioner interface {
	// EnsureHumanUser registers a new Matrix account for this human, or
	// logs in an existing one. Called only during first-time provisioning
	// (Status.MatrixUserID == ""); steady-state reconciles must use
	// LoginAsHuman with the stored password instead to avoid triggering
	// the orphan-recovery password reset inside matrix.EnsureUser, which
	// would clobber any user-initiated password change made in Element.
	EnsureHumanUser(ctx context.Context, username string) (*HumanCredentials, error)

	// LoginAsHuman obtains a fresh access token for an already-provisioned
	// human using the caller-supplied password. Returns an error when the
	// password no longer matches (e.g. the user changed it in Element);
	// callers treat that as a soft failure and fall back to admin-only
	// room management on this reconcile pass.
	LoginAsHuman(ctx context.Context, username, password string) (string, error)

	// RegisterAppServiceUser registers (or logs in to) a Matrix account
	// via the AppService API. Returns Created=true on first registration.
	RegisterAppServiceUser(ctx context.Context, username string) (*HumanCredentials, error)

	// RegisterLegacyUser registers via the registration_token flow.
	RegisterLegacyUser(ctx context.Context, username string) (*HumanCredentials, error)

	// SetUserPassword writes a password for the given user via the admin bot.
	SetUserPassword(ctx context.Context, userID, password string) error

	// LoginAppServiceUser obtains a token via AS login (no password).
	LoginAppServiceUser(ctx context.Context, username string) (string, error)

	// LoginWithPassword obtains a token via the password login flow.
	LoginWithPassword(ctx context.Context, username, password string) (string, error)

	// SetDisplayName updates the Matrix profile displayname for the user.
	// Requires a user-scoped access token.
	SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error

	// MatrixUserID builds the full "@<name>:<domain>" form.
	MatrixUserID(name string) string

	// InviteToRoom invites userID to roomID using the admin token.
	// Idempotent: returns nil when the user is already joined/invited.
	InviteToRoom(ctx context.Context, roomID, userID string) error

	// JoinRoomAs joins roomID with the given user access token. Required
	// for private (trusted_private_chat) rooms, which need the invitee to
	// accept the pending invite before membership takes effect.
	JoinRoomAs(ctx context.Context, roomID, userToken string) error

	// KickFromRoom removes userID from roomID using the admin token.
	// Idempotent: returns nil when the user is not a member.
	KickFromRoom(ctx context.Context, roomID, userID, reason string) error

	// ForceLeaveRoom asks the Tuwunel admin bot to force-leave userID out
	// of roomID via "!admin users force-leave-room". Fire-and-forget at
	// the bot layer, but the admin message delivery itself is confirmed.
	ForceLeaveRoom(ctx context.Context, userID, roomID string) error

	// DeactivateHumanUser disables a Human Matrix account after membership removal.
	DeactivateHumanUser(ctx context.Context, userID string) error

	MatrixAppServiceEnabled() bool
}

// HumanCredentials is the subset of matrix.UserCredentials that the Human
// reconcile path consumes. Decoupled from matrix.UserCredentials so the
// reconciler does not import internal/matrix directly.
type HumanCredentials struct {
	UserID      string
	AccessToken string
	Password    string
	Created     bool
}

// Compile-time interface satisfaction checks.
var (
	_ WorkerProvisioner = (*Provisioner)(nil)
	_ WorkerDeployer    = (*Deployer)(nil)
	_ WorkerEnvBuilderI = (*WorkerEnvBuilder)(nil)

	_ ManagerProvisioner = (*Provisioner)(nil)
	_ ManagerDeployer    = (*Deployer)(nil)
	_ ManagerEnvBuilderI = (*WorkerEnvBuilder)(nil)

	_ HumanProvisioner = (*Provisioner)(nil)
)
