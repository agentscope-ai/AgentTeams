package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/agentconfig"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Team cache field indexer keys. Registered in app.initFieldIndexers and
// consumed by the auth enricher to resolve team membership by worker name
// without enumerating every Team.
const (
	TeamLeaderNameField    = "spec.leader.name"
	TeamWorkerNameField    = "spec.workerNames"
	TeamWorkerMembersField = "spec.workerMembers.name"
	migrationFinalizerName = "agentteams.io/migration-in-flight"
)

// TeamReconciler reconciles Team resources that reference existing Worker CRs
// through spec.workerMembers.
type TeamReconciler struct {
	client.Client

	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Backend     *backend.Registry
	EnvBuilder  service.WorkerEnvBuilderI
	Legacy      *service.LegacyCompat // nil in incluster mode

	// DefaultRuntime is forwarded into MemberDeps.DefaultRuntime for every
	// team member this reconciler converges. Sourced from
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime) — NOT from
	// AGENTTEAMS_MANAGER_RUNTIME — because team leader and worker containers are
	// both created through backend.WorkerBackend.Create as worker-type pods.
	// Empty means "no operator preference"; backend.ResolveRuntime then falls
	// back to RuntimeOpenClaw.
	DefaultRuntime string

	// DefaultBackendRuntime is the cluster-level default backendRuntime ("pod" or "sandbox").
	// Used for inline team members and as fallback for decoupled members without spec.backendRuntime.
	// Sourced from AGENTTEAMS_WORKER_BACKEND_RUNTIME env var.
	DefaultBackendRuntime string

	AgentFSDir string // for writing inline configs to the local agent FS

	// ControllerName, when non-empty, is merged as agentteams.io/controller
	// into the PodLabels of every team member MemberContext this reconciler
	// builds, so the resulting Pods match the owning controller instance's
	// label-scoped cache. Post-refactor (PR #666) Teams no longer create
	// child Worker CRs, so the label is applied directly to Pods via
	// MemberContext.PodLabels → backend.CreateRequest.Labels. Empty in
	// embedded mode.
	ControllerName string

	// WorkerDepsStorageBucket/Endpoint identify the main workspace OSS bucket
	// used for the built-in sandbox token/env/data mounts.
	WorkerDepsStorageBucket   string
	WorkerDepsStorageEndpoint string
	MountAuthType             string
	MountRoleName             string

	// ResourcePrefix scopes team-member ServiceAccount and Pod names per
	// AgentTeams tenant instance. Forwarded into MemberDeps.ResourcePrefix so
	// createMemberContainer uses it when computing saName. Empty collapses
	// to DefaultResourcePrefix ("hiclaw-").
	ResourcePrefix auth.ResourcePrefix

	GatewayClient               gateway.Client // gateway client for modelProvider resolution
	DynamicClient               dynamic.Interface
	RemoteDynamicClientProvider backend.RemoteDynamicClientProvider
	AuthTokenExpirationSeconds  int64

	// SystemAdminUser is the global system admin username (from
	// AGENTTEAMS_ADMIN_USER). Resolved to a full Matrix user ID and always
	// included in every worker's allowlist so the operator admin retains
	// visibility regardless of team membership.
	SystemAdminUser string
}

type teamAdminActor struct {
	MatrixUserID string
	Token        string
	Username     string
}

