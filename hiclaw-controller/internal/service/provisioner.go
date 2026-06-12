package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// --- Request / Result types ---

// WorkerProvisionRequest describes the infrastructure to provision for a worker.
type WorkerProvisionRequest struct {
	Name            string
	CredentialName  string
	ModelProviderID string
	Role            string // "standalone" | "team_leader" | "worker"
	TeamName        string
	TeamLeaderName  string
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
type TeamRoomRequest struct {
	TeamName             string
	LeaderName           string
	LeaderCredentialName string
	WorkerNames          []string
	AdminSpec            *v1beta1.TeamAdminSpec
	HumanMembers         []v1beta1.TeamMemberSpec
	TeamAdminActorToken  string
	TeamAdminActorName   string
}

// TeamRoomResult contains the created room IDs.
type TeamRoomResult struct {
	TeamRoomID     string
	LeaderDMRoomID string
}

// RefreshResult contains refreshed credentials for update operations.
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
	// ServiceAccount created by the provisioner via hiclaw.io/controller.
	ControllerName string

	// Pre-generated Manager secrets (from install script env).
	// When set, used instead of generating random credentials.
	ManagerPassword   string
	ManagerGatewayKey string

	// AIGatewayURL is the data-plane URL of the AI gateway (e.g.
	// "http://aigw-local.hiclaw.io:8080"). Used by IsManagerLLMAuthReady to
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
	// HICLAW_MANAGER_MODEL → HICLAW_DEFAULT_MODEL → "qwen3.6-plus".
	ManagerModel string

	// ManagerEnabled reflects HICLAW_MANAGER_ENABLED. When false, no Manager
	// CR is ever created, so the Matrix user `@manager:<domain>` does not
	// exist on Tuwunel. Worker room creation must therefore skip inviting
	// the manager; otherwise Conduwuit/Tuwunel returns HTTP 403 (it rejects
	// invites to non-existent local users).
	ManagerEnabled bool
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
	}
}

// MatrixUserID builds a full Matrix user ID from a localpart.
func (p *Provisioner) MatrixUserID(name string) string {
	return p.matrix.UserID(name)
}

// MatrixAppServiceEnabled reports whether the controller is running in
// Matrix AppService mode. In this mode, user registration and login use
// the Application Service API instead of passwords.
func (p *Provisioner) MatrixAppServiceEnabled() bool {
	return p.matrixConfig.AppServiceEnabled
}

