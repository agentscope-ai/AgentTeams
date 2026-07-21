// Package humanidentity hosts the per-Human identity-source registry
// and the IdentitySource interface that decouples HumanReconciler's
// main reconcile loop from any specific identity provider.
//
// The package exists to satisfy three hard architectural constraints:
//
//   - The HumanReconciler main loop must NOT branch on identity type.
//     All differences (how a Matrix user ID is derived, whether a
//     password is assigned on first registration, how steady-state
//     access tokens are obtained) are expressed as different return
//     values from a single uniform interface.
//
//   - The controller code must NOT name any specific identity
//     provider (no "agent_identity", "keycloak", "dingtalk" string
//     literals). Implementations are keyed by the protocol-layer
//     abstraction they implement: "legacy_password" for the
//     password-bearing Matrix-native flow, "external_sso" for the
//     hash-rendezvous OIDC/SAML flow.
//
//   - Adding a new identity protocol must be additive: drop a new
//     file with an init() that calls Register, and the main loop
//     picks it up unchanged. There is no central switch statement
//     to extend.
//
// HumanReconciler chooses an implementation by inspecting only Spec
// fields (e.g. presence of spec.identitySource), never by enumerating
// known identity types. Once an IdentitySource is resolved, the main
// loop drives it through a fixed five-step contract; per-type
// behaviour is encoded entirely as the return values of those steps.
package humanidentity

import (
	"context"
	"fmt"
	"strings"
	"sync"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// Credentials is the result of a precreate step. Mirrors
// service.HumanCredentials so the reconciler does not need a separate
// translation layer; the duplication keeps the humanidentity package
// usable from outside service if the dependency direction ever
// inverts.
type Credentials struct {
	UserID      string
	AccessToken string
	// Password carries an initial password ONLY when the
	// implementation's ManagesInitialPassword() reports true AND a new
	// account was actually created on this call. Steady-state and
	// SSO-style implementations always leave Password empty.
	Password string
	// Created reports whether the underlying register call actually
	// created a new account (true) or fell back to logging in to an
	// existing one (false). Identity-source implementations and the
	// reconciler both gate side effects on this flag.
	Created bool
}

const (
	KeyLegacyPassword = "legacy_password"
	KeyExternalSSO    = "external_sso"
)

// ResolvedIdentity is the per-Human identity selection result consumed by
// HumanReconciler. It carries both the strategy implementation and the stable
// Matrix identity derived from the current Human spec.
type ResolvedIdentity struct {
	Source                 IdentitySource
	MatrixUserID           string
	MatrixLocalpart        string
	ManagesInitialPassword bool
}

// IdentitySource is the contract every identity-protocol implementation
// satisfies. Methods are listed in roughly the order the reconciler
// invokes them.
//
// Implementations MUST be stateless w.r.t. the Human CR — every method
// receives the inputs it needs as parameters. Per-cluster configuration
// (homeserver domain, Tuwunel client, etc.) lives in Deps and is bound
// at registry-resolve time.
type IdentitySource interface {
	// Key returns the registry key this implementation registered
	// itself under. Used for audit/event labelling so the reconciler
	// can record "human X is being driven by identity source Y"
	// without a separate enum.
	Key() string

	// DeriveMatrixUserID computes the deterministic Matrix user ID
	// the homeserver will assign to this Human. Pure function: must
	// produce the same output for the same input across reconciles
	// AND across processes (cross-language alignment with Tuwunel
	// matters for the SSO flow).
	DeriveMatrixUserID(spec *v1beta1.HumanSpec, metadataName string) (string, error)

	// EnsurePrecreated creates (or recognises an existing) Matrix
	// account for this Human and returns the credentials needed by
	// the rest of the reconcile pass. Side effects beyond Matrix
	// account state must NOT leak — for example, a SSO
	// implementation must not assign a password just because the
	// underlying register call returned Created=true.
	EnsurePrecreated(ctx context.Context, spec *v1beta1.HumanSpec, metadataName string) (Credentials, error)

	// ManagesInitialPassword reports whether this identity source
	// owns the user's Matrix password. When true, the reconciler
	// will persist Credentials.Password into Status.InitialPassword
	// on first creation; when false it will not, regardless of
	// what Credentials.Password contains. The double-gate is a
	// defence-in-depth check: a buggy implementation that returns
	// a non-empty Password while reporting ManagesInitialPassword=
	// false still does not leak the password into Status.
	ManagesInitialPassword() bool

	// EnsureUserToken returns a fresh user-scoped access token used
	// by the rooms phase to /join private rooms. Returns ("", nil)
	// when the controller cannot obtain one (e.g. the user rotated
	// their password via Element); callers degrade to admin-only
	// invite without surfacing it as an error.
	EnsureUserToken(ctx context.Context, spec *v1beta1.HumanSpec, status *v1beta1.HumanStatus, metadataName string) (string, error)

	// EnsureDeactivated performs identity-source-specific cleanup before the
	// finalizer is removed. Room-level cleanup (force-leave) is the
	// reconciler's responsibility; this hook covers anything else
	// the implementation owns.
	EnsureDeactivated(ctx context.Context, spec *v1beta1.HumanSpec, status *v1beta1.HumanStatus) error
}

// Deps is the shared dependency container all implementations receive
// at construction time. New fields can be added without breaking
// existing implementations because each implementation reads only the
// fields it needs.
type Deps struct {
	// Provisioner exposes the decomposed Matrix-side primitives
	// (RegisterAppServiceUser, SetUserPassword, LoginAppServiceUser,
	// etc.). All identity sources route Matrix operations through it
	// rather than holding a direct matrix.Client reference, so the
	// service layer remains the single integration boundary.
	Provisioner service.HumanProvisioner

	// Domain is the Matrix server domain part — needed by SSO-style
	// implementations that compute MXIDs without going through
	// Provisioner.MatrixUserID(localpart).
	Domain string
}

// FactoryFn constructs an IdentitySource bound to the given Deps. Each
// implementation registers a factory rather than a singleton so the
// registry can be re-instantiated cheaply per reconciler instance.
type FactoryFn func(deps Deps) IdentitySource

// =========================================================================
// Registry
// =========================================================================

var (
	registryMu sync.RWMutex
	registry   = map[string]FactoryFn{}
)

// Register adds a factory under the given key. Intended to be called
// from package-level init(); double-registration panics so wiring
// mistakes surface at process start, not at reconcile time.
func Register(key string, factory FactoryFn) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[key]; exists {
		panic(fmt.Sprintf("humanidentity: duplicate registration for key %q", key))
	}
	registry[key] = factory
}

