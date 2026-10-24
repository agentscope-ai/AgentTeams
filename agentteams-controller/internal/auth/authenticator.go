package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Role constants.
const (
	RoleAdmin      = "admin"
	RoleManager    = "manager"
	RoleTeamLeader = "team-leader"
	RoleWorker     = "worker"
	// RoleHuman is a human (Human CR permissionLevel 2 or 3) authenticated
	// by Matrix token. It is a read-only viewer scoped to accessibleTeams
	// (L2) or, for worker-scoped humans, to accessibleWorkers (L3, read
	// only) — deliberately NOT RoleTeamLeader, which would grant worker
	// management (create/update/wake) and credential refresh. The L2/L3
	// distinction is carried by AccessibleWorkers (non-empty for L3), not
	// by a separate role value.
	RoleHuman = "human"
)

// DefaultAudience is the SA token audience used by TokenReview when a caller
// does not specify one explicitly.
const DefaultAudience = "agentteams-controller"

const (
	defaultCacheTTL        = 5 * time.Minute
	defaultCacheMax        = 1000
	defaultCleanupInterval = 1 * time.Minute
)

// CallerIdentity represents the authenticated caller.
type CallerIdentity struct {
	Role     string   // admin | manager | team-leader | worker
	Username string   // canonical name (worker name, "manager", or "admin")
	Team     string   // team name (filled by Enricher, empty for standalone)
	Teams    []string // multi-team set for L2 humans (Human CR accessibleTeams); empty for SA-based callers
	// Capabilities are the granted capability values for L2 humans (Human CR
	// capabilities, #1220 §3). Never populated for SA-based callers
	// (admin/manager/leader/worker) — team leaders never hold capabilities
	// (#1220 §5).
	Capabilities []string
	// AccessibleWorkers is the explicit worker assignment for L3
	// (worker-scoped) humans (Human CR accessibleWorkers, #1220 §2/Q2).
	// Only L3 humans carry it — L2 humans and every SA-based identity keep
	// it empty, so every existing team-scope check is unaffected. It grants
	// reads only: mutation paths keep the strict team-scope predicate,
	// which L3 humans (no teams) fail — Q2: L3 is read-only.
	AccessibleWorkers       []string
	WorkerName              string // equals Username when Role is worker or team-leader
	ServiceAccountNamespace string // namespace parsed from TokenReview username
	ServiceAccountName      string // service account parsed from TokenReview username
}

// TeamMatches reports whether the caller can access the given team. For L2
// humans the authoritative set is Teams (Human CR accessibleTeams); for
// SA-based team leaders the legacy single Team field is consulted.
func (c *CallerIdentity) TeamMatches(team string) bool {
	if team == "" {
		return false
	}
	if len(c.Teams) > 0 {
		for _, t := range c.Teams {
			if t == team {
				return true
			}
		}
		return false
	}
	return c.Team == team
}

// WorkerReadable reports whether the caller may READ the given worker:
// either through the team scope (the caller controls the team the worker
// belongs to) or through an explicit worker assignment (L3 humans — the
// worker name appears in AccessibleWorkers, whether the worker is a team
// member or standalone). Mutation paths must not use this: writes stay
// team-scoped (TeamMatches), and L3 humans carry no teams, so they are
// denied there — Q2: L3 is read-only. SA callers and L2 humans carry no
// AccessibleWorkers, so for them this reduces to TeamMatches exactly.
func (c *CallerIdentity) WorkerReadable(team, workerName string) bool {
	for _, w := range c.AccessibleWorkers {
		if w == workerName {
			return true
		}
	}
	return c.TeamMatches(team)
}

// IsWorkerScoped reports whether the caller is an L3 (worker-scoped)
// human — a RoleHuman identity whose access leg is AccessibleWorkers.
// L2 humans (teams + capabilities) and every SA-based identity always
// carry an empty AccessibleWorkers, so this is false for them. Callers
// use it to apply L3-only response handling (e.g. credential sanitization
// on read-only surfaces); it never grants access by itself.
func (c *CallerIdentity) IsWorkerScoped() bool {
	return c.Role == RoleHuman && len(c.AccessibleWorkers) > 0
}

// Authenticator validates a bearer token against the local Kubernetes API and
// returns a basic identity.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*CallerIdentity, error)
}