// roomAliasLocalpart is the single source of truth for how controller-managed
// rooms are named on the Matrix homeserver. The chosen shape
// "hiclaw-<kind>-<name>" is deliberately verbose to avoid colliding with rooms
// created manually or by unrelated tooling. Changing this format in place
// would orphan every existing room — callers must instead introduce a new
// kind and handle migration explicitly.
func roomAliasLocalpart(kind, name string) string {
	return "hiclaw-" + kind + "-" + name
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
	logger := log.FromContext(ctx)
	workerName := req.Name
	credentialName := req.CredentialName
	if credentialName == "" {
		credentialName = workerName
	}
	consumerName := "worker-" + workerName
	workerMatrixID := p.matrix.UserID(workerName)
	managerMatrixID := p.matrix.UserID("manager")
	adminMatrixID := p.matrix.UserID(p.adminUser)

	isTeamWorker := req.TeamLeaderName != ""

	// Step 1: Load or generate credentials
	creds, err := p.loadWorkerCredentials(ctx, credentialName, workerName)
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	if creds == nil {
		creds, err = GenerateCredentials()
		if err != nil {
			return nil, fmt.Errorf("generate credentials: %w", err)
		}
		if err := p.creds.Save(ctx, credentialName, creds); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
		}
	}

	// Step 2: Register Matrix account
	logger.Info("registering Matrix account", "name", workerName)
	var userCreds *matrix.UserCredentials
	if p.MatrixAppServiceEnabled() {
		userCreds, err = p.matrix.EnsureAppServiceUser(ctx, workerName)
		if err != nil {
			return nil, fmt.Errorf("Matrix AS registration failed: %w", err)
		}
		creds.MatrixPassword = "" // No password in AppService mode
	} else {
		userCreds, err = p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{
			Username: workerName,
			Password: creds.MatrixPassword,
		})
		if err != nil {
			return nil, fmt.Errorf("Matrix registration failed: %w", err)
		}
		creds.MatrixPassword = userCreds.Password
	}
	// Cache the freshly issued access token so subsequent reconciles can reuse
	// it via RefreshCredentials instead of issuing a new login (which would
	// rotate channels.matrix.accessToken in openclaw.json and trigger a
	// gateway restart).
	if userCreds.AccessToken != "" {
		creds.MatrixToken = userCreds.AccessToken
	}

	// Step 3: Create MinIO user (embedded mode only)
	if p.ossAdmin != nil {
		logger.Info("creating MinIO user", "name", workerName)
		if err := p.ossAdmin.EnsureUser(ctx, workerName, creds.MinIOPassword); err != nil {
			return nil, fmt.Errorf("MinIO user creation failed: %w", err)
		}
		if err := p.ossAdmin.EnsurePolicy(ctx, oss.PolicyRequest{
			WorkerName: workerName,
			TeamName:   req.TeamName,
		}); err != nil {
			return nil, fmt.Errorf("MinIO policy creation failed: %w", err)
		}
	}

	// Step 4: Create Matrix room
	logger.Info("creating Matrix room", "name", workerName)

	// Pick an authority for the room.
	//   - Team worker  : the team leader (always provisioned before team workers).
	//   - Standalone   : the Manager if enabled, else the admin user.
	var authorityID string
	switch {
	case isTeamWorker:
		authorityID = p.matrix.UserID(req.TeamLeaderName)
	case p.managerEnabled:
		authorityID = managerMatrixID
	default:
		authorityID = adminMatrixID
	}

	powerLevels := map[string]int{
		managerMatrixID: 100,
		adminMatrixID:   100,
		authorityID:     100,
		workerMatrixID:  0,
	}

	invite := []string{adminMatrixID}
	if authorityID != adminMatrixID {
		invite = append(invite, authorityID)
	}
	invite = append(invite, workerMatrixID)

	roomInfo, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Worker: %s", workerName),
		Topic:         fmt.Sprintf("Communication channel for %s", workerName),
		Invite:        invite,
		PowerLevels:   powerLevels,
		RoomAliasName: roomAliasLocalpart("worker", workerName),
	})
	if err != nil {
		return nil, fmt.Errorf("Matrix room creation failed: %w", err)
	}
	roomID := roomInfo.RoomID
	logger.Info("Matrix room ready", "roomID", roomID, "created", roomInfo.Created)

	// Persist the freshly-registered Matrix token. Room identity is no
	// longer stored here — the Matrix alias is the sole source of truth
	// and is resolved via CreateRoom on every reconcile.
	if err := p.creds.Save(ctx, credentialName, creds); err != nil {
		logger.Error(err, "failed to persist credentials (non-fatal)")
	}

	// Step 4a: When an existing alias was resolved, CreateRoom returned
	// without sending fresh invites. Reconcile membership so late-added
	// authorities (e.g. a team admin joining after initial
	// provisioning) or recovered power levels are applied. This may
	// (re)invite the worker if it had been removed from the room.
	if !roomInfo.Created {
		if err := p.ReconcileRoomMembership(ctx, roomID, []string{adminMatrixID, authorityID, workerMatrixID}); err != nil {
			logger.Error(err, "failed to reconcile worker room membership (non-fatal)", "roomID", roomID)
		}
	}

	// Step 4b: Have the worker accept the room invite on its behalf.
	// Some worker runtimes (e.g. hermes-agent) don't auto-join invited
	// rooms, so the controller does it explicitly here using the
	// worker's freshly issued access token. JoinRoom is idempotent — if
	// the worker already joined (e.g. CoPaw runtime which auto-accepts),
	// the homeserver returns 200 OK. This decouples room membership from
	// any runtime-specific Matrix client behaviour.
	//
	// IMPORTANT: "membership = join" is necessary but NOT sufficient for
	// "worker is ready to process messages". CoPaw, in particular,
	// suppresses message callbacks during its first-boot catch-up sync
	// (see copaw/src/matrix/channel.py::_sync_loop). Any message that
	// arrives in that catch-up window is silently dropped. Tests and
	// managers must therefore implement at-least-once send semantics
	// (see tests/lib/matrix-client.sh::matrix_send_and_wait_for_reply)
	// rather than treating membership=join as a readiness signal.
	if userCreds.AccessToken != "" && roomID != "" {
		if err := p.matrix.JoinRoom(ctx, roomID, userCreds.AccessToken); err != nil {
			logger.Error(err, "failed to join worker into its own room (non-fatal)",
				"name", workerName, "roomID", roomID)
		} else {
			logger.Info("worker joined own room", "name", workerName, "roomID", roomID)
		}
	}

	// Step 5: Gateway consumer and authorization
	logger.Info("creating gateway consumer", "consumer", consumerName)
	consumerResult, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: creds.GatewayKey,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway consumer creation failed: %w", err)
	}
	if consumerResult.APIKey != "" && consumerResult.APIKey != creds.GatewayKey {
		creds.GatewayKey = consumerResult.APIKey
		_ = p.creds.Save(ctx, credentialName, creds)
	}

	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, req.ModelProviderID); err != nil {
		return nil, fmt.Errorf("AI route authorization failed: %w", err)
	}
	// Higress WASM key-auth plugin needs ~1-2s to sync after route update.
	// Without this, the worker's first LLM call may get 401.
	time.Sleep(2 * time.Second)

	return &WorkerProvisionResult{
		MatrixUserID:   workerMatrixID,
		MatrixToken:    userCreds.AccessToken,
		RoomID:         roomID,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
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
func (p *Provisioner) ensureMatrixToken(ctx context.Context, matrixUsername string, creds *WorkerCredentials) (string, error) {
	// Always reuse cached token. Re-login invalidates the old token on
	// Tuwunel, breaking running Workers. On-demand refresh is available
	// via POST /api/v1/credentials/matrix-token for 401 recovery.
	if creds.MatrixToken != "" {
		return creds.MatrixToken, nil
	}
	var tok string
	var err error
	if p.MatrixAppServiceEnabled() {
		tok, err = p.matrix.LoginAppServiceUser(ctx, matrixUsername)
	} else {
		tok, err = p.matrix.Login(ctx, matrixUsername, creds.MatrixPassword)
	}
	if err != nil {
		return "", err
	}
	creds.MatrixToken = tok
	return tok, nil
}

// ForceRefreshMatrixToken issues a fresh Matrix access token for the given
// worker/manager, bypassing the cache. Called when the caller reports a 401
// from the homeserver. Persists the new token to the credential store.
func (p *Provisioner) ForceRefreshMatrixToken(ctx context.Context, name string) (*RefreshResult, error) {
	creds, err := p.creds.Load(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("load credentials for %s: %w", name, err)
	}
	if creds == nil {
		return nil, fmt.Errorf("no credentials found for %s", name)
	}

	// Clear cached token to force re-login
	creds.MatrixToken = ""

	var tok string
	if p.MatrixAppServiceEnabled() {
		tok, err = p.matrix.LoginAppServiceUser(ctx, name)
	} else {
		tok, err = p.matrix.Login(ctx, name, creds.MatrixPassword)
	}
	if err != nil {
		return nil, fmt.Errorf("re-login for %s: %w", name, err)
	}

	creds.MatrixToken = tok
	if saveErr := p.creds.Save(ctx, name, creds); saveErr != nil {
		// Non-fatal: token is valid even if persistence fails
		log.FromContext(ctx).Error(saveErr, "failed to persist refreshed matrix token", "name", name)
	}

	return &RefreshResult{MatrixToken: tok}, nil
}

// RefreshCredentials loads persisted credentials and obtains a Matrix token,
// reusing the cached token when present. Used during update operations.
func (p *Provisioner) RefreshCredentials(ctx context.Context, workerName string) (*RefreshResult, error) {
	return p.RefreshWorkerCredentials(ctx, workerName, workerName)
}

// RefreshWorkerCredentials loads worker credentials by their owning CR key while
// refreshing the Matrix token for the runtime worker identity.
func (p *Provisioner) RefreshWorkerCredentials(ctx context.Context, credentialName, workerName string) (*RefreshResult, error) {
	if credentialName == "" {
		credentialName = workerName
	}
	creds, err := p.loadWorkerCredentials(ctx, credentialName, workerName)
	if err != nil || creds == nil {
		return nil, fmt.Errorf("credentials not found for %s", credentialName)
	}

	hadToken := creds.MatrixToken != ""
	matrixToken, err := p.ensureMatrixToken(ctx, workerName, creds)
	if err != nil {
		return nil, fmt.Errorf("Matrix login failed: %w", err)
	}
	if !hadToken {
		if err := p.creds.Save(ctx, credentialName, creds); err != nil {
			return nil, fmt.Errorf("persist matrix token: %w", err)
		}
	}

	return &RefreshResult{
		MatrixToken:    matrixToken,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}

func (p *Provisioner) loadWorkerCredentials(ctx context.Context, credentialName, workerName string) (*WorkerCredentials, error) {
	creds, err := p.creds.Load(ctx, credentialName)
	if err != nil || creds != nil || credentialName == workerName {
		return creds, err
	}

	legacyCreds, err := p.creds.Load(ctx, workerName)
	if err != nil || legacyCreds == nil {
		return legacyCreds, err
	}
	if err := p.creds.Save(ctx, credentialName, legacyCreds); err != nil {
		return nil, fmt.Errorf("migrate legacy worker credentials: %w", err)
	}
	return legacyCreds, nil
}

// RefreshManagerCredentials loads persisted credentials for the Manager and
// returns a Matrix access token, reusing the cached token when present. The
// Manager CR name (e.g. "default") differs from the Matrix username (always
// "manager"), so this uses a dedicated method.
func (p *Provisioner) RefreshManagerCredentials(ctx context.Context, managerName string) (*RefreshResult, error) {
	creds, err := p.creds.Load(ctx, managerName)
	if err != nil || creds == nil {
		return nil, fmt.Errorf("credentials not found for manager %s", managerName)
	}

	hadToken := creds.MatrixToken != ""
	matrixToken, err := p.ensureMatrixToken(ctx, "manager", creds)
	if err != nil {
		return nil, fmt.Errorf("Matrix login failed: %w", err)
	}
	if !hadToken {
		if err := p.creds.Save(ctx, managerName, creds); err != nil {
			return nil, fmt.Errorf("persist matrix token: %w", err)
		}
	}

	return &RefreshResult{
		MatrixToken:    matrixToken,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}

// EnsureManagerGatewayAuth ensures the Manager's gateway consumer exists and is
// authorized on AI routes. Called during container recreation to restore auth
// that may have been lost (e.g. after upgrade with fresh Higress state).
func (p *Provisioner) EnsureManagerGatewayAuth(ctx context.Context, managerName, gatewayKey, modelProviderID string) error {
	consumerName := "manager"
	_, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: gatewayKey,
	})
	if err != nil {
		return fmt.Errorf("ensure consumer: %w", err)
	}
	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, modelProviderID); err != nil {
		return fmt.Errorf("authorize AI routes: %w", err)
	}
	return nil
}

