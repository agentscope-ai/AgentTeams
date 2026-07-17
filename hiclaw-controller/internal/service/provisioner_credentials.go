package service

import (
	"context"
	"fmt"

	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (p *Provisioner) ensureMatrixToken(ctx context.Context, matrixUsername string, creds *WorkerCredentials) (string, error) {
	// Always reuse cached token. Re-login invalidates the old token on
	// Tuwunel, breaking running Workers. On-demand refresh is available
	// via POST /api/v1/credentials/matrix-token for 401 recovery.
	if creds.MatrixToken != "" {
		return creds.MatrixToken, nil
	}
	var tok string
	var err error
	if p.MatrixAppServiceEnabled() {
		tok, err = p.matrix.LoginAppServiceUser(ctx, matrixUsername)
	} else {
		tok, err = p.matrix.Login(ctx, matrixUsername, creds.MatrixPassword)
	}
	if err != nil {
		return "", err
	}
	creds.MatrixToken = tok
	return tok, nil
}

// ForceRefreshMatrixToken issues a fresh Matrix access token for the given
// worker/manager, bypassing the cache. Called when the caller reports a 401
// from the homeserver. Persists the new token to the credential store.
func (p *Provisioner) ForceRefreshMatrixToken(ctx context.Context, name string) (*RefreshResult, error) {
	creds, err := p.creds.Load(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("load credentials for %s: %w", name, err)
	}
	if creds == nil {
		return nil, fmt.Errorf("no credentials found for %s", name)
	}

	// Clear cached token to force re-login
	creds.MatrixToken = ""

	var tok string
	if p.MatrixAppServiceEnabled() {
		tok, err = p.matrix.LoginAppServiceUser(ctx, name)
	} else {
		tok, err = p.matrix.Login(ctx, name, creds.MatrixPassword)
	}
	if err != nil {
		return nil, fmt.Errorf("re-login for %s: %w", name, err)
	}

	creds.MatrixToken = tok
	if saveErr := p.creds.Save(ctx, name, creds); saveErr != nil {
		// Non-fatal: token is valid even if persistence fails
		log.FromContext(ctx).Error(saveErr, "failed to persist refreshed matrix token", "name", name)
	}

	return &RefreshResult{MatrixToken: tok}, nil
}

// RefreshCredentials loads persisted credentials and obtains a Matrix token,
// reusing the cached token when present. Used during update operations.
func (p *Provisioner) RefreshCredentials(ctx context.Context, workerName string) (*RefreshResult, error) {
	return p.RefreshWorkerCredentials(ctx, workerName, workerName, "")
}

// RefreshWorkerCredentials loads worker credentials by their owning CR key while
// refreshing the Matrix token for the runtime worker identity.
func (p *Provisioner) RefreshWorkerCredentials(ctx context.Context, credentialName, workerName, teamName string) (*RefreshResult, error) {
	if credentialName == "" {
		credentialName = workerName
	}
	creds, err := p.loadWorkerCredentials(ctx, credentialName, workerName)
	if err != nil || creds == nil {
		return nil, fmt.Errorf("credentials not found for %s", credentialName)
	}

	hadToken := creds.MatrixToken != ""
	matrixToken, err := p.ensureMatrixToken(ctx, workerName, creds)
	if err != nil {
		return nil, fmt.Errorf("Matrix login failed: %w", err)
	}
	if !hadToken {
		if err := p.creds.Save(ctx, credentialName, creds); err != nil {
			return nil, fmt.Errorf("persist matrix token: %w", err)
		}
	}
	if p.ossAdmin != nil {
		if err := p.ossAdmin.EnsureUser(ctx, workerName, creds.MinIOPassword); err != nil {
			return nil, fmt.Errorf("MinIO user refresh failed: %w", err)
		}
		if err := p.ossAdmin.EnsurePolicy(ctx, oss.PolicyRequest{
			WorkerName: workerName,
			TeamName:   teamName,
		}); err != nil {
			return nil, fmt.Errorf("MinIO policy refresh failed: %w", err)
		}
	}

	return &RefreshResult{
		MatrixToken:    matrixToken,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}
func (p *Provisioner) loadWorkerCredentials(ctx context.Context, credentialName, workerName string) (*WorkerCredentials, error) {
	creds, err := p.creds.Load(ctx, credentialName)
	if err != nil || creds != nil || credentialName == workerName {
		return creds, err
	}

	legacyCreds, err := p.creds.Load(ctx, workerName)
	if err != nil || legacyCreds == nil {
		return legacyCreds, err
	}
	if err := p.creds.Save(ctx, credentialName, legacyCreds); err != nil {
		return nil, fmt.Errorf("migrate legacy worker credentials: %w", err)
	}
	return legacyCreds, nil
}

// RefreshManagerCredentials loads persisted credentials for the Manager and
// returns a Matrix access token, reusing the cached token when present.
func (p *Provisioner) RefreshManagerCredentials(ctx context.Context, managerName string) (*RefreshResult, error) {
	creds, err := p.creds.Load(ctx, managerName)
	if err != nil || creds == nil {
		return nil, fmt.Errorf("credentials not found for manager %s", managerName)
	}

	hadToken := creds.MatrixToken != ""
	matrixToken, err := p.ensureMatrixToken(ctx, "manager", creds)
	if err != nil {
		return nil, fmt.Errorf("Matrix login failed: %w", err)
	}
	if !hadToken {
		if err := p.creds.Save(ctx, managerName, creds); err != nil {
			return nil, fmt.Errorf("persist matrix token: %w", err)
		}
	}

	return &RefreshResult{
		MatrixToken:    matrixToken,
		GatewayKey:     creds.GatewayKey,
		MinIOPassword:  creds.MinIOPassword,
		MatrixPassword: creds.MatrixPassword,
	}, nil
}

// EnsureManagerGatewayAuth ensures the Manager's gateway consumer exists and is
// authorized on AI routes. Called during container recreation to restore auth
// that may have been lost (e.g. after upgrade with fresh Higress state).
func (p *Provisioner) EnsureManagerGatewayAuth(ctx context.Context, managerName, gatewayKey string) error {
	consumerName := "manager"
	_, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: gatewayKey,
	})
	if err != nil {
		return fmt.Errorf("ensure consumer: %w", err)
	}
	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		return fmt.Errorf("authorize AI routes: %w", err)
	}
	return nil
}

