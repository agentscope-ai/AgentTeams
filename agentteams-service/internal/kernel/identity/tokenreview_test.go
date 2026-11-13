package identity

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReviewer counts reviews and can switch between success and failure.
type fakeReviewer struct {
	mu    sync.Mutex
	calls int
	last  string
	id    *Identity
	err   error
}

func (f *fakeReviewer) TokenReview(ctx context.Context, token string) (*Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = token
	if f.err != nil {
		return nil, f.err
	}
	return f.id, nil
}

func (f *fakeReviewer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeReviewer) set(id *Identity, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id, f.err = id, err
}

// fakeClock is the injectable clock used to cross the 60s TTL instantly.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func adminIdentity() *Identity {
	return &Identity{
		MatrixUserID:         "@admin:agentteams.local",
		HumanPermissionLevel: 1,
		Role:                 RoleAdmin,
		Username:             "admin",
		Team:                 "t1",
		Teams:                []string{"t1", "t2"},
		Capabilities:         []string{"workers.read"},
		AccessibleWorkers:    []string{"w1"},
	}
}

// TestNewCacheDefaults pins the frozen 60s TTL and the wired reviewer.
func TestNewCacheDefaults(t *testing.T) {
	if DefaultCacheTTL != 60*time.Second {
		t.Fatalf("DefaultCacheTTL = %v, want the frozen 60s (58 §5)", DefaultCacheTTL)
	}
	c := NewCache(&fakeReviewer{})
	if c.TTL != DefaultCacheTTL {
		t.Fatalf("TTL = %v, want %v", c.TTL, DefaultCacheTTL)
	}
	if c.Reviewer == nil {
		t.Fatal("NewCache must keep the reviewer")
	}
	if c.Now != nil {
		t.Fatal("Now must default to nil (wall clock)")
	}
}

// TestCacheHitAvoidsSecondReview is the core contract: same token twice, one
// controller token-review.
func TestCacheHitAvoidsSecondReview(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	clk := newFakeClock()
	c := NewCache(r)
	c.Now = clk.Now
	ctx := context.Background()

	first, err := c.Resolve(ctx, "tok-1")
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := c.Resolve(ctx, "tok-1")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", r.count())
	}
	if !reflect.DeepEqual(first, adminIdentity()) || !reflect.DeepEqual(second, adminIdentity()) {
		t.Fatalf("identities = %+v / %+v, want the reviewed identity", first, second)
	}
	if first == second {
		t.Fatal("cache must return copies, not the same pointer")
	}
	if r.last != "tok-1" {
		t.Fatalf("reviewed token = %q", r.last)
	}
}

// TestCacheReturnsDefensiveCopy proves a handler cannot corrupt the cache by
// mutating the identity it was handed (slices included).
func TestCacheReturnsDefensiveCopy(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	c := NewCache(r)
	c.Now = newFakeClock().Now
	ctx := context.Background()

	first, err := c.Resolve(ctx, "tok-1")
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	first.Teams[0] = "hacked"
	first.Capabilities[0] = "root"
	first.AccessibleWorkers = append(first.AccessibleWorkers, "other")

	second, err := c.Resolve(ctx, "tok-1")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if !reflect.DeepEqual(second, adminIdentity()) {
		t.Fatalf("cached identity was mutated: %+v", second)
	}
	if r.count() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", r.count())
	}
}

// TestCacheExpiresAfterTTL advances the injected clock past 60s.
func TestCacheExpiresAfterTTL(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	clk := newFakeClock()
	c := NewCache(r)
	c.Now = clk.Now
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	clk.Advance(59 * time.Second)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve within TTL: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("reviewer calls within TTL = %d, want 1", r.count())
	}

	clk.Advance(2 * time.Second) // t0 + 61s
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve after TTL: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls after TTL = %d, want 2", r.count())
	}
}

// TestCacheExpiryBoundaryIsExclusive documents that the entry is stale at
// exactly t0+TTL (now.Before(expires) is false at equality).
func TestCacheExpiryBoundaryIsExclusive(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	clk := newFakeClock()
	c := NewCache(r)
	c.Now = clk.Now
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	clk.Advance(DefaultCacheTTL)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve at TTL boundary: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls = %d, want 2 at the TTL boundary", r.count())
	}
}

// TestCacheZeroTTLUsesFrozen60s covers a directly-built cache: TTL 0 means the
// frozen 60s, not "never expires", and the entry map is initialized lazily.
func TestCacheZeroTTLUsesFrozen60s(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	clk := newFakeClock()
	c := &Cache{Reviewer: r, Now: clk.Now}
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	clk.Advance(59 * time.Second)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve within TTL: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("reviewer calls within TTL = %d, want 1", r.count())
	}
	clk.Advance(2 * time.Second)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve after TTL: %v", err)
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls after TTL = %d, want 2", r.count())
	}
}

// TestCacheDoesNotCacheFailures: a failed review must be retried next time,
// otherwise a 60s outage (or a revoked token race) would pin the failure.
func TestCacheDoesNotCacheFailures(t *testing.T) {
	boom := errors.New("controller unavailable")
	r := &fakeReviewer{err: boom}
	clk := newFakeClock()
	c := NewCache(r)
	c.Now = clk.Now
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); !errors.Is(err, boom) {
		t.Fatalf("first Resolve err = %v, want the reviewer error", err)
	}
	if _, err := c.Resolve(ctx, "tok-1"); !errors.Is(err, boom) {
		t.Fatalf("second Resolve err = %v, want the reviewer error", err)
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls = %d, want 2 (failures are never cached)", r.count())
	}

	r.set(adminIdentity(), nil)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve after recovery: %v", err)
	}
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve after recovery (cached): %v", err)
	}
	if r.count() != 3 {
		t.Fatalf("reviewer calls = %d, want 3 (success is cached)", r.count())
	}
}

