package service

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- Request / Result types ---

// WorkerProvisionRequest describes the infrastructure to provision for a worker.
type WorkerProvisionRequest struct {
	Name           string
	CredentialName string
	Role           string // "standalone" | "team_leader" | "worker"
	TeamName       string
	TeamLeaderName string
}

// WorkerProvisionResult contains all outputs from a successful provision.
type WorkerProvisionResult struct {
	MatrixUserID   string
	MatrixToken    string
	RoomID         string
	GatewayKey     string
	MinIOPassword  string
	MatrixPassword string
}

// WorkerDeprovisionRequest describes which infrastructure to clean up.
type WorkerDeprovisionRequest struct {
	Name         string
	IsTeamWorker bool
	ExposedPorts []v1beta1.ExposedPortStatus
	ExposeSpec   []v1beta1.ExposePort
}

// TeamRoomRequest describes rooms to create for a team.
type RefreshResult struct {
	MatrixToken    string
	GatewayKey     string
	MinIOPassword  string
	MatrixPassword string
}

// --- Provisioner ---

// ProvisionerConfig holds configuration for constructing a Provisioner.
type ProvisionerConfig struct {
	Matrix       matrix.Client
	MatrixConfig matrix.Config
	Gateway      gateway.Client
	OSSAdmin     oss.StorageAdminClient // nil in incluster/cloud mode
	Creds        CredentialStore
	K8sClient    kubernetes.Interface
	KubeMode     string
	Namespace    string
	AuthAudience string
	MatrixDomain string
	AdminUser    string

	// ResourcePrefix is the tenant prefix used when creating SAs and their
	// labels. Empty falls back to auth.DefaultResourcePrefix ("hiclaw-").
	ResourcePrefix authpkg.ResourcePrefix

	// ControllerName identifies this controller instance. Stamped on every
	// ServiceAccount created by the provisioner via agentteams.io/controller.
	ControllerName string

	// Pre-generated Manager secrets (from install script env).
	// When set, used instead of generating random credentials.
	ManagerPassword   string
	ManagerGatewayKey string

	// AIGatewayURL is the data-plane URL of the AI gateway (e.g.
	// "http://aigw-local.agentteams.io:8080"). Used by IsManagerLLMAuthReady to
	// probe whether the gateway can actually serve a chat-completions
	// request bearing the manager's bearer token — i.e. whether Higress's
	// WASM key-auth filter has finished syncing the freshly-bound consumer
	// credential AND the upstream provider answers with the configured
	// model. Auth propagation alone takes ~40-45s on first install, far
	// longer than the manager Matrix user's auto-join of the Admin DM
	// (~5-10s after container start), so "manager joined the DM room" is
	// NOT a sufficient readiness signal for the welcome prompt: the prompt
	// would land while the agent's first /v1/chat/completions call still
	// 401s, and the onboarding turn would be silently lost.
	AIGatewayURL string

	// ManagerModel is the model name the Manager Agent will use when it
	// composes its first reply to the welcome prompt. The probe in
	// IsManagerLLMAuthReady issues a real chat-completions request against
	// this model so a 200 response proves the entire path the manager
	// will exercise (auth filter → route → upstream → model resolution)
	// is live. Sourced from Config.ManagerModel which already resolves
	// AGENTTEAMS_MANAGER_MODEL → AGENTTEAMS_DEFAULT_MODEL → "qwen3.6-plus".
	ManagerModel string

	// ManagerEnabled reflects AGENTTEAMS_MANAGER_ENABLED. When false, no Manager
	// CR is ever created, so the Matrix user `@manager:<domain>` does not
	// exist on Tuwunel. Worker room creation must therefore skip inviting
	// the manager; otherwise Conduwuit/Tuwunel returns HTTP 403 (it rejects
	// invites to non-existent local users).
	ManagerEnabled bool

	// RemoteCache resolves remote cluster clients for cross-cluster SA operations.
	// May be nil when remote mode is not configured.
	RemoteCache backend.RemoteClientProvider
}