// TokenReviewAuthenticator validates tokens via the K8s TokenReview API.
//
// Cache bounds: expired entries are swept by StartCleanup at cleanupInterval,
// and inserts past cacheMax trigger an opportunistic expired-sweep followed by
// oldest-expiry eviction. This keeps memory usage proportional to live tokens
// even under adversarial input.
type TokenReviewAuthenticator struct {
	client   kubernetes.Interface
	audience string
	prefix   ResourcePrefix

	cacheMu         sync.RWMutex
	cache           map[[32]byte]cachedResult
	cacheTTL        time.Duration
	cacheMax        int
	cleanupInterval time.Duration
}

type cachedResult struct {
	identity *CallerIdentity
	expiry   time.Time
}

// NewTokenReviewAuthenticator creates an authenticator backed by the K8s
// TokenReview API. audience is the expected token audience (typically
// "agentteams-controller"); prefix is the tenant resource prefix used to parse
// SA usernames back into CallerIdentity.
func NewTokenReviewAuthenticator(client kubernetes.Interface, audience string, prefix ResourcePrefix) *TokenReviewAuthenticator {
	if audience == "" {
		audience = DefaultAudience
	}
	return &TokenReviewAuthenticator{
		client:          client,
		audience:        audience,
		prefix:          prefix.Or(DefaultResourcePrefix),
		cache:           make(map[[32]byte]cachedResult),
		cacheTTL:        defaultCacheTTL,
		cacheMax:        defaultCacheMax,
		cleanupInterval: defaultCleanupInterval,
	}
}

func (a *TokenReviewAuthenticator) Authenticate(ctx context.Context, token string) (*CallerIdentity, error) {
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}

	key := sha256.Sum256([]byte(token))

	if id := a.getFromCache(key); id != nil {
		return id, nil
	}

	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{a.audience},
		},
	}

	var result *authenticationv1.TokenReview
	var err error

	result, err = a.client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("token review request failed: %w", err)
	}

	if !result.Status.Authenticated {
		return nil, fmt.Errorf("token not authenticated: %s", result.Status.Error)
	}

	identity, err := a.prefix.ParseSAUsername(result.Status.User.Username)
	if err != nil {
		return nil, err
	}
	a.putInCache(key, identity)
	return identity, nil
}

func (a *TokenReviewAuthenticator) getFromCache(key [32]byte) *CallerIdentity {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	if entry, ok := a.cache[key]; ok && time.Now().Before(entry.expiry) {
		cp := *entry.identity
		return &cp
	}
	return nil
}

func (a *TokenReviewAuthenticator) putInCache(key [32]byte, identity *CallerIdentity) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if a.cacheMax > 0 && len(a.cache) >= a.cacheMax {
		if a.sweepExpiredLocked(time.Now()) == 0 {
			a.evictOldestLocked()
		}
	}
	cp := *identity
	a.cache[key] = cachedResult{
		identity: &cp,
		expiry:   time.Now().Add(a.cacheTTL),
	}
}

// InvalidateCache removes all cached entries. Useful after SA deletion.
func (a *TokenReviewAuthenticator) InvalidateCache() {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	a.cache = make(map[[32]byte]cachedResult)
}

// SweepExpired removes all entries whose expiry has passed. Returns the number
// of removed entries. Safe to call concurrently with Authenticate.
func (a *TokenReviewAuthenticator) SweepExpired() int {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	return a.sweepExpiredLocked(time.Now())
}

// StartCleanup runs SweepExpired periodically until ctx is cancelled. Intended
// to be launched as a goroutine from app wiring. Returns immediately when the
// cleanup interval is non-positive, leaving cache management purely
// insert-driven.
func (a *TokenReviewAuthenticator) StartCleanup(ctx context.Context) {
	if a.cleanupInterval <= 0 {
		return
	}
	ticker := time.NewTicker(a.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.SweepExpired()
		}
	}
}

// sweepExpiredLocked deletes entries whose expiry is at or before now. Caller
// must hold a.cacheMu for write.
func (a *TokenReviewAuthenticator) sweepExpiredLocked(now time.Time) int {
	n := 0
	for k, v := range a.cache {
		if !now.Before(v.expiry) {
			delete(a.cache, k)
			n++
		}
	}
	return n
}

// evictOldestLocked deletes the entry with the earliest expiry — equivalent to
// LRU here because every insert uses the same TTL, so insertion order matches
// expiry order. Caller must hold a.cacheMu for write.
func (a *TokenReviewAuthenticator) evictOldestLocked() {
	var oldestKey [32]byte
	var oldestExp time.Time
	found := false
	for k, v := range a.cache {
		if !found || v.expiry.Before(oldestExp) {
			oldestKey = k
			oldestExp = v.expiry
			found = true
		}
	}
	if found {
		delete(a.cache, oldestKey)
	}
}
