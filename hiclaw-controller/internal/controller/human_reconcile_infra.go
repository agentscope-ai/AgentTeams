package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// reconcileHumanInfra brings the Matrix account into the desired state.
//
// First-time provisioning (Status.MatrixUserID == ""):
//   - EnsureHumanUser registers the account and returns a generated
//     password + initial access token. We persist Password
//     (Status.InitialPassword) and the full Matrix user ID
//     (Status.MatrixUserID), and seed scope.userToken with the just-
//     issued token so the subsequent rooms phase can /join without an
//     extra Login round-trip.
//
// Steady-state (Status.MatrixUserID != ""):
//   - **Do nothing.** scope.userToken is intentionally left empty; the
//     rooms phase will call ensureUserToken() *only if* it actually has a
//     new room to /join. This is the laziness that prevents device
//     bloat: the reconciler's periodic 5-minute requeue would otherwise
//     Login on every tick, and `POST /_matrix/client/v3/login` without
//     a device_id creates a fresh device session every time (matching
//     the regression Worker/Manager already fixed via the cached
//     WorkerCredentials.MatrixToken path). A Human has no equivalent
//     credential store, so we avoid the call altogether unless needed.
//
// We deliberately never fall back to EnsureHumanUser after the first
// provisioning: its orphan-recovery branch issues
// "!admin users reset-password" and would silently overwrite a password
// the user may have rotated via Element.
func (r *HumanReconciler) reconcileHumanInfra(ctx context.Context, s *humanScope) error {
	h := s.human
	username := s.username
	expectedUserID := r.Provisioner.MatrixUserID(username)

	needsProvision := h.Status.MatrixUserID == "" || h.Status.MatrixUserID != expectedUserID
	if needsProvision {
		creds, err := r.Provisioner.EnsureHumanUser(ctx, username)
		if err != nil {
			return fmt.Errorf("matrix registration failed: %w", err)
		}
		h.Status.MatrixUserID = creds.UserID
		h.Status.InitialPassword = creds.Password
		s.userToken = creds.AccessToken

		log.FromContext(ctx).Info("human created",
			"name", h.Name, "username", username, "matrixUserID", creds.UserID)
	}

	// Sync Matrix profile displayName on first provisioning and when spec changes.
	shouldSyncDisplayName := needsProvision || h.Status.DisplayNameSyncedGeneration != h.Generation
	if shouldSyncDisplayName {
		token := s.userToken
		if token == "" && h.Status.InitialPassword != "" {
			if t, err := r.Provisioner.LoginAsHuman(ctx, username, h.Status.InitialPassword); err == nil {
				token = t
				s.userToken = t
			} else {
				log.FromContext(ctx).Info("human login failed before displayName sync; skipping this cycle",
					"name", h.Name, "username", username, "err", err.Error())
			}
		}
		if token != "" {
			if err := r.Provisioner.SetDisplayName(ctx, h.Status.MatrixUserID, token, h.Spec.DisplayName); err != nil {
				log.FromContext(ctx).Error(err, "failed to sync human displayName (non-fatal)",
					"name", h.Name, "username", username)
			} else {
				h.Status.DisplayNameSyncedGeneration = h.Generation
			}
		}
	}

	return nil
}
