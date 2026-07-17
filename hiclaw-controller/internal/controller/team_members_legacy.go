package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
	derivedTeam = forceSoloPeerMentions(derivedTeam, r.SoloOperator)

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

	deps := r.memberDeps()

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

// reconcileMember runs the shared member phases for one team member and
// writes the resulting runtime state into ms. The leader never has
// ExposedPorts (the Leader phase always produces zero ports), so that field
// stays nil for RoleTeamLeader entries.
//
// ms.Observed is flipped to true the instant ReconcileMemberInfra succeeds —
// see the Step 4 comment in reconcileTeamNormal for why post-infra failures
// must not revoke observed status (token-rotation hazard).
