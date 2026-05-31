package service

import (
	"context"
	"fmt"

	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EnsureHumanUser registers (or logs in) a Matrix account for a Human CR.
// See HumanProvisioner.EnsureHumanUser for the contract around when this
// must be called. This implementation is a thin adapter around
// matrix.Client.EnsureUser — Humans have no persisted WorkerCredentials
// envelope (unlike Workers/Managers), so the caller is responsible for
// recording the returned password in the CR status if needed.
func (p *Provisioner) EnsureHumanUser(ctx context.Context, username string) (*HumanCredentials, error) {
	if p.MatrixAppServiceEnabled() {
		uc, err := p.matrix.EnsureAppServiceUser(ctx, username)
		if err != nil {
			return nil, fmt.Errorf("ensure human AS user %s: %w", username, err)
		}
		// Set an initial password so the human can log in via Element.
		password, err := matrix.GeneratePassword(16)
		if err != nil {
			return nil, fmt.Errorf("generate human password: %w", err)
		}
		if err := p.matrix.SetPasswordAsAdmin(ctx, uc.UserID, password); err != nil {
			return nil, fmt.Errorf("set human password via admin: %w", err)
		}
		return &HumanCredentials{
			UserID:      uc.UserID,
			AccessToken: uc.AccessToken,
			Password:    password,
		}, nil
	}

	// Legacy path
	uc, err := p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("ensure human matrix user %s: %w", username, err)
	}
	return &HumanCredentials{
		UserID:      uc.UserID,
		AccessToken: uc.AccessToken,
		Password:    uc.Password,
	}, nil
}

// LoginAsHuman obtains a fresh access token for an already-provisioned
// Human without touching their password. This is the steady-state path
// the reconciler uses once Status.MatrixUserID is non-empty; it must NOT
// fall back to EnsureUser on failure because EnsureUser's orphan-recovery
// branch issues "!admin users reset-password", which would silently
// overwrite any password the user changed via Element.
func (p *Provisioner) LoginAsHuman(ctx context.Context, username, password string) (string, error) {
	if p.MatrixAppServiceEnabled() {
		return p.matrix.LoginAppServiceUser(ctx, username)
	}
	return p.matrix.Login(ctx, username, password)
}

// SetDisplayName updates the Matrix profile displayname for a human user.
func (p *Provisioner) SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error {
	return p.matrix.SetDisplayName(ctx, userID, accessToken, displayName)
}

// InviteToRoom invites the given Matrix user into roomID using the admin
// access token. Idempotent; see matrix.Client.InviteToRoom.
func (p *Provisioner) InviteToRoom(ctx context.Context, roomID, userID string) error {
	return p.matrix.InviteToRoom(ctx, roomID, userID)
}

// JoinRoomAs joins roomID with the supplied user access token. Required
// for Tuwunel's trusted_private_chat preset (the rooms the controller
// creates), which leaves an invite pending until the invitee explicitly
// /joins — an admin-side invite alone is not sufficient to make the user
// a full member.
func (p *Provisioner) JoinRoomAs(ctx context.Context, roomID, userToken string) error {
	return p.matrix.JoinRoom(ctx, roomID, userToken)
}

// KickFromRoom removes userID from roomID using the admin token. Idempotent.
func (p *Provisioner) KickFromRoom(ctx context.Context, roomID, userID, reason string) error {
	return p.matrix.KickFromRoom(ctx, roomID, userID, reason)
}

// ForceLeaveRoom asks the Tuwunel admin bot to force-leave userID out of
// roomID. Used by the Human delete flow where the controller no longer
// holds a valid user token (password may be stale) and must rely on the
// admin bot instead of /leave. Fire-and-forget at the bot layer.
func (p *Provisioner) ForceLeaveRoom(ctx context.Context, userID, roomID string) error {
	cmd := fmt.Sprintf("!admin users force-leave-room %s %s", userID, roomID)
	log.FromContext(ctx).Info("sending tuwunel force-leave-room admin command", "room", roomID, "user", userID, "command", cmd)
	return p.matrix.AdminCommand(ctx, cmd)
}
