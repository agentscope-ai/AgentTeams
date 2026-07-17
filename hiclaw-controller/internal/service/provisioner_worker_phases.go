package service

import (
	"context"
	"fmt"
	"time"

	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RoomMembershipOptions configures how ReconcileRoomMembership drives a room
// toward a desired member set. ActorToken/ActorName enable invite/kick using a
// joined member's credentials instead of the admin bot.
type RoomMembershipOptions struct {
	ActorToken string
	ActorName  string
}

// ReconcileRoomMembership drives the membership of roomID to match desired.
func (p *Provisioner) ReconcileRoomMembership(ctx context.Context, roomID string, desired []string) error {
	return p.ReconcileRoomMembershipWithOptions(ctx, roomID, desired, RoomMembershipOptions{})
}

func (p *Provisioner) ReconcileRoomMembershipWithInviteToken(ctx context.Context, roomID string, desired []string, inviteToken, inviteActor string) error {
	return p.ReconcileRoomMembershipWithOptions(ctx, roomID, desired, RoomMembershipOptions{
		ActorToken: inviteToken,
		ActorName:  inviteActor,
	})
}

func (p *Provisioner) ReconcileRoomMembershipWithActorToken(ctx context.Context, roomID string, desired []string, actorToken, actorName string) error {
	return p.ReconcileRoomMembershipWithOptions(ctx, roomID, desired, RoomMembershipOptions{
		ActorToken: actorToken,
		ActorName:  actorName,
	})
}

// ReconcileRoomMembershipWithOptions drives room membership to match desired.
func (p *Provisioner) ReconcileRoomMembershipWithOptions(ctx context.Context, roomID string, desired []string, opts RoomMembershipOptions) error {
	logger := log.FromContext(ctx)

	var current []matrix.RoomMember
	var err error
	if opts.ActorToken != "" {
		current, err = p.matrix.ListRoomMembersWithToken(ctx, roomID, opts.ActorToken)
	} else {
		current, err = p.matrix.ListRoomMembers(ctx, roomID)
	}
	if err != nil {
		return fmt.Errorf("list members of %s: %w", roomID, err)
	}

	desiredSet := make(map[string]struct{}, len(desired))
	for _, u := range desired {
		if u == "" {
			continue
		}
		desiredSet[u] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, m := range current {
		currentSet[m.UserID] = struct{}{}
	}

	var firstErr error

	for _, u := range desired {
		if _, ok := currentSet[u]; ok {
			continue
		}
		var inviteErr error
		if opts.ActorToken != "" {
			logger.Info("inviting user to room with joined member token", "room", roomID, "user", u, "actor", opts.ActorName)
			inviteErr = p.matrix.InviteToRoomWithToken(ctx, roomID, u, opts.ActorToken)
		} else {
			inviteErr = p.matrix.InviteToRoom(ctx, roomID, u)
		}
		if inviteErr != nil {
			logger.Error(inviteErr, "failed to invite user to room", "room", roomID, "user", u)
			if firstErr == nil {
				firstErr = inviteErr
			}
		}
	}

	for _, m := range current {
		if _, ok := desiredSet[m.UserID]; ok {
			continue
		}
		if m.UserID == p.matrix.UserID(p.adminUser) {
			continue
		}
		logger.Info("room member not desired; attempting removal",
			"room", roomID,
			"user", m.UserID,
			"membership", m.Membership,
			"currentCount", len(currentSet),
			"desiredCount", len(desiredSet))
		var kickErr error
		if opts.ActorToken != "" {
			logger.Info("kicking user from room with joined member token", "room", roomID, "user", m.UserID, "actor", opts.ActorName)
			kickErr = p.matrix.KickFromRoomWithToken(ctx, roomID, m.UserID, "removed from desired member set", opts.ActorToken)
		} else {
			kickErr = p.matrix.KickFromRoom(ctx, roomID, m.UserID, "removed from desired member set")
		}
		if kickErr != nil {
			logger.Error(kickErr, "failed to kick user from room", "room", roomID, "user", m.UserID)
			if shouldForceLeaveAfterKickError(kickErr) {
				forceErr := p.ForceLeaveRoom(ctx, m.UserID, roomID)
				if forceErr == nil {
					logger.Info("force-leave-room command sent after kick failed", "room", roomID, "user", m.UserID)
					stillPresent, memberships, checkErr := p.observedRoomMembership(ctx, roomID, m.UserID)
					if checkErr != nil {
						logger.Error(checkErr, "failed to verify force-leave-room result", "room", roomID, "user", m.UserID)
					} else {
						logger.Info("force-leave-room post-check",
							"room", roomID,
							"user", m.UserID,
							"stillPresent", stillPresent,
							"memberships", memberships)
					}
					continue
				}
				logger.Error(forceErr, "failed to send force-leave-room command", "room", roomID, "user", m.UserID)
				kickErr = forceErr
			}
			if firstErr == nil {
				firstErr = kickErr
			}
		}
	}

	return firstErr
}

// workerProvisionState carries intermediate state through ProvisionWorker phases.
type workerProvisionState struct {
	workerName      string
	credentialName  string
	consumerName    string
	workerMatrixID  string
	managerMatrixID string
	adminMatrixID   string
	isTeamWorker    bool
	creds           *WorkerCredentials
	generatedCreds  bool
	userCreds       *matrix.UserCredentials
	authorityID     string
	roomID          string
	roomCreated     bool
}

func (p *Provisioner) ensureWorkerCredentials(ctx context.Context, req WorkerProvisionRequest) (*workerProvisionState, error) {
	workerName := req.Name
	credentialName := req.CredentialName
	if credentialName == "" {
		credentialName = workerName
	}
	creds, err := p.loadWorkerCredentials(ctx, credentialName, workerName)
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	generatedCreds := false
	if creds == nil {
		creds, err = GenerateCredentials()
		if err != nil {
			return nil, fmt.Errorf("generate credentials: %w", err)
		}
		if err := p.creds.Save(ctx, credentialName, creds); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
		}
		generatedCreds = true
	}
	return &workerProvisionState{
		workerName:      workerName,
		credentialName:  credentialName,
		consumerName:    "worker-" + workerName,
		workerMatrixID:  p.matrix.UserID(workerName),
		managerMatrixID: p.matrix.UserID("manager"),
		adminMatrixID:   p.matrix.UserID(p.adminUser),
		isTeamWorker:    req.TeamLeaderName != "",
		creds:           creds,
		generatedCreds:  generatedCreds,
	}, nil
}

func (p *Provisioner) ensureWorkerMatrixIdentity(ctx context.Context, st *workerProvisionState) error {
	logger := log.FromContext(ctx)
	logger.Info("registering Matrix account", "name", st.workerName)
	var userCreds *matrix.UserCredentials
	var err error
	if p.MatrixAppServiceEnabled() {
		userCreds, err = p.matrix.EnsureAppServiceUser(ctx, st.workerName)
		if err != nil {
			return fmt.Errorf("Matrix AS registration failed: %w", err)
		}
		st.creds.MatrixPassword = ""
	} else {
		userCreds, err = p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{
			Username: st.workerName,
			Password: st.creds.MatrixPassword,
		})
		if err != nil {
			return fmt.Errorf("Matrix registration failed: %w", err)
		}
		st.creds.MatrixPassword = userCreds.Password
	}
	if userCreds.AccessToken != "" {
		st.creds.MatrixToken = userCreds.AccessToken
	}
	st.userCreds = userCreds
	return nil
}