// EnsureWorkerGatewayAuth ensures the Worker's gateway consumer exists and is
// authorized on AI routes. Called during controller restart / member reconcile
// to defensively restore auth that may have been lost (e.g. if the Higress
// route was rewritten, or after upgrade with fresh Higress state). Mirrors
// EnsureManagerGatewayAuth but uses the worker-scoped consumer name.
func (p *Provisioner) EnsureWorkerGatewayAuth(ctx context.Context, workerName, gatewayKey string) error {
	consumerName := "worker-" + workerName
	_, err := p.gateway.EnsureConsumer(ctx, gateway.ConsumerRequest{
		Name:          consumerName,
		CredentialKey: gatewayKey,
	})
	if err != nil {
		return fmt.Errorf("ensure consumer: %w", err)
	}
	if err := p.gateway.AuthorizeAIRoutes(ctx, consumerName, ""); err != nil {
		return fmt.Errorf("authorize AI routes: %w", err)
	}
	return nil
}

// ProvisionTeamRooms creates (or resolves) the team room and leader DM room
// and reconciles their Matrix memberships against the desired member set.
// Idempotency is guaranteed by the Matrix alias: repeated calls always land
// on the same RoomID regardless of K8s informer cache state, so no
// "existing room ID" inputs are threaded through. Membership is reconciled
// unconditionally on every call so newly-added workers are invited and
// removed workers are kicked.
func (p *Provisioner) DeleteCredentials(ctx context.Context, workerName string) error {
	return p.DeleteWorkerCredentials(ctx, workerName)
}

// DeleteWorkerCredentials removes persisted credentials for a worker-like CR.
func (p *Provisioner) DeleteWorkerCredentials(ctx context.Context, credentialName string) error {
	return p.creds.Delete(ctx, credentialName)
}

// DeleteTeamRoomAliases removes the room aliases that identify a team's group
// room and the leader DM room so a future Team CR with the same name can
// reclaim the aliases cleanly. Best-effort: alias removal does not affect
// the underlying room, which is intentionally left intact to preserve chat
// history; it only detaches the controller's stable identifier from it.
func (p *Provisioner) CredentialNames(ctx context.Context) ([]string, error) {
	return p.creds.List(ctx)
}

// BackfillLegacyPasswords generates and sets Matrix passwords for workers
// and managers that were created in AppService mode (no password) when the
// controller is switched back to legacy password-based mode. This ensures
// a seamless rollback without manual intervention.
func (p *Provisioner) BackfillLegacyPasswords(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("backfill")

	names, err := p.creds.List(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	if len(names) == 0 {
		return nil
	}

	var firstErr error
	backfilled := 0
	for _, name := range names {
		creds, err := p.creds.Load(ctx, name)
		if err != nil {
			logger.Error(err, "failed to load credentials", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if creds == nil {
			continue
		}
		// Already has a password — nothing to do.
		if creds.MatrixPassword != "" {
			continue
		}

		password, err := matrix.GeneratePassword(16)
		if err != nil {
			logger.Error(err, "failed to generate password", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		userID := p.matrix.UserID(name)
		if err := p.matrix.SetPasswordAsAdmin(ctx, userID, password); err != nil {
			logger.Error(err, "failed to set password via admin", "name", name, "userID", userID)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		creds.MatrixPassword = password
		// Clear cached AS token — it's no longer valid after password reset
		// and legacy mode will obtain a new token via password login.
		creds.MatrixToken = ""
		if err := p.creds.Save(ctx, name, creds); err != nil {
			logger.Error(err, "failed to save backfilled credentials", "name", name)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		backfilled++
		logger.Info("backfilled legacy password", "name", name, "userID", userID)
	}

	if backfilled > 0 {
		logger.Info("legacy password backfill complete", "backfilled", backfilled, "total", len(names))
	}
	return firstErr
}
