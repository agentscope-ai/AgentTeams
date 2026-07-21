package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MemberRole classifies the reconcilable entity at the worker-infra layer.
type MemberRole string

const (
	RoleStandalone MemberRole = "standalone"
	RoleTeamLeader MemberRole = "team_leader"
	RoleTeamWorker MemberRole = "worker"
)

const (
	runtimeRemoteManagedLocal        = "remote-managed-local"
	dockerHostInternalExtraHost      = "host.docker.internal:host-gateway"
	sandboxSetTokenRetryAfter        = 30 * time.Second
	sandboxSetTokenRefreshJitterMax  = 60 * time.Second
	defaultWorkerDepsStorageCapacity = "1Pi"
	accessKeyWorkerDepsPVCapacity    = "50Gi"
	accessKeyWorkerDepsStorageClass  = "test"
	accessKeyWorkerDepsOtherOpts     = "-o umask=022 -o allow_other"
	workerDepsLayoutVersion          = "worker-deps-agentteams-pv-effective-name-v3"
	ackOSSCSIProvisioner             = "ossplugin.csi.alibabacloud.com"
	workerDepsAuthTypeRRSA           = "RRSA"
	workerDepsAuthTypeAccessKey      = "AccessKey"
)

var (
	workerDepsStorageClassGVR       = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	workerDepsPVGVR                 = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"}
	workerDepsPVCGVR                = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	workerDepsCredentialProviderGVR = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "credentialproviders"}
	workerDepsAgentIdentityGVR      = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentidentities"}
	workerDepsAgentRoleGVR          = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentroles"}
	workerDepsAgentRoleBindingGVR   = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentrolebindings"}

	sandboxSetTokenProjections sync.Map
)

type sandboxSetTokenProjectionState struct {
	NextRefresh time.Time
	Expiration  time.Time
}

// String renders the role as stored in annotations / legacy registries.
func (r MemberRole) String() string { return string(r) }

// MemberContext carries the CR-independent inputs needed to converge a single
// worker-like member (standalone worker, team leader, or team worker). The
// WorkerReconciler builds one from a Worker CR; the TeamReconciler builds one
// per Team member directly from the Team CR without ever materializing a
// Worker CR.
type MemberContext struct {
	Name        string // Kubernetes resource identity (CR/Pod/SA key)
	RuntimeName string // business/runtime identity (Matrix/OSS/room alias key)
	Namespace   string
	Role        MemberRole
	Spec        v1beta1.WorkerSpec

	// Generation / ObservedGeneration are metadata included in logs to aid
	// debugging. They are NOT used for spec-change detection — callers must
	// set SpecChanged explicitly (see field doc below).
	Generation         int64
	ObservedGeneration int64

	// SpecChanged indicates the member's desired spec differs from the spec
	// at which its container was last successfully provisioned. When true,
	// ReconcileMemberContainer recreates the container; when false, a
	// running/starting container is left alone.
	//
	// Callers are responsible for computing this correctly:
	//   WorkerReconciler: desired pod hash != Worker.status.specHash
	//   TeamReconciler:   desired pod hash != Team.status.members[].specHash
	//
	// Using a boolean (instead of reusing Generation != ObservedGeneration)
	// isolates the "did the spec change" question from the transport that
	// answers it, so Team members — which have no per-member Generation —
	// can participate without abusing the int64 fields.
	//
	SpecChanged bool

	// AppliedSpecHash is the controller-computed hash of the source spec
	// (excluding State). Owning reconcilers write this value to their own
	// status.specHash after all phases succeed.
	AppliedSpecHash string

	// CurrentSpecHash is the owning CR status hash from the start of this
	// reconcile. Empty means a brand-new or pre-upgrade status; sandbox live
	// annotations may be read only as a migration fallback in that case.
	CurrentSpecHash string

	// IsUpdate indicates the member has been successfully provisioned before;
	// controls MCP reauthorization and deployer "update" semantics.
	IsUpdate bool

	// Team linkage (empty for standalone).
	TeamName           string
	TeamLeaderName     string
	TeamRoomID         string
	LeaderDMRoomID     string
	TeamAdminName      string
	TeamAdminMatrixID  string
	TeamCoordinatorIDs []string
	TeamMembers        []service.RuntimeConfigTeamMember

	// Heartbeat config from Team CR leader spec (nil for non-leader members)
	Heartbeat *agentconfig.HeartbeatConfig

	// ExistingMatrixUserID is non-empty when prior provisioning has recorded a
	// Matrix user; the Infra phase then uses RefreshWorkerCredentials instead of
	// ProvisionWorker.
	ExistingMatrixUserID string
	// ExistingRoomID is the last-observed RoomID from the owning CR's status.
	// It is a read-through cache used by the refresh path to populate
	// downstream env builders without a round-trip to the Matrix server;
	// it is NOT used as an idempotency key (the room alias is — see
	// service.Provisioner.ProvisionWorker). Safe to leave empty; the alias
	// resolution in ProvisionWorker will populate RoomID on first run.
	ExistingRoomID      string
	CurrentExposedPorts []v1beta1.ExposedPortStatus

	// PodLabels are merged into backend.CreateRequest.Labels. Used by Team
	// members to tag pods with the team identity label so the Team
	// reconciler can watch member pod lifecycle events.
	PodLabels map[string]string

	// Owner is the CR that logically owns the member's Pod lifecycle. The
	// K8s backend stamps it as the Pod's controller OwnerReference so that
	// deleting the owning CR garbage-collects the Pod. For standalone
	// Workers this is the Worker CR; for Team members (leader or worker)
	// this is the Team CR.
	Owner metav1.Object

	// ModelProviderInfo is the resolved APIG Model API info when
	// spec.modelProvider is set. Nil when not set or on non-ai-gateway.
	ModelProviderInfo *gateway.ModelProviderInfo

	// DeployMode specifies where the member runs: "Local" (default) for
	// controller-managed pods. "Edge" is handled before pod reconciliation.
	DeployMode string
	// ServiceEnabled controls whether a ClusterIP Service is created
	// alongside the member pod. Sourced from spec.serviceEnabled.
	ServiceEnabled bool

	// Resources overrides the backend worker resource defaults. The compact
	// Worker API uses one cpu/memory value for both requests and limits; this
	// field carries the backend-expanded form.
	Resources *backend.ResourceRequirements

	// BackendRuntime is the desired backend type from spec.backendRuntime.
	// Empty means default ("pod").
	BackendRuntime string

	// StatusBackendRuntime is the currently deployed backend type from
	// status.backendRuntime. Empty means first reconcile (new worker or
	// migration from a pre-upgrade controller).
	StatusBackendRuntime string
}

// MemberState captures reconcile outputs that the caller writes back to the
// owning CR's status (or aggregates across members for the Team case).
type MemberState struct {
	MatrixUserID   string
	RoomID         string
	ContainerState string
	ExposedPorts   []v1beta1.ExposedPortStatus
	// ProvResult is the credentials bundle produced by Infra; passed through
	// Config and Container phases for idempotent reuse within one reconcile.
	ProvResult *service.WorkerProvisionResult

	// BackendRuntime is set during reconcile when a backend switch occurs
	// or on first reconcile (migration). Written back to
	// Worker.Status.BackendRuntime by the owning reconciler.
	BackendRuntime string

	// Message holds backend-reported status message (e.g. failing condition
	// detail). Written to Worker.Status.Message when reconcile succeeds but
	// the container is not fully healthy.
	Message string

	// RequeueAfter records the next background reconcile needed by member
	// internals such as sandbox token projection.
	RequeueAfter time.Duration
}

// resolveBackendForMember returns the worker backend matching the requested
// backendRuntime. "pod" remains the only open-source incluster backend. When
// the registry does not have a pod backend (e.g. Docker / embedded mode), it
// falls back to DetectWorkerBackend so legacy single-backend deployments keep
// working. Explicit sandbox requests must resolve to a registered sandbox
// backend and never silently fall back to pods.
func resolveBackendForMember(registry *backend.Registry, backendRuntime string, m MemberContext) (backend.WorkerBackend, error) {
	if registry == nil {
		return nil, fmt.Errorf("no backend registry configured for member %s", m.Name)
	}
	if backendRuntime == v1beta1.BackendRuntimeSandbox {
		wb, err := registry.GetBackendForType(context.Background(), backendRuntime)
		if err != nil {
			return nil, fmt.Errorf("backendRuntime %q is not supported in the open-source controller; use backendRuntime %q", backendRuntime, v1beta1.BackendRuntimePod)
		}
		return wb, nil
	}
	wb, err := registry.GetBackendForType(context.Background(), backendRuntime)
	if err != nil {
		// Fallback: DetectWorkerBackend (covers Docker/embedded mode
		// where the requested backend type is not registered).
		wb = registry.DetectWorkerBackend(context.Background())
		if wb == nil {
			return nil, fmt.Errorf("no backend available for member %s", m.Name)
		}
	}

	return wb, nil
}