func (r *TeamReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("team", start, reterr) }()

	logger := log.FromContext(ctx)

	var team v1beta1.Team
	if err := r.Get(ctx, req.NamespacedName, &team); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !team.DeletionTimestamp.IsZero() {
		changed := false
		if controllerutil.ContainsFinalizer(&team, finalizerName) {
			if err := r.handleDelete(ctx, &team); err != nil {
				logger.Error(err, "failed to delete team", "name", team.Name)
				return reconcile.Result{RequeueAfter: 30 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(&team, finalizerName)
			changed = true
		}
		if controllerutil.ContainsFinalizer(&team, migrationFinalizerName) {
			controllerutil.RemoveFinalizer(&team, migrationFinalizerName)
			changed = true
		}
		if changed {
			if err := r.Update(ctx, &team); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&team, finalizerName) {
		controllerutil.AddFinalizer(&team, finalizerName)
		if err := r.Update(ctx, &team); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileTeamNormal(ctx, &team)
}

func (r *TeamReconciler) resolveTeamAdminActor(ctx context.Context, t *v1beta1.Team) (teamAdminActor, error) {
	if t.Spec.Admin == nil {
		return teamAdminActor{}, nil
	}
	if strings.TrimSpace(t.Spec.Admin.Name) == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human name is required")
	}

	var human v1beta1.Human
	key := client.ObjectKey{Name: t.Spec.Admin.Name, Namespace: t.Namespace}
	if err := r.Get(ctx, key, &human); err != nil {
		return teamAdminActor{}, fmt.Errorf("load team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}

	humanProv, ok := r.Provisioner.(service.HumanProvisioner)
	if !ok {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s requires HumanProvisioner support", key.Namespace, key.Name)
	}
	identity, err := humanidentity.ResolveHuman(&human.Spec, human.Name, humanidentity.Deps{Provisioner: humanProv})
	if err != nil {
		return teamAdminActor{}, fmt.Errorf("resolve team admin human %s/%s identity: %w", key.Namespace, key.Name, err)
	}
	matrixUserID := human.Status.MatrixUserID
	if matrixUserID == "" {
		if human.Spec.IdentitySource != nil {
			return teamAdminActor{}, fmt.Errorf("team admin human %s/%s uses an external identity source but is not provisioned yet",
				key.Namespace, key.Name)
		}
		matrixUserID = identity.MatrixUserID
	}
	if matrixUserID != identity.MatrixUserID {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s status.matrixUserID %q does not match resolved identity %q",
			key.Namespace, key.Name, matrixUserID, identity.MatrixUserID)
	}
	if t.Spec.Admin.MatrixUserID != "" && t.Spec.Admin.MatrixUserID != matrixUserID {
		return teamAdminActor{}, fmt.Errorf("team admin matrixUserId %q does not match Human %s/%s matrix user %q",
			t.Spec.Admin.MatrixUserID, key.Namespace, key.Name, matrixUserID)
	}
	if identity.ManagesInitialPassword && !r.Provisioner.MatrixAppServiceEnabled() && human.Status.InitialPassword == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s has no initial password; cannot obtain Matrix token",
			key.Namespace, key.Name)
	}

	token, err := identity.Source.EnsureUserToken(ctx, &human.Spec, &human.Status, human.Name)
	if err != nil {
		return teamAdminActor{}, fmt.Errorf("login as team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}
	if token == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s has no Matrix token", key.Namespace, key.Name)
	}
	return teamAdminActor{
		MatrixUserID: matrixUserID,
		Token:        token,
		Username:     identity.MatrixLocalpart,
	}, nil
}

// deriveTeamWithResolvedIdentities returns a deep copy of t with the team
// admin and every human member's MatrixUserID populated from the
// authoritative Human-CR identity. This makes the rest of the reconcile —
// room invites, coordinator power levels, channel policies, runtime roster —
// operate on the real Matrix identity for both legacy-password and SSO
// Humans instead of the legacy "localpart == name" derivation. The
// spec-provided matrixUserId is only kept when the referenced Human CR is
// missing or not yet provisioned.
func (r *TeamReconciler) deriveTeamWithResolvedIdentities(ctx context.Context, t *v1beta1.Team, adminActor teamAdminActor) *v1beta1.Team {
	derived := t.DeepCopy()
	if adminActor.MatrixUserID != "" {
		if derived.Spec.Admin == nil {
			derived.Spec.Admin = &v1beta1.TeamAdminSpec{}
		}
		derived.Spec.Admin.MatrixUserID = adminActor.MatrixUserID
	}
	r.appendAccessibleTeamHumans(ctx, derived)
	for i := range derived.Spec.HumanMembers {
		derived.Spec.HumanMembers[i].MatrixUserID = r.resolveHumanMemberMatrixUserID(ctx, t.Namespace, derived.Spec.HumanMembers[i])
	}
	return derived
}

func (r *TeamReconciler) appendAccessibleTeamHumans(ctx context.Context, t *v1beta1.Team) {
	var humans v1beta1.HumanList
	if err := r.List(ctx, &humans, client.InNamespace(t.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "failed to list humans for accessibleTeams")
		return
	}

	seen := make(map[string]struct{}, len(t.Spec.HumanMembers))
	for _, member := range t.Spec.HumanMembers {
		if member.Name != "" {
			seen[member.Name] = struct{}{}
		}
		if member.MatrixUserID != "" {
			seen[member.MatrixUserID] = struct{}{}
		}
	}
	for i := range humans.Items {
		human := &humans.Items[i]
		if !containsString(human.Spec.AccessibleTeams, t.Name) {
			continue
		}
		if _, ok := seen[human.Name]; ok {
			continue
		}
		matrixUserID, err := r.resolveHumanMatrixUserID(human)
		if err != nil {
			log.FromContext(ctx).Info("human accessibleTeam member not ready",
				"team", t.Name, "human", human.Name, "err", err.Error())
			continue
		}
		if _, ok := seen[matrixUserID]; ok {
			continue
		}
		t.Spec.HumanMembers = append(t.Spec.HumanMembers, v1beta1.TeamMemberSpec{
			Name:         human.Name,
			Role:         "coordinator",
			MatrixUserID: matrixUserID,
		})
		seen[human.Name] = struct{}{}
		seen[matrixUserID] = struct{}{}
	}
}

func (r *TeamReconciler) syncTeamRoomHumanStatuses(ctx context.Context, namespace, teamName, roomID string, members []v1beta1.TeamMemberSpec) {
	if roomID == "" {
		return
	}
	var humans v1beta1.HumanList
	if err := r.List(ctx, &humans, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "failed to list humans for team room status sync",
			"team", teamName, "room", roomID)
		return
	}

	desiredNames := make(map[string]struct{}, len(members))
	desiredMatrixIDs := make(map[string]struct{}, len(members))
	for _, member := range members {
		if member.Name != "" {
			desiredNames[member.Name] = struct{}{}
		}
		if member.MatrixUserID != "" {
			desiredMatrixIDs[member.MatrixUserID] = struct{}{}
		}
	}

	logger := log.FromContext(ctx)
	for i := range humans.Items {
		human := &humans.Items[i]
		_, desiredByName := desiredNames[human.Name]
		_, desiredByMatrixID := desiredMatrixIDs[human.Status.MatrixUserID]
		desired := desiredByName || desiredByMatrixID || containsString(human.Spec.AccessibleTeams, teamName)
		hasRoom := containsString(human.Status.Rooms, roomID)
		if desired == hasRoom {
			continue
		}

		base := human.DeepCopy()
		if desired {
			human.Status.Rooms = append(human.Status.Rooms, roomID)
		} else {
			human.Status.Rooms = removeString(human.Status.Rooms, roomID)
		}
		if err := r.Status().Patch(ctx, human, client.MergeFrom(base)); err != nil {
			logger.Error(err, "failed to sync human team room status",
				"team", teamName, "human", human.Name, "room", roomID)
		}
	}
}

func (r *TeamReconciler) resolveHumanMatrixUserID(human *v1beta1.Human) (string, error) {
	if human.Status.MatrixUserID != "" {
		return human.Status.MatrixUserID, nil
	}
	humanProv, ok := r.Provisioner.(service.HumanProvisioner)
	if !ok {
		return "", fmt.Errorf("human %s requires HumanProvisioner support", human.Name)
	}
	identity, err := humanidentity.ResolveHuman(&human.Spec, human.Name, humanidentity.Deps{Provisioner: humanProv})
	if err != nil {
		return "", err
	}
	if human.Spec.IdentitySource != nil {
		return "", fmt.Errorf("human %s uses an external identity source but is not provisioned yet", human.Name)
	}
	return identity.MatrixUserID, nil
}

// resolveHumanMemberMatrixUserID returns the authoritative Matrix user ID for
// a team human member, preferring the referenced Human CR's provisioned
// identity over the spec-provided hint. Falls back to the spec value when the
// Human CR is missing or not yet provisioned, so legacy behavior (and callers
// that pass an explicit matrixUserId without a backing Human CR) is preserved.
func (r *TeamReconciler) resolveHumanMemberMatrixUserID(ctx context.Context, namespace string, member v1beta1.TeamMemberSpec) string {
	if strings.TrimSpace(member.Name) != "" {
		var human v1beta1.Human
		key := client.ObjectKey{Name: member.Name, Namespace: namespace}
		if err := r.Get(ctx, key, &human); err == nil && human.Status.MatrixUserID != "" {
			return human.Status.MatrixUserID
		}
	}
	return member.MatrixUserID
}

func (r *TeamReconciler) reconcileTeamNormal(ctx context.Context, t *v1beta1.Team) (reconcile.Result, error) {
	patchBase := client.MergeFrom(t.DeepCopy())
	if t.Status.Phase == "" {
		t.Status.Phase = "Pending"
		if err := r.Status().Patch(ctx, t, patchBase); err != nil {
			return reconcile.Result{}, err
		}
		patchBase = client.MergeFrom(t.DeepCopy())
	}

	if len(t.Spec.WorkerMembers) == 0 && (t.Spec.Leader.Name != "" || len(t.Spec.Workers) > 0) {
		return r.reconcileTeamLegacy(ctx, t, patchBase)
	}
	return r.reconcileTeamDecoupled(ctx, t, patchBase)
}

func (r *TeamReconciler) reconcileTeamLegacy(ctx context.Context, t *v1beta1.Team, patchBase client.Patch) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if t.Spec.Leader.Name == "" {
		return r.failTeam(ctx, t, patchBase, "leader.name is required")
	}

	adminActor, err := r.resolveTeamAdminActor(ctx, t)
	if err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	derivedTeam := r.deriveTeamWithResolvedIdentities(ctx, t, adminActor)
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	leaderRuntimeName := t.Spec.Leader.EffectiveWorkerName()
	workerRuntimeNames := make([]string, 0, len(t.Spec.Workers))
	for _, worker := range t.Spec.Workers {
		workerRuntimeNames = append(workerRuntimeNames, worker.EffectiveWorkerName())
	}

	rooms, err := r.Provisioner.ProvisionTeamRooms(ctx, service.TeamRoomRequest{
		TeamName:             teamRuntimeName,
		LeaderName:           leaderRuntimeName,
		LeaderCredentialName: t.Spec.Leader.Name,
		WorkerNames:          workerRuntimeNames,
		AdminSpec:            derivedTeam.Spec.Admin,
		HumanMembers:         derivedTeam.Spec.HumanMembers,
		TeamAdminActorToken:  adminActor.Token,
		TeamAdminActorName:   adminActor.Username,
	})
	if err != nil {
		return r.failTeam(ctx, t, patchBase, fmt.Sprintf("provision team rooms: %v", err))
	}
	t.Status.TeamRoomID = rooms.TeamRoomID
	t.Status.LeaderDMRoomID = rooms.LeaderDMRoomID
	r.syncTeamRoomHumanStatuses(ctx, t.Namespace, t.Name, rooms.TeamRoomID, derivedTeam.Spec.HumanMembers)

	if err := r.Deployer.EnsureTeamStorage(ctx, teamRuntimeName); err != nil {
		logger.Error(err, "team shared storage init failed (non-fatal)", "name", t.Name, "teamName", teamRuntimeName)
	}

	members := r.legacyTeamMembers(derivedTeam, rooms, teamRuntimeName, leaderRuntimeName)
	keep := make(map[string]struct{}, len(members))
	for _, member := range members {
		keep[member.Name] = struct{}{}
	}
	r.cleanupStaleLegacyMembers(ctx, t, keep)
	pruneMembers(&t.Status, keep)
	roster := r.runtimeConfigTeamMembers(derivedTeam, members)

	deps := MemberDeps{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}

	var requeueAfter time.Duration
	var degradedMessages []string
	for i := range members {
		members[i].TeamMembers = roster
		if members[i].Spec.ModelProvider != "" && r.GatewayClient != nil {
			info, err := r.GatewayClient.ResolveModelProvider(ctx, members[i].Spec.ModelProvider)
			if err != nil {
				return r.failTeam(ctx, t, patchBase, fmt.Sprintf("resolve model provider %q: %v", members[i].Spec.ModelProvider, err))
			}
			members[i].ModelProviderInfo = info
		}
		ms := memberStatus(&t.Status, members[i].Name, members[i].Role)
		res, err := r.reconcileMember(ctx, deps, members[i], ms)
		r.reconcileLegacyMember(ctx, derivedTeam, members[i], ms)
		if err != nil {
			ms.Phase = "Failed"
			ms.Message = err.Error()
			degradedMessages = append(degradedMessages, fmt.Sprintf("%s: %v", members[i].Name, err))
			requeueAfter = minPositiveDuration(requeueAfter, reconcileRetryDelay)
			continue
		}
		requeueAfter = minPositiveDuration(requeueAfter, res.RequeueAfter)
	}

	if err := r.injectLegacyTeamContext(ctx, derivedTeam, members, rooms, roster); err != nil {
		logger.Error(err, "legacy team context injection failed (non-fatal)")
	}
	if r.Legacy != nil && r.Legacy.Enabled() {
		if err := r.Legacy.UpdateManagerGroupAllowFrom(r.Legacy.MatrixUserID(leaderRuntimeName), true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom for team leader (non-fatal)")
		}
		if err := r.Legacy.UpdateTeamsRegistry(service.TeamRegistryEntry{
			Name:           teamRuntimeName,
			Leader:         t.Spec.Leader.Name,
			Workers:        legacyTeamWorkerNames(t.Spec.Workers),
			TeamRoomID:     rooms.TeamRoomID,
			LeaderDMRoomID: rooms.LeaderDMRoomID,
			Admin:          teamAdminRegistryEntry(derivedTeam.Spec.Admin),
			Members:        teamMemberRegistryEntries(derivedTeam.Spec.HumanMembers),
		}); err != nil {
			logger.Error(err, "teams-registry update failed (non-fatal)")
		}
	}

	leaderReady, readyWorkers := r.summarizeBackendReadiness(ctx, t, members)
	sortMembers(&t.Status)
	t.Status.TotalWorkers = len(t.Spec.Workers)
	t.Status.LeaderReady = leaderReady
	t.Status.ReadyWorkers = readyWorkers
	if len(degradedMessages) > 0 {
		t.Status.Phase = "Degraded"
		t.Status.Message = strings.Join(degradedMessages, "; ")
	} else if leaderReady && readyWorkers == len(t.Spec.Workers) {
		t.Status.Phase = "Active"
		t.Status.Message = ""
	} else {
		t.Status.Phase = "Pending"
		t.Status.Message = ""
	}
	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		logger.Error(err, "failed to patch team status (non-fatal)")
	}

	logger.Info("team reconciled",
		"name", t.Name,
		"phase", t.Status.Phase,
		"leaderReady", leaderReady,
		"readyWorkers", readyWorkers,
		"totalWorkers", t.Status.TotalWorkers,
		"members", observedMemberNames(&t.Status))
	return reconcile.Result{RequeueAfter: minPositiveDuration(reconcileInterval, requeueAfter)}, nil
}

func (r *TeamReconciler) legacyTeamMembers(t *v1beta1.Team, rooms *service.TeamRoomResult, teamRuntimeName, leaderRuntimeName string) []MemberContext {
	members := make([]MemberContext, 0, 1+len(t.Spec.Workers))
	leaderSpec := legacyLeaderWorkerSpec(t.Spec.Leader)
	leader := r.legacyMemberContext(t, t.Spec.Leader.Name, leaderRuntimeName, RoleTeamLeader, leaderSpec, teamRuntimeName, "", rooms)
	switch {
	case t.Spec.Leader.Heartbeat != nil:
		leader.Heartbeat = &agentconfig.HeartbeatConfig{
			Enabled: t.Spec.Leader.Heartbeat.Enabled,
			Every:   t.Spec.Leader.Heartbeat.Every,
		}
	case t.Spec.HeartbeatEvery != "":
		leader.Heartbeat = &agentconfig.HeartbeatConfig{Enabled: true, Every: t.Spec.HeartbeatEvery}
	}
	members = append(members, leader)

	for _, worker := range t.Spec.Workers {
		members = append(members, r.legacyMemberContext(
			t,
			worker.Name,
			worker.EffectiveWorkerName(),
			RoleTeamWorker,
			legacyTeamWorkerSpec(worker),
			teamRuntimeName,
			leaderRuntimeName,
			rooms,
		))
	}
	return members
}

func (r *TeamReconciler) legacyMemberContext(t *v1beta1.Team, name string, runtimeName string, role MemberRole, spec v1beta1.WorkerSpec, teamRuntimeName string, leaderRuntimeName string, rooms *service.TeamRoomResult) MemberContext {
	if runtimeName == "" {
		runtimeName = name
	}
	effectiveRuntime := backend.ResolveRuntime(spec.Runtime, r.DefaultRuntime)
	backendRuntime := spec.GetBackendRuntime()
	if backendRuntime == "" {
		backendRuntime = r.DefaultBackendRuntime
	}
	appliedSpecHash := hashAppliedWorkerSpecForRuntimeAndResources(
		workerSpecWithEffectiveBackendRuntimeForHash(spec, backendRuntime),
		effectiveRuntime,
		spec.Resources,
	)

	var currentHash, existingMatrixUserID, existingRoomID string
	var observedGeneration int64
	var currentExposedPorts []v1beta1.ExposedPortStatus
	var isUpdate bool
	if ms := t.Status.MemberByName(name); ms != nil {
		currentHash = ms.SpecHash
		existingMatrixUserID = ms.MatrixUserID
		existingRoomID = ms.RoomID
		currentExposedPorts = ms.ExposedPorts
		isUpdate = ms.Observed
		if ms.Observed {
			observedGeneration = t.Generation
		}
	}

	deployMode := v1beta1.DeployModeLocal
	if spec.DeployMode != nil {
		deployMode = *spec.DeployMode
	}
	var serviceEnabled bool
	if spec.ServiceEnabled != nil {
		serviceEnabled = *spec.ServiceEnabled
	}

	teamLeaderName := ""
	if role == RoleTeamWorker {
		teamLeaderName = leaderRuntimeName
	}
	systemLabels := map[string]string{
		v1beta1.LabelController: r.ControllerName,
		v1beta1.LabelRole:       role.String(),
		v1beta1.LabelTeam:       t.Name,
	}

	return MemberContext{
		Name:                 name,
		RuntimeName:          runtimeName,
		Namespace:            t.Namespace,
		Role:                 role,
		Spec:                 spec,
		Generation:           t.Generation,
		ObservedGeneration:   observedGeneration,
		SpecChanged:          currentHash != "" && currentHash != appliedSpecHash,
		AppliedSpecHash:      appliedSpecHash,
		CurrentSpecHash:      currentHash,
		IsUpdate:             isUpdate,
		TeamName:             teamRuntimeName,
		TeamLeaderName:       teamLeaderName,
		TeamRoomID:           rooms.TeamRoomID,
		LeaderDMRoomID:       rooms.LeaderDMRoomID,
		TeamAdminName:        teamAdminName(t),
		TeamAdminMatrixID:    teamAdminMatrixID(t),
		TeamCoordinatorIDs:   teamCoordinatorIDs(t),
		ExistingMatrixUserID: existingMatrixUserID,
		ExistingRoomID:       existingRoomID,
		CurrentExposedPorts:  currentExposedPorts,
		PodLabels: mergeLabels(
			t.ObjectMeta.Labels,
			spec.Labels,
			systemLabels,
		),
		Owner:                t,
		DeployMode:           deployMode,
		ServiceEnabled:       serviceEnabled,
		Resources:            agentResourcesToBackend(spec.Resources),
		BackendRuntime:       backendRuntime,
		StatusBackendRuntime: "",
	}
}

