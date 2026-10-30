package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Per-path lease lock for the human file area (#1426 round 5, P1-2).
//
// MinIO and RustFS 1.0.4 both lack a conditional DELETE (RustFS
// app/object/delete.rs has no if_match), so a soft delete cannot be
// guarded against a concurrent writer at the storage layer. The lease
// below serializes every in-API operation that mutates the same rel
// (PUT, DELETE, RESTORE) on a per-path lock object:
//
//	humans/<u>/.control/locks/<sha256hex(rel)>
//
// outside files/ and .trash/, so it never counts toward quota usage and
// is unreachable through the file API.
//
// Protocol:
//
//	acquire — PutObjectIfAbsent (If-None-Match:*). If a lease already
//	          exists: read it; live -> backoff retry (up to 10 attempts,
//	          ~1s total budget) then errHumanPathBusy; expired ->
//	          PutObjectIfMatch CAS steal against the just-read ETag, and
//	          a lost steal goes back to the retry loop.
//	release — GetObject, verify the owner is us, DeleteObject. Any error
//	          is ignored: the TTL bounds a leaked lease.
//
// The lease only coordinates writers that go through this API; a direct
// storage write (mc/ops) bypassing the API is outside the protocol.

const (
	// humanPathLockTTL bounds a leaked lease; it comfortably covers the
	// slowest in-protocol operation (a 64 MiB upload) without renewal.
	humanPathLockTTL = 5 * time.Minute
	// humanPathLockRetries and humanPathLockBudget cap the acquisition
	// backoff: up to 10 attempts within a ~1 second total wait budget.
	humanPathLockRetries = 10
	humanPathLockBudget  = time.Second
)

// errHumanPathBusy is returned when the lease cannot be acquired within
// the retry budget; handlers map it to 409 file_busy.
var errHumanPathBusy = errors.New("human file path lease is held by another operation")

// errHumanPathContended marks "try again" outcomes inside the acquisition
// loop: a live foreign lease, a lease object that vanished mid-read, or a
// lost steal CAS.
var errHumanPathContended = errors.New("human file path lease contended")

// humanPathLockPayload is the lease body.
type humanPathLockPayload struct {
	Owner      string `json:"owner"`      // v4 uuid of the holder
	Path       string `json:"path"`       // the locked rel (diagnostic)
	AcquiredAt string `json:"acquiredAt"` // RFC3339 UTC
	ExpiresAt  string `json:"expiresAt"`  // RFC3339 UTC
}

// expired reports whether the lease may be taken over. A body whose
// expiry cannot be parsed is treated as expired so a corrupt lease cannot
// block the path forever; the TTL still bounds the damage.
func (p humanPathLockPayload) expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return true
	}
	return !now.Before(t)
}

// humanPathLockKey derives the lease object key for one human's rel. The
// sha256 digest keeps the key opaque (no path disclosure through listing
// the locks prefix) and of bounded length.
func humanPathLockKey(username, rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return humanAreaPrefix(username) + ".control/locks/" + hex.EncodeToString(sum[:])
}

// newHumanPathLockOwner generates a v4 uuid for the lease holder.
func newHumanPathLockOwner() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// humanPathLock is an acquired per-path lease.
//
// The lease object is never deleted. Releasing CASes it to an expired
// tombstone carrying the ETag this holder acquired, so a successor that
// took over in the meantime keeps its lease: an owner check followed by an
// unconditional delete (the earlier shape) could remove a fresher holder's
// lease and let a third writer in behind it. The TTL bounds a holder that
// dies without releasing.
type humanPathLock struct {
	h     *humanFilesHandler
	key   string
	rel   string
	owner string
	etag  string // ETag of the lease version this holder owns
	held  bool
}

// errHumanPathLeaseLost reports that this holder no longer owns the lease
// (expired and taken over, or replaced by a successor).
var errHumanPathLeaseLost = errors.New("human file path lease was taken over")

// acquireHumanPathLock takes the per-path lease for rel, retrying on
// contention within the ~1s budget. The returned lock must be released
// (defer) before the handler returns; the TTL covers a crash in between.
func (h *humanFilesHandler) acquireHumanPathLock(ctx context.Context, username, rel string, ttl time.Duration) (*humanPathLock, error) {
	key := humanPathLockKey(username, rel)
	owner, err := newHumanPathLockOwner()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(humanPathLockBudget)
	backoff := 10 * time.Millisecond
	for attempt := 0; attempt < humanPathLockRetries; attempt++ {
		lock, err := h.tryAcquireHumanPathLock(ctx, key, rel, owner, ttl)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errHumanPathContended) {
			return nil, err
		}
		if attempt == humanPathLockRetries-1 {
			break
		}
		if time.Now().Add(backoff).After(deadline) {
			break
		}
		time.Sleep(backoff)
		if backoff < 100*time.Millisecond {
			backoff *= 2
		}
	}
	return nil, errHumanPathBusy
}