// MemberDeps aggregates the service-layer dependencies the member phases
// invoke. Both WorkerReconciler and TeamReconciler build a MemberDeps once
// and pass it through each phase.
type MemberDeps struct {
	Provisioner                 service.WorkerProvisioner
	Deployer                    service.WorkerDeployer
	Backend                     *backend.Registry
	EnvBuilder                  service.WorkerEnvBuilderI
	GatewayClient               gateway.Client
	DynamicClient               dynamic.Interface
	RemoteDynamicClientProvider backend.RemoteDynamicClientProvider
	AuthTokenExpirationSeconds  int64

	// ResourcePrefix is the tenant-level prefix that scopes ServiceAccount
	// (and Pod) names for every member this reconciler provisions. Empty
	// prefix collapses to the pre-multi-tenant naming scheme
	// (`agentteams-worker-<name>`), preserving single-tenant deployments. It is
	// populated by the owning reconciler (WorkerReconciler / TeamReconciler)
	// from Config.ResourcePrefix.
	ResourcePrefix authpkg.ResourcePrefix

	// DefaultRuntime is forwarded into backend.CreateRequest.RuntimeFallback
	// by createMemberContainer when a member leaves spec.runtime empty.
	// Populated by the owning reconciler (WorkerReconciler / TeamReconciler)
	// from AGENTTEAMS_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime). An
	// empty string means "no operator preference" — backend.ResolveRuntime
	// will then fall back to RuntimeOpenClaw. ManagerReconciler uses its own
	// DefaultRuntime field (sourced from AGENTTEAMS_MANAGER_RUNTIME) directly on
	// backend.CreateRequest and does not go through MemberDeps, since
	// Backend.Create is shared between Worker and Manager paths and only the
	// caller knows which env var applies.
	DefaultRuntime string

	// ControllerName identifies this controller instance. It remains the
	// controller identity label/watch filter and is not used as the sandbox
	// instance name.
	ControllerName string

	// WorkerDepsStorageBucket/Endpoint identify the main AgentTeams workspace OSS
	// bucket. Built-in token/env/data worker-deps always use this storage;
	// Worker.spec.volumes is reserved for custom OSS mounts.
	WorkerDepsStorageBucket   string
	WorkerDepsStorageEndpoint string
	MountAuthType             string
	MountRoleName             string
}

// ValidateMemberDeployment checks the deployment fields for managed pod
// reconciliation. Open-source managed workers are local-cluster pods only;
// Edge workers are handled by the dedicated Edge flow before this validation.
func ValidateMemberDeployment(m MemberContext) error {
	switch m.DeployMode {
	case "", v1beta1.DeployModeLocal:
		return nil
	case v1beta1.DeployModeRemote:
		return fmt.Errorf("deployMode %q is not supported in the open-source controller; use %q or the Edge worker flow", m.DeployMode, v1beta1.DeployModeLocal)
	case v1beta1.DeployModeEdge:
		return fmt.Errorf("deployMode %q must use the Edge worker flow and is not valid for managed pod reconciliation", m.DeployMode)
	default:
		return fmt.Errorf("invalid deployMode %q: must be %q or %q", m.DeployMode, v1beta1.DeployModeLocal, v1beta1.DeployModeEdge)
	}
}

// ReconcileMemberInfra ensures Matrix account, Gateway consumer, MinIO user,
// and DM room are provisioned (or credentials refreshed). Writes MatrixUserID,
// RoomID, and ProvResult into state.
func ReconcileMemberInfra(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) (reconcile.Result, error) {
	if m.ExistingMatrixUserID != "" {
		refreshResult, err := d.Provisioner.RefreshWorkerCredentials(ctx, m.Name, m.RuntimeName, m.TeamName)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials: %w", err)
		}

		state.MatrixUserID = m.ExistingMatrixUserID
		state.RoomID = m.ExistingRoomID
		state.ProvResult = &service.WorkerProvisionResult{
			MatrixUserID:   m.ExistingMatrixUserID,
			MatrixToken:    refreshResult.MatrixToken,
			RoomID:         m.ExistingRoomID,
			GatewayKey:     refreshResult.GatewayKey,
			MinIOPassword:  refreshResult.MinIOPassword,
			MatrixPassword: refreshResult.MatrixPassword,
		}
		return reconcile.Result{}, nil
	}

	log.FromContext(ctx).Info("provisioning member infrastructure", "name", m.Name, "runtimeName", m.RuntimeName, "role", m.Role)

	provResult, err := d.Provisioner.ProvisionWorker(ctx, service.WorkerProvisionRequest{
		Name:           m.RuntimeName,
		CredentialName: m.Name,
		Role:           m.Role.String(),
		TeamName:       m.TeamName,
		TeamLeaderName: m.TeamLeaderName,
	})
	if err != nil {
		if errors.Is(err, matrix.ErrAppServiceNotReady) {
			log.FromContext(ctx).Info("Matrix AppService not active yet; requeueing member provisioning",
				"name", m.Name, "runtimeName", m.RuntimeName)
			return reconcile.Result{RequeueAfter: appServiceNotReadyRequeue}, nil
		}
		return reconcile.Result{}, fmt.Errorf("provision worker: %w", err)
	}

	state.MatrixUserID = provResult.MatrixUserID
	state.RoomID = provResult.RoomID
	state.ProvResult = provResult
	return reconcile.Result{}, nil
}

// EnsureModelProviderAuth authorizes the member's gateway consumer on the
// model provider's HttpApi. No-op when modelProvider is not set.
func EnsureModelProviderAuth(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) error {
	if m.ModelProviderInfo == nil || d.GatewayClient == nil {
		return nil
	}
	if state.ProvResult == nil || state.ProvResult.GatewayKey == "" {
		return nil
	}
	consumerName := "worker-" + m.RuntimeName
	if err := d.GatewayClient.AuthorizeAIRoutes(ctx, consumerName, m.ModelProviderInfo.HttpApiID); err != nil {
		return fmt.Errorf("authorize model provider %s: %w", m.ModelProviderInfo.HttpApiID, err)
	}
	return nil
}

// EnsureMemberServiceAccount ensures the Kubernetes ServiceAccount used by the
// member pod exists. Separated from Infra because SA creation can race with
// the K8s API after namespace setup and benefits from independent retry.
func EnsureMemberServiceAccount(ctx context.Context, d MemberDeps, m MemberContext) error {
	if err := d.Provisioner.EnsureServiceAccount(ctx, m.Name); err != nil {
		return fmt.Errorf("ServiceAccount: %w", err)
	}
	return nil
}

// ReconcileMemberConfig pushes all OSS config (package, inline configs,
// openclaw.json, mcporter, AGENTS.md, builtin skills) for the member.
func ReconcileMemberConfig(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) error {
	if state.ProvResult == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	effectiveRuntime := backend.ResolveRuntime(m.Spec.Runtime, d.DefaultRuntime)
	var aiGatewayURL string
	if m.ModelProviderInfo != nil {
		aiGatewayURL = m.ModelProviderInfo.IntranetURL
	}

	if effectiveRuntime == backend.RuntimeQwenPaw || m.DeployMode == v1beta1.DeployModeEdge {
		leaderRuntimeName := m.TeamLeaderName
		if leaderRuntimeName == "" && m.Role == RoleTeamLeader {
			leaderRuntimeName = m.RuntimeName
		}
		leaderName := leaderRuntimeName
		if leaderName == "" && m.Role == RoleTeamLeader {
			leaderName = m.Name
		}
		runtime := effectiveRuntime
		var matrixAccessToken, gatewayKey string
		skillRegistryURL, skillRegistryAuthType := runtimeSkillRegistryConfig(d, m, state)
		if m.DeployMode == v1beta1.DeployModeEdge {
			runtime = runtimeRemoteManagedLocal
			matrixAccessToken = state.ProvResult.MatrixToken
			gatewayKey = state.ProvResult.GatewayKey
		}
		if err := d.Deployer.DeployMemberRuntimeConfig(ctx, service.MemberRuntimeConfigDeployRequest{
			Name:                  m.Name,
			RuntimeName:           m.RuntimeName,
			Runtime:               runtime,
			Role:                  m.Role.String(),
			Generation:            m.Generation,
			Spec:                  m.Spec,
			MatrixUserID:          state.MatrixUserID,
			PersonalRoomID:        state.RoomID,
			MatrixAccessToken:     matrixAccessToken,
			GatewayKey:            gatewayKey,
			AIGatewayURL:          aiGatewayURL,
			SkillRegistryURL:      skillRegistryURL,
			SkillRegistryAuthType: skillRegistryAuthType,
			TeamName:              m.TeamName,
			TeamRoomID:            m.TeamRoomID,
			LeaderName:            leaderName,
			LeaderRuntimeName:     leaderRuntimeName,
			LeaderDMRoomID:        m.LeaderDMRoomID,
			TeamAdminName:         m.TeamAdminName,
			TeamAdminMatrixID:     m.TeamAdminMatrixID,
			TeamMembers:           m.TeamMembers,
		}); err != nil {
			return fmt.Errorf("deploy runtime config: %w", err)
		}
		return nil
	}

	if err := d.Deployer.DeployPackage(ctx, m.RuntimeName, m.Spec.Package, m.IsUpdate); err != nil {
		return fmt.Errorf("deploy package: %w", err)
	}
	if err := d.Deployer.WriteInlineConfigs(m.RuntimeName, m.Spec); err != nil {
		return fmt.Errorf("write inline configs: %w", err)
	}

	if err := d.Deployer.DeployWorkerConfig(ctx, service.WorkerDeployRequest{
		Name:              m.RuntimeName,
		Spec:              m.Spec,
		Role:              m.Role.String(),
		TeamName:          m.TeamName,
		TeamLeaderName:    m.TeamLeaderName,
		TeamRoomID:        m.TeamRoomID,
		LeaderDMRoomID:    m.LeaderDMRoomID,
		TeamMembers:       m.TeamMembers,
		MatrixToken:       state.ProvResult.MatrixToken,
		GatewayKey:        state.ProvResult.GatewayKey,
		MatrixPassword:    state.ProvResult.MatrixPassword,
		McpServers:        m.Spec.McpServers,
		TeamAdminMatrixID: m.TeamAdminMatrixID,
		Heartbeat:         m.Heartbeat,
		IsUpdate:          m.IsUpdate,
		AIGatewayURL:      aiGatewayURL,
	}); err != nil {
		return fmt.Errorf("deploy worker config: %w", err)
	}

	if err := d.Deployer.PushOnDemandSkills(ctx, m.RuntimeName, m.Spec.Skills, m.Spec.RemoteSkills); err != nil {
		logger.Info("skill push failed", "error", err)
	}
	return nil
}

