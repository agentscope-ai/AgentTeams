package controller

import (
	"context"
	"fmt"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
	// Force PeerMentions=true in solo mode on the decoupled path too (mirror
	// reconcileTeamLegacy) — decoupledChannelPolicy consumes PeerMentions from
	// derivedTeam, so without this an AGENTTEAMS_SOLO_OPERATOR Team using
	// spec.workerMembers would never get the forced peer visibility.
	derivedTeam = forceSoloPeerMentions(derivedTeam, r.SoloOperator)

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
	standaloneAllowFrom := slicesx.UniqueNonEmpty([]string{managerMatrixID, systemAdminID, teamAdminMatrixID(t)})
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
