package controller

import (
	"context"
	"fmt"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/controller/humanidentity"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type teamAdminActor struct {
	MatrixUserID string
	Token        string
	Username     string
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
		if !slicesx.Contains(human.Spec.AccessibleTeams, t.Name) {
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
		desired := desiredByName || desiredByMatrixID || slicesx.Contains(human.Spec.AccessibleTeams, teamName)
		hasRoom := slicesx.Contains(human.Status.Rooms, roomID)
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
func forceSoloPeerMentions(t *v1beta1.Team, solo bool) *v1beta1.Team {
	if t == nil || !solo {
		return t
	}
	clone := t.DeepCopy()
	tr := true
	clone.Spec.PeerMentions = &tr
	return clone
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