func runtimeSkillRegistryConfig(d MemberDeps, m MemberContext, state *MemberState) (string, string) {
	if d.EnvBuilder == nil || state == nil || state.ProvResult == nil {
		return "", ""
	}
	env := d.EnvBuilder.Build(m.RuntimeName, state.ProvResult)
	return env["SKILLS_API_URL"], env["NACOS_AUTH_TYPE"]
}

// ReconcileMemberContainer converges the member's backend pod/container with
// the desired lifecycle state (Running / Sleeping / Stopped). Idempotent.
func ReconcileMemberContainer(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) (reconcile.Result, error) {
	if state.ProvResult == nil {
		return reconcile.Result{}, nil
	}

	// Skip container management for non-container workers (remote).
	// When ContainerManaged is explicitly set to false, the controller
	// should not create/delete containers — the user manages the worker
	// process externally (e.g., via systemd).
	if !m.Spec.DesiredContainerMan() {
		log.FromContext(ctx).Info("container management disabled for member, skipping", "name", m.Name)
		return reconcile.Result{}, nil
	}

	desired := m.Spec.DesiredState()
	switch desired {
	case "Stopped":
		return ensureMemberContainerAbsent(ctx, d, m, true, state)
	case "Sleeping":
		return ensureMemberContainerAbsent(ctx, d, m, false, state)
	default:
		return ensureMemberContainerPresent(ctx, d, m, state)
	}
}

func ensureMemberContainerPresent(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) (reconcile.Result, error) {
	if d.Backend == nil {
		return reconcile.Result{}, nil
	}
	logger := log.FromContext(ctx)

	desiredBackend := m.BackendRuntime
	if desiredBackend == "" {
		desiredBackend = v1beta1.BackendRuntimePod
	}
	currentBackend := m.StatusBackendRuntime

	// First reconcile: status is empty, record desired as current directly.
	if currentBackend == "" {
		state.BackendRuntime = desiredBackend
		currentBackend = desiredBackend
	}

	var wb backend.WorkerBackend
	var result *backend.WorkerResult

	// Backend switch: tear down whatever the previous backend created
	// before provisioning the new backend's resource.
	if desiredBackend != currentBackend {
		logger.Info("backend switch detected",
			"name", m.Name, "current", currentBackend, "desired", desiredBackend)
		if oldWb, oldErr := resolveBackendForMember(d.Backend, currentBackend, m); oldErr == nil {
			if delErr := oldWb.Delete(ctx, m.Name); delErr != nil && !errors.Is(delErr, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete old backend resource during switch: %w", delErr)
			}
		}
		state.BackendRuntime = desiredBackend
	}

	if wb == nil {
		var err error
		wb, err = resolveBackendForMember(d.Backend, desiredBackend, m)
		if err != nil {
			logger.Info("no worker backend available, member needs manual start", "name", m.Name, "error", err.Error())
			return reconcile.Result{}, nil
		}
	}

	if result == nil {
		var err error
		result, err = wb.Status(ctx, m.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("query container status: %w", err)
		}
	}
	state.Message = result.Message

	// Spec-change decision is owned by the caller (see MemberContext.SpecChanged
	// doc). Both Worker and Team paths fill this boolean with their own
	// equivalence check so this phase stays agnostic of the upstream CR.
	specChanged := m.SpecChanged
	if specChanged && result.Status != backend.StatusNotFound {
		logger.Info("spec changed, deleting stale backend resource before recreate",
			"name", m.Name,
			"backend", wb.Name(),
			"status", result.Status,
			"rawStatus", result.RawStatus)
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete stale backend resource before recreate: %w", err)
		}
		state.ContainerState = "stopping"
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
	}

	switch result.Status {
	case backend.StatusRunning, backend.StatusStarting, backend.StatusReady:
		state.ContainerState = string(result.Status)
		if wb.Name() == "sandbox" && memberUsesSandboxClaim(m) {
			requeueAfter, tokenMessage, err := refreshSandboxSetWorkerDeps(ctx, d, m, state.ProvResult)
			state.RequeueAfter = minPositiveDuration(state.RequeueAfter, requeueAfter)
			if tokenMessage != "" {
				state.Message = tokenMessage
			}
			if err != nil {
				return reconcile.Result{RequeueAfter: reconcileRetryDelay}, err
			}
		}

		if wb.Name() == "sandbox" {
			if result.Status == backend.StatusStarting {
				return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
			}
			if !memberRuntimeStale(result, m, false) {
				return reconcile.Result{}, nil
			}
			logger.Info("sandbox spec hash mismatch, recreating container",
				"name", m.Name,
				"currentHash", m.CurrentSpecHash,
				"legacyAppliedHash", result.AppliedSpecHash,
				"desiredHash", m.AppliedSpecHash)
			if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete container for recreate: %w", err)
			}
			state.ContainerState = "stopping"
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
		}

		return reconcile.Result{}, nil

	case backend.StatusStopped, backend.StatusSleeping:
		state.ContainerState = string(result.Status)

		// Docker has no annotation account; spec changes are handled by
		// the pre-switch delete path above, so unchanged stopped containers
		// can be resumed here.
		if wb.Name() == "docker" {
			if err := wb.Start(ctx, m.Name); err != nil {
				return reconcile.Result{}, fmt.Errorf("start container: %w", err)
			}
			return reconcile.Result{}, nil
		}

		if wb.Name() == "sandbox" {
			if !memberRuntimeStale(result, m, true) {
				if err := wb.Start(ctx, m.Name); err != nil {
					return reconcile.Result{}, fmt.Errorf("resume sandbox: %w", err)
				}
				return reconcile.Result{}, nil
			}
			logger.Info("sandbox stopped with stale or missing hash, recreating",
				"name", m.Name,
				"currentHash", m.CurrentSpecHash,
				"legacyAppliedHash", result.AppliedSpecHash,
				"desiredHash", m.AppliedSpecHash)
			if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
				return reconcile.Result{}, fmt.Errorf("delete stale sandbox: %w", err)
			}
			state.ContainerState = "stopping"
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
		}

		// Other backends (k8s) keep the historical delete+create path.
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete stale container: %w", err)
		}
		res, err := createMemberContainer(ctx, d, m, state, wb)
		if err == nil {
			state.ContainerState = string(backend.StatusStarting)
		}
		return res, err

	case backend.StatusNotFound:
		res, err := createMemberContainer(ctx, d, m, state, wb)
		if err == nil {
			state.ContainerState = string(backend.StatusStarting)
		} else {
			state.ContainerState = "create_failed"
		}
		return res, err

	default:
		// Transient / unknown state (e.g. PhaseFailed, provider unreachable).
		// Historically this branch eagerly called wb.Delete + recreate,
		// which turned any momentary status blip into a full delete/recreate
		// cycle — the root cause of "sandbox unexpectedly deleted" reports.
		// We now only record the observed state and requeue; recovery is
		// driven by either a watch event on phase transition or the next
		// periodic reconcile.
		state.ContainerState = string(result.Status)
		logger.Info("container in transient state, waiting (no delete)",
			"name", m.Name,
			"status", result.Status,
			"rawStatus", result.RawStatus)
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
	}
}

func ensureMemberContainerAbsent(ctx context.Context, d MemberDeps, m MemberContext, remove bool, state *MemberState) (reconcile.Result, error) {
	if d.Backend == nil {
		return reconcile.Result{}, nil
	}

	// Use the recorded status.backendRuntime to address the actual deployed
	// resource. Falls back to spec when status is empty (first reconcile)
	// and finally to the default "pod" backend.
	currentBackend := m.StatusBackendRuntime
	if currentBackend == "" {
		currentBackend = m.BackendRuntime
	}
	if currentBackend == "" {
		currentBackend = v1beta1.BackendRuntimePod
	}

	wb, err := resolveBackendForMember(d.Backend, currentBackend, m)
	if err != nil {
		return reconcile.Result{}, nil
	}

	result, err := wb.Status(ctx, m.Name)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("query status for absent: %w", err)
	}
	state.ContainerState = string(result.Status)

	// Already gone
	if result.Status == backend.StatusNotFound {
		return reconcile.Result{}, nil
	}

	if remove {
		// Stopped: need full deletion regardless of current state
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container: %w", err)
		}
		state.ContainerState = "stopping"
	} else {
		// Sleeping: skip if already sleeping/stopped
		if result.Status == backend.StatusStopped || result.Status == backend.StatusSleeping {
			return reconcile.Result{}, nil
		}
		if err := wb.Stop(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("stop container: %w", err)
		}
		state.ContainerState = "stopping"
	}
	return reconcile.Result{}, nil
}