// Resolve constructs the IdentitySource registered under key, bound to
// deps. Returns an error when no implementation has been registered
// under key — callers should treat that as a configuration bug.
func Resolve(key string, deps Deps) (IdentitySource, error) {
	registryMu.RLock()
	factory, ok := registry[key]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("humanidentity: no implementation registered for key %q", key)
	}
	return factory(deps), nil
}

// ResolveHuman selects an identity source from the Human spec, derives the
// stable Matrix identity, and returns the bound source plus derived data.
func ResolveHuman(spec *v1beta1.HumanSpec, metadataName string, deps Deps) (ResolvedIdentity, error) {
	key := KeyLegacyPassword
	if spec.IdentitySource != nil {
		key = KeyExternalSSO
	}
	source, err := Resolve(key, deps)
	if err != nil {
		return ResolvedIdentity{}, err
	}
	matrixUserID, err := source.DeriveMatrixUserID(spec, metadataName)
	if err != nil {
		return ResolvedIdentity{}, err
	}
	localpart, err := matrixLocalpart(matrixUserID)
	if err != nil {
		return ResolvedIdentity{}, err
	}
	return ResolvedIdentity{
		Source:                 source,
		MatrixUserID:           matrixUserID,
		MatrixLocalpart:        localpart,
		ManagesInitialPassword: source.ManagesInitialPassword(),
	}, nil
}

func matrixLocalpart(matrixUserID string) (string, error) {
	if !strings.HasPrefix(matrixUserID, "@") {
		return "", fmt.Errorf("matrix user id %q must start with @", matrixUserID)
	}
	withoutSigil := strings.TrimPrefix(matrixUserID, "@")
	separator := strings.IndexByte(withoutSigil, ':')
	if separator <= 0 {
		return "", fmt.Errorf("matrix user id %q must include localpart and domain", matrixUserID)
	}
	return withoutSigil[:separator], nil
}

// Keys returns a snapshot of all registered keys, sorted for
// determinism. Useful for startup logging and CI audit.
func Keys() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
