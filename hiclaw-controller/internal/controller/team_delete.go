package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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

// ---------------------------------------------------------------------------
// Decoupled path: Team references standalone Worker CRs via spec.workerMembers
// ---------------------------------------------------------------------------
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
