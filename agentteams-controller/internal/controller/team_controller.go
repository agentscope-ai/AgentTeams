package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
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
	TeamWorkerMembersField = "spec.workerMembers.name"
)

// TeamReconciler reconciles Team resources that reference existing Worker CRs
// through spec.workerMembers.
type TeamReconciler struct {
	client.Client

	Provisioner   service.WorkerProvisioner
	Deployer      service.WorkerDeployer
	ManagerConfig *service.ManagerConfigStore // nil in incluster mode

	// DefaultRuntime is forwarded into MemberDeps.DefaultRuntime for every
	// team member this reconciler converges. Sourced from
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime) — NOT from
	// AGENTTEAMS_MANAGER_RUNTIME — because team leader and worker containers are
	// both created through backend.WorkerBackend.Create as worker-type pods.
	// Empty means "no operator preference"; backend.ResolveRuntime then falls
	// back to RuntimeOpenClaw.
	DefaultRuntime string

	GatewayClient gateway.Client // gateway client for modelProvider resolution

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
// operate on the real Matrix identity for both managerConfig-password and SSO
// Humans instead of the managerConfig "localpart == name" derivation. The
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
// Human CR is missing or not yet provisioned, so managerConfig behavior (and callers
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

	return r.reconcileTeam(ctx, t, patchBase)
}

func (r *TeamReconciler) handleDelete(ctx context.Context, t *v1beta1.Team) error {
	return r.handleDeleteTeam(ctx, t)
}

// ---------------------------------------------------------------------------
// TeamReferences path: Team references standalone Worker CRs via spec.workerMembers
// ---------------------------------------------------------------------------

type teamWorkerMember struct {
	ref         v1beta1.TeamWorkerRef
	worker      v1beta1.Worker
	runtimeName string
}

func (r *TeamReconciler) teamMemberRuntime(member teamWorkerMember) string {
	return backend.ResolveRuntime(member.worker.Spec.Runtime, r.DefaultRuntime)
}

// reconcileTeam manages Team-owned organization (rooms, coordination context,
// heartbeat injection, and status aggregation) for referenced Worker CRs.
func (r *TeamReconciler) reconcileTeam(ctx context.Context, t *v1beta1.Team, patchBase client.Patch) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Validate workerMembers
	leaderRef, workerRefs, err := validateWorkerMembers(t.Spec.WorkerMembers)
	if err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}

	// 2. Resolve team-reference membership snapshot from Worker CRs.
	members, degradedMsgs := r.resolveTeamMembers(ctx, t)
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
	leaderMember := teamLeaderMember(members, leaderRef.Name)
	leaderRuntimeName := leaderMember.runtimeName
	workerRuntimeNames := teamWorkerRuntimeNames(members, leaderRef.Name)

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
	teamWorkerEntries := teamWorkerEntries(members, leaderRef.Name)
	leaderRuntime := r.teamMemberRuntime(leaderMember)

	if leaderRuntime != backend.RuntimeQwenPaw {
		// Overlay Team Leader built-ins onto the team-reference leader Worker before
		// injecting the team coordination context. The Worker still owns its
		// lifecycle and credentials; this only restores role-specific prompt and
		// skill assets generated for Team members.
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
			WorkerIdleTimeout:  "",
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
		if r.teamMemberRuntime(rm) == backend.RuntimeQwenPaw {
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
	if err := r.deployTeamRuntimeConfigs(ctx, derivedTeam, members, leaderRef.Name, teamRuntimeName, leaderRuntimeName, rooms); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}

	// 6. Channel authorization
	if r.ManagerConfig != nil && r.ManagerConfig.Enabled() {
		managerMatrixID := r.ManagerConfig.MatrixUserID("manager")
		leaderMatrixID := r.ManagerConfig.MatrixUserID(leaderRuntimeName)
		if err := r.ManagerConfig.UpdateManagerGroupAllowFrom(leaderMatrixID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom for team leader (non-fatal)")
		}
		for _, rm := range members {
			if rm.ref.Name != leaderRef.Name {
				if rm.worker.Status.RoomID != "" {
					if err := r.Provisioner.KickFromRoom(
						ctx,
						rm.worker.Status.RoomID,
						managerMatrixID,
						"Team workers communicate through the Team Room",
					); err != nil {
						return r.failTeam(ctx, t, patchBase,
							fmt.Sprintf("remove Manager from Worker %q personal room: %v", rm.ref.Name, err))
					}
				}
				if err := r.ManagerConfig.UpdateManagerGroupAllowFrom(r.ManagerConfig.MatrixUserID(rm.runtimeName), false); err != nil {
					logger.Error(err, "failed to revoke Manager groupAllowFrom for team worker (non-fatal)", "worker", rm.runtimeName)
				}
			}
		}
	}
	for _, rm := range members {
		role := RoleTeamWorker
		if rm.ref.Name == leaderRef.Name {
			role = RoleTeamLeader
		}
		if r.teamMemberRuntime(rm) != backend.RuntimeQwenPaw {
			policy := r.teamChannelPolicy(derivedTeam, members, leaderRef.Name, rm, role)
			if err := r.Deployer.InjectChannelPolicy(ctx, service.InjectChannelPolicyRequest{
				WorkerName:     rm.runtimeName,
				GroupAllowFrom: policy.GroupAllowFrom,
				DMAllowFrom:    policy.DMAllowFrom,
			}); err != nil {
				logger.Error(err, "channel policy injection failed (non-fatal)", "worker", rm.runtimeName)
			}
		}
	}

	// 7. Status aggregation
	if err := r.cleanupStaleTeamMembers(ctx, derivedTeam, members); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	leaderReady, readyWorkers := aggregateTeamStatus(t, members, leaderRef.Name, len(workerRefs))

	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		logger.Error(err, "failed to patch team status (non-fatal)")
	}

	logger.Info("team reconciled (team-reference)",
		"name", t.Name,
		"phase", t.Status.Phase,
		"leaderReady", leaderReady,
		"readyWorkers", readyWorkers,
		"totalWorkers", t.Status.TotalWorkers)
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *TeamReconciler) resolveTeamMembers(ctx context.Context, t *v1beta1.Team) ([]teamWorkerMember, []string) {
	members := make([]teamWorkerMember, 0, len(t.Spec.WorkerMembers))
	var degradedMsgs []string

	for _, ref := range t.Spec.WorkerMembers {
		var w v1beta1.Worker
		key := client.ObjectKey{Name: ref.Name, Namespace: t.Namespace}
		if err := r.Get(ctx, key, &w); err != nil {
			degradedMsgs = append(degradedMsgs, fmt.Sprintf("Worker %q not found", ref.Name))
			continue
		}
		members = append(members, teamWorkerMember{
			ref:         ref,
			worker:      w,
			runtimeName: w.Spec.EffectiveWorkerName(w.Name),
		})
	}
	return members, degradedMsgs
}