func legacyLeaderWorkerSpec(spec v1beta1.LeaderSpec) v1beta1.WorkerSpec {
	runtime := spec.Runtime
	if runtime == "" {
		runtime = backend.RuntimeCopaw
	}
	return v1beta1.WorkerSpec{
		Model:          spec.Model,
		ModelProvider:  spec.ModelProvider,
		Runtime:        runtime,
		Image:          spec.Image,
		WorkerName:     spec.WorkerName,
		Identity:       spec.Identity,
		Soul:           spec.Soul,
		Agents:         spec.Agents,
		RemoteSkills:   spec.RemoteSkills,
		McpServers:     spec.McpServers,
		Package:        spec.Package,
		ChannelPolicy:  spec.ChannelPolicy,
		State:          spec.State,
		AccessEntries:  spec.AccessEntries,
		DeployMode:     spec.DeployMode,
		ServiceEnabled: spec.ServiceEnabled,
		Env:            spec.Env,
		Labels:         spec.Labels,
		Resources:      spec.Resources,
	}
}

func legacyTeamWorkerSpec(spec v1beta1.TeamWorkerSpec) v1beta1.WorkerSpec {
	return v1beta1.WorkerSpec{
		Model:          spec.Model,
		ModelProvider:  spec.ModelProvider,
		Runtime:        spec.Runtime,
		Image:          spec.Image,
		WorkerName:     spec.WorkerName,
		Identity:       spec.Identity,
		Soul:           spec.Soul,
		Agents:         spec.Agents,
		Skills:         spec.Skills,
		RemoteSkills:   spec.RemoteSkills,
		McpServers:     spec.McpServers,
		Package:        spec.Package,
		Expose:         spec.Expose,
		ChannelPolicy:  spec.ChannelPolicy,
		IdleTimeout:    spec.IdleTimeout,
		State:          spec.State,
		AccessEntries:  spec.AccessEntries,
		DeployMode:     spec.DeployMode,
		ServiceEnabled: spec.ServiceEnabled,
		Env:            spec.Env,
		Labels:         spec.Labels,
		Resources:      spec.Resources,
	}
}

func legacyTeamWorkerNames(workers []v1beta1.TeamWorkerSpec) []string {
	names := make([]string, 0, len(workers))
	for _, worker := range workers {
		names = append(names, worker.Name)
	}
	return names
}

func (r *TeamReconciler) injectLegacyTeamContext(ctx context.Context, t *v1beta1.Team, members []MemberContext, rooms *service.TeamRoomResult, roster []service.RuntimeConfigTeamMember) error {
	logger := log.FromContext(ctx)
	var leader *MemberContext
	workerEntries := make([]service.TeamWorkerEntry, 0, len(members))
	for i := range members {
		if members[i].Role == RoleTeamLeader {
			leader = &members[i]
			continue
		}
		roomID := ""
		if ms := t.Status.MemberByName(members[i].Name); ms != nil {
			roomID = ms.RoomID
		}
		workerEntries = append(workerEntries, service.TeamWorkerEntry{Name: members[i].RuntimeName, RoomID: roomID})
	}
	if leader == nil {
		return fmt.Errorf("legacy team leader member is missing")
	}

	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	leaderRuntime := backend.ResolveRuntime(leader.Spec.Runtime, r.DefaultRuntime)
	if leaderRuntime != backend.RuntimeQwenPaw {
		if err := r.Deployer.SyncTeamLeaderAssets(ctx, service.SyncTeamLeaderAssetsRequest{
			WorkerName: leader.RuntimeName,
			Runtime:    leader.Spec.Runtime,
		}); err != nil {
			logger.Error(err, "team leader asset sync failed (non-fatal)", "worker", leader.RuntimeName)
		}
		if err := r.Deployer.InjectCoordinationContext(ctx, service.CoordinationDeployRequest{
			LeaderName:         leader.RuntimeName,
			Role:               RoleTeamLeader.String(),
			TeamName:           teamRuntimeName,
			TeamRoomID:         rooms.TeamRoomID,
			LeaderDMRoomID:     rooms.LeaderDMRoomID,
			HeartbeatEvery:     t.Spec.HeartbeatEvery,
			WorkerIdleTimeout:  t.Spec.Leader.WorkerIdleTimeout,
			TeamWorkers:        workerEntries,
			TeamAdminID:        teamAdminMatrixID(t),
			TeamCoordinatorIDs: teamCoordinatorIDs(t),
			LeaderSoul:         t.Spec.Leader.Soul,
		}); err != nil {
			logger.Error(err, "leader coordination context injection failed (non-fatal)")
		}
		if leader.Heartbeat != nil && leader.Heartbeat.Enabled {
			if err := r.Deployer.InjectHeartbeatConfig(ctx, service.InjectHeartbeatRequest{
				WorkerName: leader.RuntimeName,
				Enabled:    leader.Heartbeat.Enabled,
				Every:      leader.Heartbeat.Every,
			}); err != nil {
				logger.Error(err, "leader heartbeat config injection failed (non-fatal)")
			}
		}
	}

	for _, member := range members {
		runtime := backend.ResolveRuntime(member.Spec.Runtime, r.DefaultRuntime)
		if runtime == backend.RuntimeQwenPaw {
			continue
		}
		if member.Role == RoleTeamWorker {
			if err := r.Deployer.InjectWorkerCoordination(ctx, service.WorkerCoordinationRequest{
				WorkerName:         member.RuntimeName,
				TeamName:           teamRuntimeName,
				TeamLeaderName:     leader.RuntimeName,
				TeamAdminID:        teamAdminMatrixID(t),
				TeamCoordinatorIDs: teamCoordinatorIDs(t),
			}); err != nil {
				logger.Error(err, "worker coordination context injection failed (non-fatal)", "worker", member.RuntimeName)
			}
		}
		if err := r.deployLegacyRuntimeConfig(ctx, t, member, leader.RuntimeName, rooms, roster); err != nil {
			return err
		}
		policy := r.legacyChannelPolicy(t, members, member, leader.RuntimeName)
		if err := r.Deployer.InjectChannelPolicy(ctx, service.InjectChannelPolicyRequest{
			WorkerName:     member.RuntimeName,
			GroupAllowFrom: policy.GroupAllowFrom,
			DMAllowFrom:    policy.DMAllowFrom,
		}); err != nil {
			logger.Error(err, "channel policy injection failed (non-fatal)", "worker", member.RuntimeName)
		}
	}
	return nil
}

func (r *TeamReconciler) deployLegacyRuntimeConfig(ctx context.Context, t *v1beta1.Team, member MemberContext, leaderRuntimeName string, rooms *service.TeamRoomResult, roster []service.RuntimeConfigTeamMember) error {
	runtime := backend.ResolveRuntime(member.Spec.Runtime, r.DefaultRuntime)
	deployMode := v1beta1.DeployModeLocal
	if member.Spec.DeployMode != nil {
		deployMode = *member.Spec.DeployMode
	}
	if runtime != backend.RuntimeQwenPaw && deployMode != v1beta1.DeployModeEdge {
		return nil
	}
	aiGatewayURL, err := r.runtimeConfigAIGatewayURL(ctx, member.Spec, member.Name)
	if err != nil {
		return err
	}
	req := service.MemberRuntimeConfigDeployRequest{
		Name:              member.Name,
		RuntimeName:       member.RuntimeName,
		Runtime:           runtime,
		Role:              member.Role.String(),
		Generation:        t.Generation,
		Spec:              member.Spec,
		AIGatewayURL:      aiGatewayURL,
		MatrixUserID:      member.ExistingMatrixUserID,
		PersonalRoomID:    member.ExistingRoomID,
		TeamName:          t.Spec.EffectiveTeamName(t.Name),
		TeamRoomID:        rooms.TeamRoomID,
		LeaderName:        t.Spec.Leader.Name,
		LeaderRuntimeName: leaderRuntimeName,
		LeaderDMRoomID:    rooms.LeaderDMRoomID,
		TeamAdminName:     teamAdminName(t),
		TeamAdminMatrixID: teamAdminMatrixID(t),
		TeamMembers:       roster,
	}
	if ms := t.Status.MemberByName(member.Name); ms != nil {
		req.MatrixUserID = ms.MatrixUserID
		req.PersonalRoomID = ms.RoomID
	}
	if deployMode == v1beta1.DeployModeEdge {
		req.Runtime = runtimeRemoteManagedLocal
		if err := r.Deployer.MergeMemberRuntimeTeamContext(ctx, req); err != nil {
			return fmt.Errorf("merge runtime team context for %s: %w", member.RuntimeName, err)
		}
		return nil
	}
	if err := r.Deployer.DeployMemberRuntimeConfig(ctx, req); err != nil {
		return fmt.Errorf("deploy runtime config for %s: %w", member.RuntimeName, err)
	}
	return nil
}