// Provisioner orchestrates infrastructure provisioning and deprovisioning
// for workers and teams: Matrix accounts/rooms, Gateway consumers, MinIO
// users, K8s ServiceAccounts, and port exposure.
type Provisioner struct {
	matrix         matrix.Client
	matrixConfig   matrix.Config
	gateway        gateway.Client
	ossAdmin       oss.StorageAdminClient
	creds          CredentialStore
	k8sClient      kubernetes.Interface
	kubeMode       string
	namespace      string
	authAudience   string
	matrixDomain   string
	adminUser      string
	resourcePrefix authpkg.ResourcePrefix
	controllerName string
	remoteCache    backend.RemoteClientProvider

	managerPassword   string
	managerGatewayKey string
	managerEnabled    bool

	// aiGatewayURL is the data-plane base URL used by IsManagerLLMAuthReady.
	// Empty in tests / unconfigured deploys; the probe treats empty as
	// "ready" so the welcome reconcile does not block forever in those
	// scenarios (the actual send may still surface auth errors, which the
	// reconcile logs but does not retry — see manager_reconcile_welcome.go).
	aiGatewayURL string
	// managerModel is the LLM the welcome-readiness probe asks for when
	// it issues its tiny chat-completions request. Empty → probe falls
	// back to the same "treat as ready" behavior as empty aiGatewayURL,
	// so misconfigured / test deploys never wedge the welcome.
	managerModel string
}

func NewProvisioner(cfg ProvisionerConfig) *Provisioner {
	return &Provisioner{
		matrix:            cfg.Matrix,
		matrixConfig:      cfg.MatrixConfig,
		gateway:           cfg.Gateway,
		ossAdmin:          cfg.OSSAdmin,
		creds:             cfg.Creds,
		k8sClient:         cfg.K8sClient,
		kubeMode:          cfg.KubeMode,
		namespace:         cfg.Namespace,
		authAudience:      cfg.AuthAudience,
		matrixDomain:      cfg.MatrixDomain,
		adminUser:         cfg.AdminUser,
		resourcePrefix:    cfg.ResourcePrefix.Or(authpkg.DefaultResourcePrefix),
		controllerName:    cfg.ControllerName,
		managerPassword:   cfg.ManagerPassword,
		managerGatewayKey: cfg.ManagerGatewayKey,
		managerEnabled:    cfg.ManagerEnabled,
		aiGatewayURL:      cfg.AIGatewayURL,
		managerModel:      cfg.ManagerModel,
		remoteCache:       cfg.RemoteCache,
	}
}

// MatrixUserID builds a full Matrix user ID from a localpart.
func (p *Provisioner) MatrixUserID(name string) string {
	return p.matrix.UserID(name)
}

// SendAdminMessage delivers body to roomID using the homeserver-admin
// identity, bypassing the recipient's own token. Used by the message-
// injection HTTP endpoints (plan #17) to post operator-authored messages
// directly into a Manager's Admin DM room or a Team's leader room.
func (p *Provisioner) SendAdminMessage(ctx context.Context, roomID, body string) error {
	return p.matrix.SendMessageAsAdmin(ctx, roomID, body)
}

// MatrixAppServiceEnabled reports whether the controller is running in
// Matrix AppService mode. In this mode, user registration and login use
// the Application Service API instead of passwords.
func (p *Provisioner) MatrixAppServiceEnabled() bool {
	return p.matrixConfig.AppServiceEnabled
}

// roomAliasLocalpart is the single source of truth for how controller-managed
// rooms are named on the Matrix homeserver. The chosen shape
// "agentteams-<kind>-<name>" is deliberately verbose to avoid colliding with rooms
// created manually or by unrelated tooling. Changing this format in place
// would orphan every existing room — callers must instead introduce a new
// kind and handle migration explicitly.
func roomAliasLocalpart(kind, name string) string {
	return "agentteams-" + kind + "-" + name
}