func teamLeaderMember(members []teamWorkerMember, leaderName string) teamWorkerMember {
	for _, member := range members {
		if member.ref.Name == leaderName {
			return member
		}
	}
	return teamWorkerMember{}
}

func teamWorkerRuntimeNames(members []teamWorkerMember, leaderName string) []string {
	names := make([]string, 0, len(members))
	for _, member := range members {
		if member.ref.Name == leaderName {
			continue
		}
		names = append(names, member.runtimeName)
	}
	return names
}

func teamWorkerEntries(members []teamWorkerMember, leaderName string) []service.TeamWorkerEntry {
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

func (r *TeamReconciler) deployTeamRuntimeConfigs(
	ctx context.Context,
	t *v1beta1.Team,
	members []teamWorkerMember,
	leaderName string,
	teamRuntimeName string,
	leaderRuntimeName string,
	rooms *service.TeamRoomResult,
) error {
	roster := runtimeConfigTeamMembers(t, members, leaderName)
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

func runtimeConfigTeamMembers(t *v1beta1.Team, members []teamWorkerMember, leaderName string) []service.RuntimeConfigTeamMember {
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

type teamChannelAllowLists struct {
	GroupAllowFrom []string
	DMAllowFrom    []string
}

func teamMemberStatusSnapshot(member teamWorkerMember, role MemberRole) v1beta1.TeamMemberStatus {
	ms := v1beta1.TeamMemberStatus{Name: member.ref.Name, Role: role.String()}
	syncTeamMemberStatus(&ms, member)
	return ms
}

func syncTeamMemberStatus(ms *v1beta1.TeamMemberStatus, member teamWorkerMember) {
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

func (r *TeamReconciler) cleanupStaleTeamMembers(ctx context.Context, t *v1beta1.Team, members []teamWorkerMember) error {
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
			if err := r.detachTeamMember(ctx, t, &w); err != nil {
				return fmt.Errorf("detach stale Team member %q: %w", ms.Name, err)
			}
			continue
		}
	}
	return nil
}

func (r *TeamReconciler) detachTeamMember(ctx context.Context, t *v1beta1.Team, w *v1beta1.Worker) error {
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

	if r.ManagerConfig == nil || !r.ManagerConfig.Enabled() {
		return nil
	}
	managerMatrixID := r.ManagerConfig.MatrixUserID("manager")
	if w.Status.RoomID != "" {
		if err := r.Provisioner.InviteToRoom(ctx, w.Status.RoomID, managerMatrixID); err != nil {
			return fmt.Errorf("restore Manager to Worker %q personal room: %w", w.Name, err)
		}
	}
	if runtime == backend.RuntimeQwenPaw {
		return nil
	}
	if err := r.ManagerConfig.UpdateManagerGroupAllowFrom(r.ManagerConfig.MatrixUserID(runtimeName), false); err != nil {
		logger.Error(err, "failed to revoke Manager groupAllowFrom for detached member (non-fatal)", "worker", runtimeName)
	}
	var systemAdminID string
	if r.SystemAdminUser != "" {
		systemAdminID = r.ManagerConfig.MatrixUserID(r.SystemAdminUser)
	}
	standaloneAllowFrom := uniqueTeamStrings([]string{managerMatrixID, systemAdminID, teamAdminMatrixID(t)})
	if err := r.Deployer.InjectChannelPolicy(ctx, service.InjectChannelPolicyRequest{
		WorkerName:     runtimeName,
		GroupAllowFrom: standaloneAllowFrom,
		DMAllowFrom:    standaloneAllowFrom,
	}); err != nil {
		logger.Error(err, "failed to reset worker channel policy (non-fatal)", "worker", runtimeName)
	}
	return nil
}

func (r *TeamReconciler) teamChannelPolicy(t *v1beta1.Team, members []teamWorkerMember, leaderName string, current teamWorkerMember, role MemberRole) teamChannelAllowLists {
	resolve := func(value string) string {
		if value == "" || strings.HasPrefix(value, "@") {
			return value
		}
		if r.ManagerConfig != nil && r.ManagerConfig.Enabled() {
			return r.ManagerConfig.MatrixUserID(value)
		}
		if r.Provisioner != nil {
			return r.Provisioner.MatrixUserID(value)
		}
		return value
	}

	leaderRuntimeName := teamLeaderMember(members, leaderName).runtimeName
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

	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, individualTeamChannelPolicy(t, current, role))
	if policy != nil {
		groupAllow = applyChannelAllowPolicy(groupAllow, policy.GroupAllowExtra, policy.GroupDenyExtra, resolve)
		dmAllow = applyChannelAllowPolicy(dmAllow, policy.DmAllowExtra, policy.DmDenyExtra, resolve)
	}
	return teamChannelAllowLists{
		GroupAllowFrom: uniqueTeamStrings(groupAllow),
		DMAllowFrom:    uniqueTeamStrings(dmAllow),
	}
}

func individualTeamChannelPolicy(t *v1beta1.Team, member teamWorkerMember, role MemberRole) *v1beta1.ChannelPolicySpec {
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

func aggregateTeamStatus(t *v1beta1.Team, members []teamWorkerMember, leaderName string, totalWorkers int) (bool, int) {
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
		syncTeamMemberStatus(ms, member)
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

// handleDeleteTeam removes Team-owned state while preserving referenced Workers.
// It revokes team coordination from members (writing standalone context back),
// removes registry entries, and deletes room aliases. It does NOT destroy
// member Workers — they have independent lifecycles.
func (r *TeamReconciler) handleDeleteTeam(ctx context.Context, t *v1beta1.Team) error {
	logger := log.FromContext(ctx)
	logger.Info("deleting team (team-reference)", "name", t.Name)
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
		if err := r.detachTeamMember(ctx, t, &w); err != nil {
			return fmt.Errorf("detach Team member %q: %w", ref.Name, err)
		}
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

	// Revoke the deleted Team Leader's Manager-room authorization.
	if r.ManagerConfig != nil && r.ManagerConfig.Enabled() {
		if leaderRef != nil {
			var leaderW v1beta1.Worker
			key := client.ObjectKey{Name: leaderRef.Name, Namespace: t.Namespace}
			if err := r.Get(ctx, key, &leaderW); err == nil {
				leaderRN := leaderW.Spec.EffectiveWorkerName(leaderW.Name)
				leaderMatrixID := r.ManagerConfig.MatrixUserID(leaderRN)
				if err := r.ManagerConfig.UpdateManagerGroupAllowFrom(leaderMatrixID, false); err != nil {
					logger.Error(err, "failed to revoke Manager groupAllowFrom (non-fatal)")
				}
			}
		}
	}

	// Delete team room aliases so a fresh Team CR with the same name gets
	// clean aliases.
	leaderRuntimeName := r.teamLeaderRuntimeName(ctx, t, leaderRef)
	r.archiveTeamRooms(ctx, t, teamRuntimeName, leaderRuntimeName)
	if err := r.Provisioner.DeleteTeamRoomAliases(ctx, teamRuntimeName, leaderRuntimeName); err != nil {
		logger.Error(err, "failed to delete team room aliases (non-fatal)")
	}

	return nil
}

func (r *TeamReconciler) teamLeaderRuntimeName(ctx context.Context, t *v1beta1.Team, leaderRef *v1beta1.TeamWorkerRef) string {
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
		} else if role == RoleTeamWorker.String() {
			workers = append(workers, *ref)
		} else {
			return nil, nil, fmt.Errorf("workerMembers[%d].role must be %q or %q", i, RoleTeamLeader.String(), RoleTeamWorker.String())
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

	// Watch Worker CRs whose status changes. When a
	// referenced Worker's status changes, the owning Team is enqueued via the
	// spec.workerMembers.name field indexer.
	bldr = bldr.Watches(
		&v1beta1.Worker{},
		handler.EnqueueRequestsFromMapFunc(r.workerToTeamRequests),
		builder.WithPredicates(workerStatusChangePredicate()),
	)

	return bldr.Build(r)
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