// reconcileMember runs the shared member phases for one team member and
// writes the resulting runtime state into ms. The leader never has
// ExposedPorts (the Leader phase always produces zero ports), so that field
// stays nil for RoleTeamLeader entries.
//
// ms.Observed is flipped to true the instant ReconcileMemberInfra succeeds —
// see the Step 4 comment in reconcileTeamNormal for why post-infra failures
// must not revoke observed status (token-rotation hazard).
func (r *TeamReconciler) reconcileMember(ctx context.Context, deps MemberDeps, m MemberContext, ms *v1beta1.TeamMemberStatus) (reconcile.Result, error) {
	// Validate cross-cluster deployment fields before entering phases.
	if err := ValidateMemberDeployment(m); err != nil {
		return reconcile.Result{}, err
	}

	state := &MemberState{}

	// Pre-populate ExistingMatrixUserID when we've already provisioned the
	// member before, forcing the Refresh path instead of Provision.
	if m.IsUpdate {
		m.ExistingMatrixUserID = r.Provisioner.MatrixUserID(m.RuntimeName)
	}

	res, err := ReconcileMemberInfra(ctx, deps, m, state)
	if err != nil {
		return reconcile.Result{}, err
	}
	if res.RequeueAfter > 0 {
		// ReconcileMemberInfra signals a short backoff (rather than an
		// error) only when the Matrix AppService token is not active yet
		// (transient startup race). Stop before flipping ms.Observed or
		// running later phases — Matrix/Gateway/Room are not provisioned —
		// and propagate the sentinel so the caller requeues quickly
		// instead of treating infra as successful.
		return reconcile.Result{}, matrix.ErrAppServiceNotReady
	}
	if err := EnsureModelProviderAuth(ctx, deps, m, state); err != nil {
		return reconcile.Result{}, err
	}
	ms.Observed = true
	if state.RoomID != "" {
		ms.RoomID = state.RoomID
	}
	if state.MatrixUserID != "" {
		ms.MatrixUserID = state.MatrixUserID
	}
	ms.RuntimeName = m.RuntimeName
	if err := EnsureMemberServiceAccount(ctx, deps, m); err != nil {
		return reconcile.Result{}, err
	}
	if err := ReconcileMemberConfig(ctx, deps, m, state); err != nil {
		return reconcile.Result{}, err
	}
	containerRes, err := ReconcileMemberContainer(ctx, deps, m, state)
	if state.ContainerState != "" {
		ms.ContainerState = state.ContainerState
		ms.Phase = computeMemberPhase(ms.Phase, ms.MatrixUserID, m.Spec.DesiredState(), ms.ContainerState, nil)
	}
	if state.Message != "" {
		ms.Message = state.Message
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if containerRes.RequeueAfter > 0 {
		return containerRes, nil
	}
	if _, err := ReconcileMemberService(ctx, &m, &deps); err != nil {
		return reconcile.Result{}, err
	}
	_ = ReconcileMemberExpose(ctx, deps, m, state)

	if m.Role == RoleTeamWorker {
		ms.ExposedPorts = state.ExposedPorts
	} else {
		ms.ExposedPorts = nil
	}
	ms.SpecHash = m.AppliedSpecHash
	return reconcile.Result{}, nil
}

// summarizeBackendReadiness queries each member's pod/container status from
// the backend and writes ms.Ready per member. Used instead of reading Worker
// CR status because team members no longer have Worker CRs.
//
// On a backend-unreachable path (Backend == nil or DetectWorkerBackend nil)
// this preserves any previously-recorded ms.Ready value — callers should NOT
// treat a false/true gap across reconciles as a transition, since a transient
// backend outage would otherwise flap Phase=Active back to Pending.
func (r *TeamReconciler) summarizeBackendReadiness(ctx context.Context, t *v1beta1.Team, members []MemberContext) (leaderReady bool, readyWorkers int) {
	if r.Backend == nil {
		return false, 0
	}
	for _, m := range members {
		mwb, err := resolveBackendForMember(r.Backend, m.BackendRuntime, m)
		if err != nil {
			logger := log.FromContext(ctx)
			logger.Error(err, "failed to resolve member backend", "member", m.Name, "role", m.Role)
			// Preserve previously-recorded readiness, consistent with the
			// nil-backend early-return contract (see function doc).
			if ms := t.Status.MemberByName(m.Name); ms != nil && ms.Ready {
				if m.Role == RoleTeamLeader {
					leaderReady = true
				} else {
					readyWorkers++
				}
			}
			continue
		}
		result, err := mwb.Status(ctx, m.Name)
		if err != nil {
			logger := log.FromContext(ctx)
			logger.Error(err, "failed to query member backend status", "member", m.Name, "role", m.Role)
			// Preserve previously-recorded readiness in the return values
			// to avoid phase flapping on transient backend errors, consistent
			// with the nil-backend early-return contract (see function doc).
			if ms := t.Status.MemberByName(m.Name); ms != nil && ms.Ready {
				if m.Role == RoleTeamLeader {
					leaderReady = true
				} else {
					readyWorkers++
				}
			}
			continue
		}
		ready := result.Status == backend.StatusRunning || result.Status == backend.StatusReady
		if ms := t.Status.MemberByName(m.Name); ms != nil {
			ms.Ready = ready
			ms.ContainerState = string(result.Status)
			ms.Message = result.Message
			ms.Phase = computeMemberPhase(ms.Phase, ms.MatrixUserID, m.Spec.DesiredState(), ms.ContainerState, nil)
		}
		if m.Role == RoleTeamLeader {
			leaderReady = ready
			continue
		}
		if ready {
			readyWorkers++
		}
	}
	return leaderReady, readyWorkers
}

func (r *TeamReconciler) writeInlineConfigs(t *v1beta1.Team) error {
	return nil
}

func (r *TeamReconciler) handleDelete(ctx context.Context, t *v1beta1.Team) error {
	if len(t.Spec.WorkerMembers) == 0 && (t.Spec.Leader.Name != "" || len(t.Spec.Workers) > 0 || len(t.Status.Members) > 0) {
		return r.handleDeleteLegacy(ctx, t)
	}
	return r.handleDeleteDecoupled(ctx, t)
}

func (r *TeamReconciler) handleDeleteLegacy(ctx context.Context, t *v1beta1.Team) error {
	logger := log.FromContext(ctx)
	deps := r.memberDeps()
	members := r.legacyDeleteMembers(t)
	for _, member := range members {
		if err := ReconcileMemberDelete(ctx, deps, member); err != nil {
			logger.Error(err, "legacy team member delete failed (non-fatal)", "member", member.Name)
		}
		r.removeLegacyMember(ctx, member.RuntimeName)
	}
	if r.Legacy != nil && r.Legacy.Enabled() {
		if err := r.Legacy.RemoveFromTeamsRegistry(ctx, t.Spec.EffectiveTeamName(t.Name)); err != nil {
			logger.Error(err, "teams-registry delete failed (non-fatal)", "team", t.Name)
		}
	}
	return nil
}

func (r *TeamReconciler) cleanupStaleLegacyMembers(ctx context.Context, t *v1beta1.Team, keep map[string]struct{}) {
	logger := log.FromContext(ctx)
	deps := r.memberDeps()
	for _, status := range t.Status.Members {
		if _, ok := keep[status.Name]; ok {
			continue
		}
		member := r.legacyDeleteMemberFromStatus(t, status)
		if err := ReconcileMemberDelete(ctx, deps, member); err != nil {
			logger.Error(err, "stale legacy team member delete failed (non-fatal)", "member", member.Name)
		}
		r.removeLegacyMember(ctx, member.RuntimeName)
	}
}

func (r *TeamReconciler) legacyDeleteMembers(t *v1beta1.Team) []MemberContext {
	if len(t.Status.Members) > 0 {
		members := make([]MemberContext, 0, len(t.Status.Members))
		for _, status := range t.Status.Members {
			members = append(members, r.legacyDeleteMemberFromStatus(t, status))
		}
		return members
	}
	rooms := &service.TeamRoomResult{
		TeamRoomID:     t.Status.TeamRoomID,
		LeaderDMRoomID: t.Status.LeaderDMRoomID,
	}
	return r.legacyTeamMembers(t, rooms, t.Spec.EffectiveTeamName(t.Name), t.Spec.Leader.EffectiveWorkerName())
}

func (r *TeamReconciler) legacyDeleteMemberFromStatus(t *v1beta1.Team, status v1beta1.TeamMemberStatus) MemberContext {
	runtimeName := status.RuntimeName
	if runtimeName == "" {
		runtimeName = status.Name
	}
	role := RoleTeamWorker
	if status.Role == RoleTeamLeader.String() {
		role = RoleTeamLeader
	}
	return MemberContext{
		Name:                 status.Name,
		RuntimeName:          runtimeName,
		Namespace:            t.Namespace,
		Role:                 role,
		ExistingMatrixUserID: status.MatrixUserID,
		ExistingRoomID:       status.RoomID,
		CurrentExposedPorts:  status.ExposedPorts,
		Owner:                t,
		BackendRuntime:       r.DefaultBackendRuntime,
	}
}

func (r *TeamReconciler) memberDeps() MemberDeps {
	return MemberDeps{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}
}

// ---------------------------------------------------------------------------
// Decoupled path: Team references standalone Worker CRs via spec.workerMembers
// ---------------------------------------------------------------------------

type decoupledTeamMember struct {
	ref         v1beta1.TeamWorkerRef
	worker      v1beta1.Worker
	runtimeName string
}

func (r *TeamReconciler) decoupledMemberRuntime(member decoupledTeamMember) string {
	return backend.ResolveRuntime(member.worker.Spec.Runtime, r.DefaultRuntime)
}

// reconcileTeamDecoupled is the new path for Teams whose spec.workerMembers
// is populated. It manages team organization (rooms, coordination context,
// heartbeat injection, status aggregation) without managing member runtime.
func (r *TeamReconciler) reconcileTeamDecoupled(ctx context.Context, t *v1beta1.Team, patchBase client.Patch) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Validate workerMembers
	leaderRef, workerRefs, err := validateWorkerMembers(t.Spec.WorkerMembers)
	if err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}

	// 2. Resolve decoupled membership snapshot from Worker CRs.
	members, degradedMsgs := r.resolveDecoupledMembers(ctx, t)
	if len(degradedMsgs) > 0 {
		t.Status.Phase = "Degraded"
		t.Status.Message = strings.Join(degradedMsgs, "; ")
		if err := r.Status().Patch(ctx, t, patchBase); err != nil {
			logger.Error(err, "failed to patch team status (non-fatal)")
		}
		return reconcile.Result{RequeueAfter: reconcileRetryDelay}, nil
	}

	// 3. Resolve admin actor
	adminActor, err := r.resolveTeamAdminActor(ctx, t)
	if err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	derivedTeam := r.deriveTeamWithResolvedIdentities(ctx, t, adminActor)

	// 4. Team-level infrastructure
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	leaderMember := decoupledLeaderMember(members, leaderRef.Name)
	leaderRuntimeName := leaderMember.runtimeName
	workerRuntimeNames := decoupledWorkerRuntimeNames(members, leaderRef.Name)

	rooms, err := r.Provisioner.ProvisionTeamRooms(ctx, service.TeamRoomRequest{
		TeamName:             teamRuntimeName,
		LeaderName:           leaderRuntimeName,
		LeaderCredentialName: leaderRef.Name,
		WorkerNames:          workerRuntimeNames,
		AdminSpec:            derivedTeam.Spec.Admin,
		HumanMembers:         derivedTeam.Spec.HumanMembers,
		TeamAdminActorToken:  adminActor.Token,
		TeamAdminActorName:   adminActor.Username,
	})
	if err != nil {
		return r.failTeam(ctx, t, patchBase, fmt.Sprintf("provision team rooms: %v", err))
	}
	t.Status.TeamRoomID = rooms.TeamRoomID
	t.Status.LeaderDMRoomID = rooms.LeaderDMRoomID
	r.syncTeamRoomHumanStatuses(ctx, t.Namespace, t.Name, rooms.TeamRoomID, derivedTeam.Spec.HumanMembers)

	if err := r.Deployer.EnsureTeamStorage(ctx, teamRuntimeName); err != nil {
		logger.Error(err, "team shared storage init failed (non-fatal)", "name", t.Name, "teamName", teamRuntimeName)
	}

	// 5. Coordination context + heartbeat injection
	teamWorkerEntries := decoupledTeamWorkerEntries(members, leaderRef.Name)
	leaderRuntime := r.decoupledMemberRuntime(leaderMember)

	if leaderRuntime != backend.RuntimeQwenPaw {
		// Overlay Team Leader built-ins onto the decoupled leader Worker before
		// injecting the team coordination context. The Worker still owns its
		// lifecycle and credentials; this only restores role-specific prompt and
		// skill assets that legacy Teams had generated directly.
		if err := r.Deployer.SyncTeamLeaderAssets(ctx, service.SyncTeamLeaderAssetsRequest{
			WorkerName: leaderRuntimeName,
			Runtime:    leaderMember.worker.Spec.Runtime,
		}); err != nil {
			logger.Error(err, "team leader asset sync failed (non-fatal)", "worker", leaderRuntimeName)
		}

		// Leader coordination context
		if err := r.Deployer.InjectCoordinationContext(ctx, service.CoordinationDeployRequest{
			LeaderName:         leaderRuntimeName,
			Role:               RoleTeamLeader.String(),
			TeamName:           teamRuntimeName,
			TeamRoomID:         rooms.TeamRoomID,
			LeaderDMRoomID:     rooms.LeaderDMRoomID,
			HeartbeatEvery:     t.Spec.HeartbeatEvery,
			WorkerIdleTimeout:  "", // decoupled path does not inject
			TeamWorkers:        teamWorkerEntries,
			TeamAdminID:        teamAdminMatrixID(derivedTeam),
			TeamCoordinatorIDs: teamCoordinatorIDs(derivedTeam),
			LeaderSoul:         leaderMember.worker.Spec.Soul,
		}); err != nil {
			logger.Error(err, "leader coordination context injection failed (non-fatal)")
		}

		// Leader heartbeat injection
		if t.Spec.HeartbeatEvery != "" {
			if err := r.Deployer.InjectHeartbeatConfig(ctx, service.InjectHeartbeatRequest{
				WorkerName: leaderRuntimeName,
				Enabled:    true,
				Every:      t.Spec.HeartbeatEvery,
			}); err != nil {
				logger.Error(err, "leader heartbeat config injection failed (non-fatal)")
			}
		}
	}

	// Worker coordination context
	for _, rm := range members {
		if rm.ref.Name == leaderRef.Name {
			continue
		}
		if r.decoupledMemberRuntime(rm) == backend.RuntimeQwenPaw {
			continue
		}
		if err := r.Deployer.InjectWorkerCoordination(ctx, service.WorkerCoordinationRequest{
			WorkerName:         rm.runtimeName,
			TeamName:           teamRuntimeName,
			TeamLeaderName:     leaderRuntimeName,
			TeamAdminID:        teamAdminMatrixID(derivedTeam),
			TeamCoordinatorIDs: teamCoordinatorIDs(derivedTeam),
		}); err != nil {
			logger.Error(err, "worker coordination context injection failed (non-fatal)", "worker", rm.runtimeName)
		}
	}
	if err := r.deployDecoupledRuntimeConfigs(ctx, derivedTeam, members, leaderRef.Name, teamRuntimeName, leaderRuntimeName, rooms); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}

	// 6. Legacy registry updates
	if r.Legacy != nil && r.Legacy.Enabled() {
		leaderMatrixID := r.Legacy.MatrixUserID(leaderRuntimeName)
		if err := r.Legacy.UpdateManagerGroupAllowFrom(leaderMatrixID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom for team leader (non-fatal)")
		}
		workerNames := make([]string, 0, len(workerRefs))
		for _, ref := range workerRefs {
			workerNames = append(workerNames, ref.Name)
		}
		if err := r.Legacy.UpdateTeamsRegistry(service.TeamRegistryEntry{
			Name:           teamRuntimeName,
			Leader:         leaderRef.Name,
			Workers:        workerNames,
			TeamRoomID:     rooms.TeamRoomID,
			LeaderDMRoomID: rooms.LeaderDMRoomID,
			Admin:          teamAdminRegistryEntry(derivedTeam.Spec.Admin),
			Members:        teamMemberRegistryEntries(derivedTeam.Spec.HumanMembers),
		}); err != nil {
			logger.Error(err, "teams-registry update failed (non-fatal)")
		}

		for _, rm := range members {
			role := RoleTeamWorker
			if rm.ref.Name == leaderRef.Name {
				role = RoleTeamLeader
			} else if err := r.Legacy.UpdateManagerGroupAllowFrom(r.Legacy.MatrixUserID(rm.runtimeName), false); err != nil {
				logger.Error(err, "failed to revoke Manager groupAllowFrom for team worker (non-fatal)", "worker", rm.runtimeName)
			}
			ms := decoupledMemberStatusSnapshot(rm, role)
			r.reconcileLegacyMember(ctx, derivedTeam, decoupledMemberContext(derivedTeam, rm, role, teamRuntimeName, leaderRuntimeName, r.DefaultBackendRuntime), &ms)

			if r.decoupledMemberRuntime(rm) != backend.RuntimeQwenPaw {
				policy := r.decoupledChannelPolicy(derivedTeam, members, leaderRef.Name, rm, role)
				if err := r.Deployer.InjectChannelPolicy(ctx, service.InjectChannelPolicyRequest{
					WorkerName:     rm.runtimeName,
					GroupAllowFrom: policy.GroupAllowFrom,
					DMAllowFrom:    policy.DMAllowFrom,
				}); err != nil {
					logger.Error(err, "channel policy injection failed (non-fatal)", "worker", rm.runtimeName)
				}
			}
		}
	}

	// 7. Status aggregation
	r.cleanupStaleDecoupledMembers(ctx, derivedTeam, members)
	leaderReady, readyWorkers := aggregateDecoupledTeamStatus(t, members, leaderRef.Name, len(workerRefs))

	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		logger.Error(err, "failed to patch team status (non-fatal)")
	}

	logger.Info("team reconciled (decoupled)",
		"name", t.Name,
		"phase", t.Status.Phase,
		"leaderReady", leaderReady,
		"readyWorkers", readyWorkers,
		"totalWorkers", t.Status.TotalWorkers)
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *TeamReconciler) resolveDecoupledMembers(ctx context.Context, t *v1beta1.Team) ([]decoupledTeamMember, []string) {
	members := make([]decoupledTeamMember, 0, len(t.Spec.WorkerMembers))
	var degradedMsgs []string

	for _, ref := range t.Spec.WorkerMembers {
		var w v1beta1.Worker
		key := client.ObjectKey{Name: ref.Name, Namespace: t.Namespace}
		if err := r.Get(ctx, key, &w); err != nil {
			degradedMsgs = append(degradedMsgs, fmt.Sprintf("Worker %q not found", ref.Name))
			continue
		}
		members = append(members, decoupledTeamMember{
			ref:         ref,
			worker:      w,
			runtimeName: w.Spec.EffectiveWorkerName(w.Name),
		})
	}
	return members, degradedMsgs
}