// EnsureWorkerGatewayAuth ensures the Worker's gateway consumer exists and is
// authorized on AI routes. Called during controller restart / member reconcile
// to defensively restore auth that may have been lost (e.g. if the Higress
// route was rewritten, or after upgrade with fresh Higress state). Mirrors
// EnsureManagerGatewayAuth but uses the worker-scoped consumer name.
func (p *Provisioner) EnsureWorkerGatewayAuth(ctx context.Context, workerName, gatewayKey, modelProviderID string) error {
	consumerName := "worker-" + workerName
	_, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: gatewayKey,
	})
	if err != nil {
		return fmt.Errorf("ensure consumer: %w", err)
	}
	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, modelProviderID); err != nil {
		return fmt.Errorf("authorize AI routes: %w", err)
	}
	return nil
}

// ProvisionTeamRooms creates (or resolves) the team room and leader DM room
// and reconciles their Matrix memberships against the desired member set.
// Idempotency is guaranteed by the Matrix alias: repeated calls always land
// on the same RoomID regardless of K8s informer cache state, so no
// "existing room ID" inputs are threaded through. Membership is reconciled
// unconditionally on every call so newly-added workers are invited and
// removed workers are kicked.
func (p *Provisioner) ProvisionTeamRooms(ctx context.Context, req TeamRoomRequest) (*TeamRoomResult, error) {
	logger := log.FromContext(ctx)
	managerMatrixID := p.matrix.UserID("manager")
	adminMatrixID := p.matrix.UserID(p.adminUser)
	teamCoordinatorIDs := p.resolveTeamCoordinatorMatrixIDs(req.AdminSpec, req.HumanMembers)
	teamMemberIDs := p.resolveTeamMemberMatrixIDs(req.HumanMembers)
	leaderMatrixID := p.matrix.UserID(req.LeaderName)
	teamAdminID, hasTeamAdmin := p.resolveTeamAdminMatrixID(req.AdminSpec)
	if req.AdminSpec != nil && !hasTeamAdmin {
		return nil, fmt.Errorf("team admin is configured but has no matrix identity")
	}
	if hasTeamAdmin && req.TeamAdminActorToken == "" {
		return nil, fmt.Errorf("team admin actor token is required when team admin is configured")
	}

	// Team Room: teamAdmin creates and owns the room when configured. Without
	// teamAdmin, keep the legacy Admin bootstrap and membership fallback.
	teamDesired := []string{}
	if hasTeamAdmin {
		teamDesired = appendUniqueStrings(teamDesired, teamAdminID)
	} else {
		teamDesired = appendUniqueStrings(teamDesired, adminMatrixID)
	}
	teamDesired = appendUniqueStrings(teamDesired, leaderMatrixID)
	teamDesired = appendUniqueStrings(teamDesired, teamCoordinatorIDs...)
	teamDesired = appendUniqueStrings(teamDesired, teamMemberIDs...)
	for _, wn := range req.WorkerNames {
		teamDesired = appendUniqueStrings(teamDesired, p.matrix.UserID(wn))
	}
	teamInvites := teamDesired
	teamRoomPowerLevels := map[string]int{
		managerMatrixID: 100,
		leaderMatrixID:  100,
	}
	if hasTeamAdmin {
		teamRoomPowerLevels[teamAdminID] = 100
		teamInvites = withoutString(teamDesired, teamAdminID)
	} else {
		teamRoomPowerLevels[adminMatrixID] = 100
	}

	teamRoom, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Team: %s", req.TeamName),
		Topic:         fmt.Sprintf("Team room for %s", req.TeamName),
		Invite:        teamInvites,
		PowerLevels:   teamRoomPowerLevels,
		CreatorToken:  req.TeamAdminActorToken,
		RoomAliasName: roomAliasLocalpart("team", req.TeamName),
	})
	if err != nil {
		return nil, fmt.Errorf("team room creation failed: %w", err)
	}
	logger.Info("team room ready", "roomID", teamRoom.RoomID, "created", teamRoom.Created)

	// Reconcile unconditionally: on fresh creation the invite list already
	// took effect and Reconcile is a no-op; on alias resolution it catches
	// up members added/removed since the previous run.
	if hasTeamAdmin {
		if err := p.matrix.JoinRoom(ctx, teamRoom.RoomID, req.TeamAdminActorToken); err != nil {
			return nil, fmt.Errorf("team admin join team room: %w", err)
		}
		if err := p.ReconcileRoomMembershipWithActorToken(ctx, teamRoom.RoomID, teamDesired, req.TeamAdminActorToken, req.TeamAdminActorName); err != nil {
			return nil, fmt.Errorf("reconcile team room membership as team admin: %w", err)
		}
		if teamAdminID != adminMatrixID {
			if present, _, err := p.observedRoomMembershipWithToken(ctx, teamRoom.RoomID, adminMatrixID, req.TeamAdminActorToken); err != nil {
				return nil, fmt.Errorf("check global admin team room membership: %w", err)
			} else if present {
				if err := p.matrix.LeaveRoom(ctx, teamRoom.RoomID, ""); err != nil {
					return nil, fmt.Errorf("global admin leave team room: %w", err)
				}
			}
		}
	} else if err := p.ReconcileRoomMembership(ctx, teamRoom.RoomID, teamDesired); err != nil {
		return nil, fmt.Errorf("reconcile team room membership: %w", err)
	}

	// Leader DM Room: only Leader + Team Admin when configured; otherwise
	// fallback to the global Admin for legacy teams.
	leaderDMDesired := []string{leaderMatrixID}
	if hasTeamAdmin {
		leaderDMDesired = appendUniqueStrings(leaderDMDesired, teamAdminID)
	} else {
		leaderDMDesired = appendUniqueStrings(leaderDMDesired, adminMatrixID)
	}
	leaderDMInvites := leaderDMDesired
	if hasTeamAdmin {
		leaderDMInvites = withoutString(leaderDMDesired, teamAdminID)
	}
	leaderDMRoom, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Leader DM: %s", req.LeaderName),
		Topic:         fmt.Sprintf("DM channel for team leader %s", req.LeaderName),
		Invite:        leaderDMInvites,
		PowerLevels:   p.leaderDMPowerLevels(managerMatrixID, adminMatrixID, leaderMatrixID, teamAdminID, hasTeamAdmin),
		CreatorToken:  req.TeamAdminActorToken,
		RoomAliasName: roomAliasLocalpart("leader-dm", req.LeaderName),
	})
	if err != nil {
		return nil, fmt.Errorf("leader DM room creation failed: %w", err)
	}
	logger.Info("leader DM room ready", "roomID", leaderDMRoom.RoomID, "created", leaderDMRoom.Created)

	if hasTeamAdmin {
		if err := p.ensureTeamAdminJoinedLeaderDM(ctx, leaderDMRoom.RoomID, teamAdminID, req.TeamAdminActorToken, req.LeaderCredentialName, req.LeaderName, leaderDMRoom.Created); err != nil {
			return nil, err
		}
	}

	leaderDMInviteToken := ""
	leaderDMInviteActor := ""
	if hasTeamAdmin {
		leaderDMInviteToken = req.TeamAdminActorToken
		leaderDMInviteActor = req.TeamAdminActorName
	} else if !leaderDMRoom.Created {
		if token, err := p.leaderInviteToken(ctx, req.LeaderCredentialName, req.LeaderName); err != nil {
			logger.Error(err, "failed to load leader token for existing leader DM; falling back to admin invite", "leader", req.LeaderName)
		} else {
			leaderDMInviteToken = token
			leaderDMInviteActor = "leader"
		}
	}
	if err := p.ReconcileRoomMembershipWithActorToken(ctx, leaderDMRoom.RoomID, leaderDMDesired, leaderDMInviteToken, leaderDMInviteActor); err != nil {
		return nil, fmt.Errorf("reconcile leader DM membership: %w", err)
	}

	return &TeamRoomResult{
		TeamRoomID:     teamRoom.RoomID,
		LeaderDMRoomID: leaderDMRoom.RoomID,
	}, nil
}

