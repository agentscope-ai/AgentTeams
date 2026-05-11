package controller

import (
	"context"
	"errors"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// String renders the role as stored in annotations / legacy registries.
func (r MemberRole) String() string { return string(r) }

// MemberContext carries the CR-independent inputs needed to converge a single
// worker-like member (standalone worker, team leader, or team worker). The
// WorkerReconciler builds one from a Worker CR; the TeamReconciler builds one
// per Team member directly from the Team CR without ever materializing a
// Worker CR.
type MemberContext struct {
	Name      string
	Namespace string
	Role      MemberRole
	Spec      v1beta1.WorkerSpec

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
	//   WorkerReconciler: w.Generation != w.Status.ObservedGeneration
	//   TeamReconciler:   hashOf(spec) != Team.Status.MemberSpecHashes[name]
	//
	// Using a boolean (instead of reusing Generation != ObservedGeneration)
	// isolates the "did the spec change" question from the transport that
	// answers it, so Team members — which have no per-member Generation —
	// can participate without abusing the int64 fields.
	SpecChanged bool

	// IsUpdate indicates the member has been successfully provisioned before;
	// controls MCP reauthorization and deployer "update" semantics.
	IsUpdate bool

	// Team linkage (empty for standalone).
	TeamName          string
	TeamLeaderName    string
	TeamAdminMatrixID string

	// Heartbeat config from Team CR leader spec (nil for non-leader members)
	Heartbeat *agentconfig.HeartbeatConfig

	// ExistingMatrixUserID is non-empty when prior provisioning has recorded a
	// Matrix user; the Infra phase then uses RefreshCredentials instead of
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
	// members to tag pods with "hiclaw.io/team=<teamName>" so the Team
	// reconciler can watch member pod lifecycle events.
	PodLabels map[string]string

	// Owner is the CR that logically owns the member's Pod lifecycle. The
	// K8s backend stamps it as the Pod's controller OwnerReference so that
	// deleting the owning CR garbage-collects the Pod. For standalone
	// Workers this is the Worker CR; for Team members (leader or worker)
	// this is the Team CR.
	Owner metav1.Object
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
}

// MemberDeps aggregates the service-layer dependencies the member phases
// invoke. Both WorkerReconciler and TeamReconciler build a MemberDeps once
// and pass it through each phase.
type MemberDeps struct {
	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Backend     *backend.Registry
	EnvBuilder  service.WorkerEnvBuilderI

	// ResourcePrefix is the tenant-level prefix that scopes ServiceAccount
	// (and Pod) names for every member this reconciler provisions. Empty
	// prefix collapses to the pre-multi-tenant naming scheme
	// (`hiclaw-worker-<name>`), preserving single-tenant deployments. It is
	// populated by the owning reconciler (WorkerReconciler / TeamReconciler)
	// from Config.ResourcePrefix.
	ResourcePrefix authpkg.ResourcePrefix

	// DefaultRuntime is forwarded into backend.CreateRequest.RuntimeFallback
	// by createMemberContainer when a member leaves spec.runtime empty.
	// Populated by the owning reconciler (WorkerReconciler / TeamReconciler)
	// from HICLAW_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime). An
	// empty string means "no operator preference" — backend.ResolveRuntime
	// will then fall back to RuntimeOpenClaw. ManagerReconciler uses its own
	// DefaultRuntime field (sourced from HICLAW_MANAGER_RUNTIME) directly on
	// backend.CreateRequest and does not go through MemberDeps, since
	// Backend.Create is shared between Worker and Manager paths and only the
	// caller knows which env var applies.
	DefaultRuntime string
}

// ReconcileMemberInfra ensures Matrix account, Gateway consumer, MinIO user,
// and DM room are provisioned (or credentials refreshed). Writes MatrixUserID,
// RoomID, and ProvResult into state.
func ReconcileMemberInfra(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) (reconcile.Result, error) {
	if m.ExistingMatrixUserID != "" {
		refreshResult, err := d.Provisioner.RefreshCredentials(ctx, m.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials: %w", err)
		}

		// Defensively re-authorize the worker on AI routes. Mirrors the
		// Manager restart path: if the Higress skeleton was ever rewritten
		// (historically by the Initializer's EnsureAIRoute, or by a fresh
		// Higress state after upgrade), allowedConsumers may have been
		// reset to [] and the worker would stay locked out with 403s until
		// the next spec-change event. Surfacing errors lets controller-
		// runtime re-queue with backoff.
		if err := d.Provisioner.EnsureWorkerGatewayAuth(ctx, m.Name, refreshResult.GatewayKey); err != nil {
			return reconcile.Result{}, fmt.Errorf("restore worker gateway auth: %w", err)
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

	log.FromContext(ctx).Info("provisioning member infrastructure", "name", m.Name, "role", m.Role)

	provResult, err := d.Provisioner.ProvisionWorker(ctx, service.WorkerProvisionRequest{
		Name:           m.Name,
		Role:           m.Role.String(),
		TeamName:       m.TeamName,
		TeamLeaderName: m.TeamLeaderName,
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("provision worker: %w", err)
	}

	state.MatrixUserID = provResult.MatrixUserID
	state.RoomID = provResult.RoomID
	state.ProvResult = provResult
	return reconcile.Result{}, nil
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

	if err := d.Deployer.DeployPackage(ctx, m.Name, m.Spec.Package, m.IsUpdate); err != nil {
		return fmt.Errorf("deploy package: %w", err)
	}
	if err := d.Deployer.WriteInlineConfigs(m.Name, m.Spec); err != nil {
		return fmt.Errorf("write inline configs: %w", err)
	}

	if err := d.Deployer.DeployWorkerConfig(ctx, service.WorkerDeployRequest{
		Name:              m.Name,
		Spec:              m.Spec,
		Role:              m.Role.String(),
		TeamName:          m.TeamName,
		TeamLeaderName:    m.TeamLeaderName,
		MatrixToken:       state.ProvResult.MatrixToken,
		GatewayKey:        state.ProvResult.GatewayKey,
		MatrixPassword:    state.ProvResult.MatrixPassword,
		McpServers:        m.Spec.McpServers,
		TeamAdminMatrixID: m.TeamAdminMatrixID,
		Heartbeat:         m.Heartbeat,
		IsUpdate:          m.IsUpdate,
	}); err != nil {
		return fmt.Errorf("deploy worker config: %w", err)
	}

	if err := d.Deployer.PushOnDemandSkills(ctx, m.Name, m.Spec.Skills); err != nil {
		logger.Error(err, "skill push failed (non-fatal)")
	}
	return nil
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
		return ensureMemberContainerAbsent(ctx, d, m, true)
	case "Sleeping":
		return ensureMemberContainerAbsent(ctx, d, m, false)
	default:
		return ensureMemberContainerPresent(ctx, d, m, state)
	}
}

func ensureMemberContainerPresent(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) (reconcile.Result, error) {
	if d.Backend == nil {
		return reconcile.Result{}, nil
	}
	wb := d.Backend.DetectWorkerBackend(ctx)
	if wb == nil {
		log.FromContext(ctx).Info("no worker backend available, member needs manual start", "name", m.Name)
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx)
	result, err := wb.Status(ctx, m.Name)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("query container status: %w", err)
	}

	// Spec-change decision is owned by the caller (see MemberContext.SpecChanged
	// doc). Both Worker and Team paths fill this boolean with their own
	// equivalence check so this phase stays agnostic of the upstream CR.
	specChanged := m.SpecChanged

	switch result.Status {
	case backend.StatusRunning, backend.StatusStarting, backend.StatusReady:
		state.ContainerState = string(result.Status)
		if !specChanged {
			return reconcile.Result{}, nil
		}
		logger.Info("spec changed, recreating container",
			"name", m.Name,
			"generation", m.Generation,
			"observedGeneration", m.ObservedGeneration)
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container for recreate: %w", err)
		}
		return createMemberContainer(ctx, d, m, state, wb)

	case backend.StatusStopped:
		state.ContainerState = string(result.Status)
		if wb.Name() == "docker" && !specChanged {
			if err := wb.Start(ctx, m.Name); err != nil {
				return reconcile.Result{}, fmt.Errorf("start container: %w", err)
			}
			return reconcile.Result{}, nil
		}
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete stale container: %w", err)
		}
		return createMemberContainer(ctx, d, m, state, wb)

	case backend.StatusNotFound:
		return createMemberContainer(ctx, d, m, state, wb)

	default:
		state.ContainerState = string(result.Status)
		logger.Info("container in unexpected state, recreating", "name", m.Name, "status", result.Status)
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container in unknown state: %w", err)
		}
		return createMemberContainer(ctx, d, m, state, wb)
	}
}