func (p *Provisioner) ensureWorkerMinIOUser(ctx context.Context, st *workerProvisionState, teamName string) error {
	if p.ossAdmin == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	logger.Info("creating MinIO user", "name", st.workerName)
	if err := p.ossAdmin.EnsureUser(ctx, st.workerName, st.creds.MinIOPassword); err != nil {
		return fmt.Errorf("MinIO user creation failed: %w", err)
	}
	if err := p.ossAdmin.EnsurePolicy(ctx, oss.PolicyRequest{
		WorkerName: st.workerName,
		TeamName:   teamName,
	}); err != nil {
		return fmt.Errorf("MinIO policy creation failed: %w", err)
	}
	return nil
}

func (p *Provisioner) ensureWorkerPersonalRoom(ctx context.Context, req WorkerProvisionRequest, st *workerProvisionState) error {
	logger := log.FromContext(ctx)
	logger.Info("creating Matrix room", "name", st.workerName)

	switch {
	case st.isTeamWorker:
		st.authorityID = p.matrix.UserID(req.TeamLeaderName)
	case p.managerEnabled:
		st.authorityID = st.managerMatrixID
	default:
		st.authorityID = st.adminMatrixID
	}

	powerLevels := map[string]int{
		st.managerMatrixID: 100,
		st.adminMatrixID:   100,
		st.authorityID:     100,
		st.workerMatrixID:  0,
	}

	invite := []string{st.adminMatrixID}
	if st.authorityID != st.adminMatrixID {
		invite = append(invite, st.authorityID)
	}
	invite = append(invite, st.workerMatrixID)

	leaderMatrixID := ""
	if req.TeamLeaderName != "" {
		leaderMatrixID = p.matrix.UserID(req.TeamLeaderName)
	}
	workerMeta := workerRoomMeta(req, st.workerMatrixID, leaderMatrixID)
	roomReq := matrix.CreateRoomRequest{
		Name:          fmt.Sprintf("Worker: %s", st.workerName),
		Topic:         fmt.Sprintf("Communication channel for %s", st.workerName),
		Invite:        invite,
		PowerLevels:   powerLevels,
		InitialState:  roomMetaState(workerMeta),
		RoomAliasName: roomAliasLocalpart("worker", st.workerName),
	}
	roomInfo, err := p.matrix.CreateRoom(ctx, roomReq)
	if err != nil {
		return fmt.Errorf("Matrix room creation failed: %w", err)
	}
	if st.generatedCreds && !roomInfo.Created {
		alias := p.roomAliasFull(roomReq.RoomAliasName)
		logger.Info("worker room alias resolved to existing room for fresh credentials; recreating room",
			"alias", alias, "oldRoomID", roomInfo.RoomID)
		if err := p.matrix.DeleteRoomAlias(ctx, alias); err != nil {
			return fmt.Errorf("delete stale worker room alias %s: %w", alias, err)
		}
		roomInfo, err = p.matrix.CreateRoom(ctx, roomReq)
		if err != nil {
			return fmt.Errorf("Matrix room creation after stale alias cleanup failed: %w", err)
		}
		if !roomInfo.Created {
			return fmt.Errorf("worker room alias %s still resolves to existing room %s after cleanup", alias, roomInfo.RoomID)
		}
	}
	st.roomID = roomInfo.RoomID
	st.roomCreated = roomInfo.Created
	logger.Info("Matrix room ready", "roomID", st.roomID, "created", st.roomCreated)

	if err := p.creds.Save(ctx, st.credentialName, st.creds); err != nil {
		logger.Error(err, "failed to persist credentials (non-fatal)")
	}

	if !st.roomCreated {
		if err := p.ReconcileRoomMembership(ctx, st.roomID, []string{st.adminMatrixID, st.authorityID, st.workerMatrixID}); err != nil {
			logger.Error(err, "failed to reconcile worker room membership (non-fatal)", "roomID", st.roomID)
		}
	}
	if err := p.matrix.SetRoomState(ctx, st.roomID, roomMetaEventType, "", workerMeta, ""); err != nil {
		return fmt.Errorf("set worker room meta: %w", err)
	}

	if st.userCreds.AccessToken != "" && st.roomID != "" {
		if err := p.matrix.JoinRoom(ctx, st.roomID, st.userCreds.AccessToken); err != nil {
			logger.Error(err, "failed to join worker into its own room (non-fatal)",
				"name", st.workerName, "roomID", st.roomID)
		} else {
			logger.Info("worker joined own room", "name", st.workerName, "roomID", st.roomID)
		}
	}
	return nil
}

func (p *Provisioner) ensureWorkerGatewayConsumer(ctx context.Context, st *workerProvisionState) error {
	logger := log.FromContext(ctx)
	logger.Info("creating gateway consumer", "consumer", st.consumerName)
	consumerResult, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          st.consumerName,
		CredentialKey: st.creds.GatewayKey,
	})
	if err != nil {
		return fmt.Errorf("gateway consumer creation failed: %w", err)
	}
	if consumerResult.APIKey != "" && consumerResult.APIKey != st.creds.GatewayKey {
		st.creds.GatewayKey = consumerResult.APIKey
		_ = p.creds.Save(ctx, st.credentialName, st.creds)
	}

	if err := p.gateway.AuthorizeAIRoutes(ctx, st.consumerName, ""); err != nil {
		return fmt.Errorf("AI route authorization failed: %w", err)
	}
	time.Sleep(2 * time.Second)
	return nil
}
