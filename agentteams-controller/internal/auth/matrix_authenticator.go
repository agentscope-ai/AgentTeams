package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MatrixWhoami validates a Matrix access token and returns the owning user id
// (e.g. "@alice:matrix.local"). Implemented by the tuwunel client.
type MatrixWhoami interface {
	Whoami(ctx context.Context, accessToken string) (userID string, err error)
}

// MatrixTokenAuthenticator authenticates a bearer token as a Matrix user and
// resolves it to an L2 human identity via the Human CR.
//
// Chain (direction A2): the caller presents a Matrix access token obtained by
// logging into the homeserver; we validate it with whoami, map the localpart
// to a Human CR (username / metadata.name), and build a read-only human
// identity whose Teams set is the Human's accessibleTeams. This lets an L2
// user view every team they control with a single token — no per-team SA
// switching.
//
// The identity role is RoleHuman (read-only), NOT RoleTeamLeader: team leaders
// are coordinating workers and may manage their team's workers; an L2 human is
// a viewer and must not get worker-management or credential-refresh powers.
//
// Human permission levels: 2 = Team (L2 — teams + capabilities scope) and
// 3 = Worker (L3 — read-only scope over the explicitly assigned
// accessibleWorkers, #1220 §2/Q2). Level 1 (admin) humans are not resolved
// here; they should use the admin SA.
type MatrixTokenAuthenticator struct {
	k8s       client.Client
	namespace string
	whoami    MatrixWhoami

	cacheMu         sync.Mutex
	cache           map[[32]byte]cachedIdentity
	cacheTTL        time.Duration
	cleanupInterval time.Duration
}

type cachedIdentity struct {
	identity *CallerIdentity
	expiry   time.Time
}

// NewMatrixTokenAuthenticator builds an authenticator backed by Matrix whoami
// and the Human CR.
func NewMatrixTokenAuthenticator(k8s client.Client, namespace string, whoami MatrixWhoami) *MatrixTokenAuthenticator {
	return &MatrixTokenAuthenticator{
		k8s:             k8s,
		namespace:       namespace,
		whoami:          whoami,
		cache:           make(map[[32]byte]cachedIdentity),
		cacheTTL:        5 * time.Minute,
		cleanupInterval: time.Minute,
	}
}

// Authenticate validates the token via whoami and resolves the Human CR.
func (a *MatrixTokenAuthenticator) Authenticate(ctx context.Context, token string) (*CallerIdentity, error) {
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}

	key := sha256.Sum256([]byte(token))
	if id := a.getFromCache(key); id != nil {
		return id, nil
	}

	userID, err := a.whoami.Whoami(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("matrix whoami: %w", err)
	}
	if localpartFromUserID(userID) == "" {
		return nil, fmt.Errorf("matrix whoami returned invalid user id %q", userID)
	}

	identity, err := a.resolveHuman(ctx, userID)
	if err != nil {
		return nil, err
	}

	a.putToCache(key, identity)
	return identity, nil
}