func ensureMemberContainerAbsent(ctx context.Context, d MemberDeps, m MemberContext, remove bool) (reconcile.Result, error) {
	if d.Backend == nil {
		return reconcile.Result{}, nil
	}
	wb := d.Backend.DetectWorkerBackend(ctx)
	if wb == nil {
		return reconcile.Result{}, nil
	}
	if remove {
		if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("delete container: %w", err)
		}
	} else {
		if err := wb.Stop(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return reconcile.Result{}, fmt.Errorf("stop container: %w", err)
		}
	}
	return reconcile.Result{}, nil
}

func createMemberContainer(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState, wb backend.WorkerBackend) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	prov := state.ProvResult
	if prov == nil || prov.MatrixToken == "" {
		refreshResult, err := d.Provisioner.RefreshCredentials(ctx, m.Name)
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

	workerEnv := d.EnvBuilder.Build(m.Name, prov)
	mergeUserEnv(workerEnv, m.Spec.Env, logger, string(m.Role)+"/"+m.Name)
	saName := d.ResourcePrefix.SAName(authpkg.RoleWorker, m.Name)

	// Identity labels: callers own the full label set now that the backend
	// is stateless (see A7). The backend only stamps hiclaw.io/runtime.
	labels := make(map[string]string, len(m.PodLabels)+2)
	for k, v := range m.PodLabels {
		labels[k] = v
	}
	labels["app"] = d.ResourcePrefix.WorkerAppLabel()
	labels["hiclaw.io/worker"] = m.Name

	createReq := backend.CreateRequest{
		Name:               m.Name,
		Image:              m.Spec.Image,
		Runtime:            m.Spec.Runtime,
		RuntimeFallback:    d.DefaultRuntime,
		Env:                workerEnv,
		ServiceAccountName: saName,
		Labels:             labels,
		Owner:              m.Owner,
	}
	if wb.Name() != "k8s" {
		token, err := d.Provisioner.RequestSAToken(ctx, m.Name)
		if err != nil {
			logger.Error(err, "SA token request failed (non-fatal, worker auth will fail)")
		}
		createReq.AuthToken = token
	}

	if _, err := wb.Create(ctx, createReq); err != nil {
		if errors.Is(err, backend.ErrConflict) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("create container: %w", err)
	}
	return reconcile.Result{}, nil
}