func memberRuntimeStale(result *backend.WorkerResult, m MemberContext, missingHashMeansStale bool) bool {
	if m.CurrentSpecHash != "" {
		return m.SpecChanged
	}
	if result != nil && result.AppliedSpecHash != "" {
		return result.AppliedSpecHash != m.AppliedSpecHash
	}
	return m.SpecChanged || missingHashMeansStale
}

func createMemberContainer(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState, wb backend.WorkerBackend) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	prov := state.ProvResult
	if prov == nil || prov.MatrixToken == "" {
		refreshResult, err := d.Provisioner.RefreshWorkerCredentials(ctx, m.Name, m.RuntimeName, m.TeamName)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials for container: %w", err)
		}
		prov = &service.WorkerProvisionResult{
			MatrixUserID:   state.MatrixUserID,
			MatrixToken:    refreshResult.MatrixToken,
			RoomID:         state.RoomID,
			GatewayKey:     refreshResult.GatewayKey,
			MinIOPassword:  refreshResult.MinIOPassword,
			MatrixPassword: refreshResult.MatrixPassword,
		}
		state.ProvResult = prov
	}

	workerEnv, err := buildMemberWorkerEnv(ctx, d, m, prov)
	if err != nil {
		return reconcile.Result{}, err
	}
	workerDeps, tokenRequeueAfter, tokenMessage, err := prepareMemberWorkerDeps(ctx, d, m, workerEnv, true)
	if err != nil {
		return reconcile.Result{}, err
	}
	state.RequeueAfter = minPositiveDuration(state.RequeueAfter, tokenRequeueAfter)
	if tokenMessage != "" {
		state.Message = tokenMessage
	}
	saName := d.ResourcePrefix.SAName(authpkg.RoleWorker, m.Name)

	// Identity labels: callers own the full label set now that the backend
	// is stateless (see A7). The backend only stamps the runtime label.
	labels := make(map[string]string, len(m.PodLabels)+2)
	for k, v := range m.PodLabels {
		labels[k] = v
	}
	labels["app"] = d.ResourcePrefix.WorkerAppLabel()
	labels[v1beta1.LabelWorker] = m.Name

	createReq := backend.CreateRequest{
		Name:               m.Name,
		Image:              m.Spec.Image,
		Runtime:            m.Spec.Runtime,
		RuntimeFallback:    d.DefaultRuntime,
		Env:                workerEnv,
		BackendRuntime:     m.BackendRuntime,
		SandboxSetName:     backend.BuiltinSandboxInstanceName,
		ExtraHosts:         extraHostsForBackend(wb),
		ServiceAccountName: saName,
		AuthExpirationSeconds: backend.NormalizeAuthTokenExpirationSeconds(
			d.AuthTokenExpirationSeconds,
		),
		Resources:   m.Resources,
		Labels:      labels,
		Owner:       m.Owner,
		DeployMode:  m.DeployMode,
		WorkersDeps: workerDeps,
	}
	if wb.Name() != "k8s" && wb.Name() != "sandbox" {
		token, _, err := d.Provisioner.RequestSAToken(ctx, m.Name)
		if err != nil {
			logger.Error(err, "SA token request failed (non-fatal, worker auth will fail)")
		}
		createReq.AuthToken = token

		if err := waitForScopedWorkerConfig(ctx, m.Name, workerEnv); err != nil {
			return reconcile.Result{}, fmt.Errorf("worker scoped storage config is not readable: %w", err)
		}
	}

	if _, err := wb.Create(ctx, createReq); err != nil {
		if errors.Is(err, backend.ErrConflict) {
			return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
		}
		return reconcile.Result{}, fmt.Errorf("create container: %w", err)
	}
	return reconcile.Result{}, nil
}

