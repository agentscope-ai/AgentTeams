package service

import (
	"context"
	"fmt"

	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// =========================================================================
// Decomposed primitives — five explicit one-action calls
// =========================================================================
//
// The original EnsureHumanUser / LoginAsHuman composites bundled
// "register + set password" and "AS-or-password login" into single
// black boxes. That coupling made it impossible to express
// per-identity-type behaviour (a SSO Human must register without ever
// being assigned a password, for example) without growing if/else
// branches inside the composite. The decomposition below splits each
// composite into the smallest semantic unit so callers — both legacy
// reconcile paths and future identity-source implementations — can
// pick exactly the steps they need.
//
// All five methods are pure adapters over internal/matrix; the
// decision about *whether* to invoke a given step lives at the call
// site.

// RegisterAppServiceUser performs a single AS-register call. When the
// account already exists (M_USER_IN_USE) the underlying client falls
// back to LoginAppServiceUser and reports Created=false. The returned
// HumanCredentials carries an empty Password — the AS protocol does
// not assign one and callers that want password login must follow up
// with SetUserPassword explicitly.
func (p *Provisioner) RegisterAppServiceUser(ctx context.Context, username string) (*HumanCredentials, error) {
	uc, err := p.matrix.EnsureAppServiceUser(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("AS register human %s: %w", username, err)
	}
	return &HumanCredentials{
		UserID:      uc.UserID,
		AccessToken: uc.AccessToken,
		Password:    "",
		Created:     uc.Created,
	}, nil
}

// RegisterLegacyUser performs a single registration_token-based
// register; on M_USER_IN_USE the underlying client falls through to
// orphan-recovery (admin reset-password + login). The returned
// HumanCredentials always carries a Password since legacy auth has no
// AS bypass.
func (p *Provisioner) RegisterLegacyUser(ctx context.Context, username string) (*HumanCredentials, error) {
	uc, err := p.matrix.EnsureUser(ctx, matrix.EnsureUserRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("register legacy human %s: %w", username, err)
	}
	return &HumanCredentials{
		UserID:      uc.UserID,
		AccessToken: uc.AccessToken,
		Password:    uc.Password,
		Created:     uc.Created,
	}, nil
}

// SetUserPassword writes a password for an existing Matrix account via
// the admin bot. Best-effort — admin command delivery is confirmed but
// the bot itself executes the reset asynchronously. Callers that must
// confirm propagation are expected to test by attempting a login
// afterwards.
func (p *Provisioner) SetUserPassword(ctx context.Context, userID, password string) error {
	return p.matrix.SetPasswordAsAdmin(ctx, userID, password)
}

// LoginAppServiceUser obtains a fresh access token via the AS login
// flow (no password required). Used by both legacy_password and
// external_sso identity sources when the controller runs in AS mode.
func (p *Provisioner) LoginAppServiceUser(ctx context.Context, username string) (string, error) {
	return p.matrix.LoginAppServiceUser(ctx, username)
}

// LoginWithPassword obtains a fresh access token via the password
// login flow. Used by legacy_password when AS mode is disabled and
// the controller has the user's stored InitialPassword.
func (p *Provisioner) LoginWithPassword(ctx context.Context, username, password string) (string, error) {
	return p.matrix.Login(ctx, username, password)
}

// =========================================================================
// Composite wrappers retained for incremental migration
// =========================================================================
//
// EnsureHumanUser and LoginAsHuman remain as backward-compatible
// shims over the new primitives. In-tree callers that need
// per-identity-type behaviour migrate to the primitives directly via
// the humanidentity registry (see internal/controller/humanidentity).
// The wrappers are kept so the WorkerProvisioner / HumanProvisioner
// interface contracts stay stable for the team-admin login path and
// the existing mock-driven tests.
//
// IMPORTANT (P0-2 legacy fix): the AS branch below now calls
// SetUserPassword **only** when RegisterAppServiceUser actually
// created a new account. The previous implementation reset the
// password on every reconcile that hit this method, which would
// silently overwrite any password the user had rotated via Element
// the moment the controller decided to "re-provision".

// EnsureHumanUser registers (or logs in) a Matrix account for a Human CR.
// See HumanProvisioner.EnsureHumanUser for the contract around when this
// must be called. This implementation now routes through the explicit
// register / set-password primitives so the "set password" side effect
// is only triggered on first creation.
func (p *Provisioner) EnsureHumanUser(ctx context.Context, username string) (*HumanCredentials, error) {
	if p.MatrixAppServiceEnabled() {
		creds, err := p.RegisterAppServiceUser(ctx, username)
		if err != nil {
			return nil, fmt.Errorf("ensure human AS user %s: %w", username, err)
		}
		// Only assign an initial password on first registration. When
		// the account already existed (Created=false) we return the
		// AS-issued token without resetting whatever password the user
		// may have rotated via Element.
		if creds.Created {
			password, err := matrix.GeneratePassword(16)
			if err != nil {
				return nil, fmt.Errorf("generate human password: %w", err)
			}
			if err := p.SetUserPassword(ctx, creds.UserID, password); err != nil {
				return nil, fmt.Errorf("set human password via admin: %w", err)
			}
			creds.Password = password
		}
		return creds, nil
	}

	// Legacy path
	creds, err := p.RegisterLegacyUser(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("ensure human matrix user %s: %w", username, err)
	}
	return creds, nil
}

// LoginAsHuman obtains a fresh access token for an already-provisioned
// Human without touching their password. This is the steady-state path
// the reconciler uses once Status.MatrixUserID is non-empty; it must NOT
// fall back to EnsureUser on failure because EnsureUser's orphan-recovery
// branch issues "!admin users reset-password", which would silently
// overwrite any password the user changed via Element.
func (p *Provisioner) LoginAsHuman(ctx context.Context, username, password string) (string, error) {
	if p.MatrixAppServiceEnabled() {
		return p.LoginAppServiceUser(ctx, username)
	}
	return p.LoginWithPassword(ctx, username, password)
}

// =========================================================================
// Other Matrix-side operations Humans need (unchanged)
// =========================================================================

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

// DeactivateHumanUser disables a Matrix account through the Tuwunel admin bot.
// Tuwunel owns the exact deactivate/revoke semantics; the controller treats a
// successful command delivery as the offboard handoff point.
func (p *Provisioner) DeactivateHumanUser(ctx context.Context, userID string) error {
	cmd := fmt.Sprintf("!admin users deactivate %s", userID)
	log.FromContext(ctx).Info("sending tuwunel human deactivate admin command", "user", userID, "command", cmd)
	return p.matrix.AdminCommand(ctx, cmd)
}
