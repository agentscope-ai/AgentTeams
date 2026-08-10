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
	expectedUserID := s.identity.MatrixUserID
	logger := log.FromContext(ctx).WithValues(
		"name", h.Name,
		"identitySource", s.identity.Source.Key(),
		"matrixUserID", expectedUserID,
		"matrixLocalpart", s.identity.MatrixLocalpart,
	)

	needsProvision := h.Status.MatrixUserID == "" ||
		(s.identity.ManagesInitialPassword && h.Status.MatrixUserID != expectedUserID)
	if needsProvision {
		logger.Info("provisioning Matrix account for human",
			"currentStatusMatrixUserID", h.Status.MatrixUserID,
			"managesInitialPassword", s.identity.ManagesInitialPassword)
		creds, err := s.identity.Source.EnsurePrecreated(ctx, &h.Spec, h.Name)
		if err != nil {
			logger.Error(err, "matrix account provisioning failed; status.matrixUserID will stay empty")
			return fmt.Errorf("matrix registration failed: %w", err)
		}
		if creds.UserID != "" && creds.UserID != expectedUserID {
			logger.Error(nil, "matrix registration returned unexpected user id",
				"registeredUserID", creds.UserID)
			return fmt.Errorf("matrix registration returned %s, want %s", creds.UserID, expectedUserID)
		}
		h.Status.MatrixUserID = expectedUserID
		if s.identity.ManagesInitialPassword {
			h.Status.InitialPassword = creds.Password
		} else {
			h.Status.InitialPassword = ""
		}
		s.userToken = creds.AccessToken

		logger.Info("human Matrix account provisioned; status.matrixUserID set",
			"username", username,
			"created", creds.Created,
			"hasAccessToken", creds.AccessToken != "")
	} else if !s.identity.ManagesInitialPassword {
		h.Status.InitialPassword = ""
	}

	// Sync Matrix profile displayName on first provisioning and when spec changes.
	shouldSyncDisplayName := needsProvision || h.Status.DisplayNameSyncedGeneration != h.Generation
	if shouldSyncDisplayName {
		token := r.ensureUserToken(ctx, s)
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