// ReconcileMemberExpose reconciles Higress port exposure for the member.
// Non-fatal: logs and returns current state unchanged on failure. The returned
// slice overwrites state.ExposedPorts on success.
func ReconcileMemberExpose(ctx context.Context, d MemberDeps, m MemberContext, state *MemberState) error {
	if len(m.Spec.Expose) == 0 && len(m.CurrentExposedPorts) == 0 {
		state.ExposedPorts = nil
		return nil
	}
	exposedPorts, err := d.Provisioner.ReconcileExpose(ctx, m.Name, m.Spec.Expose, m.CurrentExposedPorts)
	if err != nil {
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

	if err := d.Provisioner.LeaveAllWorkerRooms(ctx, m.Name); err != nil {
		logger.Error(err, "member leave-all-rooms failed (non-fatal)", "name", m.Name)
	}
	if m.ExistingRoomID != "" {
		if err := d.Provisioner.DeleteWorkerRoom(ctx, m.ExistingRoomID); err != nil {
			logger.Error(err, "member room delete command failed (non-fatal)",
				"name", m.Name, "roomID", m.ExistingRoomID)
		}
	}

	isTeamWorker := m.Role == RoleTeamWorker || m.Role == RoleTeamLeader
	if err := d.Provisioner.DeprovisionWorker(ctx, service.WorkerDeprovisionRequest{
		Name:         m.Name,
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
		if wb := d.Backend.DetectWorkerBackend(ctx); wb != nil {
			if err := wb.Delete(ctx, m.Name); err != nil && !errors.Is(err, backend.ErrNotFound) {
				logger.Error(err, "failed to delete member container (may already be removed)", "name", m.Name)
			}
		}
	}

	if err := d.Deployer.CleanupOSSData(ctx, m.Name); err != nil {
		logger.Error(err, "failed to clean up OSS agent data (non-fatal)", "name", m.Name)
	}
	if err := d.Provisioner.DeleteCredentials(ctx, m.Name); err != nil {
		logger.Error(err, "failed to delete credentials (non-fatal)", "name", m.Name)
	}
	if err := d.Provisioner.DeleteServiceAccount(ctx, m.Name); err != nil {
		logger.Error(err, "failed to delete ServiceAccount (non-fatal)", "name", m.Name)
	}
	// Every worker (standalone, team leader, team worker) owns a per-worker
	// comm room created by ProvisionWorker. Release its alias here so a
	// future Worker/Team CR with the same name can reclaim it cleanly —
	// the underlying room is left intact to preserve history.
	if err := d.Provisioner.DeleteWorkerRoomAlias(ctx, m.Name); err != nil {
		logger.Error(err, "failed to delete worker room alias (non-fatal)", "name", m.Name)
	}
	return nil
}
