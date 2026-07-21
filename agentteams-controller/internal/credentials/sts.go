package credentials

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/accessresolver"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
)

// STSConfig holds configuration for the STS token service.
type STSConfig struct {
	// OSSBucket is the primary workspace bucket name. It is returned
	// to callers as part of STSToken.OSSBucket so they know which
	// bucket the issued token is most relevant to. It is also used
	// by the accessresolver to resolve `bucketRef: workspace` in
	// AccessEntry scopes.
	OSSBucket string

	// OSSEndpoint is the public-facing OSS endpoint returned to worker
	// callers in STSToken.OSSEndpoint. It is sourced from controller
	// static config (AGENTTEAMS_FS_ENDPOINT / storage.oss.endpoint) and NOT
	// from the credential-provider sidecar — endpoint is deployment-time
	// configuration, orthogonal to the short-lived STS triple.
	OSSEndpoint string
}

// STSService issues scoped STS tokens for Worker/Manager callers.
//
// It accepts a CallerIdentity from the HTTP layer, asks the
// accessresolver to turn the caller's CR-declared (or defaulted)
// AccessEntries into a fully-resolved credprovider.IssueRequest, and
// delegates to the credprovider.Client which forwards the request to
// the hiclaw-credential-provider sidecar.
//
// The service is agnostic to how the sidecar actually produces the
// tokens: the production sidecar calls AssumeRoleWithOIDC, the
// mock-credential-provider calls AssumeRole with a long-lived AK/SK,
// but both speak the same HTTP contract.
type STSService struct {
	config   STSConfig
	resolver *accessresolver.Resolver
	provider credprovider.Client
}

// NewSTSService constructs an STSService. When either resolver or
// provider is nil the service is considered unconfigured and
// IssueForCaller will fail; server wiring is expected to translate
// that into HTTP 503.
func NewSTSService(cfg STSConfig, resolver *accessresolver.Resolver, provider credprovider.Client) *STSService {
	return &STSService{config: cfg, resolver: resolver, provider: provider}
}

// Configured reports whether both the accessresolver and the
// credential-provider client have been wired in. Callers can use this
// to gate the exposure of credential-related HTTP endpoints in
// deployments that do not run the sidecar.
func (s *STSService) Configured() bool {
	return s != nil && s.provider != nil && s.resolver != nil
}

// IssueForCaller asks the sidecar for an STS triple whose inline
// policy matches the caller's resolved AccessEntries.
func (s *STSService) IssueForCaller(ctx context.Context, caller *auth.CallerIdentity) (*STSToken, error) {
	if !s.Configured() {
		return nil, fmt.Errorf("STS service not configured: no credential provider URL set")
	}
	callerSessionName, entries, err := s.resolver.ResolveForCaller(ctx, caller)
	if err != nil {
		return nil, fmt.Errorf("resolve access entries: %w", err)
	}
	sessionName := uuid.NewString()
	log.FromContext(ctx).Info("issuing STS token",
		"sessionName", sessionName,
		"callerSessionName", callerSessionName,
		"callerRole", caller.Role,
		"callerUsername", caller.Username,
		"callerTeam", caller.Team,
		"entries", len(entries),
	)
	resp, err := s.provider.Issue(ctx, credprovider.IssueRequest{
		SessionName: sessionName,
		Entries:     entries,
	})
	if err != nil {
		return nil, err
	}
	return &STSToken{
		AccessKeyID:     resp.AccessKeyID,
		AccessKeySecret: resp.AccessKeySecret,
		SecurityToken:   resp.SecurityToken,
		Expiration:      resp.Expiration,
		ExpiresInSec:    resp.ExpiresInSec,
		OSSEndpoint:     s.config.OSSEndpoint,
		OSSBucket:       s.config.OSSBucket,
	}, nil
}
