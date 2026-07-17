package service

import (
	"context"
	"fmt"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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

// TeamRoomArchiveRequest describes Team-owned rooms to mark as deleted while
// preserving their history.
type TeamRoomArchiveRequest struct {
	TeamName       string
	LeaderName     string
	TeamRoomID     string
	LeaderDMRoomID string
	ActorToken     string
}

// RefreshResult contains refreshed credentials for update operations.
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
		teamDesired = slicesx.AppendUnique(teamDesired, teamAdminID)
	} else {
		teamDesired = slicesx.AppendUnique(teamDesired, adminMatrixID)
	}
	teamDesired = slicesx.AppendUnique(teamDesired, leaderMatrixID)
	teamDesired = slicesx.AppendUnique(teamDesired, teamCoordinatorIDs...)
	teamDesired = slicesx.AppendUnique(teamDesired, teamMemberIDs...)
	for _, wn := range req.WorkerNames {
		teamDesired = slicesx.AppendUnique(teamDesired, p.matrix.UserID(wn))
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

	teamMeta := teamRoomMeta(req, teamAdminID, leaderMatrixID, p.matrix.UserID)
	teamRoom, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Team: %s", req.TeamName),
		Topic:         fmt.Sprintf("Team room for %s", req.TeamName),
		Invite:        teamInvites,
		PowerLevels:   teamRoomPowerLevels,
		CreatorToken:  req.TeamAdminActorToken,
		InitialState:  roomMetaState(teamMeta),
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
	teamMetaToken := ""
	if hasTeamAdmin {
		teamMetaToken = req.TeamAdminActorToken
	}
	if err := p.matrix.SetRoomState(ctx, teamRoom.RoomID, roomMetaEventType, "", teamMeta, teamMetaToken); err != nil {
		return nil, fmt.Errorf("set team room meta: %w", err)
	}

	// Leader DM Room: only Leader + Team Admin when configured; otherwise
	// fallback to the global Admin for legacy teams.
	leaderDMDesired := []string{leaderMatrixID}
	if hasTeamAdmin {
		leaderDMDesired = slicesx.AppendUnique(leaderDMDesired, teamAdminID)
	} else {
		leaderDMDesired = slicesx.AppendUnique(leaderDMDesired, adminMatrixID)
	}
	leaderDMInvites := leaderDMDesired
	if hasTeamAdmin {
		leaderDMInvites = withoutString(leaderDMDesired, teamAdminID)
	}
	leaderDMMeta := leaderDMRoomMeta(req, teamAdminID, leaderMatrixID)
	leaderDMRoom, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Leader DM: %s", req.LeaderName),
		Topic:         fmt.Sprintf("DM channel for team leader %s", req.LeaderName),
		Invite:        leaderDMInvites,
		PowerLevels:   p.leaderDMPowerLevels(managerMatrixID, adminMatrixID, leaderMatrixID, teamAdminID, hasTeamAdmin),
		CreatorToken:  req.TeamAdminActorToken,
		IsDirect:      true,
		InitialState:  roomMetaState(leaderDMMeta),
		RoomAliasName: roomAliasLocalpart("leader-dm", req.LeaderName),
	})
	if err != nil {
		return nil, fmt.Errorf("leader DM room creation failed: %w", err)
	}
	logger.Info("leader DM room ready", "roomID", leaderDMRoom.RoomID, "created", leaderDMRoom.Created)

	if hasTeamAdmin {
		if err := p.ensureTeamAdminJoinedLeaderDM(ctx, leaderDMRoom.RoomID, teamAdminID, req.TeamAdminActorToken, req.LeaderCredentialName, req.LeaderName, req.TeamName, leaderDMRoom.Created); err != nil {
			return nil, err
		}
	}

	leaderDMInviteToken := ""
	leaderDMInviteActor := ""
	if hasTeamAdmin {
		leaderDMInviteToken = req.TeamAdminActorToken
		leaderDMInviteActor = req.TeamAdminActorName
	} else if !leaderDMRoom.Created {
		if token, err := p.leaderInviteToken(ctx, req.LeaderCredentialName, req.LeaderName, req.TeamName); err != nil {
			logger.Error(err, "failed to load leader token for existing leader DM; falling back to admin invite", "leader", req.LeaderName)
		} else {
			leaderDMInviteToken = token
			leaderDMInviteActor = "leader"
			if err := p.matrix.JoinRoom(ctx, leaderDMRoom.RoomID, token); err != nil {
				return nil, fmt.Errorf("leader join leader DM room: %w", err)
			}
		}
	}
	if hasTeamAdmin || leaderDMInviteToken != "" {
		if err := p.ReconcileRoomMembershipWithActorToken(ctx, leaderDMRoom.RoomID, leaderDMDesired, leaderDMInviteToken, leaderDMInviteActor); err != nil {
			return nil, fmt.Errorf("reconcile leader DM membership: %w", err)
		}
	}
	leaderDMMetaToken := ""
	if hasTeamAdmin {
		leaderDMMetaToken = req.TeamAdminActorToken
	} else if leaderDMInviteToken != "" {
		leaderDMMetaToken = leaderDMInviteToken
	}
	if err := p.matrix.SetRoomState(ctx, leaderDMRoom.RoomID, roomMetaEventType, "", leaderDMMeta, leaderDMMetaToken); err != nil {
		return nil, fmt.Errorf("set leader DM room meta: %w", err)
	}

	return &TeamRoomResult{
		TeamRoomID:     teamRoom.RoomID,
		LeaderDMRoomID: leaderDMRoom.RoomID,
	}, nil
}
func (p *Provisioner) ensureTeamAdminJoinedLeaderDM(ctx context.Context, roomID, teamAdminID, teamAdminToken, leaderCredentialName, leaderName, teamName string, created bool) error {
	if err := p.matrix.JoinRoom(ctx, roomID, teamAdminToken); err == nil {
		return nil
	} else if created {
		return fmt.Errorf("team admin join leader DM room: %w", err)
	} else {
		joinErr := err
		leaderToken, tokenErr := p.leaderInviteToken(ctx, leaderCredentialName, leaderName, teamName)
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
		if !v1beta1.TeamMemberIsCoordinator(member) {
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
	return slicesx.UniqueNonEmpty(ids)
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
	return slicesx.UniqueNonEmpty(ids)
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
func (p *Provisioner) leaderInviteToken(ctx context.Context, credentialName, leaderName, teamName string) (string, error) {
	if p.creds == nil {
		return "", fmt.Errorf("credential store unavailable")
	}
	if credentialName == "" {
		credentialName = leaderName
	}
	refresh, err := p.RefreshWorkerCredentials(ctx, credentialName, leaderName, teamName)
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

// ArchiveTeamRooms marks preserved Team rooms with a stable deleted suffix so
// humans can distinguish them from active rooms after aliases are released.
func (p *Provisioner) ArchiveTeamRooms(ctx context.Context, req TeamRoomArchiveRequest) error {
	logger := log.FromContext(ctx)
	if req.TeamRoomID != "" {
		name := fmt.Sprintf("Team: %s [deleted]", req.TeamName)
		if err := p.matrix.SetRoomName(ctx, req.TeamRoomID, name, req.ActorToken); err != nil {
			logger.Error(err, "failed to archive team room name (non-fatal)", "roomID", req.TeamRoomID, "name", name)
		}
	}
	if req.LeaderDMRoomID != "" {
		name := fmt.Sprintf("Leader DM: %s [deleted]", req.LeaderName)
		if err := p.matrix.SetRoomName(ctx, req.LeaderDMRoomID, name, req.ActorToken); err != nil {
			logger.Error(err, "failed to archive leader DM room name (non-fatal)", "roomID", req.LeaderDMRoomID, "name", name)
		}
	}
	return nil
}

// DeleteWorkerRoomAlias removes the alias that identifies a worker's comm
// channel. Same semantics as DeleteTeamRoomAliases — the underlying room is
// preserved, only the controller's handle to it is released.