func decoupledLeaderMember(members []decoupledTeamMember, leaderName string) decoupledTeamMember {
	for _, member := range members {
		if member.ref.Name == leaderName {
			return member
		}
	}
	return decoupledTeamMember{}
}

func decoupledWorkerRuntimeNames(members []decoupledTeamMember, leaderName string) []string {
	names := make([]string, 0, len(members))
	for _, member := range members {
		if member.ref.Name == leaderName {
			continue
		}
		names = append(names, member.runtimeName)
	}
	return names
}

func decoupledTeamWorkerEntries(members []decoupledTeamMember, leaderName string) []service.TeamWorkerEntry {
	entries := make([]service.TeamWorkerEntry, 0, len(members))
	for _, member := range members {
		if member.ref.Name == leaderName {
			continue
		}
		entries = append(entries, service.TeamWorkerEntry{
			Name:   member.runtimeName,
			RoomID: member.worker.Status.RoomID,
		})
	}
	return entries
}

func (r *TeamReconciler) deployDecoupledRuntimeConfigs(
	ctx context.Context,
	t *v1beta1.Team,
	members []decoupledTeamMember,
	leaderName string,
	teamRuntimeName string,
	leaderRuntimeName string,
	rooms *service.TeamRoomResult,
) error {
	roster := decoupledRuntimeConfigTeamMembers(t, members, leaderName)
	for _, member := range members {
		// Skip members being deleted — their runtime config no longer needs
		// updating and the model provider may already have been removed.
		if !member.worker.DeletionTimestamp.IsZero() {
			continue
		}
		runtime := backend.ResolveRuntime(member.worker.Spec.Runtime, r.DefaultRuntime)
		deployMode := v1beta1.DeployModeLocal
		if member.worker.Spec.DeployMode != nil {
			deployMode = *member.worker.Spec.DeployMode
		}
		if runtime != backend.RuntimeQwenPaw && deployMode != v1beta1.DeployModeEdge {
			continue
		}
		role := RoleTeamWorker
		if member.ref.Name == leaderName {
			role = RoleTeamLeader
		}
		leaderNameFact := leaderName
		if leaderNameFact == "" {
			leaderNameFact = leaderRuntimeName
		}
		aiGatewayURL, err := r.runtimeConfigAIGatewayURL(ctx, member.worker.Spec, member.ref.Name)
		if err != nil {
			return err
		}
		req := service.MemberRuntimeConfigDeployRequest{
			Name:              member.ref.Name,
			RuntimeName:       member.runtimeName,
			Runtime:           runtime,
			Role:              role.String(),
			Generation:        member.worker.Generation,
			Spec:              member.worker.Spec,
			AIGatewayURL:      aiGatewayURL,
			MatrixUserID:      member.worker.Status.MatrixUserID,
			PersonalRoomID:    member.worker.Status.RoomID,
			TeamName:          teamRuntimeName,
			TeamRoomID:        rooms.TeamRoomID,
			LeaderName:        leaderNameFact,
			LeaderRuntimeName: leaderRuntimeName,
			LeaderDMRoomID:    rooms.LeaderDMRoomID,
			TeamAdminName:     teamAdminName(t),
			TeamAdminMatrixID: teamAdminMatrixID(t),
			TeamMembers:       roster,
		}
		if deployMode == v1beta1.DeployModeEdge {
			req.Runtime = runtimeRemoteManagedLocal
			if err := r.Deployer.MergeMemberRuntimeTeamContext(ctx, req); err != nil {
				return fmt.Errorf("merge runtime team context for %s: %w", member.runtimeName, err)
			}
			continue
		}
		if err := r.Deployer.DeployMemberRuntimeConfig(ctx, req); err != nil {
			return fmt.Errorf("deploy runtime config for %s: %w", member.runtimeName, err)
		}
	}
	return nil
}