// roomAliasFull builds the full "#localpart:domain" form used by
// ResolveRoomAlias / DeleteRoomAlias.
func (p *Provisioner) roomAliasFull(localpart string) string {
	return "#" + localpart + ":" + p.matrixDomain
}

// leaveAllRooms logs in (or refreshes credentials via orphan recovery) as
// the given Matrix localpart and asks the homeserver to make the user
// leave every room they are currently joined to. Errors leaving individual
// rooms are logged but not returned, so the overall delete flow remains
// best-effort.
//
// credsKey is the storage key passed to the credential loader, which may
// differ from matrixUsername (e.g. manager credentials are stored under
// the Manager CR name, but the Matrix localpart is always "manager").
func (p *Provisioner) leaveAllRooms(ctx context.Context, credsKey, matrixUsername string) error {
	logger := log.FromContext(ctx)

	creds, err := p.creds.Load(ctx, credsKey)
	if err != nil {
		return fmt.Errorf("load credentials for %s: %w", credsKey, err)
	}
	if creds == nil {
		logger.Info("no credentials found; skipping leave-all-rooms", "credsKey", credsKey)
		return nil
	}

	token, err := p.ensureMatrixToken(ctx, matrixUsername, creds)
	if err != nil {
		return fmt.Errorf("login %s: %w", matrixUsername, err)
	}

	rooms, err := p.matrix.ListJoinedRooms(ctx, token)
	if err != nil {
		return fmt.Errorf("list joined rooms for %s: %w", matrixUsername, err)
	}

	for _, roomID := range rooms {
		if err := p.matrix.LeaveRoom(ctx, roomID, token); err != nil {
			logger.Error(err, "leave room (best-effort)",
				"user", matrixUsername, "roomID", roomID)
		}
	}
	return nil
}

// deleteRoom issues a fire-and-forget `!admin rooms delete-room` command
// to the Tuwunel admin bot. Tuwunel processes it asynchronously, and the
// `delete_rooms_after_leave`/`forget_forced_upon_leave` homeserver
// settings act as a fallback if this never lands.
func (p *Provisioner) deleteRoom(ctx context.Context, roomID string) error {
	if roomID == "" {
		return nil
	}
	cmd := fmt.Sprintf("!admin rooms delete-room %s", roomID)
	return p.matrix.AdminCommand(ctx, cmd)
}

// LeaveAllWorkerRooms makes the worker leave every Matrix room it is
// joined to. Used during worker deletion so that rooms where the worker
// was the last local member get pruned via the tuwunel
// delete_rooms_after_leave setting.
func (p *Provisioner) LeaveAllWorkerRooms(ctx context.Context, workerName string) error {
	return p.leaveAllRooms(ctx, workerName, workerName)
}

// DeleteWorkerRoom asks tuwunel to delete the worker's exclusive DM room.
// Fire-and-forget; callers should treat errors as non-fatal.
func (p *Provisioner) DeleteWorkerRoom(ctx context.Context, roomID string) error {
	return p.deleteRoom(ctx, roomID)
}

// LeaveAllManagerRooms makes the manager leave every Matrix room it is
// joined to. Used during manager deletion.
func (p *Provisioner) LeaveAllManagerRooms(ctx context.Context, managerName string) error {
	return p.leaveAllRooms(ctx, managerName, "manager")
}

// DeleteManagerRoom asks tuwunel to delete the manager's exclusive DM
// room. Fire-and-forget.
func (p *Provisioner) DeleteManagerRoom(ctx context.Context, roomID string) error {
	return p.deleteRoom(ctx, roomID)
}