func (p *Provisioner) ensureTeamAdminJoinedLeaderDM(ctx context.Context, roomID, teamAdminID, teamAdminToken, leaderCredentialName, leaderName string, created bool) error {
	if err := p.matrix.JoinRoom(ctx, roomID, teamAdminToken); err == nil {
		return nil
	} else if created {
		return fmt.Errorf("team admin join leader DM room: %w", err)
	} else {
		joinErr := err
		leaderToken, tokenErr := p.leaderInviteToken(ctx, leaderCredentialName, leaderName)
		if tokenErr != nil {
			return fmt.Errorf("team admin join leader DM room: %w", joinErr)
		}
		if inviteErr := p.matrix.InviteToRoomWithToken(ctx, roomID, teamAdminID, leaderToken); inviteErr != nil {
			return fmt.Errorf("leader invite team admin to leader DM room: %w", inviteErr)
		}
		if retryErr := p.matrix.JoinRoom(ctx, roomID, teamAdminToken); retryErr != nil {
			return fmt.Errorf("team admin join leader DM room after leader invite: %w", retryErr)
		}
		return nil
	}
}

func (p *Provisioner) leaderDMPowerLevels(managerMatrixID, adminMatrixID, leaderMatrixID, teamAdminID string, hasTeamAdmin bool) map[string]int {
	levels := map[string]int{
		managerMatrixID: 100,
		leaderMatrixID:  100,
	}
	if hasTeamAdmin {
		levels[teamAdminID] = 100
	} else {
		levels[adminMatrixID] = 100
	}
	return levels
}

func (p *Provisioner) resolveTeamAdminMatrixID(admin *v1beta1.TeamAdminSpec) (string, bool) {
	if admin == nil {
		return "", false
	}
	if admin.MatrixUserID != "" {
		return admin.MatrixUserID, true
	}
	if admin.Name != "" {
		return p.matrix.UserID(admin.Name), true
	}
	return "", false
}

func (p *Provisioner) resolveTeamCoordinatorMatrixIDs(admin *v1beta1.TeamAdminSpec, members []v1beta1.TeamMemberSpec) []string {
	ids := make([]string, 0, 1+len(members))
	if id, ok := p.resolveTeamAdminMatrixID(admin); ok {
		ids = append(ids, id)
	}
	for _, member := range members {
		if !teamMemberIsCoordinator(member) {
			continue
		}
		if member.MatrixUserID != "" {
			ids = append(ids, member.MatrixUserID)
			continue
		}
		if member.Name != "" {
			ids = append(ids, p.matrix.UserID(member.Name))
		}
	}
	return uniqueStrings(ids)
}