// resolveHuman looks up the Human CR whose Matrix identity matches the token
// owner and builds the L2 team-leader identity.
//
// Matching is by the authoritative Matrix user id when the Human has been
// reconciled (Status.MatrixUserID — the id derived by the identity source,
// which for external_sso is a deterministic hash of issuer+subject and may
// differ from spec.username). Humans that have not been reconciled yet (no
// Status.MatrixUserID) fall back to the effective-username localpart, which
// matches the legacy_password identity source.
func (a *MatrixTokenAuthenticator) resolveHuman(ctx context.Context, userID string) (*CallerIdentity, error) {
	localpart := localpartFromUserID(userID)
	var humans v1beta1.HumanList
	if err := a.k8s.List(ctx, &humans, client.InNamespace(a.namespace)); err != nil {
		return nil, fmt.Errorf("list humans: %w", err)
	}
	for i := range humans.Items {
		h := &humans.Items[i]
		if h.Status.MatrixUserID != "" {
			if h.Status.MatrixUserID != userID {
				continue
			}
		} else if localpart == "" || h.Spec.EffectiveUsername(h.Name) != localpart {
			continue
		}
		switch h.Spec.PermissionLevel {
		case 2:
			teams := make([]string, len(h.Spec.AccessibleTeams))
			copy(teams, h.Spec.AccessibleTeams)
			// Capabilities are granted on the Human CR (#1220 §3). Only L2
			// humans carry them — level-3 humans are rejected to their own
			// branch below, and SA-based identities never pass through here
			// (#1220 §5: team leaders never hold capabilities).
			caps := make([]string, len(h.Spec.Capabilities))
			copy(caps, h.Spec.Capabilities)
			return &CallerIdentity{
				Role:         RoleHuman,
				Username:     h.Name,
				Teams:        teams,
				Capabilities: caps,
			}, nil
		case 3:
			// L3 (worker-scoped, #1220 §2/Q2): read-only access to exactly
			// the assigned workers. Teams and capabilities are L2 fields
			// and stay empty for L3 identities, whatever the CR carries —
			// the level is the discriminator (low-privilege item E: no new
			// Role value, AccessibleWorkers non-empty marks the L3 read
			// leg).
			workers := make([]string, len(h.Spec.AccessibleWorkers))
			copy(workers, h.Spec.AccessibleWorkers)
			return &CallerIdentity{
				Role:              RoleHuman,
				Username:          h.Name,
				AccessibleWorkers: workers,
			}, nil
		default:
			return nil, fmt.Errorf("human %q has unsupported permissionLevel %d (2=team, 3=worker)", localpart, h.Spec.PermissionLevel)
		}
	}
	return nil, fmt.Errorf("no human with a supported permission level (2=team, 3=worker) matches matrix user %q", userID)
}

func (a *MatrixTokenAuthenticator) getFromCache(key [32]byte) *CallerIdentity {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	now := time.Now()
	if a.cleanupInterval > 0 && len(a.cache) > 0 {
		// Opportunistic sweep of expired entries to bound memory.
		for k, v := range a.cache {
			if now.After(v.expiry) {
				delete(a.cache, k)
			}
		}
	}
	if v, ok := a.cache[key]; ok && now.Before(v.expiry) {
		return v.identity
	}
	return nil
}

func (a *MatrixTokenAuthenticator) putToCache(key [32]byte, identity *CallerIdentity) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	a.cache[key] = cachedIdentity{identity: identity, expiry: time.Now().Add(a.cacheTTL)}
}

// localpartFromUserID extracts the localpart from a Matrix user id
// ("@alice:matrix.local" → "alice").
func localpartFromUserID(userID string) string {
	if !strings.HasPrefix(userID, "@") {
		return ""
	}
	rest := userID[1:]
	if idx := strings.IndexByte(rest, ':'); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// CompositeAuthenticator tries a chain of authenticators in order and returns
// the first successful identity. This is how A2 coexists with the existing SA
// token path: SA TokenReview first, then Matrix whoami → Human.
type CompositeAuthenticator struct {
	authenticators []Authenticator
}

// NewCompositeAuthenticator builds a fallback authenticator chain.
func NewCompositeAuthenticator(authenticators ...Authenticator) *CompositeAuthenticator {
	return &CompositeAuthenticator{authenticators: authenticators}
}

// Authenticate tries each underlying authenticator until one succeeds.
func (c *CompositeAuthenticator) Authenticate(ctx context.Context, token string) (*CallerIdentity, error) {
	var lastErr error
	for _, a := range c.authenticators {
		if a == nil {
			continue
		}
		id, err := a.Authenticate(ctx, token)
		if err == nil {
			return id, nil
		}
		lastErr = err
		log.Printf("[AUTH] authenticator %T failed: %v", a, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no authenticators configured")
	}
	return nil, fmt.Errorf("token not authenticated: %w", lastErr)
}
