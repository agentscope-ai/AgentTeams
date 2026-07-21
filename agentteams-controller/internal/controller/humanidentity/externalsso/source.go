package externalsso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const matrixLocalpartHashBytes = 16

type source struct {
	deps humanidentity.Deps
}

func init() {
	humanidentity.Register(humanidentity.KeyExternalSSO, func(deps humanidentity.Deps) humanidentity.IdentitySource {
		return source{deps: deps}
	})
}

func (s source) Key() string {
	return humanidentity.KeyExternalSSO
}

func (s source) DeriveMatrixUserID(spec *v1beta1.HumanSpec, _ string) (string, error) {
	localpart, err := s.matrixLocalpart(spec)
	if err != nil {
		return "", err
	}
	return s.deps.Provisioner.MatrixUserID(localpart), nil
}

func (s source) EnsurePrecreated(ctx context.Context, spec *v1beta1.HumanSpec, metadataName string) (humanidentity.Credentials, error) {
	logger := log.FromContext(ctx).WithValues("identitySource", humanidentity.KeyExternalSSO, "human", metadataName)

	if !s.deps.Provisioner.MatrixAppServiceEnabled() {
		logger.Error(nil, "cannot create Matrix account for SSO human: Matrix AppService mode is disabled (set AGENTTEAMS_MATRIX_APPSERVICE_ENABLED)")
		return humanidentity.Credentials{}, fmt.Errorf("external_sso requires AppService mode")
	}

	localpart, err := s.matrixLocalpart(spec)
	if err != nil {
		logger.Error(err, "failed to derive Matrix localpart from identitySource (issuer/subject)")
		return humanidentity.Credentials{}, err
	}
	expectedUserID, err := s.DeriveMatrixUserID(spec, metadataName)
	if err != nil {
		logger.Error(err, "failed to derive Matrix user ID from identitySource")
		return humanidentity.Credentials{}, err
	}

	logger.Info("creating Matrix account for SSO human via AppService register",
		"issuer", spec.IdentitySource.Issuer,
		"subject", spec.IdentitySource.Subject,
		"matrixLocalpart", localpart,
		"matrixUserID", expectedUserID)

	creds, err := s.deps.Provisioner.RegisterAppServiceUser(ctx, localpart)
	if err != nil {
		logger.Error(err, "AppService registration failed for SSO human",
			"matrixLocalpart", localpart, "matrixUserID", expectedUserID)
		return humanidentity.Credentials{}, err
	}

	logger.Info("Matrix account ready for SSO human",
		"matrixUserID", expectedUserID,
		"registeredUserID", creds.UserID,
		"created", creds.Created,
		"hasAccessToken", creds.AccessToken != "")

	return humanidentity.Credentials{
		UserID:      expectedUserID,
		AccessToken: creds.AccessToken,
		Password:    "",
		Created:     creds.Created,
	}, nil
}

func (s source) ManagesInitialPassword() bool {
	return false
}

func (s source) EnsureUserToken(ctx context.Context, spec *v1beta1.HumanSpec, _ *v1beta1.HumanStatus, _ string) (string, error) {
	if !s.deps.Provisioner.MatrixAppServiceEnabled() {
		return "", fmt.Errorf("external_sso requires AppService mode")
	}
	localpart, err := s.matrixLocalpart(spec)
	if err != nil {
		return "", err
	}
	return s.deps.Provisioner.LoginAppServiceUser(ctx, localpart)
}

func (s source) EnsureDeactivated(ctx context.Context, spec *v1beta1.HumanSpec, status *v1beta1.HumanStatus) error {
	userID := status.MatrixUserID
	if userID == "" {
		derived, err := s.DeriveMatrixUserID(spec, "")
		if err != nil {
			return err
		}
		userID = derived
	}
	return s.deps.Provisioner.DeactivateHumanUser(ctx, userID)
}

func (s source) matrixLocalpart(spec *v1beta1.HumanSpec) (string, error) {
	if spec.IdentitySource == nil {
		return "", fmt.Errorf("identitySource is required for external_sso")
	}
	issuer := spec.IdentitySource.Issuer
	subject := spec.IdentitySource.Subject
	if issuer == "" {
		return "", fmt.Errorf("identitySource.issuer must not be empty")
	}
	if subject == "" {
		return "", fmt.Errorf("identitySource.subject must not be empty")
	}
	digest := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(digest[:matrixLocalpartHashBytes]), nil
}