func (p *Provisioner) resolveTeamMemberMatrixIDs(members []v1beta1.TeamMemberSpec) []string {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		if member.MatrixUserID != "" {
			ids = append(ids, member.MatrixUserID)
			continue
		}
		if member.Name != "" {
			ids = append(ids, p.matrix.UserID(member.Name))
		}
	}
	return uniqueStrings(ids)
}

func teamMemberIsCoordinator(member v1beta1.TeamMemberSpec) bool {
	return member.Role == "" || member.Role == "coordinator"
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	out := make([]string, 0, len(base)+len(values))
	for _, v := range base {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func uniqueStrings(values []string) []string {
	return appendUniqueStrings(nil, values...)
}

func withoutString(values []string, target string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == target {
			continue
		}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// EnsureRoomMember invites userID into roomID. Idempotent (treats
// already-joined/invited as success). Returns nil on success.
func (p *Provisioner) EnsureRoomMember(ctx context.Context, roomID, userID string) error {
	return p.matrix.InviteToRoom(ctx, roomID, userID)
}

// EnsureRoomNonMember kicks userID out of roomID. Idempotent (treats
// not-in-room as success). Returns nil on success.
func (p *Provisioner) EnsureRoomNonMember(ctx context.Context, roomID, userID, reason string) error {
	return p.matrix.KickFromRoom(ctx, roomID, userID, reason)
}

// ReconcileRoomMembership drives the membership of roomID to match `desired`
// (a list of full Matrix user IDs). Users present in `desired` but not in
// the room are invited; users in the room but not in `desired` are kicked.
// Per-user errors are logged and collected; the first error encountered is
// returned after processing every user (best-effort semantics, consistent
// with DeprovisionWorker).
func (p *Provisioner) ReconcileRoomMembership(ctx context.Context, roomID string, desired []string) error {
	return p.ReconcileRoomMembershipWithActorToken(ctx, roomID, desired, "", "")
}

func (p *Provisioner) ReconcileRoomMembershipWithInviteToken(ctx context.Context, roomID string, desired []string, inviteToken, inviteActor string) error {
	return p.ReconcileRoomMembershipWithActorToken(ctx, roomID, desired, inviteToken, inviteActor)
}

func (p *Provisioner) ReconcileRoomMembershipWithActorToken(ctx context.Context, roomID string, desired []string, actorToken, actorName string) error {
	logger := log.FromContext(ctx)

	var current []matrix.RoomMember
	var err error
	if actorToken != "" {
		current, err = p.matrix.ListRoomMembersWithToken(ctx, roomID, actorToken)
	} else {
		current, err = p.matrix.ListRoomMembers(ctx, roomID)
	}
	if err != nil {
		return fmt.Errorf("list members of %s: %w", roomID, err)
	}

	desiredSet := make(map[string]struct{}, len(desired))
	for _, u := range desired {
		if u == "" {
			continue
		}
		desiredSet[u] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, m := range current {
		currentSet[m.UserID] = struct{}{}
	}

	var firstErr error

	for _, u := range desired {
		if _, ok := currentSet[u]; ok {
			continue
		}
		var err error
		if actorToken != "" {
			logger.Info("inviting user to room with joined member token", "room", roomID, "user", u, "actor", actorName)
			err = p.matrix.InviteToRoomWithToken(ctx, roomID, u, actorToken)
		} else {
			err = p.matrix.InviteToRoom(ctx, roomID, u)
		}
		if err != nil {
			logger.Error(err, "failed to invite user to room", "room", roomID, "user", u)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	for _, m := range current {
		if _, ok := desiredSet[m.UserID]; ok {
			continue
		}
		// Leave admin bot alone even if it isn't in `desired`: admin owns
		// power level 100 and some rooms (e.g. Manager Admin DM) expect it
		// implicitly. Callers must include the admin in `desired` when they
		// want it to stay.
		if m.UserID == p.matrix.UserID(p.adminUser) {
			continue
		}
		logger.Info("room member not desired; attempting removal",
			"room", roomID,
			"user", m.UserID,
			"membership", m.Membership,
			"currentCount", len(currentSet),
			"desiredCount", len(desiredSet))
		var err error
		if actorToken != "" {
			logger.Info("kicking user from room with joined member token", "room", roomID, "user", m.UserID, "actor", actorName)
			err = p.matrix.KickFromRoomWithToken(ctx, roomID, m.UserID, "removed from desired member set", actorToken)
		} else {
			err = p.matrix.KickFromRoom(ctx, roomID, m.UserID, "removed from desired member set")
		}
		if err != nil {
			logger.Error(err, "failed to kick user from room", "room", roomID, "user", m.UserID)
			if shouldForceLeaveAfterKickError(err) {
				if forceErr := p.ForceLeaveRoom(ctx, m.UserID, roomID); forceErr == nil {
					logger.Info("force-leave-room command sent after kick failed", "room", roomID, "user", m.UserID)
					stillPresent, memberships, checkErr := p.observedRoomMembership(ctx, roomID, m.UserID)
					if checkErr != nil {
						logger.Error(checkErr, "failed to verify force-leave-room result", "room", roomID, "user", m.UserID)
					} else {
						logger.Info("force-leave-room post-check",
							"room", roomID,
							"user", m.UserID,
							"stillPresent", stillPresent,
							"memberships", memberships)
					}
					continue
				} else {
					logger.Error(forceErr, "failed to send force-leave-room command", "room", roomID, "user", m.UserID)
					err = forceErr
				}
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

func (p *Provisioner) leaderInviteToken(ctx context.Context, credentialName, leaderName string) (string, error) {
	if p.creds == nil {
		return "", fmt.Errorf("credential store unavailable")
	}
	if credentialName == "" {
		credentialName = leaderName
	}
	refresh, err := p.RefreshWorkerCredentials(ctx, credentialName, leaderName)
	if err != nil {
		return "", err
	}
	if refresh.MatrixToken == "" {
		return "", fmt.Errorf("leader matrix token is empty")
	}
	return refresh.MatrixToken, nil
}

func (p *Provisioner) observedRoomMembership(ctx context.Context, roomID, userID string) (bool, []string, error) {
	members, err := p.matrix.ListRoomMembers(ctx, roomID)
	if err != nil {
		return false, nil, err
	}
	return observedMembershipFromMembers(members, userID), observedMembershipsFromMembers(members, userID), nil
}

func (p *Provisioner) observedRoomMembershipWithToken(ctx context.Context, roomID, userID, token string) (bool, []string, error) {
	members, err := p.matrix.ListRoomMembersWithToken(ctx, roomID, token)
	if err != nil {
		return false, nil, err
	}
	return observedMembershipFromMembers(members, userID), observedMembershipsFromMembers(members, userID), nil
}

func observedMembershipFromMembers(members []matrix.RoomMember, userID string) bool {
	for _, member := range members {
		if member.UserID == userID {
			return true
		}
	}
	return false
}

func observedMembershipsFromMembers(members []matrix.RoomMember, userID string) []string {
	memberships := make([]string, 0, 1)
	for _, member := range members {
		if member.UserID != userID {
			continue
		}
		memberships = append(memberships, member.Membership)
	}
	return memberships
}

func shouldForceLeaveAfterKickError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "m_forbidden") &&
		(strings.Contains(msg, "not have enough power") || strings.Contains(msg, "power"))
}

// DeleteCredentials removes persisted credentials for a worker.
func (p *Provisioner) DeleteCredentials(ctx context.Context, workerName string) error {
	return p.DeleteWorkerCredentials(ctx, workerName)
}

// DeleteWorkerCredentials removes persisted credentials for a worker-like CR.
func (p *Provisioner) DeleteWorkerCredentials(ctx context.Context, credentialName string) error {
	return p.creds.Delete(ctx, credentialName)
}

// DeleteTeamRoomAliases removes the room aliases that identify a team's group
// room and the leader DM room so a future Team CR with the same name can
// reclaim the aliases cleanly. Best-effort: alias removal does not affect
// the underlying room, which is intentionally left intact to preserve chat
// history; it only detaches the controller's stable identifier from it.
func (p *Provisioner) DeleteTeamRoomAliases(ctx context.Context, teamName, leaderName string) error {
	logger := log.FromContext(ctx)
	teamAlias := p.roomAliasFull(roomAliasLocalpart("team", teamName))
	if err := p.matrix.DeleteRoomAlias(ctx, teamAlias); err != nil {
		logger.Error(err, "failed to delete team room alias (non-fatal)", "alias", teamAlias)
	}
	if leaderName != "" {
		leaderAlias := p.roomAliasFull(roomAliasLocalpart("leader-dm", leaderName))
		if err := p.matrix.DeleteRoomAlias(ctx, leaderAlias); err != nil {
			logger.Error(err, "failed to delete leader DM alias (non-fatal)", "alias", leaderAlias)
		}
	}
	return nil
}

// DeleteWorkerRoomAlias removes the alias that identifies a worker's comm
// channel. Same semantics as DeleteTeamRoomAliases — the underlying room is
// preserved, only the controller's handle to it is released.
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
type ManagerProvisionRequest struct {
	Name            string
	ModelProviderID string
}

// ManagerProvisionResult contains all outputs from a successful Manager provision.
type ManagerProvisionResult struct {
	MatrixUserID   string
	MatrixToken    string
	RoomID         string
	GatewayKey     string
	MinIOPassword  string
	MatrixPassword string
}

// ProvisionManager executes the full infrastructure setup for a Manager Agent:
// credentials, Matrix account, MinIO user, Admin DM room, Gateway consumer.
func (p *Provisioner) ProvisionManager(ctx context.Context, req ManagerProvisionRequest) (*ManagerProvisionResult, error) {
	logger := log.FromContext(ctx)
	managerName := req.Name
	matrixUsername := "manager"
	consumerName := "manager"
	managerMatrixID := p.matrix.UserID(matrixUsername)
	adminMatrixID := p.matrix.UserID(p.adminUser)

	// Step 1: Load or generate credentials
	creds, err := p.creds.Load(ctx, managerName)
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	if creds == nil {
		creds, err = GenerateCredentials()
		if err != nil {
			return nil, fmt.Errorf("generate credentials: %w", err)
		}
		// Use pre-generated secrets from install script if available
		if p.managerPassword != "" {
			creds.MatrixPassword = p.managerPassword
		}
		if p.managerGatewayKey != "" {
			creds.GatewayKey = p.managerGatewayKey
		}
		if err := p.creds.Save(ctx, managerName, creds); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
		}
	}

	// Step 2: Register Matrix account (always "manager", matching container script)
	logger.Info("registering Manager Matrix account", "matrixUser", matrixUsername)
	var userCreds *matrix.UserCredentials
	if p.MatrixAppServiceEnabled() {
		userCreds, err = p.matrix.EnsureAppServiceUser(ctx, matrixUsername)
		if err != nil {
			return nil, fmt.Errorf("Matrix AS registration failed: %w", err)
		}
		creds.MatrixPassword = "" // No password in AppService mode
	} else {
		userCreds, err = p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{
			Username: matrixUsername,
			Password: creds.MatrixPassword,
		})
		if err != nil {
			return nil, fmt.Errorf("Matrix registration failed: %w", err)
		}
		creds.MatrixPassword = userCreds.Password
	}
	// Cache the freshly issued access token so subsequent reconciles can
	// reuse it via RefreshManagerCredentials instead of issuing a new login
	// (which would rotate channels.matrix.accessToken in openclaw.json and
	// trigger a gateway restart).
	if userCreds.AccessToken != "" {
		creds.MatrixToken = userCreds.AccessToken
	}

	// Step 3: Create MinIO user (embedded mode only)
	if p.ossAdmin != nil {
		logger.Info("creating MinIO user for Manager", "name", managerName)
		if err := p.ossAdmin.EnsureUser(ctx, managerName, creds.MinIOPassword); err != nil {
			return nil, fmt.Errorf("MinIO user creation failed: %w", err)
		}
		if err := p.ossAdmin.EnsurePolicy(ctx, oss.PolicyRequest{
			WorkerName: managerName,
			IsManager:  true,
		}); err != nil {
			return nil, fmt.Errorf("MinIO policy creation failed: %w", err)
		}
	}

	// Step 4: Create Admin DM Room (Admin + Manager only)
	logger.Info("creating Manager Admin DM room", "name", managerName)
	powerLevels := map[string]int{
		adminMatrixID:   100,
		managerMatrixID: 100,
	}
	roomInfo, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Manager: %s", managerName),
		Topic:         fmt.Sprintf("Admin DM channel for Manager %s", managerName),
		Invite:        []string{adminMatrixID, managerMatrixID},
		PowerLevels:   powerLevels,
		IsDirect:      true,
		RoomAliasName: roomAliasLocalpart("manager", managerName),
	})
	if err != nil {
		return nil, fmt.Errorf("Admin DM room creation failed: %w", err)
	}
	roomID := roomInfo.RoomID
	logger.Info("Manager Admin DM room ready", "roomID", roomID, "created", roomInfo.Created)

	if err := p.creds.Save(ctx, managerName, creds); err != nil {
		logger.Error(err, "failed to persist credentials (non-fatal)")
	}

	// Step 5: Gateway consumer and authorization
	logger.Info("creating gateway consumer for Manager", "consumer", consumerName)
	consumerResult, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: creds.GatewayKey,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway consumer creation failed: %w", err)
	}
	if consumerResult.APIKey != "" && consumerResult.APIKey != creds.GatewayKey {
		creds.GatewayKey = consumerResult.APIKey
		_ = p.creds.Save(ctx, managerName, creds)
	}

	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, req.ModelProviderID); err != nil {
		return nil, fmt.Errorf("AI route authorization failed: %w", err)
	}
	// Higress WASM key-auth plugin needs ~1-2s to sync after route update.
	// Without this, the worker's first LLM call may get 401.
	time.Sleep(2 * time.Second)

	return &ManagerProvisionResult{
		MatrixUserID:   managerMatrixID,
		MatrixToken:    userCreds.AccessToken,
		RoomID:         roomID,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}

// ManagerWelcomeRequest carries the locale hints that the controller
// renders into the first-boot onboarding prompt sent to a freshly
// provisioned Manager Agent.
type ManagerWelcomeRequest struct {
	// RoomID is the Admin DM room created by ProvisionManager (Step 4).
	RoomID string
	// Language is the install-time HICLAW_LANGUAGE selection ("zh" / "en").
	// Embedded as plain text in the prompt; the agent decides how to apply.
	Language string
	// Timezone is the install-time TZ env (IANA identifier, e.g.
	// "Asia/Shanghai"). Embedded as plain text so the agent can infer
	// the admin's likely region and offer additional language options.
	Timezone string
}

// SendManagerWelcome delivers the first-boot onboarding prompt that asks
// the Manager Agent to greet the admin and collect identity preferences
// (name / language / communication style). It is the new-architecture
// replacement for the legacy in-container welcome flow that lived in
// `start-manager-agent.sh` (lines 535-608) and only ran when
// HICLAW_RUNTIME != "k8s".
//
// Idempotency is the caller's responsibility — the controller guards
// re-send via Manager.Status.WelcomeSent. This method only checks that
// the Manager Matrix user has joined the room before sending; if not,
// it returns (sent=false, err=nil) so the reconcile loop can requeue.
//
// Returns:
//   - (true, nil)  — message was successfully delivered.
//   - (false, nil) — manager not yet joined; caller should requeue.
//   - (false, err) — unrecoverable error (admin login / Matrix API).
//
// llmAuthProbePromptTemplate renders the chat-completions body the
// readiness probe sends. It uses the same model the Manager Agent will
// use for its real first reply, and asks for a one-word answer so the
// per-probe cost is negligible (~10-20 tokens total round-trip) even
// though we may issue several probes during the gateway's WASM
// key-auth propagation window per fresh install.
//
// Format chosen to maximise compatibility:
//   - Only the universally-supported `model` and `messages` fields. No
//     `max_tokens` — some openai-compat providers (notably Bedrock-fronted
//     models and o1/o3-style reasoning families) reject the parameter
//     outright with a 400, which would defeat the point of probing
//     (readiness would never go true on those backends).
//   - The user message is a direct, brevity-instructed prompt; the
//     assistant typically replies with 1-2 tokens. We do not parse the
//     response body — only the HTTP status matters.
const llmAuthProbePromptTemplate = `{"model":%q,"messages":[{"role":"user","content":"Reply with only one word: ok"}]}`

// IsManagerLLMAuthReady reports whether the manager's bearer token can
// currently drive a real LLM call through the AI gateway — i.e. whether
// (a) Higress's WASM key-auth filter has finished syncing the
// freshly-bound consumer credential onto the AI route, and (b) the
// upstream provider is reachable and serving the configured model.
// Together these are exactly what the Manager Agent needs in order to
// successfully compose its first reply to the welcome prompt. Joining
// the Admin DM Room (~5-10s after container start) is strictly faster
// than gateway propagation (~40-45s, the gap the legacy
// `start-manager-agent.sh` papered over with `sleep 45`); sending the
// welcome on the join signal alone would deliver a prompt the manager
// receives but cannot reply to, and the onboarding turn would be
// silently lost.
//
// Probe shape:
//   - POST <AIGatewayURL>/v1/chat/completions with the manager's bearer
//     token and a tiny chat body whose `model` is the actual
//     ManagerModel and whose only user message asks for a one-word
//     answer. This is the same code path the manager will exercise on
//     its first real reply, so a successful probe is end-to-end
//     proof-of-life rather than a synthetic "auth filter only" check.
//   - HTTP 200 → ready, return (true, nil).
//   - HTTP 401 / 403 → auth not yet propagated → return (false, nil).
//     This is the expected state during the propagation window; we do
//     NOT return an error here so the reconciler requeues quietly
//     without spamming WARN-level logs.
//   - Any other status (400, 404, 429, 5xx, …) → return (false, err).
//     The reconciler surfaces it at log-level so the operator can spot
//     persistent misconfigurations (wrong model name, upstream provider
//     down, quota exhausted). Better a delayed welcome than one the
//     manager cannot answer — we never give up, only the operator's
//     attention escalates as the warnings accumulate.
//   - Network / dial errors → returned as error; same WARN-and-retry.
//
// Empty AIGatewayURL, ManagerModel, or gatewayKey → return (true, nil)
// so unit tests and bring-your-own-gateway deploys (where the
// controller doesn't know the data-plane URL or the model) do not
// stall the welcome forever.
func (p *Provisioner) IsManagerLLMAuthReady(ctx context.Context, gatewayKey string) (bool, error) {
	if p.aiGatewayURL == "" || p.managerModel == "" || gatewayKey == "" {
		return true, nil
	}
	url := strings.TrimRight(p.aiGatewayURL, "/") + "/v1/chat/completions"
	body := fmt.Sprintf(llmAuthProbePromptTemplate, p.managerModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("welcome: build llm probe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	// 30s timeout: a real LLM call can legitimately take several seconds
	// (cold-start, slow upstream); we want to wait long enough for a
	// healthy answer but not so long that a wedged backend stalls every
	// welcome reconcile for this manager.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("welcome: llm probe %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, nil
	default:
		return false, fmt.Errorf("welcome: llm probe %s returned HTTP %d (model=%q)", url, resp.StatusCode, p.managerModel)
	}
}

// IsManagerJoinedDM reports whether the Manager's Matrix user is currently
// `join`ed to the supplied DM room. Pure read; safe to poll on every
// reconcile while waiting for the agent's first /sync to land its
// auto-join. See `reconcileManagerWelcome` for the rationale on why this
// MUST be separate from the actual send: claim-before-send would otherwise
// churn the status field with claim/rollback patches on every requeue.
func (p *Provisioner) IsManagerJoinedDM(ctx context.Context, roomID string) (bool, error) {
	if roomID == "" {
		return false, fmt.Errorf("welcome: empty RoomID")
	}
	managerMatrixID := p.matrix.UserID("manager")
	members, err := p.matrix.ListRoomMembers(ctx, roomID)
	if err != nil {
		return false, fmt.Errorf("welcome: list members of %s: %w", roomID, err)
	}
	for _, m := range members {
		if m.UserID == managerMatrixID && m.Membership == "join" {
			return true, nil
		}
	}
	return false, nil
}

// SendManagerWelcomeMessage posts the first-boot onboarding prompt as the
// homeserver admin into the given DM room. The caller (reconcile loop)
// MUST have already (a) verified membership via IsManagerJoinedDM and
// (b) committed the WelcomeSent=true claim to the API server, so that a
// racing reconcile cannot also reach this point and double-deliver.
func (p *Provisioner) SendManagerWelcomeMessage(ctx context.Context, req ManagerWelcomeRequest) error {
	if req.RoomID == "" {
		return fmt.Errorf("welcome: empty RoomID")
	}
	language := req.Language
	if language == "" {
		language = "zh"
	}
	timezone := req.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	body := renderManagerWelcomeBody(language, timezone)
	if err := p.matrix.SendMessageAsAdmin(ctx, req.RoomID, body); err != nil {
		return fmt.Errorf("welcome: send to %s: %w", req.RoomID, err)
	}
	return nil
}

// renderManagerWelcomeBody returns the verbatim onboarding prompt the
// Manager Agent receives on first boot. Kept identical in spirit to the
// legacy `_welcome_msg` heredoc in `manager/scripts/init/start-manager-agent.sh`
// so the resulting agent behavior (greeting + 4-question Q&A + write
// SOUL.md + touch ~/soul-configured) is unchanged across architectures.
func renderManagerWelcomeBody(language, timezone string) string {
	return fmt.Sprintf(`This is an automated message from the HiClaw setup. This is a fresh installation.

--- Installation Context ---
User Language: %s  (zh = Chinese, en = English)
User Timezone: %s  (IANA timezone identifier)
---

You are an AI agent that manages a team of worker agents. Your identity and personality have not been configured yet — the human admin is about to meet you for the first time.

Please begin the onboarding conversation:

1. Greet the admin warmly and briefly describe what you can do (coordinate workers, manage tasks, run multi-agent projects)
2. The user has selected "%s" as their preferred language during installation. Use this language for your greeting and all subsequent communication.
3. The user's timezone is %s. Based on this timezone, you may infer their likely region and suggest additional language options.
4. Ask them: a) What would they like to call you? b) Communication style preference? c) Any behavior guidelines? d) Confirm default language
5. After they reply, write their preferences to ~/SOUL.md
6. Confirm what you wrote, and ask if they would like to adjust anything
7. Once confirmed, run: touch ~/soul-configured

The human admin will start chatting shortly.`, language, timezone, language, timezone)
}

// DeprovisionManager cleans up infrastructure for a deleted Manager.
func (p *Provisioner) DeprovisionManager(ctx context.Context, name string) error {
	logger := log.FromContext(ctx)
	consumerName := "manager"

	if err := p.gateway.DeauthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		logger.Error(err, "failed to deauthorize AI routes (non-fatal)")
	}
	if err := p.gateway.DeleteConsumer(ctx, consumerName); err != nil {
		logger.Error(err, "failed to delete gateway consumer (non-fatal)")
	}

	if p.ossAdmin != nil {
		if err := p.ossAdmin.DeleteUser(ctx, name); err != nil {
			logger.Error(err, "failed to delete MinIO user (non-fatal)")
		}
	}

	return nil
}

// CredentialNames returns all credential store keys (worker/manager names).
func (p *Provisioner) CredentialNames(ctx context.Context) ([]string, error) {
	return p.creds.List(ctx)
}

// BackfillLegacyPasswords generates and sets Matrix passwords for workers
// and managers that were created in AppService mode (no password) when the
// controller is switched back to legacy password-based mode. This ensures
// a seamless rollback without manual intervention.
func (p *Provisioner) BackfillLegacyPasswords(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("backfill")

	names, err := p.creds.List(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	if len(names) == 0 {
		return nil
	}

	var firstErr error
	backfilled := 0
	for _, name := range names {
		creds, err := p.creds.Load(ctx, name)
		if err != nil {
			logger.Error(err, "failed to load credentials", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if creds == nil {
			continue
		}
		// Already has a password — nothing to do.
		if creds.MatrixPassword != "" {
			continue
		}

		password, err := matrix.GeneratePassword(16)
		if err != nil {
			logger.Error(err, "failed to generate password", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		userID := p.matrix.UserID(name)
		if err := p.matrix.SetPasswordAsAdmin(ctx, userID, password); err != nil {
			logger.Error(err, "failed to set password via admin", "name", name, "userID", userID)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		creds.MatrixPassword = password
		// Clear cached AS token — it's no longer valid after password reset
		// and legacy mode will obtain a new token via password login.
		creds.MatrixToken = ""
		if err := p.creds.Save(ctx, name, creds); err != nil {
			logger.Error(err, "failed to save backfilled credentials", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		backfilled++
		logger.Info("backfilled legacy password", "name", name, "userID", userID)
	}

	if backfilled > 0 {
		logger.Info("legacy password backfill complete", "backfilled", backfilled, "total", len(names))
	}
	return firstErr
}