func (r *TeamReconciler) runtimeConfigAIGatewayURL(ctx context.Context, spec v1beta1.WorkerSpec, memberName string) (string, error) {
	if spec.ModelProvider == "" || r.GatewayClient == nil {
		return "", nil
	}
	info, err := r.GatewayClient.ResolveModelProvider(ctx, spec.ModelProvider)
	if err != nil {
		return "", fmt.Errorf("resolve model provider %q for %s: %w", spec.ModelProvider, memberName, err)
	}
	if info == nil {
		return "", nil
	}
	return info.IntranetURL, nil
}

func decoupledRuntimeConfigTeamMembers(t *v1beta1.Team, members []decoupledTeamMember, leaderName string) []service.RuntimeConfigTeamMember {
	roster := make([]service.RuntimeConfigTeamMember, 0, len(members)+len(t.Spec.HumanMembers))
	for _, member := range members {
		role := RoleTeamWorker
		if member.ref.Name == leaderName {
			role = RoleTeamLeader
		}
		roster = append(roster, service.RuntimeConfigTeamMember{
			Name:           member.ref.Name,
			RuntimeName:    member.runtimeName,
			Role:           role.String(),
			MatrixUserID:   member.worker.Status.MatrixUserID,
			PersonalRoomID: member.worker.Status.RoomID,
		})
	}
	for _, human := range t.Spec.HumanMembers {
		role := human.Role
		if role == "" {
			role = "coordinator"
		}
		roster = append(roster, service.RuntimeConfigTeamMember{
			Name:         human.Name,
			Role:         role,
			MatrixUserID: human.MatrixUserID,
		})
	}
	return roster
}

type decoupledChannelAllowLists struct {
	GroupAllowFrom []string
	DMAllowFrom    []string
}

func decoupledMemberStatusSnapshot(member decoupledTeamMember, role MemberRole) v1beta1.TeamMemberStatus {
	ms := v1beta1.TeamMemberStatus{Name: member.ref.Name, Role: role.String()}
	syncDecoupledMemberStatus(&ms, member)
	return ms
}

func syncDecoupledMemberStatus(ms *v1beta1.TeamMemberStatus, member decoupledTeamMember) {
	ms.RuntimeName = member.runtimeName
	ms.MatrixUserID = member.worker.Status.MatrixUserID
	ms.RoomID = member.worker.Status.RoomID
	ms.SpecHash = member.worker.Status.SpecHash
	ms.Observed = true
	ms.Ready = member.worker.Status.Phase == "Running"
	ms.Phase = member.worker.Status.Phase
	ms.ContainerState = member.worker.Status.ContainerState
	ms.Message = member.worker.Status.Message
	ms.LastActiveAt = member.worker.Status.LastActiveAt
	ms.LastHeartbeat = member.worker.Status.LastHeartbeat
	ms.ExposedPorts = member.worker.Status.ExposedPorts
}

func (r *TeamReconciler) cleanupStaleDecoupledMembers(ctx context.Context, t *v1beta1.Team, members []decoupledTeamMember) {
	desired := make(map[string]struct{}, len(members))
	for _, member := range members {
		desired[member.ref.Name] = struct{}{}
	}
	for _, ms := range t.Status.Members {
		if _, ok := desired[ms.Name]; ok {
			continue
		}
		var w v1beta1.Worker
		key := client.ObjectKey{Name: ms.Name, Namespace: t.Namespace}
		if err := r.Get(ctx, key, &w); err == nil {
			r.detachDecoupledMember(ctx, t, &w)
			continue
		}
		runtimeName := ms.RuntimeName
		if runtimeName == "" {
			runtimeName = ms.Name
		}
		r.removeLegacyMember(ctx, runtimeName)
	}
}

func (r *TeamReconciler) detachDecoupledMember(ctx context.Context, t *v1beta1.Team, w *v1beta1.Worker) {
	logger := log.FromContext(ctx)
	runtimeName := w.Spec.EffectiveWorkerName(w.Name)
	runtime := backend.ResolveRuntime(w.Spec.Runtime, r.DefaultRuntime)
	if runtime != backend.RuntimeQwenPaw {
		if err := r.Deployer.InjectWorkerCoordination(ctx, service.WorkerCoordinationRequest{
			WorkerName:         runtimeName,
			TeamName:           "",
			TeamLeaderName:     "",
			TeamAdminID:        "",
			TeamCoordinatorIDs: nil,
		}); err != nil {
			logger.Error(err, "failed to revert worker coordination to standalone (non-fatal)", "worker", runtimeName)
		}
	}

	aiGatewayURL, resolveErr := r.runtimeConfigAIGatewayURL(ctx, w.Spec, w.Name)
	if resolveErr != nil {
		logger.Error(resolveErr, "failed to resolve worker model provider for runtime config reset", "worker", runtimeName)
	}
	if err := r.Deployer.DeployMemberRuntimeConfig(ctx, service.MemberRuntimeConfigDeployRequest{
		Name:            w.Name,
		RuntimeName:     runtimeName,
		Runtime:         w.Spec.Runtime,
		Role:            RoleStandalone.String(),
		Generation:      w.Generation,
		Spec:            w.Spec,
		AIGatewayURL:    aiGatewayURL,
		MatrixUserID:    w.Status.MatrixUserID,
		PersonalRoomID:  w.Status.RoomID,
		DropTeamContext: true,
	}); err != nil {
		logger.Error(err, "failed to drop worker runtime team context (non-fatal)", "worker", runtimeName)
	}

	r.removeLegacyMember(ctx, runtimeName)
	if r.Legacy == nil || !r.Legacy.Enabled() || runtime == backend.RuntimeQwenPaw {
		return
	}
	if err := r.Legacy.UpdateManagerGroupAllowFrom(r.Legacy.MatrixUserID(runtimeName), false); err != nil {
		logger.Error(err, "failed to revoke Manager groupAllowFrom for detached member (non-fatal)", "worker", runtimeName)
	}
	managerMatrixID := r.Legacy.MatrixUserID("manager")
	var systemAdminID string
	if r.SystemAdminUser != "" {
		systemAdminID = r.Legacy.MatrixUserID(r.SystemAdminUser)
	}
	standaloneAllowFrom := uniqueTeamStrings([]string{managerMatrixID, systemAdminID, teamAdminMatrixID(t)})
	if err := r.Deployer.InjectChannelPolicy(ctx, service.InjectChannelPolicyRequest{
		WorkerName:     runtimeName,
		GroupAllowFrom: standaloneAllowFrom,
		DMAllowFrom:    standaloneAllowFrom,
	}); err != nil {
		logger.Error(err, "failed to reset worker channel policy (non-fatal)", "worker", runtimeName)
	}
}

func decoupledMemberContext(t *v1beta1.Team, member decoupledTeamMember, role MemberRole, teamRuntimeName, leaderRuntimeName string, defaultBackendRuntime string) MemberContext {
	teamLeaderName := ""
	if role == RoleTeamWorker {
		teamLeaderName = leaderRuntimeName
	}
	backendRuntime := member.worker.Spec.GetBackendRuntime()
	if backendRuntime == "" {
		backendRuntime = defaultBackendRuntime
	}
	return MemberContext{
		Name:                 member.ref.Name,
		RuntimeName:          member.runtimeName,
		Namespace:            member.worker.Namespace,
		Role:                 role,
		Spec:                 member.worker.Spec,
		TeamName:             teamRuntimeName,
		TeamLeaderName:       teamLeaderName,
		TeamAdminMatrixID:    teamAdminMatrixID(t),
		TeamCoordinatorIDs:   teamCoordinatorIDs(t),
		BackendRuntime:       backendRuntime,
		StatusBackendRuntime: member.worker.Status.BackendRuntime,
		CurrentSpecHash:      member.worker.Status.SpecHash,
	}
}

