package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

// newTestAuthenticator builds an authenticator with the given cache bounds.
// k8s client is nil — these tests exercise cache eviction only, never the
// TokenReview path.
func newTestAuthenticator(ttl time.Duration, max int) *TokenReviewAuthenticator {
	return &TokenReviewAuthenticator{
		prefix:          DefaultResourcePrefix,
		cache:           make(map[[32]byte]cachedResult),
		cacheTTL:        ttl,
		cacheMax:        max,
		cleanupInterval: 10 * time.Millisecond,
	}
}

func fakeKey(seed int) [32]byte {
	return sha256.Sum256(fmt.Appendf(nil, "token-%d", seed))
}

func TestPutInCache_EvictsExpiredBeforeOldest(t *testing.T) {
	a := newTestAuthenticator(50*time.Millisecond, 3)
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	a.putInCache(fakeKey(1), id)
	a.putInCache(fakeKey(2), id)
	a.putInCache(fakeKey(3), id)

	time.Sleep(60 * time.Millisecond) // all three expire

	a.putInCache(fakeKey(4), id)

	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	if got := len(a.cache); got != 1 {
		t.Fatalf("expected expired sweep to leave only the new entry, got %d entries", got)
	}
	if _, ok := a.cache[fakeKey(4)]; !ok {
		t.Errorf("freshly inserted key should remain after sweep")
	}
}

func TestPutInCache_EvictsOldestWhenAllLive(t *testing.T) {
	a := newTestAuthenticator(10*time.Minute, 3)
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	// Insert with manual expiries so we can predict eviction order.
	now := time.Now()
	a.cache[fakeKey(1)] = cachedResult{identity: id, expiry: now.Add(1 * time.Minute)}
	a.cache[fakeKey(2)] = cachedResult{identity: id, expiry: now.Add(2 * time.Minute)}
	a.cache[fakeKey(3)] = cachedResult{identity: id, expiry: now.Add(3 * time.Minute)}

	a.putInCache(fakeKey(4), id) // should kick fakeKey(1) out

	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	if len(a.cache) != 3 {
		t.Fatalf("cap exceeded: got %d entries, want 3", len(a.cache))
	}
	if _, ok := a.cache[fakeKey(1)]; ok {
		t.Errorf("oldest-expiry entry should have been evicted")
	}
	if _, ok := a.cache[fakeKey(4)]; !ok {
		t.Errorf("newly inserted entry missing")
	}
}

func TestSweepExpired(t *testing.T) {
	a := newTestAuthenticator(0, 100) // ttl=0 forces immediate expiry
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}

	now := time.Now()
	a.cache[fakeKey(1)] = cachedResult{identity: id, expiry: now.Add(-time.Second)}
	a.cache[fakeKey(2)] = cachedResult{identity: id, expiry: now.Add(-time.Second)}
	a.cache[fakeKey(3)] = cachedResult{identity: id, expiry: now.Add(time.Hour)}

	removed := a.SweepExpired()
	if removed != 2 {
		t.Errorf("SweepExpired removed %d, want 2", removed)
	}
	if _, ok := a.cache[fakeKey(3)]; !ok {
		t.Errorf("live entry must survive sweep")
	}
}

func TestStartCleanup_ExitsOnContextCancel(t *testing.T) {
	a := newTestAuthenticator(10*time.Minute, 100)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		a.StartCleanup(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartCleanup did not exit within 1s of ctx cancel")
	}
}

func TestStartCleanup_RemovesExpiredOnTick(t *testing.T) {
	a := newTestAuthenticator(10*time.Millisecond, 100)
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}
	a.putInCache(fakeKey(1), id)
	a.putInCache(fakeKey(2), id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.StartCleanup(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.cacheMu.RLock()
		n := len(a.cache)
		a.cacheMu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("StartCleanup did not drain expired entries within 1s")
}

func TestGetFromCache_IgnoresExpired(t *testing.T) {
	a := newTestAuthenticator(0, 100)
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}
	a.cache[fakeKey(1)] = cachedResult{identity: id, expiry: time.Now().Add(-time.Second)}

	if got := a.getFromCache(fakeKey(1)); got != nil {
		t.Errorf("expired entry must not be returned, got %+v", got)
	}
}

func TestCacheMaxZero_DisablesCap(t *testing.T) {
	a := newTestAuthenticator(10*time.Minute, 0)
	id := &CallerIdentity{Role: RoleWorker, Username: "alice", WorkerName: "alice"}
	for i := range 50 {
		a.putInCache(fakeKey(i), id)
	}
	if got := len(a.cache); got != 50 {
		t.Errorf("cap=0 should allow unbounded growth, got %d entries", got)
	}
}