func waitForScopedWorkerConfig(ctx context.Context, workerName string, workerEnv map[string]string) error {
	accessKey := strings.TrimSpace(workerEnv["AGENTTEAMS_FS_ACCESS_KEY"])
	secretKey := strings.TrimSpace(workerEnv["AGENTTEAMS_FS_SECRET_KEY"])
	if accessKey == "" || secretKey == "" {
		return nil
	}

	endpoint := strings.TrimSpace(os.Getenv("AGENTTEAMS_FS_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(workerEnv["AGENTTEAMS_FS_ENDPOINT"])
	}
	bucket := strings.TrimSpace(workerEnv["AGENTTEAMS_FS_BUCKET"])
	storagePrefix := strings.TrimSpace(workerEnv["AGENTTEAMS_STORAGE_PREFIX"])
	if endpoint == "" || bucket == "" || storagePrefix == "" {
		return nil
	}

	client := oss.NewMinIOClient(oss.Config{
		Alias:         "worker-scoped-readiness-" + workerName,
		Endpoint:      endpoint,
		AccessKey:     accessKey,
		SecretKey:     secretKey,
		Bucket:        bucket,
		StoragePrefix: storagePrefix,
	})

	key := "agents/" + workerName + "/openclaw.json"
	var lastErr error
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := client.Stat(ctx, key); err == nil {
			log.FromContext(ctx).Info("worker scoped storage config readable", "worker", workerName, "key", key)
			return nil
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("stat %s with worker credentials timed out: %w", key, lastErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func refreshSandboxSetWorkerDeps(ctx context.Context, d MemberDeps, m MemberContext, prov *service.WorkerProvisionResult) (time.Duration, string, error) {
	workerEnv, err := buildMemberWorkerEnv(ctx, d, m, prov)
	if err != nil {
		return 0, "", err
	}
	_, requeueAfter, message, err := prepareMemberWorkerDeps(ctx, d, m, workerEnv, false)
	return requeueAfter, message, err
}

func buildMemberWorkerEnv(ctx context.Context, d MemberDeps, m MemberContext, prov *service.WorkerProvisionResult) (map[string]string, error) {
	if d.EnvBuilder == nil {
		return nil, fmt.Errorf("worker env builder is not configured")
	}
	if prov == nil {
		return nil, fmt.Errorf("worker provision result is not available")
	}
	logger := log.FromContext(ctx)
	workerEnv := d.EnvBuilder.Build(m.RuntimeName, prov)
	workerEnv["AGENTTEAMS_WORKER_CR_NAME"] = m.Name
	// Legacy runtime scripts still read this fallback while AgentTeams env adoption is in progress.
	workerEnv["AGENTTEAMS_WORKER_CR_NAME"] = m.Name
	if m.ModelProviderInfo != nil && m.ModelProviderInfo.IntranetURL != "" {
		workerEnv["AGENTTEAMS_AI_GATEWAY_URL"] = m.ModelProviderInfo.IntranetURL
	}
	workerEnv["AGENTTEAMS_WORKER_ROLE"] = m.Role.String()
	mergeUserEnv(workerEnv, m.Spec.Env, logger, string(m.Role)+"/"+m.Name)
	return workerEnv, nil
}

func memberUsesSandboxClaim(m MemberContext) bool {
	return m.BackendRuntime == v1beta1.BackendRuntimeSandbox
}

func prepareMemberWorkerDeps(ctx context.Context, d MemberDeps, m MemberContext, workerEnv map[string]string, forceTokenProjection bool) (*backend.WorkerDepsSpec, time.Duration, string, error) {
	usesSandboxClaim := memberUsesSandboxClaim(m)
	if !usesSandboxClaim {
		if len(m.Spec.Volumes) > 0 || len(m.Spec.Mounts) > 0 {
			return nil, 0, "", fmt.Errorf("spec.volumes/spec.mounts are only supported when backendRuntime is sandbox")
		}
		return nil, 0, "", nil
	}

	resolved, err := resolveSandboxWorkerDeps(d, m)
	if err != nil {
		return nil, 0, "", err
	}

	projection, err := projectSandboxSetWorkerToken(ctx, d, m, forceTokenProjection)
	if err != nil {
		return nil, 0, "", err
	}
	tokenMount := resolved.Mounts[workerDepsMountToken]
	envMount := resolved.Mounts[workerDepsMountEnv]
	workerEnv["AGENTTEAMS_AUTH_TOKEN_FILE"] = strings.TrimRight(tokenMount.MountPath, "/") + "/token"
	workerEnv["AGENTTEAMS_WORKER_ENV_MOUNT_DIR"] = strings.TrimRight(envMount.MountPath, "/")
	workerEnv["AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED"] = "1"
	if workerEnv["AGENTTEAMS_RUNTIME"] == "" {
		workerEnv["AGENTTEAMS_RUNTIME"] = "k8s"
	}
	if err := validateSandboxWorkerDepsStoragePrefix(workerEnv["AGENTTEAMS_STORAGE_PREFIX"], d.WorkerDepsStorageBucket); err != nil {
		return nil, 0, "", err
	}

	if err := ensureWorkerDepsMountResources(ctx, d, m, resolved.BuiltinVolume, true); err != nil {
		return nil, 0, "", err
	}
	for _, volume := range resolved.UniqueCustomVolumes() {
		if err := ensureWorkerDepsMountResources(ctx, d, m, volume, false); err != nil {
			return nil, 0, "", err
		}
	}

	if err := prepareWorkerDepsObjects(ctx, d, m, resolved, workerEnv, projection.Token, projection.Write); err != nil {
		return nil, 0, "", err
	}

	deps := &backend.WorkerDepsSpec{
		InplaceUpdateImage: m.Spec.Image,
	}
	for _, name := range []string{workerDepsMountToken, workerDepsMountEnv, workerDepsMountData} {
		mount := resolved.Mounts[name]
		deps.DynamicVolumeMounts = append(deps.DynamicVolumeMounts, backend.WorkerDepsDynamicVolumeMount{
			PVName:     mount.VolumeRef,
			MountPath:  mount.MountPath,
			SubPath:    mount.SubPath,
			ReadOnly:   mount.ReadOnly,
			Attributes: workerDepsDynamicMountAttributes(resolved.BuiltinVolume, name),
		})
	}
	for _, mount := range resolved.CustomMounts {
		deps.DynamicVolumeMounts = append(deps.DynamicVolumeMounts, backend.WorkerDepsDynamicVolumeMount{
			PVName:    mount.VolumeRef,
			MountPath: mount.MountPath,
			SubPath:   mount.SubPath,
			ReadOnly:  mount.ReadOnly,
		})
	}
	return deps, projection.RequeueAfter, projection.Message, nil
}

const (
	workerDepsMountToken = "token"
	workerDepsMountEnv   = "env"
	workerDepsMountData  = "data"
)

var workerDepsMountReadOnly = map[string]bool{
	workerDepsMountToken: true,
	workerDepsMountEnv:   true,
	workerDepsMountData:  false,
}

type sandboxWorkerDeps struct {
	Mounts        map[string]v1beta1.WorkerMountSpec
	CustomMounts  []v1beta1.WorkerMountSpec
	BuiltinVolume v1beta1.WorkerVolumeSpec
	Volumes       map[string]v1beta1.WorkerVolumeSpec
}

func (d sandboxWorkerDeps) UniqueCustomVolumes() []v1beta1.WorkerVolumeSpec {
	seen := map[string]struct{}{}
	out := make([]v1beta1.WorkerVolumeSpec, 0, len(d.Volumes))
	for _, mount := range d.CustomMounts {
		if _, ok := seen[mount.VolumeRef]; ok {
			continue
		}
		seen[mount.VolumeRef] = struct{}{}
		out = append(out, d.Volumes[mount.VolumeRef])
	}
	return out
}

func resolveSandboxWorkerDeps(deps MemberDeps, m MemberContext) (sandboxWorkerDeps, error) {
	instanceName := backend.BuiltinSandboxInstanceName
	builtinAuth, err := builtinWorkerDepsAuth(deps, instanceName)
	if err != nil {
		return sandboxWorkerDeps{}, err
	}
	resolved := sandboxWorkerDeps{
		Mounts: defaultWorkerDepsMounts(instanceName, m.RuntimeName),
		BuiltinVolume: v1beta1.WorkerVolumeSpec{
			Name: instanceName,
			Type: v1beta1.WorkerVolumeTypeOSS,
			OSS: &v1beta1.WorkerOSSVolumeSpec{
				Bucket:   deps.WorkerDepsStorageBucket,
				Endpoint: deps.WorkerDepsStorageEndpoint,
				Auth:     builtinAuth,
			},
		},
		Volumes: map[string]v1beta1.WorkerVolumeSpec{},
	}
	if resolved.BuiltinVolume.OSS.Bucket == "" || resolved.BuiltinVolume.OSS.Endpoint == "" {
		return resolved, fmt.Errorf("sandbox worker-deps requires main workspace OSS bucket and endpoint")
	}
	if err := validateWorkerDepsVolume(-1, resolved.BuiltinVolume); err != nil {
		return resolved, err
	}
	for i, volume := range m.Spec.Volumes {
		if err := validateWorkerDepsVolume(i, volume); err != nil {
			return resolved, err
		}
		if _, exists := resolved.Volumes[volume.Name]; exists {
			return resolved, fmt.Errorf("spec.volumes[%d].name %q is duplicated", i, volume.Name)
		}
		if volume.Name == instanceName {
			return resolved, fmt.Errorf("spec.volumes[%d].name %q is reserved for built-in worker-deps PV", i, volume.Name)
		}
		resolved.Volumes[volume.Name] = volume
	}
	seenMounts := map[string]struct{}{}
	for i, mount := range m.Spec.Mounts {
		if _, exists := seenMounts[mount.Name]; exists {
			return resolved, fmt.Errorf("spec.mounts[%d].name %q is duplicated", i, mount.Name)
		}
		seenMounts[mount.Name] = struct{}{}
		if isWorkerDepsReservedMount(mount.Name) {
			continue
		}
		if err := validateCustomWorkerMount(i, mount, resolved.Volumes); err != nil {
			return resolved, err
		}
		resolved.CustomMounts = append(resolved.CustomMounts, mount)
	}
	return resolved, nil
}

func builtinWorkerDepsAuth(deps MemberDeps, instanceName string) (v1beta1.WorkerOSSAuthSpec, error) {
	authType, err := normalizeWorkerDepsMountAuthType(deps.MountAuthType)
	if err != nil {
		return v1beta1.WorkerOSSAuthSpec{}, err
	}
	switch authType {
	case workerDepsAuthTypeAccessKey:
		return v1beta1.WorkerOSSAuthSpec{
			Type: workerDepsAuthTypeAccessKey,
			AccessKey: &v1beta1.WorkerAccessKeyAuthSpec{
				SecretRef: v1beta1.NamespacedSecretRef{Name: instanceName},
			},
		}, nil
	case workerDepsAuthTypeRRSA:
		roleName := strings.TrimSpace(deps.MountRoleName)
		if roleName == "" {
			return v1beta1.WorkerOSSAuthSpec{}, fmt.Errorf("AGENTTEAMS_MOUNT_ROLE_NAME is required when AGENTTEAMS_MOUNT_AUTH_TYPE=RRSA")
		}
		return v1beta1.WorkerOSSAuthSpec{
			Type: workerDepsAuthTypeRRSA,
			RRSA: &v1beta1.WorkerOSSRRSASpec{RoleName: roleName},
		}, nil
	default:
		return v1beta1.WorkerOSSAuthSpec{}, fmt.Errorf("unsupported AGENTTEAMS_MOUNT_AUTH_TYPE %q", deps.MountAuthType)
	}
}

func workerDepsDynamicMountAttributes(volume v1beta1.WorkerVolumeSpec, mountName string) map[string]string {
	if volume.OSS == nil || volume.OSS.Auth.Type != workerDepsAuthTypeRRSA {
		return nil
	}
	return map[string]string{"credentialProviderName": workerDepsCredentialProviderName(mountName)}
}

func normalizeWorkerDepsMountAuthType(authType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "", "rrsa":
		return workerDepsAuthTypeRRSA, nil
	case "accesskey", "access-key", "access_key":
		return workerDepsAuthTypeAccessKey, nil
	default:
		return "", fmt.Errorf("AGENTTEAMS_MOUNT_AUTH_TYPE must be RRSA or AccessKey")
	}
}

func defaultWorkerDepsMounts(instanceName, workerName string) map[string]v1beta1.WorkerMountSpec {
	return map[string]v1beta1.WorkerMountSpec{
		workerDepsMountToken: {
			Name:      workerDepsMountToken,
			VolumeRef: instanceName,
			SubPath:   workerDepsSubPath(workerName, workerDepsMountToken),
			MountPath: "/var/run/secrets/agentteams",
			ReadOnly:  true,
		},
		workerDepsMountEnv: {
			Name:      workerDepsMountEnv,
			VolumeRef: instanceName,
			SubPath:   workerDepsSubPath(workerName, workerDepsMountEnv),
			MountPath: "/mnt/agentteams/env",
			ReadOnly:  true,
		},
		workerDepsMountData: {
			Name:      workerDepsMountData,
			VolumeRef: instanceName,
			SubPath:   workerDepsSubPath(workerName, workerDepsMountData),
			MountPath: "/mnt/agentteams/data",
			ReadOnly:  false,
		},
	}
}

func workerDepsSubPath(workerName, name string) string {
	return path.Join("workers-deps", workerName, name)
}

func validateSandboxWorkerDepsStoragePrefix(storagePrefix, bucket string) error {
	storagePrefix = strings.Trim(storagePrefix, "/")
	bucket = strings.TrimSpace(bucket)
	if storagePrefix == "" || bucket == "" {
		return nil
	}
	parts := strings.Split(storagePrefix, "/")
	if len(parts) != 2 || parts[1] != bucket {
		return fmt.Errorf("sandbox built-in worker-deps requires AGENTTEAMS_STORAGE_PREFIX to be <alias>/<bucket> matching AGENTTEAMS_FS_BUCKET; got %q with bucket %q", storagePrefix, bucket)
	}
	return nil
}

func isWorkerDepsReservedMount(name string) bool {
	_, ok := workerDepsMountReadOnly[name]
	return ok
}

func validateWorkerDepsVolume(i int, volume v1beta1.WorkerVolumeSpec) error {
	prefix := "built-in worker-deps volume"
	if i >= 0 {
		prefix = fmt.Sprintf("spec.volumes[%d]", i)
	}
	if volume.Name == "" {
		return fmt.Errorf("%s.name is required", prefix)
	}
	if volume.Type != v1beta1.WorkerVolumeTypeOSS {
		return fmt.Errorf("%s.type must be OSS", prefix)
	}
	if volume.OSS == nil {
		return fmt.Errorf("%s.oss is required when type is OSS", prefix)
	}
	if volume.OSS.Bucket == "" || volume.OSS.Endpoint == "" {
		return fmt.Errorf("%s.oss.bucket and endpoint are required", prefix)
	}
	switch volume.OSS.Auth.Type {
	case "RRSA":
		if volume.OSS.Auth.RRSA == nil || (volume.OSS.Auth.RRSA.RoleName == "" && volume.OSS.Auth.RRSA.RoleARN == "") {
			return fmt.Errorf("%s.oss.auth.rrsa.roleName or roleArn is required", prefix)
		}
	case "AccessKey":
		if volume.OSS.Auth.AccessKey == nil || volume.OSS.Auth.AccessKey.SecretRef.Name == "" {
			return fmt.Errorf("%s.oss.auth.accessKey.secretRef.name is required", prefix)
		}
		if volume.OSS.Auth.AccessKey.SecretRef.Namespace != "" {
			return fmt.Errorf("%s.oss.auth.accessKey.secretRef.namespace must be empty; AccessKey secrets are looked up by CSI in the worker targetNamespace", prefix)
		}
	case "":
		return fmt.Errorf("%s.oss.auth.type is required", prefix)
	default:
		return fmt.Errorf("%s.oss.auth.type must be RRSA or AccessKey", prefix)
	}
	return nil
}

func validateCustomWorkerMount(i int, mount v1beta1.WorkerMountSpec, volumes map[string]v1beta1.WorkerVolumeSpec) error {
	if mount.Name == "" {
		return fmt.Errorf("spec.mounts[%d].name is required", i)
	}
	if mount.VolumeRef == "" {
		return fmt.Errorf("spec.mounts[%d].volumeRef is required", i)
	}
	if _, ok := volumes[mount.VolumeRef]; !ok {
		return fmt.Errorf("spec.mounts[%d].volumeRef %q does not reference spec.volumes", i, mount.VolumeRef)
	}
	if mount.SubPath == "" {
		return fmt.Errorf("spec.mounts[%d].subPath is required", i)
	}
	if mount.MountPath == "" {
		return fmt.Errorf("spec.mounts[%d].mountPath is required", i)
	}
	if isBuiltinWorkerDepsMountPath(mount.MountPath) {
		return fmt.Errorf("spec.mounts[%d].mountPath %q overlaps built-in worker-deps mount paths", i, mount.MountPath)
	}
	return nil
}

func isBuiltinWorkerDepsMountPath(mountPath string) bool {
	clean := path.Clean(mountPath)
	for _, builtin := range []string{"/var/run/secrets/agentteams", "/mnt/agentteams/env", "/mnt/agentteams/data"} {
		if clean == builtin || strings.HasPrefix(builtin, clean+"/") || strings.HasPrefix(clean, builtin+"/") {
			return true
		}
	}
	return false
}

func prepareWorkerDepsObjects(ctx context.Context, d MemberDeps, m MemberContext, deps sandboxWorkerDeps, workerEnv map[string]string, token string, writeToken bool) error {
	req := service.WorkerDepsPrepareRequest{
		WorkerName:  m.RuntimeName,
		DataSubPath: deps.Mounts[workerDepsMountData].SubPath,
		Token:       token,
		Env:         workerEnv,
		EnvSubPath:  deps.Mounts[workerDepsMountEnv].SubPath,
		UseEnv:      true,
	}
	if writeToken {
		req.TokenSubPath = deps.Mounts[workerDepsMountToken].SubPath
		req.UseToken = true
	}
	return d.Deployer.PrepareWorkerDeps(ctx, req)
}

type sandboxSetTokenProjection struct {
	Token        string
	Write        bool
	RequeueAfter time.Duration
	Message      string
}

func projectSandboxSetWorkerToken(ctx context.Context, d MemberDeps, m MemberContext, force bool) (sandboxSetTokenProjection, error) {
	if d.Provisioner == nil {
		return sandboxSetTokenProjection{}, fmt.Errorf("sandbox claim token projector requires worker provisioner")
	}
	key := sandboxSetTokenProjectionKey(m)
	now := time.Now()
	if !force {
		if cached, ok := sandboxSetTokenProjections.Load(key); ok {
			state := cached.(sandboxSetTokenProjectionState)
			if !state.NextRefresh.IsZero() && now.Before(state.NextRefresh) {
				return sandboxSetTokenProjection{RequeueAfter: time.Until(state.NextRefresh)}, nil
			}
		}
	}
	expirationSeconds := backend.NormalizeAuthTokenExpirationSeconds(d.AuthTokenExpirationSeconds)
	projection, err := d.Provisioner.ProjectSAToken(ctx, m.Name, expirationSeconds)
	if err != nil {
		if cached, ok := sandboxSetTokenProjections.Load(key); ok {
			state := cached.(sandboxSetTokenProjectionState)
			if now.Before(state.Expiration) {
				message := fmt.Sprintf("Degraded: sandbox claim token refresh failed; existing token is valid until %s and will retry in %s", state.Expiration.Format(time.RFC3339), sandboxSetTokenRetryAfter)
				log.FromContext(ctx).Error(err, "sandbox claim token projection refresh failed; existing token is still valid",
					"name", m.Name, "expiration", state.Expiration)
				return sandboxSetTokenProjection{RequeueAfter: sandboxSetTokenRetryAfter, Message: message}, nil
			}
		}
		return sandboxSetTokenProjection{}, fmt.Errorf("request sandbox claim token for worker %s: %w", m.Name, err)
	}
	if projection == nil || projection.Token == "" {
		return sandboxSetTokenProjection{}, fmt.Errorf("request sandbox claim token for worker %s: empty token", m.Name)
	}
	issuedAt := projection.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = now
	}
	expiresAt := projection.ExpirationTimestamp
	if expiresAt.IsZero() {
		expiresAt = issuedAt.Add(time.Duration(projection.ExpirationSeconds) * time.Second)
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return sandboxSetTokenProjection{}, fmt.Errorf("request sandbox claim token for worker %s: invalid expiration %s", m.Name, expiresAt.Format(time.RFC3339))
	}
	nextRefresh := sandboxSetTokenNextRefresh(m, issuedAt, expiresAt)
	sandboxSetTokenProjections.Store(key, sandboxSetTokenProjectionState{
		NextRefresh: nextRefresh,
		Expiration:  expiresAt,
	})
	return sandboxSetTokenProjection{
		Token:        projection.Token,
		Write:        true,
		RequeueAfter: time.Until(nextRefresh),
	}, nil
}

func sandboxSetTokenProjectionKey(m MemberContext) string {
	return m.Namespace + "/" + m.Name + "/" + m.RuntimeName
}

func sandboxSetTokenNextRefresh(m MemberContext, issuedAt, expiresAt time.Time) time.Time {
	ttl := expiresAt.Sub(issuedAt)
	if ttl <= 0 {
		return time.Now().Add(sandboxSetTokenRetryAfter)
	}
	byAge := issuedAt.Add(ttl * 8 / 10)
	byDeadline := expiresAt.Add(-10 * time.Minute)
	next := byAge
	if byDeadline.Before(next) {
		next = byDeadline
	}
	next = next.Add(tokenRefreshJitter(m.Name))
	latest := expiresAt.Add(-1 * time.Minute)
	if next.After(latest) {
		next = latest
	}
	minNext := time.Now().Add(sandboxSetTokenRetryAfter)
	if next.Before(minNext) {
		next = minNext
	}
	return next
}

func tokenRefreshJitter(key string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum32()%uint32(sandboxSetTokenRefreshJitterMax/time.Second)) * time.Second
}

func ensureWorkerDepsMountResources(ctx context.Context, d MemberDeps, m MemberContext, volume v1beta1.WorkerVolumeSpec, builtIn bool) error {
	dynClient, namespace, err := resolveMemberDynamicClient(ctx, d, m)
	if err != nil {
		return err
	}
	if dynClient == nil {
		return fmt.Errorf("workers-deps volume %s requires a dynamic client to create storage resources", volume.Name)
	}
	objects := workerDepsMountResourceObjects(volume, namespace, builtIn)
	for _, obj := range objects {
		if err := createWorkerDepsObjectIfMissing(ctx, dynClient, obj); err != nil {
			return err
		}
	}
	return nil
}

func workerDepsMountResourceObjects(volume v1beta1.WorkerVolumeSpec, namespace string, builtIn bool) []*unstructured.Unstructured {
	if volume.OSS != nil && volume.OSS.Auth.Type == workerDepsAuthTypeAccessKey {
		return []*unstructured.Unstructured{buildAccessKeyWorkerDepsPersistentVolume(volume, namespace)}
	}
	if builtIn && volume.OSS != nil && volume.OSS.Auth.Type == workerDepsAuthTypeRRSA {
		return []*unstructured.Unstructured{
			buildRRSAWorkerDepsPersistentVolume(volume),
			buildWorkerDepsCredentialProvider(volume, namespace, workerDepsMountEnv),
			buildWorkerDepsCredentialProvider(volume, namespace, workerDepsMountToken),
			buildWorkerDepsCredentialProvider(volume, namespace, workerDepsMountData),
			buildWorkerDepsAgentIdentity(namespace),
			buildWorkerDepsAgentRole(namespace),
			buildWorkerDepsAgentRoleBinding(namespace),
		}
	}
	return []*unstructured.Unstructured{
		buildWorkerDepsStorageClass(volume, namespace),
		buildWorkerDepsPersistentVolume(volume, namespace),
		buildWorkerDepsPersistentVolumeClaim(volume, namespace),
	}
}

func resolveMemberDynamicClient(ctx context.Context, d MemberDeps, m MemberContext) (dynamic.Interface, string, error) {
	namespace := m.Namespace
	return d.DynamicClient, namespace, nil
}

func createWorkerDepsObjectIfMissing(ctx context.Context, dynClient dynamic.Interface, obj *unstructured.Unstructured) error {
	gvr := workerDepsObjectGVR(obj)
	name := obj.GetName()
	ns := obj.GetNamespace()
	if ns != "" {
		res := dynClient.Resource(gvr).Namespace(ns)
		existing, err := res.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return updateWorkerDepsObjectIfNeeded(ctx, res, existing, obj)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
		}
		if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
		}
		return nil
	}
	res := dynClient.Resource(gvr)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return updateWorkerDepsObjectIfNeeded(ctx, res, existing, obj)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	return nil
}