// ProvisionWorker executes the full infrastructure setup for a new worker:
// credentials, Matrix account, MinIO user, Matrix room, Gateway consumer.
func (p *Provisioner) ProvisionWorker(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResult, error) {
	st, err := p.ensureWorkerCredentials(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := p.ensureWorkerMatrixIdentity(ctx, st); err != nil {
		return nil, err
	}
	if err := p.ensureWorkerMinIOUser(ctx, st, req.TeamName); err != nil {
		return nil, err
	}
	if err := p.ensureWorkerPersonalRoom(ctx, req, st); err != nil {
		return nil, err
	}
	if err := p.ensureWorkerGatewayConsumer(ctx, st); err != nil {
		return nil, err
	}

	return &WorkerProvisionResult{
		MatrixUserID:   st.workerMatrixID,
		MatrixToken:    st.userCreds.AccessToken,
		RoomID:         st.roomID,
		GatewayKey:     st.creds.GatewayKey,
		MinIOPassword:  st.creds.MinIOPassword,
		MatrixPassword: st.creds.MatrixPassword,
	}, nil
}

// DeprovisionWorker cleans up infrastructure for a deleted worker:
// exposed ports, container, gateway auth, MinIO user.
// Best-effort: individual step errors are logged but don't fail the operation.
func (p *Provisioner) DeprovisionWorker(ctx context.Context, req WorkerDeprovisionRequest) error {
	logger := log.FromContext(ctx)
	consumerName := "worker-" + req.Name

	// Clean up exposed ports
	currentExposed := req.ExposedPorts
	if len(currentExposed) == 0 && len(req.ExposeSpec) > 0 {
		for _, ep := range req.ExposeSpec {
			currentExposed = append(currentExposed, v1beta1.ExposedPortStatus{
				Port:   ep.Port,
				Domain: domainForExpose(req.Name, ep.Port),
			})
		}
	}
	if len(currentExposed) > 0 {
		if _, err := p.ReconcileExpose(ctx, req.Name, nil, currentExposed); err != nil {
			logger.Error(err, "failed to clean up exposed ports (non-fatal)")
		}
	}

	// Deauthorize gateway
	if err := p.gateway.DeauthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		logger.Error(err, "failed to deauthorize AI routes (non-fatal)")
	}
	if err := p.gateway.DeleteConsumer(ctx, consumerName); err != nil {
		logger.Error(err, "failed to delete gateway consumer (non-fatal)")
	}

	// Delete MinIO user (embedded mode)
	if p.ossAdmin != nil {
		if err := p.ossAdmin.DeleteUser(ctx, req.Name); err != nil {
			logger.Error(err, "failed to delete MinIO user (non-fatal)")
		}
	}

	return nil
}

// ensureMatrixToken obtains a Matrix access token for the given user.
//
// Always reuses the cached token when present, regardless of AS or legacy
// mode. Re-login on Tuwunel (conduwuit) invalidates the previous access
// token, which would break any running Worker that still holds it. Token
// refresh is handled on-demand via POST /api/v1/credentials/matrix-token
// when a Worker encounters a 401 from the homeserver.
//
// Callers should Save the updated creds back to the credential store after
// this returns so the token survives controller restarts.
func (p *Provisioner) DeleteWorkerRoomAlias(ctx context.Context, workerName string) error {
	logger := log.FromContext(ctx)
	alias := p.roomAliasFull(roomAliasLocalpart("worker", workerName))
	if err := p.matrix.DeleteRoomAlias(ctx, alias); err != nil {
		logger.Error(err, "failed to delete worker room alias (non-fatal)", "alias", alias)
	}
	return nil
}

// DeleteManagerRoomAlias removes the alias for the Manager's Admin DM room.
// Same preservation semantics as the worker/team variants.
func (p *Provisioner) DeleteManagerRoomAlias(ctx context.Context, managerName string) error {
	logger := log.FromContext(ctx)
	alias := p.roomAliasFull(roomAliasLocalpart("manager", managerName))
	if err := p.matrix.DeleteRoomAlias(ctx, alias); err != nil {
		logger.Error(err, "failed to delete manager room alias (non-fatal)", "alias", alias)
	}
	return nil
}

// --- Manager Provisioning ---

// ManagerProvisionRequest describes the infrastructure to provision for a Manager.