// tryAcquireHumanPathLock makes one acquisition attempt. It returns
// errHumanPathContended when the caller should simply retry.
func (h *humanFilesHandler) tryAcquireHumanPathLock(ctx context.Context, key, rel, owner string, ttl time.Duration) (*humanPathLock, error) {
	now := time.Now().UTC()
	payload := humanPathLockPayload{
		Owner:      owner,
		Path:       rel,
		AcquiredAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(ttl).Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err := h.oss.PutObjectIfAbsent(ctx, key, body); err == nil {
		return h.settleHumanPathLock(ctx, key, rel, owner)
	} else if !errors.Is(err, oss.ErrPreconditionFailed) {
		return nil, err
	}

	// A lease exists: decide between waiting and stealing.
	current, etag, err := h.readHumanPathLease(ctx, key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errHumanPathContended // vanished mid-read; retry the create
		}
		return nil, err
	}
	if !current.expired(time.Now().UTC()) {
		return nil, errHumanPathContended // live foreign lease; wait
	}
	if err := h.oss.PutObjectIfMatch(ctx, key, body, etag); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			return nil, errHumanPathContended // lost the steal race; retry
		}
		return nil, err
	}
	return h.settleHumanPathLock(ctx, key, rel, owner)
}

// settleHumanPathLock reads back the lease we just wrote (create or steal)
// and binds this holder to its ETag. Without the binding a later Verify
// could not tell "still mine" from "replaced by a successor with identical
// content".
func (h *humanFilesHandler) settleHumanPathLock(ctx context.Context, key, rel, owner string) (*humanPathLock, error) {
	current, etag, err := h.readHumanPathLease(ctx, key)
	if err != nil {
		return nil, err
	}
	if current.Owner != owner || etag == "" {
		return nil, errHumanPathContended
	}
	return &humanPathLock{h: h, key: key, rel: rel, owner: owner, etag: etag, held: true}, nil
}

// readHumanPathLease returns the lease body bound to the ETag that
// describes exactly those bytes (the same validate-and-retry snapshot the
// quota ledger uses): a lease that changed between the two reads cannot be
// judged, so the attempt is contended. An empty or mismatching ETag means
// the body and version are unbindable — also contended. A corrupt body is
// returned as a zero payload, which expires immediately and can be stolen.
func (h *humanFilesHandler) readHumanPathLease(ctx context.Context, key string) (humanPathLockPayload, string, error) {
	data, err := h.oss.GetObject(ctx, key)
	if err != nil {
		return humanPathLockPayload{}, "", err
	}
	meta, err := h.oss.StatMeta(ctx, key)
	if err != nil {
		return humanPathLockPayload{}, "", err
	}
	if meta.ETag == "" || !strings.EqualFold(strings.Trim(meta.ETag, `"`), md5Hex(data)) {
		return humanPathLockPayload{}, "", errHumanPathContended
	}
	var p humanPathLockPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return humanPathLockPayload{}, meta.ETag, nil
	}
	return p, meta.ETag, nil
}

// Verify reports whether this holder still owns the exact lease version it
// acquired. Call it immediately before a mutating storage operation: a
// holder whose lease expired and was taken over must not keep changing the
// file unfenced, and a released holder must not act at all.
func (l *humanPathLock) Verify(ctx context.Context) error {
	if l == nil || !l.held {
		return errHumanPathLeaseLost
	}
	current, etag, err := l.h.readHumanPathLease(ctx, l.key)
	if err != nil {
		return errHumanPathLeaseLost
	}
	if current.Owner != l.owner || etag != l.etag {
		return errHumanPathLeaseLost
	}
	if current.expired(time.Now().UTC()) {
		return errHumanPathLeaseLost
	}
	return nil
}

// Release yields the lease by CASing it to an expired tombstone that still
// carries this holder's version. It never deletes the object: an
// unconditional delete after an owner check races a successor that acquired
// the lease in between (the check and the delete are two operations), and
// removing that successor's lease would admit a third writer. A failed CAS
// means the lease was already taken over — exactly when this holder must
// leave it alone. All errors are ignored; the TTL bounds a leaked lease.
func (l *humanPathLock) Release(ctx context.Context) {
	if l == nil || !l.held {
		return
	}
	l.held = false
	now := time.Now().UTC()
	tomb, err := json.Marshal(humanPathLockPayload{
		Owner:      "",
		Path:       l.rel,
		AcquiredAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(-time.Second).Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	_ = l.h.oss.PutObjectIfMatch(ctx, l.key, tomb, l.etag)
}