func updateWorkerDepsObjectIfNeeded(ctx context.Context, res dynamic.ResourceInterface, existing, desired *unstructured.Unstructured) error {
	switch desired.GetKind() {
	case "CredentialProvider", "AgentIdentity", "AgentRole", "AgentRoleBinding":
	default:
		return nil
	}

	labels := map[string]string{}
	for k, v := range existing.GetLabels() {
		labels[k] = v
	}
	for k, v := range desired.GetLabels() {
		labels[k] = v
	}
	desiredSpec, ok, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("desired %s spec is missing", desired.GetKind())
	}
	existingSpec, _, err := unstructured.NestedMap(existing.Object, "spec")
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.GetLabels(), labels) && reflect.DeepEqual(existingSpec, desiredSpec) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.SetLabels(labels)
	updated.Object["spec"] = desiredSpec
	if _, err := res.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update workers-deps %s %s: %w", desired.GetKind(), desired.GetName(), err)
	}
	return nil
}

func workerDepsObjectGVR(obj *unstructured.Unstructured) schema.GroupVersionResource {
	switch obj.GetKind() {
	case "StorageClass":
		return workerDepsStorageClassGVR
	case "PersistentVolume":
		return workerDepsPVGVR
	case "CredentialProvider":
		return workerDepsCredentialProviderGVR
	case "AgentIdentity":
		return workerDepsAgentIdentityGVR
	case "AgentRole":
		return workerDepsAgentRoleGVR
	case "AgentRoleBinding":
		return workerDepsAgentRoleBindingGVR
	default:
		return workerDepsPVCGVR
	}
}