func (r *TeamReconciler) decoupledChannelPolicy(t *v1beta1.Team, members []decoupledTeamMember, leaderName string, current decoupledTeamMember, role MemberRole) decoupledChannelAllowLists {
	resolve := func(value string) string {
		if value == "" || strings.HasPrefix(value, "@") {
			return value
		}
		if r.Legacy != nil && r.Legacy.Enabled() {
			return r.Legacy.MatrixUserID(value)
		}
		if r.Provisioner != nil {
			return r.Provisioner.MatrixUserID(value)
		}
		return value
	}

	leaderRuntimeName := decoupledLeaderMember(members, leaderName).runtimeName
	managerMatrixID := resolve("manager")
	coordinatorIDs := teamCoordinatorIDs(t)

	// Always include the system admin so the operator retains visibility.
	var systemAdminID string
	if r.SystemAdminUser != "" {
		systemAdminID = resolve(r.SystemAdminUser)
	}

	groupAllow := make([]string, 0)
	dmAllow := make([]string, 0)

	switch role {
	case RoleTeamLeader:
		groupAllow = append(groupAllow, managerMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		for _, member := range members {
			if member.ref.Name == leaderName {
				continue
			}
			groupAllow = append(groupAllow, resolve(member.runtimeName))
		}
		dmAllow = append(dmAllow, managerMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	default:
		leaderMatrixID := resolve(leaderRuntimeName)
		groupAllow = append(groupAllow, leaderMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		if t.Spec.PeerMentions == nil || *t.Spec.PeerMentions {
			for _, member := range members {
				if member.ref.Name == leaderName || member.ref.Name == current.ref.Name {
					continue
				}
				groupAllow = append(groupAllow, resolve(member.runtimeName))
			}
		}
		dmAllow = append(dmAllow, leaderMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	}

	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, decoupledIndividualChannelPolicy(t, current, role))
	if policy != nil {
		groupAllow = applyChannelAllowPolicy(groupAllow, policy.GroupAllowExtra, policy.GroupDenyExtra, resolve)
		dmAllow = applyChannelAllowPolicy(dmAllow, policy.DmAllowExtra, policy.DmDenyExtra, resolve)
	}
	return decoupledChannelAllowLists{
		GroupAllowFrom: uniqueTeamStrings(groupAllow),
		DMAllowFrom:    uniqueTeamStrings(dmAllow),
	}
}

func (r *TeamReconciler) legacyChannelPolicy(t *v1beta1.Team, members []MemberContext, current MemberContext, leaderRuntimeName string) decoupledChannelAllowLists {
	resolve := func(value string) string {
		if value == "" || strings.HasPrefix(value, "@") {
			return value
		}
		if r.Legacy != nil && r.Legacy.Enabled() {
			return r.Legacy.MatrixUserID(value)
		}
		if r.Provisioner != nil {
			return r.Provisioner.MatrixUserID(value)
		}
		return value
	}

	if leaderRuntimeName == "" {
		for _, member := range members {
			if member.Role == RoleTeamLeader {
				leaderRuntimeName = member.RuntimeName
				break
			}
		}
	}
	managerMatrixID := resolve("manager")
	coordinatorIDs := teamCoordinatorIDs(t)

	var systemAdminID string
	if r.SystemAdminUser != "" {
		systemAdminID = resolve(r.SystemAdminUser)
	}

	groupAllow := make([]string, 0)
	dmAllow := make([]string, 0)

	if current.Role == RoleTeamLeader {
		groupAllow = append(groupAllow, managerMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		for _, member := range members {
			if member.Role == RoleTeamLeader {
				continue
			}
			groupAllow = append(groupAllow, resolve(member.RuntimeName))
		}
		dmAllow = append(dmAllow, managerMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	} else {
		leaderMatrixID := resolve(leaderRuntimeName)
		groupAllow = append(groupAllow, leaderMatrixID, systemAdminID)
		groupAllow = appendResolved(groupAllow, resolve, coordinatorIDs...)
		if t.Spec.PeerMentions == nil || *t.Spec.PeerMentions {
			for _, member := range members {
				if member.Role == RoleTeamLeader || member.Name == current.Name {
					continue
				}
				groupAllow = append(groupAllow, resolve(member.RuntimeName))
			}
		}
		dmAllow = append(dmAllow, leaderMatrixID, systemAdminID)
		dmAllow = appendResolved(dmAllow, resolve, coordinatorIDs...)
	}

	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, current.Spec.ChannelPolicy)
	if policy != nil {
		groupAllow = applyChannelAllowPolicy(groupAllow, policy.GroupAllowExtra, policy.GroupDenyExtra, resolve)
		dmAllow = applyChannelAllowPolicy(dmAllow, policy.DmAllowExtra, policy.DmDenyExtra, resolve)
	}
	return decoupledChannelAllowLists{
		GroupAllowFrom: uniqueTeamStrings(groupAllow),
		DMAllowFrom:    uniqueTeamStrings(dmAllow),
	}
}

func decoupledIndividualChannelPolicy(t *v1beta1.Team, member decoupledTeamMember, role MemberRole) *v1beta1.ChannelPolicySpec {
	return member.worker.Spec.ChannelPolicy
}

func appendResolved(values []string, resolve func(string) string, items ...string) []string {
	for _, item := range items {
		values = append(values, resolve(item))
	}
	return values
}

func applyChannelAllowPolicy(base, allowExtra, denyExtra []string, resolve func(string) string) []string {
	out := append([]string{}, base...)
	out = appendResolved(out, resolve, allowExtra...)
	deny := make(map[string]struct{}, len(denyExtra)*2)
	for _, item := range denyExtra {
		if item == "" {
			continue
		}
		deny[item] = struct{}{}
		deny[resolve(item)] = struct{}{}
	}
	filtered := make([]string, 0, len(out))
	for _, item := range out {
		if item == "" {
			continue
		}
		if _, ok := deny[item]; ok {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func aggregateDecoupledTeamStatus(t *v1beta1.Team, members []decoupledTeamMember, leaderName string, totalWorkers int) (bool, int) {
	desiredNames := make(map[string]struct{}, len(t.Spec.WorkerMembers))
	for _, ref := range t.Spec.WorkerMembers {
		desiredNames[ref.Name] = struct{}{}
	}
	pruneMembers(&t.Status, desiredNames)

	var leaderReady bool
	readyWorkers := 0
	for _, member := range members {
		role := "worker"
		if member.ref.Name == leaderName {
			role = RoleTeamLeader.String()
		}
		ms := memberStatus(&t.Status, member.ref.Name, MemberRole(role))
		syncDecoupledMemberStatus(ms, member)
		if member.ref.Name == leaderName {
			leaderReady = ms.Ready
		} else if ms.Ready {
			readyWorkers++
		}
	}

	sortMembers(&t.Status)
	t.Status.TotalWorkers = totalWorkers
	t.Status.LeaderReady = leaderReady
	t.Status.ReadyWorkers = readyWorkers

	t.Status.Phase = "Active"
	t.Status.Message = ""
	return leaderReady, readyWorkers
}

// handleDeleteDecoupled handles Team deletion for the decoupled path.
// It revokes team coordination from members (writing standalone context back),
// removes registry entries, and deletes room aliases. It does NOT destroy
// member Workers — they have independent lifecycles.
func (r *TeamReconciler) handleDeleteDecoupled(ctx context.Context, t *v1beta1.Team) error {
	logger := log.FromContext(ctx)
	logger.Info("deleting team (decoupled)", "name", t.Name)
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	r.syncTeamRoomHumanStatuses(ctx, t.Namespace, "", t.Status.TeamRoomID, nil)

	// Revert each member's coordination context to standalone.
	for _, ref := range t.Spec.WorkerMembers {
		var w v1beta1.Worker
		key := client.ObjectKey{Name: ref.Name, Namespace: t.Namespace}
		if err := r.Get(ctx, key, &w); err != nil {
			// Worker already deleted or not found — nothing to revert.
			continue
		}
		r.detachDecoupledMember(ctx, t, &w)
	}

	// Remove heartbeat config from the leader.
	leaderRef, _, _ := validateWorkerMembers(t.Spec.WorkerMembers)
	if leaderRef != nil {
		var leaderW v1beta1.Worker
		key := client.ObjectKey{Name: leaderRef.Name, Namespace: t.Namespace}
		if err := r.Get(ctx, key, &leaderW); err == nil {
			leaderRN := leaderW.Spec.EffectiveWorkerName(leaderW.Name)
			runtime := backend.ResolveRuntime(leaderW.Spec.Runtime, r.DefaultRuntime)
			if runtime != backend.RuntimeQwenPaw {
				if err := r.Deployer.InjectHeartbeatConfig(ctx, service.InjectHeartbeatRequest{
					WorkerName: leaderRN,
					Enabled:    false,
					Every:      "",
				}); err != nil {
					logger.Error(err, "failed to remove leader heartbeat config (non-fatal)")
				}
			}
		}
	}

	// Legacy registry cleanup.
	if r.Legacy != nil && r.Legacy.Enabled() {
		if leaderRef != nil {
			var leaderW v1beta1.Worker
			key := client.ObjectKey{Name: leaderRef.Name, Namespace: t.Namespace}
			if err := r.Get(ctx, key, &leaderW); err == nil {
				leaderRN := leaderW.Spec.EffectiveWorkerName(leaderW.Name)
				leaderMatrixID := r.Legacy.MatrixUserID(leaderRN)
				if err := r.Legacy.UpdateManagerGroupAllowFrom(leaderMatrixID, false); err != nil {
					logger.Error(err, "failed to revoke Manager groupAllowFrom (non-fatal)")
				}
			}
		}
		if err := r.Legacy.RemoveFromTeamsRegistry(ctx, teamRuntimeName); err != nil {
			logger.Error(err, "failed to remove team from registry (non-fatal)")
		}

	}

	// Delete team room aliases so a fresh Team CR with the same name gets
	// clean aliases.
	leaderRuntimeName := r.decoupledLeaderRuntimeName(ctx, t, leaderRef)
	r.archiveTeamRooms(ctx, t, teamRuntimeName, leaderRuntimeName)
	if err := r.Provisioner.DeleteTeamRoomAliases(ctx, teamRuntimeName, leaderRuntimeName); err != nil {
		logger.Error(err, "failed to delete team room aliases (non-fatal)")
	}

	return nil
}

func (r *TeamReconciler) decoupledLeaderRuntimeName(ctx context.Context, t *v1beta1.Team, leaderRef *v1beta1.TeamWorkerRef) string {
	if leaderRef == nil {
		return ""
	}
	for i := range t.Status.Members {
		ms := t.Status.Members[i]
		if ms.Name == leaderRef.Name && ms.RuntimeName != "" {
			return ms.RuntimeName
		}
	}
	var leaderW v1beta1.Worker
	key := client.ObjectKey{Name: leaderRef.Name, Namespace: t.Namespace}
	if err := r.Get(ctx, key, &leaderW); err == nil {
		return leaderW.Spec.EffectiveWorkerName(leaderW.Name)
	}
	if leaderRef.Name != "" {
		return leaderRef.Name
	}
	for i := range t.Status.Members {
		ms := t.Status.Members[i]
		if ms.Role == RoleTeamLeader.String() && ms.RuntimeName != "" {
			return ms.RuntimeName
		}
	}
	return leaderRef.Name
}

func (r *TeamReconciler) archiveTeamRooms(ctx context.Context, t *v1beta1.Team, teamRuntimeName, leaderRuntimeName string) {
	logger := log.FromContext(ctx)
	actorToken := ""
	if t.Spec.Admin != nil {
		actor, err := r.resolveTeamAdminActor(ctx, t)
		if err != nil {
			logger.Error(err, "failed to resolve team admin actor for room archive (non-fatal)", "team", t.Name)
		} else {
			actorToken = actor.Token
		}
	}
	if err := r.Provisioner.ArchiveTeamRooms(ctx, service.TeamRoomArchiveRequest{
		TeamName:       teamRuntimeName,
		LeaderName:     leaderRuntimeName,
		TeamRoomID:     t.Status.TeamRoomID,
		LeaderDMRoomID: t.Status.LeaderDMRoomID,
		ActorToken:     actorToken,
	}); err != nil {
		logger.Error(err, "failed to archive team room names (non-fatal)")
	}
}

// validateWorkerMembers validates the workerMembers list: exactly one
// role=team_leader, no duplicates, non-empty names. Returns the leader ref,
// the worker refs (excluding leader), and any validation error.
func validateWorkerMembers(refs []v1beta1.TeamWorkerRef) (leader *v1beta1.TeamWorkerRef, workers []v1beta1.TeamWorkerRef, err error) {
	if len(refs) == 0 {
		return nil, nil, fmt.Errorf("workerMembers must not be empty")
	}
	seen := make(map[string]struct{}, len(refs))
	var leaders []string
	workers = make([]v1beta1.TeamWorkerRef, 0, len(refs)-1)

	for i := range refs {
		ref := &refs[i]
		if ref.Name == "" {
			return nil, nil, fmt.Errorf("workerMembers[%d].name must not be empty", i)
		}
		if _, dup := seen[ref.Name]; dup {
			return nil, nil, fmt.Errorf("duplicate workerMembers name %q", ref.Name)
		}
		seen[ref.Name] = struct{}{}

		role := ref.Role
		if role == "" {
			role = "worker"
		}
		if role == RoleTeamLeader.String() {
			leaders = append(leaders, ref.Name)
			leader = ref
		} else {
			workers = append(workers, *ref)
		}
	}

	if len(leaders) == 0 {
		return nil, nil, fmt.Errorf("workerMembers must contain exactly one member with role=%q", RoleTeamLeader.String())
	}
	if len(leaders) > 1 {
		return nil, nil, fmt.Errorf("workerMembers contains multiple leaders: %v", leaders)
	}
	return leader, workers, nil
}

// reconcileLegacyMember upserts a team member (leader or worker) into the
// legacy workers-registry.json. This is the TeamReconciler counterpart to
// WorkerReconciler.reconcileLegacy — both must emit entries with identical
// field semantics (role, team_id, runtime, skills, image) so that
// manager-side tooling (find-worker.sh, push-worker-skills.sh,
// update-worker-config.sh, etc.) can treat standalone workers and team
// members uniformly.
//
// m.Role drives the role string: RoleTeamLeader -> "team_leader",
// RoleTeamWorker -> "worker". ms is the Team.Status member entry populated by
// reconcileMember; RoomID/MatrixUserID on it are the source of truth for the
// registry row, but MatrixUserID is re-derived via r.Legacy.MatrixUserID to
// stay deterministic (mirrors WorkerReconciler which uses
// r.Provisioner.MatrixUserID(w.Name)).
//
// Non-fatal: any OSS error is logged but does not fail the reconcile pass,
// matching the legacy contract in WorkerReconciler.
func (r *TeamReconciler) reconcileLegacyMember(ctx context.Context, t *v1beta1.Team, m MemberContext, ms *v1beta1.TeamMemberStatus) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)
	runtimeName := m.RuntimeName
	if runtimeName == "" {
		runtimeName = m.Name
	}

	roomID := ""
	if ms != nil {
		roomID = ms.RoomID
	}

	entry := service.WorkerRegistryEntry{
		Name:         runtimeName,
		MatrixUserID: r.Legacy.MatrixUserID(runtimeName),
		RoomID:       roomID,
		Runtime:      m.Spec.Runtime,
		Deployment:   "local",
		Skills:       m.Spec.Skills,
		Role:         m.Role.String(),
		TeamID:       nilIfEmpty(t.Spec.EffectiveTeamName(t.Name)),
		Image:        nilIfEmpty(m.Spec.Image),
	}
	if err := r.Legacy.UpdateWorkersRegistry(entry); err != nil {
		logger.Error(err, "workers-registry update failed (non-fatal)", "name", m.Name, "runtimeName", runtimeName)
	}
}

// removeLegacyMember deletes a team member from workers-registry.json. Used
// by both the stale-member cleanup in reconcileTeamNormal and the full team
// deletion in handleDelete. No-op when Legacy is disabled.
func (r *TeamReconciler) removeLegacyMember(ctx context.Context, runtimeName string) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	if runtimeName == "" {
		return
	}
	if err := r.Legacy.RemoveFromWorkersRegistry(runtimeName); err != nil {
		log.FromContext(ctx).Error(err, "workers-registry remove failed (non-fatal)", "runtimeName", runtimeName)
	}
}

func (r *TeamReconciler) failTeam(ctx context.Context, t *v1beta1.Team, patchBase client.Patch, msg string) (reconcile.Result, error) {
	t.Status.Phase = "Failed"
	t.Status.Message = msg
	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		log.FromContext(ctx).Error(err, "failed to patch team status after failure (non-fatal)")
	}
	return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("%s", msg)
}

// --- helpers ---

// memberStatus returns a pointer to the entry for name in s.Members,
// creating a zero-value entry (tagged with the given role) when absent. The
// returned pointer remains valid for in-place mutation across the reconcile
// pass because the caller treats Members as append-only for the duration of
// reconcileTeamNormal (pruneMembers runs once up front, before the per-
// member loop); no subsequent call re-slices the underlying array, so a
// pointer obtained here will not be invalidated by later memberStatus
// appends.
func memberStatus(s *v1beta1.TeamStatus, name string, role MemberRole) *v1beta1.TeamMemberStatus {
	if existing := s.MemberByName(name); existing != nil {
		if existing.Role == "" {
			existing.Role = role.String()
		}
		return existing
	}
	s.Members = append(s.Members, v1beta1.TeamMemberStatus{Name: name, Role: role.String()})
	return &s.Members[len(s.Members)-1]
}

// pruneMembers removes entries from s.Members whose names are not present in
// keep. Called exactly once per reconcile (Step 3) so the memberStatus
// pointer-stability invariant above holds.
func pruneMembers(s *v1beta1.TeamStatus, keep map[string]struct{}) {
	if len(s.Members) == 0 {
		return
	}
	filtered := s.Members[:0]
	for _, ms := range s.Members {
		if _, ok := keep[ms.Name]; ok {
			filtered = append(filtered, ms)
		}
	}
	// Zero out the trailing tail to release references to dropped entries
	// (important when ExposedPorts holds domain strings).
	for i := len(filtered); i < len(s.Members); i++ {
		s.Members[i] = v1beta1.TeamMemberStatus{}
	}
	s.Members = filtered
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func removeString(values []string, target string) []string {
	filtered := values[:0]
	for _, value := range values {
		if value != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// sortMembers orders Members by Name for stable status patches and
// deterministic test assertions. Kubernetes merge-patch compares the full
// array by index, so an unstable order would cause spurious patch churn and
// unnecessary informer events.
func sortMembers(s *v1beta1.TeamStatus) {
	sort.Slice(s.Members, func(i, j int) bool {
		return s.Members[i].Name < s.Members[j].Name
	})
}

// observedMemberNames returns the sorted names of members with Observed=true.
// Used only for logging ("team reconciled … members=[…]"). Unexported so the
// log key stays controller-internal and tests do not lock it in.
func observedMemberNames(s *v1beta1.TeamStatus) []string {
	names := make([]string, 0, len(s.Members))
	for _, ms := range s.Members {
		if ms.Observed {
			names = append(names, ms.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *TeamReconciler) runtimeConfigTeamMembers(t *v1beta1.Team, desiredMembers []MemberContext) []service.RuntimeConfigTeamMember {
	roster := make([]service.RuntimeConfigTeamMember, 0, len(desiredMembers)+len(t.Spec.HumanMembers))
	for _, member := range desiredMembers {
		entry := service.RuntimeConfigTeamMember{
			Name:        member.Name,
			RuntimeName: member.RuntimeName,
			Role:        member.Role.String(),
		}
		if ms := t.Status.MemberByName(member.Name); ms != nil {
			entry.MatrixUserID = ms.MatrixUserID
			entry.PersonalRoomID = ms.RoomID
		}
		if entry.MatrixUserID == "" && r.Provisioner != nil && entry.RuntimeName != "" {
			entry.MatrixUserID = r.Provisioner.MatrixUserID(entry.RuntimeName)
		}
		roster = append(roster, entry)
	}
	for _, human := range t.Spec.HumanMembers {
		role := human.Role
		if role == "" {
			role = "coordinator"
		}
		roster = append(roster, service.RuntimeConfigTeamMember{
			Name:         human.Name,
			Role:         role,
			MatrixUserID: human.MatrixUserID,
		})
	}
	return roster
}

func teamAdminMatrixID(t *v1beta1.Team) string {
	if t.Spec.Admin == nil {
		return ""
	}
	return t.Spec.Admin.MatrixUserID
}

func teamAdminName(t *v1beta1.Team) string {
	if t.Spec.Admin == nil {
		return ""
	}
	return t.Spec.Admin.Name
}

func teamCoordinatorIDs(t *v1beta1.Team) []string {
	ids := make([]string, 0, 1+len(t.Spec.HumanMembers))
	if adminID := teamAdminMatrixID(t); adminID != "" {
		ids = append(ids, adminID)
	}
	for _, member := range t.Spec.HumanMembers {
		if !teamMemberIsCoordinator(member) {
			continue
		}
		switch {
		case member.MatrixUserID != "":
			ids = append(ids, member.MatrixUserID)
		case member.Name != "":
			ids = append(ids, member.Name)
		}
	}
	return uniqueTeamStrings(ids)
}

func teamMemberIsCoordinator(member v1beta1.TeamMemberSpec) bool {
	return member.Role == "" || member.Role == "coordinator"
}

func uniqueTeamStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager) (controller.Controller, error) {
	bldr := ctrl.NewControllerManagedBy(mgr).For(&v1beta1.Team{})

	if r.Backend != nil {
		// Watch Pods (for pod backend workers)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "pod"); wb != nil {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
				builder.WithPredicates(PodLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
			)
		}
		// Watch Sandbox CRs and transient SandboxClaim CRs (for sandbox backend workers)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "sandbox"); wb != nil {
			if sb, ok := wb.(*backend.SandboxBackend); ok {
				bldr = bldr.Watches(
					sb.WatchObject(),
					handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
				)
				bldr = bldr.Watches(
					sb.ClaimWatchObject(),
					handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
				)
			}
		}
	}

	// Watch Worker CRs whose status changes for the decoupled path. When a
	// referenced Worker's status changes, the owning Team is enqueued via the
	// spec.workerMembers.name field indexer.
	bldr = bldr.Watches(
		&v1beta1.Worker{},
		handler.EnqueueRequestsFromMapFunc(r.workerToTeamRequests),
		builder.WithPredicates(workerStatusChangePredicate()),
	)

	return bldr.Build(r)
}

// TeamPodMapFunc returns a MapFunc for routing Pod events to Team reconcile
// requests. If namespace is non-empty, it overrides obj.GetNamespace() — used
// for remote clusters where Pod namespace != CR namespace.
func TeamPodMapFunc(namespace string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		teamName := obj.GetLabels()[v1beta1.LabelTeam]
		if teamName == "" {
			return nil
		}
		ns := namespace
		if ns == "" {
			ns = obj.GetNamespace()
		}
		return []reconcile.Request{
			{NamespacedName: client.ObjectKey{
				Name:      teamName,
				Namespace: ns,
			}},
		}
	}
}

// workerToTeamRequests maps a Worker event to the Team(s) that reference it
// via spec.workerMembers[*].name.
func (r *TeamReconciler) workerToTeamRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	var teamList v1beta1.TeamList
	if err := r.List(ctx, &teamList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{TeamWorkerMembersField: obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(teamList.Items))
	for _, t := range teamList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return reqs
}

// workerStatusChangePredicate triggers only on Worker status subresource
// changes (Phase, MatrixUserID, RoomID) and delete events.
func workerStatusChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldW, ok1 := e.ObjectOld.(*v1beta1.Worker)
			newW, ok2 := e.ObjectNew.(*v1beta1.Worker)
			if !ok1 || !ok2 {
				return false
			}
			return oldW.Generation != newW.Generation ||
				oldW.Status.ObservedGeneration != newW.Status.ObservedGeneration ||
				oldW.Status.Phase != newW.Status.Phase ||
				oldW.Status.MatrixUserID != newW.Status.MatrixUserID ||
				oldW.Status.RoomID != newW.Status.RoomID
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// --- Policy helpers ---

func mergeChannelPolicy(teamPolicy, individualPolicy *v1beta1.ChannelPolicySpec) *v1beta1.ChannelPolicySpec {
	if teamPolicy == nil && individualPolicy == nil {
		return nil
	}
	merged := &v1beta1.ChannelPolicySpec{}
	if teamPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, teamPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, teamPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, teamPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, teamPolicy.DmDenyExtra...)
	}
	if individualPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, individualPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, individualPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, individualPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, individualPolicy.DmDenyExtra...)
	}
	return merged
}

func appendGroupAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.GroupAllowExtra))
	for _, v := range policy.GroupAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.GroupAllowExtra = append(policy.GroupAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}

func appendDmAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.DmAllowExtra))
	for _, v := range policy.DmAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.DmAllowExtra = append(policy.DmAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}

func teamAdminRegistryEntry(admin *v1beta1.TeamAdminSpec) *service.TeamAdminEntry {
	if admin == nil {
		return nil
	}
	return &service.TeamAdminEntry{
		Name:         admin.Name,
		MatrixUserID: admin.MatrixUserID,
	}
}

func teamMemberRegistryEntries(members []v1beta1.TeamMemberSpec) []service.TeamMemberEntry {
	if len(members) == 0 {
		return nil
	}
	entries := make([]service.TeamMemberEntry, 0, len(members))
	for _, member := range members {
		entries = append(entries, service.TeamMemberEntry{
			Name:         member.Name,
			MatrixUserID: member.MatrixUserID,
			Role:         member.Role,
		})
	}
	return entries
}
