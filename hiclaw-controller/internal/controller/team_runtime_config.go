package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
)

// memberRuntimeConfigInput captures the fields shared by legacy inline Team
// members and decoupled Worker CR references when building runtime.yaml /
// openclaw team context deploy requests.
type memberRuntimeConfigInput struct {
	Name              string
	RuntimeName       string
	Runtime           string
	Role              MemberRole
	Generation        int64
	Spec              v1beta1.WorkerSpec
	DeployMode        string
	MatrixUserID      string
	PersonalRoomID    string
	TeamName          string
	TeamRoomID        string
	LeaderName        string
	LeaderRuntimeName string
	LeaderDMRoomID    string
	TeamMembers       []service.RuntimeConfigTeamMember
	DropTeamContext   bool
}

func buildMemberRuntimeConfigReq(t *v1beta1.Team, in memberRuntimeConfigInput, aiGatewayURL string) service.MemberRuntimeConfigDeployRequest {
	leaderName := in.LeaderName
	if leaderName == "" {
		leaderName = in.LeaderRuntimeName
	}
	return service.MemberRuntimeConfigDeployRequest{
		Name:              in.Name,
		RuntimeName:       in.RuntimeName,
		Runtime:           in.Runtime,
		Role:              in.Role.String(),
		Generation:        in.Generation,
		Spec:              in.Spec,
		AIGatewayURL:      aiGatewayURL,
		MatrixUserID:      in.MatrixUserID,
		PersonalRoomID:    in.PersonalRoomID,
		TeamName:          in.TeamName,
		TeamRoomID:        in.TeamRoomID,
		LeaderName:        leaderName,
		LeaderRuntimeName: in.LeaderRuntimeName,
		LeaderDMRoomID:    in.LeaderDMRoomID,
		TeamAdminName:     teamAdminName(t),
		TeamAdminMatrixID: teamAdminMatrixID(t),
		TeamMembers:       in.TeamMembers,
		DropTeamContext:   in.DropTeamContext,
	}
}

func (r *TeamReconciler) deployMemberRuntimeConfig(ctx context.Context, t *v1beta1.Team, in memberRuntimeConfigInput) error {
	runtime := backend.ResolveRuntime(in.Spec.Runtime, r.DefaultRuntime)
	deployMode := in.DeployMode
	if deployMode == "" {
		deployMode = v1beta1.DeployModeLocal
		if in.Spec.DeployMode != nil {
			deployMode = *in.Spec.DeployMode
		}
	}
	if runtime != backend.RuntimeQwenPaw && deployMode != v1beta1.DeployModeEdge {
		return nil
	}
	aiGatewayURL, err := r.runtimeConfigAIGatewayURL(ctx, in.Spec, in.Name)
	if err != nil {
		return err
	}
	req := buildMemberRuntimeConfigReq(t, memberRuntimeConfigInput{
		Name:              in.Name,
		RuntimeName:       in.RuntimeName,
		Runtime:           runtime,
		Role:              in.Role,
		Generation:        in.Generation,
		Spec:              in.Spec,
		DeployMode:        deployMode,
		MatrixUserID:      in.MatrixUserID,
		PersonalRoomID:    in.PersonalRoomID,
		TeamName:          in.TeamName,
		TeamRoomID:        in.TeamRoomID,
		LeaderName:        in.LeaderName,
		LeaderRuntimeName: in.LeaderRuntimeName,
		LeaderDMRoomID:    in.LeaderDMRoomID,
		TeamMembers:       in.TeamMembers,
		DropTeamContext:   in.DropTeamContext,
	}, aiGatewayURL)
	if deployMode == v1beta1.DeployModeEdge {
		req.Runtime = runtimeRemoteManagedLocal
		if err := r.Deployer.MergeMemberRuntimeTeamContext(ctx, req); err != nil {
			return fmt.Errorf("merge runtime team context for %s: %w", in.RuntimeName, err)
		}
		return nil
	}
	if err := r.Deployer.DeployMemberRuntimeConfig(ctx, req); err != nil {
		return fmt.Errorf("deploy runtime config for %s: %w", in.RuntimeName, err)
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

func (r *TeamReconciler) deployLegacyRuntimeConfig(ctx context.Context, t *v1beta1.Team, member MemberContext, leaderRuntimeName string, rooms *service.TeamRoomResult, roster []service.RuntimeConfigTeamMember) error {
	deployMode := v1beta1.DeployModeLocal
	if member.Spec.DeployMode != nil {
		deployMode = *member.Spec.DeployMode
	}
	matrixUserID := member.ExistingMatrixUserID
	personalRoomID := member.ExistingRoomID
	if ms := t.Status.MemberByName(member.Name); ms != nil {
		if ms.MatrixUserID != "" {
			matrixUserID = ms.MatrixUserID
		}
		if ms.RoomID != "" {
			personalRoomID = ms.RoomID
		}
	}
	return r.deployMemberRuntimeConfig(ctx, t, memberRuntimeConfigInput{
		Name:              member.Name,
		RuntimeName:       member.RuntimeName,
		Role:              member.Role,
		Generation:        t.Generation,
		Spec:              member.Spec,
		DeployMode:        deployMode,
		MatrixUserID:      matrixUserID,
		PersonalRoomID:    personalRoomID,
		TeamName:          t.Spec.EffectiveTeamName(t.Name),
		TeamRoomID:        rooms.TeamRoomID,
		LeaderName:        t.Spec.Leader.Name,
		LeaderRuntimeName: leaderRuntimeName,
		LeaderDMRoomID:    rooms.LeaderDMRoomID,
		TeamMembers:       roster,
	})
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
		if !member.worker.DeletionTimestamp.IsZero() {
			continue
		}
		deployMode := v1beta1.DeployModeLocal
		if member.worker.Spec.DeployMode != nil {
			deployMode = *member.worker.Spec.DeployMode
		}
		role := RoleTeamWorker
		if member.ref.Name == leaderName {
			role = RoleTeamLeader
		}
		leaderNameFact := leaderName
		if leaderNameFact == "" {
			leaderNameFact = leaderRuntimeName
		}
		if err := r.deployMemberRuntimeConfig(ctx, t, memberRuntimeConfigInput{
			Name:              member.ref.Name,
			RuntimeName:       member.runtimeName,
			Role:              role,
			Generation:        member.worker.Generation,
			Spec:              member.worker.Spec,
			DeployMode:        deployMode,
			MatrixUserID:      member.worker.Status.MatrixUserID,
			PersonalRoomID:    member.worker.Status.RoomID,
			TeamName:          teamRuntimeName,
			TeamRoomID:        rooms.TeamRoomID,
			LeaderName:        leaderNameFact,
			LeaderRuntimeName: leaderRuntimeName,
			LeaderDMRoomID:    rooms.LeaderDMRoomID,
			TeamMembers:       roster,
		}); err != nil {
			return err
		}
	}
	return nil
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