func buildWorkerDepsStorageClass(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	provisioner := ackOSSCSIProvisioner
	parameters := map[string]interface{}{}
	if volume.OSS != nil {
		parameters["bucket"] = volume.OSS.Bucket
		parameters["url"] = volume.OSS.Endpoint
		parameters["path"] = "/"
		if volume.OSS.Auth.Type == "RRSA" && volume.OSS.Auth.RRSA != nil {
			parameters["authType"] = "rrsa"
			if volume.OSS.Auth.RRSA.RoleName != "" {
				parameters["roleName"] = volume.OSS.Auth.RRSA.RoleName
			}
			if volume.OSS.Auth.RRSA.RoleARN != "" {
				parameters["roleArn"] = volume.OSS.Auth.RRSA.RoleARN
			}
		}
		if volume.OSS.Auth.Type == "AccessKey" && volume.OSS.Auth.AccessKey != nil {
			parameters["authType"] = "ak"
			parameters["csi.storage.k8s.io/node-publish-secret-name"] = volume.OSS.Auth.AccessKey.SecretRef.Name
			parameters["csi.storage.k8s.io/node-publish-secret-namespace"] = defaultSecretNamespace(volume.OSS.Auth.AccessKey.SecretRef.Namespace, namespace)
		}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": workerDepsObjectLabels(volume),
		},
		"provisioner":       provisioner,
		"reclaimPolicy":     "Retain",
		"volumeBindingMode": "Immediate",
		"parameters":        parameters,
	}}
}

func buildWorkerDepsPersistentVolume(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	driver := ackOSSCSIProvisioner
	attrs := map[string]interface{}{}
	var secretRef map[string]interface{}
	if volume.OSS != nil {
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["path"] = "/"
		if volume.OSS.Auth.Type == "RRSA" && volume.OSS.Auth.RRSA != nil {
			attrs["authType"] = "rrsa"
			if volume.OSS.Auth.RRSA.RoleName != "" {
				attrs["roleName"] = volume.OSS.Auth.RRSA.RoleName
			}
			if volume.OSS.Auth.RRSA.RoleARN != "" {
				attrs["roleArn"] = volume.OSS.Auth.RRSA.RoleARN
			}
		}
		if volume.OSS.Auth.Type == "AccessKey" && volume.OSS.Auth.AccessKey != nil {
			secretRef = map[string]interface{}{
				"name":      volume.OSS.Auth.AccessKey.SecretRef.Name,
				"namespace": defaultSecretNamespace(volume.OSS.Auth.AccessKey.SecretRef.Namespace, namespace),
			}
		}
	}
	csi := map[string]interface{}{
		"driver":           driver,
		"volumeHandle":     volume.Name,
		"volumeAttributes": attrs,
	}
	if secretRef != nil && secretRef["name"] != "" {
		csi["nodePublishSecretRef"] = secretRef
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": workerDepsObjectLabels(volume),
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": defaultWorkerDepsStorageCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              volume.Name,
			"csi":                           csi,
		},
	}}
}

func buildAccessKeyWorkerDepsPersistentVolume(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	attrs := map[string]interface{}{}
	secretRef := map[string]interface{}{}
	if volume.OSS != nil {
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["otherOpts"] = accessKeyWorkerDepsOtherOpts
		if volume.OSS.Auth.AccessKey != nil {
			secretRef = map[string]interface{}{
				"name":      volume.OSS.Auth.AccessKey.SecretRef.Name,
				"namespace": namespace,
			}
		}
	}
	labels := workerDepsObjectLabels(volume)
	labels["alicloud-pvname"] = volume.Name
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": labels,
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": accessKeyWorkerDepsPVCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              accessKeyWorkerDepsStorageClass,
			"volumeMode":                    "Filesystem",
			"csi": map[string]interface{}{
				"driver":               ackOSSCSIProvisioner,
				"nodePublishSecretRef": secretRef,
				"volumeAttributes":     attrs,
				"volumeHandle":         volume.Name,
			},
		},
	}}
}

func buildRRSAWorkerDepsPersistentVolume(volume v1beta1.WorkerVolumeSpec) *unstructured.Unstructured {
	attrs := map[string]interface{}{}
	if volume.OSS != nil {
		attrs["authType"] = "agent-identity"
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["otherOpts"] = accessKeyWorkerDepsOtherOpts
	}
	labels := workerDepsObjectLabels(volume)
	labels["alicloud-pvname"] = volume.Name
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": labels,
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": accessKeyWorkerDepsPVCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              accessKeyWorkerDepsStorageClass,
			"volumeMode":                    "Filesystem",
			"csi": map[string]interface{}{
				"driver":           ackOSSCSIProvisioner,
				"volumeAttributes": attrs,
				"volumeHandle":     volume.Name,
			},
		},
	}}
}

func workerDepsCredentialProviderName(mountName string) string {
	return backend.BuiltinSandboxInstanceName + "-" + mountName
}

func buildWorkerDepsCredentialProvider(volume v1beta1.WorkerVolumeSpec, namespace, mountName string) *unstructured.Unstructured {
	roleName := ""
	if volume.OSS != nil && volume.OSS.Auth.RRSA != nil {
		roleName = volume.OSS.Auth.RRSA.RoleName
	}
	labels := workerDepsObjectLabels(volume)
	labels["agentteams.io/sandboxset"] = backend.BuiltinSandboxInstanceName
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "CredentialProvider",
		"metadata": map[string]interface{}{
			"name":      workerDepsCredentialProviderName(mountName),
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"type": "RAM",
			"ram": map[string]interface{}{
				"source": map[string]interface{}{
					"provider": "RRSA",
					"rrsa": map[string]interface{}{
						"roleName": roleName,
						"policy":   workerDepsCredentialProviderPolicy(),
					},
				},
			},
		},
	}}
}

func buildWorkerDepsAgentIdentity(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    workerDepsFixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"description": "this is for agentteams",
		},
	}}
}

func buildWorkerDepsAgentRole(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentRole",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    workerDepsFixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"rules": []interface{}{
				workerDepsAgentRoleRule(workerDepsCredentialProviderName(workerDepsMountEnv)),
				workerDepsAgentRoleRule(workerDepsCredentialProviderName(workerDepsMountToken)),
				workerDepsAgentRoleRule(workerDepsCredentialProviderName(workerDepsMountData)),
			},
		},
	}}
}

func workerDepsAgentRoleRule(resource string) map[string]interface{} {
	return map[string]interface{}{
		"effect":   "Allow",
		"action":   "GetResourceCredential",
		"resource": "CredentialProvider/" + resource,
	}
}

func buildWorkerDepsAgentRoleBinding(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentRoleBinding",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    workerDepsFixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"agentRoleRef": map[string]interface{}{
				"apiGroup": "agentidentity.alibabacloud.com",
				"kind":     "AgentRole",
				"name":     backend.BuiltinSandboxInstanceName,
			},
			"subjects": []interface{}{
				map[string]interface{}{
					"authorizationType": "Agent",
					"agentAuthorizationConfiguration": map[string]interface{}{
						"agentName": backend.BuiltinSandboxInstanceName,
					},
				},
			},
		},
	}}
}

