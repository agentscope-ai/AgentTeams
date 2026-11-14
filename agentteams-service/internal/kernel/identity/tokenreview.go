// Token review: the service's per-request identity resolver.
//
// The public bearer token is unchanged from the live deployment (same issuer,
// same credential chain). The service cannot validate it itself in R1, so it
// asks the controller over POST /internal/v1/token-review and memoizes the
// answer for the frozen 60s TTL. Revocation therefore takes effect within one
// TTL, which is the R1 accepted staleness bound; R2 replaces the exchange once
// humans live in the service DB.
package identity

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
)

// DefaultCacheTTL is the frozen 60s review cache TTL, taken from the
// single source of frozen values (kernel/config).
const DefaultCacheTTL = config.ReviewCacheTTL

// ErrNilIdentity reports a Reviewer that returned (nil, nil). Caching nil
// would make every downstream handler treat a resolved request as anonymous
// and then panic on method calls, so it is surfaced as a review failure and
// never cached.
var ErrNilIdentity = errors.New("identity: reviewer returned no identity")

// Reviewer resolves a public bearer token into a caller identity. The
// controller adapter (internal/kernel/adapter) implements it against
// POST /internal/v1/token-review; tests substitute a fake.
type Reviewer interface {
	TokenReview(ctx context.Context, token string) (*Identity, error)
}

// Cache memoizes token reviews by token digest for TTL.
//
// Cache key: TokenDigest(token) — sha256 hex, first 32 chars. The controller's
// CallerIdentity has NO jti field in R1 (source-verified against
// agentteams-controller @ 71dc233a2), so the digest is the only stable handle
// for a token. The same digest is recorded as ops.audit_events.token_jti, so
// an audit row and a cache entry can be correlated; R2 swaps both to a real
// jti once the controller exposes one.
//
// Concurrency: safe for concurrent use. There is deliberately no singleflight
// (R1 accepts a duplicate review when several requests arrive together on a
// cold cache); each miss resolves independently and the last write wins.
type Cache struct {
	// Reviewer does the actual exchange. Required for a miss.
	Reviewer Reviewer
	// TTL is the cache lifetime; 0 (or negative) → DefaultCacheTTL (60s).
	TTL time.Duration
	// Now supplies the clock; nil → time.Now. Injectable to test expiry.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]entry
	// Sweep bounds memory for tokens that are never seen again: every
	// sweepEvery inserted/reviewed tokens, expired entries are dropped.
	// The TTL is 60s, so a busy service would otherwise accumulate one
	// dead entry per distinct bearer token forever.
	sweepEvery uint64
	resolves   uint64
}

// entry is one cached identity plus its expiry instant.
type entry struct {
	identity *Identity
	expires  time.Time
}

// NewCache builds a cache with the frozen 60s TTL and the wall clock.
func NewCache(r Reviewer) *Cache {
	return &Cache{Reviewer: r, TTL: DefaultCacheTTL, entries: make(map[string]entry)}
}

// Resolve returns the caller identity for token, serving a fresh cache entry
// when there is one and otherwise calling the Reviewer.
//
// A review failure is never cached: an invalid token must be rejected on
// every request, and a transient controller outage must not pin an error for
// 60s. The returned identity is a private copy, so a handler mutating it (or
// one of its slices) cannot corrupt the cache.
func (c *Cache) Resolve(ctx context.Context, token string) (*Identity, error) {
	key := TokenDigest(token)
	now := c.now()

	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if now.Before(e.expires) {
			id := cloneIdentity(e.identity)
			c.mu.Unlock()
			return id, nil
		}
		// Expired: drop it so a failed re-review cannot be served later.
		delete(c.entries, key)
	}
	c.mu.Unlock()

	if c.Reviewer == nil {
		return nil, errors.New("identity: cache has no reviewer")
	}
	resolved, err := c.Reviewer.TokenReview(ctx, token)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return nil, ErrNilIdentity
	}

	// The TTL runs from the instant the review started, so a slow controller
	// shortens the entry instead of extending it.
	c.mu.Lock()
	if c.entries == nil {
		// Tolerate a directly-constructed Cache (zero value besides TTL).
		c.entries = make(map[string]entry)
	}
	c.entries[key] = entry{identity: cloneIdentity(resolved), expires: now.Add(c.ttl())}
	if c.sweepEvery == 0 {
		c.sweepEvery = 256
	}
	c.resolves++
	if c.resolves%c.sweepEvery == 0 {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.mu.Unlock()

	return cloneIdentity(resolved), nil
}

// ttl returns the effective cache lifetime.
func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultCacheTTL
}

// now returns the cache clock (wall clock unless injected).
func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// cloneIdentity copies an identity and its slices, so cached state cannot be
// aliased by a caller.
func cloneIdentity(in *Identity) *Identity {
	if in == nil {
		return nil
	}
	out := *in
	out.Teams = cloneStrings(in.Teams)
	out.Capabilities = cloneStrings(in.Capabilities)
	out.AccessibleWorkers = cloneStrings(in.AccessibleWorkers)
	return &out
}

// cloneStrings copies a string slice, preserving nil (omitempty semantics).
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