// TestCacheExpiredEntryNotServedAfterFailedRefresh: the expired entry is
// dropped before the re-review, so a failure cannot resurrect stale identity.
func TestCacheExpiredEntryNotServedAfterFailedRefresh(t *testing.T) {
	boom := errors.New("controller unavailable")
	r := &fakeReviewer{id: adminIdentity()}
	clk := newFakeClock()
	c := NewCache(r)
	c.Now = clk.Now
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	clk.Advance(61 * time.Second)
	r.set(nil, boom)
	if _, err := c.Resolve(ctx, "tok-1"); !errors.Is(err, boom) {
		t.Fatalf("Resolve after expiry err = %v, want the reviewer error", err)
	}

	r.set(adminIdentity(), nil)
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("Resolve after recovery: %v", err)
	}
	if r.count() != 3 {
		t.Fatalf("reviewer calls = %d, want 3", r.count())
	}
}

// TestCacheKeysByTokenDigest: different tokens are different keys (one review
// each), and the key is the token digest, not the raw token.
func TestCacheKeysByTokenDigest(t *testing.T) {
	if TokenDigest("tok-a") == TokenDigest("tok-b") {
		t.Fatal("token digests collide")
	}
	r := &fakeReviewer{id: adminIdentity()}
	c := NewCache(r)
	c.Now = newFakeClock().Now
	ctx := context.Background()

	for _, token := range []string{"tok-a", "tok-b", "tok-a", "tok-b"} {
		if _, err := c.Resolve(ctx, token); err != nil {
			t.Fatalf("Resolve(%q): %v", token, err)
		}
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls = %d, want 2 (one per distinct token)", r.count())
	}
	if r.last != "tok-b" {
		t.Fatalf("last reviewed token = %q", r.last)
	}
}

// TestCacheNilIdentityIsAnError guards the (nil, nil) reviewer contract.
func TestCacheNilIdentityIsAnError(t *testing.T) {
	r := &fakeReviewer{}
	c := NewCache(r)
	c.Now = newFakeClock().Now
	ctx := context.Background()

	id, err := c.Resolve(ctx, "tok-1")
	if id != nil {
		t.Fatalf("identity = %+v, want nil", id)
	}
	if !errors.Is(err, ErrNilIdentity) {
		t.Fatalf("err = %v, want ErrNilIdentity", err)
	}
	if _, err := c.Resolve(ctx, "tok-1"); !errors.Is(err, ErrNilIdentity) {
		t.Fatalf("second err = %v, want ErrNilIdentity", err)
	}
	if r.count() != 2 {
		t.Fatalf("reviewer calls = %d, want 2 (nil identity is not cached)", r.count())
	}
}

// TestCacheWithoutReviewerIsAnError: a misconfigured cache must not resolve.
func TestCacheWithoutReviewerIsAnError(t *testing.T) {
	c := NewCache(nil)
	c.Now = newFakeClock().Now
	id, err := c.Resolve(context.Background(), "tok-1")
	if id != nil || err == nil {
		t.Fatalf("Resolve = (%+v, %v), want (nil, error)", id, err)
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("err = %q, want it to mention the reviewer", err)
	}
}

// TestCacheNilNowUsesWallClock keeps the default clock path exercised.
func TestCacheNilNowUsesWallClock(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	c := NewCache(r)
	ctx := context.Background()

	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := c.Resolve(ctx, "tok-1"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if r.count() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", r.count())
	}
}

// TestCacheConcurrent makes the race detector prove the lock covers the map:
// no singleflight is promised, so the call count is bounded, not fixed.
func TestCacheConcurrent(t *testing.T) {
	r := &fakeReviewer{id: adminIdentity()}
	c := NewCache(r)
	ctx := context.Background()

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		token := "tok-a"
		if i%2 == 1 {
			token = "tok-b"
		}
		go func(token string) {
			defer wg.Done()
			id, err := c.Resolve(ctx, token)
			if err != nil {
				t.Errorf("Resolve(%q): %v", token, err)
				return
			}
			if id == nil || id.Role != RoleAdmin {
				t.Errorf("Resolve(%q) = %+v", token, id)
			}
		}(token)
	}
	wg.Wait()

	if calls := r.count(); calls < 1 || calls > goroutines {
		t.Fatalf("reviewer calls = %d, want between 1 and %d", calls, goroutines)
	}
}

// TestCacheSweepsExpiredEntries bounds memory: after sweepEvery reviews the
// cache drops expired entries instead of accumulating one per token.
func TestCacheSweepsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	c := &Cache{
		Reviewer: fuzzReviewer{},
		TTL:      time.Second,
		Now:      func() time.Time { return now },
	}
	for i := 0; i < 255; i++ {
		if _, err := c.Resolve(context.Background(), "token-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if got := len(c.entries); got != 255 {
		t.Fatalf("entries = %d, want 255", got)
	}
	now = now.Add(2 * time.Second) // everything above is now expired
	if _, err := c.Resolve(context.Background(), "fresh-token"); err != nil {
		t.Fatalf("resolve fresh: %v", err)
	}
	if got := len(c.entries); got != 1 {
		t.Fatalf("entries after sweep = %d, want 1", got)
	}
}

type fuzzReviewer struct{}

func (fuzzReviewer) TokenReview(ctx context.Context, token string) (*Identity, error) {
	return &Identity{Role: RoleAdmin, Username: token}, nil
}