func workerDepsCredentialProviderPolicy() string {
	policy := map[string]interface{}{
		"Version": "1",
		"Statement": []map[string]interface{}{
			{
				"Action": []string{
					"oss:GetObject",
					"oss:PutObject",
					"oss:DeleteObject",
					"oss:AbortMultipartUpload",
					"oss:ListMultipartUploads",
				},
				"Effect": "Allow",
				"Resource": []string{
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}",
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}/*",
				},
			},
			{
				"Action": []string{
					"oss:ListObjects",
				},
				"Effect": "Allow",
				"Resource": []string{
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}",
				},
				"Condition": map[string]interface{}{
					"StringLike": map[string]interface{}{
						"oss:Prefix": []string{
							"${ack:agent-identity/storage-auth/sub-path}/*",
						},
					},
				},
			},
		},
	}
	out, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

func defaultSecretNamespace(secretNamespace, fallback string) string {
	if secretNamespace != "" {
		return secretNamespace
	}
	return fallback
}

func buildWorkerDepsPersistentVolumeClaim(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      volume.Name,
			"namespace": namespace,
			"labels":    workerDepsObjectLabels(volume),
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteMany"},
			"storageClassName": volume.Name,
			"volumeName":       volume.Name,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"storage": defaultWorkerDepsStorageCapacity,
				},
			},
		},
	}}
}

func workerDepsObjectLabels(volume v1beta1.WorkerVolumeSpec) map[string]interface{} {
	labels := workerDepsFixedObjectLabels()
	labels["agentteams.io/mount-provider"] = strings.ToLower(volume.Type)
	return labels
}

func workerDepsFixedObjectLabels() map[string]interface{} {
	return map[string]interface{}{
		"agentteams.io/managed-by":   "agentteams-controller",
		"agentteams.io/workers-deps": "true",
	}
}

func minPositiveDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

// computeMemberPhase derives the lifecycle phase from desired state,
// observed container status, and reconcile outcome. Single source of truth
// for both Worker and Team member paths.
func computeMemberPhase(currentPhase, matrixUserID, desiredState, containerState string, reconcileErr error) string {
	if reconcileErr != nil {
		if matrixUserID == "" {
			return "Failed"
		}
		if currentPhase == "" {
			return "Pending"
		}
		return currentPhase
	}
	switch desiredState {
	case "Sleeping":
		if containerState == "stopping" {
			return "Stopping"
		}
		return "Sleeping"
	case "Stopped":
		if containerState == "stopping" {
			return "Stopping"
		}
		return "Stopped"
	default: // Running
		if containerState == string(backend.StatusRunning) || containerState == string(backend.StatusReady) {
			return "Running"
		}
		if containerState == string(backend.StatusStarting) {
			return "Starting"
		}
		if containerState == string(backend.StatusFailed) {
			return "Failed"
		}
		return "Pending"
	}
}

func extraHostsForBackend(wb backend.WorkerBackend) []string {
	if wb != nil && wb.Name() == "docker" {
		return []string{dockerHostInternalExtraHost}
	}
	return nil
}

// ReconcileMemberExpose reconciles gateway port exposure for the member.
// Non-fatal: logs and returns current state unchanged on failure. The returned
// slice overwrites state.ExposedPorts on success.
func ReconcileMemberExpose(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) error {
	if len(m.Spec.Expose) == 0 && len(m.CurrentExposedPorts) == 0 {
		state.ExposedPorts = nil
		return nil
	}
	exposedPorts, err := d.Provisioner.ReconcileExpose(ctx, m.Name, m.Spec.Expose, m.CurrentExposedPorts)
	if err != nil {
		if errors.Is(err, gateway.ErrUnsupportedOp) {
			log.FromContext(ctx).V(1).Info("gateway provider does not manage exposed ports; skipping", "name", m.Name)
			state.ExposedPorts = m.CurrentExposedPorts
			return nil
		}
		log.FromContext(ctx).Error(err, "failed to reconcile exposed ports (non-fatal)", "name", m.Name)
		state.ExposedPorts = m.CurrentExposedPorts
		return nil
	}
	state.ExposedPorts = exposedPorts
	return nil
}

// ReconcileMemberDelete performs full infra/container/storage cleanup for a
// member. Does NOT remove finalizers or touch the legacy Manager groupAllowFrom
// / workers registry — those concerns belong to the owning reconciler because
// they have different rules for standalone vs team contexts.
func ReconcileMemberDelete(ctx context.Context, d MemberDeps, m MemberContext) error {
	logger := log.FromContext(ctx)
	logger.Info("deleting member", "name", m.Name, "role", m.Role)
	sandboxSetTokenProjections.Delete(sandboxSetTokenProjectionKey(m))

	if err := d.Provisioner.LeaveAllWorkerRooms(ctx, m.RuntimeName); err != nil {
		logger.Error(err, "member leave-all-rooms failed (non-fatal)", "name", m.Name, "runtimeName", m.RuntimeName)
	}
	if m.ExistingRoomID != "" {
		if err := d.Provisioner.DeleteWorkerRoom(ctx, m.ExistingRoomID); err != nil {
			logger.Error(err, "member room delete command failed (non-fatal)",
				"name", m.Name, "runtimeName", m.RuntimeName, "roomID", m.ExistingRoomID)
		}
	}

	isTeamWorker := m.Role == RoleTeamWorker || m.Role == RoleTeamLeader
	if err := d.Provisioner.DeprovisionWorker(ctx, service.WorkerDeprovisionRequest{
		Name:         m.RuntimeName,
		IsTeamWorker: isTeamWorker,
		ExposedPorts: m.CurrentExposedPorts,
		ExposeSpec:   m.Spec.Expose,
	}); err != nil {
		logger.Error(err, "deprovision failed (non-fatal)", "name", m.Name)
	}

	// Explicitly delete the member container as part of the finalizer.
	//
	// For the Kubernetes backend this is technically redundant with the
	// controller OwnerReference stamped in K8sBackend.Create — K8s GC
	// would eventually collect the Pod — but doing it here keeps Pod
	// cleanup synchronous with finalizer completion, surfaces backend
	// errors in our own logs, and still leaves OwnerReference as a
	// safety net if an operator patches the finalizer off. For the
	// Docker backend (embedded mode) there is no K8s garbage collector
	// (the embedded apiserver runs without kube-controller-manager) and
	// worker containers are Docker objects the apiserver does not know
	// about, so this is the only reliable cleanup path.
	if d.Backend != nil {
		currentBackend := m.StatusBackendRuntime
		if currentBackend == "" {
			currentBackend = m.BackendRuntime
		}
		if currentBackend == "" {
			currentBackend = v1beta1.BackendRuntimePod
		}
		if wb, err := resolveBackendForMember(d.Backend, currentBackend, m); err == nil {
			if derr := wb.Delete(ctx, m.Name); derr != nil && !errors.Is(derr, backend.ErrNotFound) {
				logger.Error(derr, "failed to delete member container (may already be removed)", "name", m.Name)
			}
		}
		// Safety net: if spec disagrees with status, also try to delete
		// from the spec-side backend so a partially-completed switch on
		// the previous reconcile does not leak resources.
		if m.BackendRuntime != "" && m.BackendRuntime != currentBackend {
			if altWb, err := resolveBackendForMember(d.Backend, m.BackendRuntime, m); err == nil {
				if derr := altWb.Delete(ctx, m.Name); derr != nil && !errors.Is(derr, backend.ErrNotFound) {
					logger.Error(derr, "failed to delete member container on alternate backend (may already be removed)", "name", m.Name)
				}
			}
		}
	}

	if err := d.Deployer.CleanupOSSData(ctx, m.RuntimeName); err != nil {
		logger.Error(err, "failed to clean up OSS agent data (non-fatal)", "name", m.Name, "runtimeName", m.RuntimeName)
	}
	if err := d.Provisioner.DeleteWorkerCredentials(ctx, m.Name); err != nil {
		logger.Error(err, "failed to delete credentials (non-fatal)", "name", m.Name, "runtimeName", m.RuntimeName)
	}
	if err := d.Provisioner.DeleteServiceAccount(ctx, m.Name); err != nil {
		logger.Error(err, "failed to delete ServiceAccount (non-fatal)", "name", m.Name)
	}
	// Every worker (standalone, team leader, team worker) owns a per-worker
	// comm room created by ProvisionWorker. Release its alias here so a
	// future Worker/Team CR with the same runtime identity can reclaim it
	// cleanly — the underlying room is left intact to preserve history.
	if err := d.Provisioner.DeleteWorkerRoomAlias(ctx, m.RuntimeName); err != nil {
		logger.Error(err, "failed to delete worker room alias (non-fatal)", "name", m.Name, "runtimeName", m.RuntimeName)
	}
	// Clean up any ClusterIP Services associated with this member.
	// Uses label-based selection to catch all naming conventions.
	if d.Backend != nil {
		svcDeps := &MemberDeps{Backend: d.Backend, ResourcePrefix: d.ResourcePrefix}
		if err := ensureServiceDeleted(ctx, &m, svcDeps); err != nil {
			logger.Error(err, "failed to delete member services (non-fatal)", "name", m.Name)
		}
	}
	return nil
}
