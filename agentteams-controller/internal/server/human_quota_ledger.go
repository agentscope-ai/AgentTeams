package server

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Quota accounting has to be atomic across controller instances. A local
// mutex is insufficient: two replicas can both observe the same usedBytes and
// both admit a write. The ledger below is a per-human object stored next to
// the workspace; reservations use object-storage conditional writes
// (If-None-Match:* for the first create, If-Match for subsequent updates), so
// every controller process contends on the same version of the same object.
//
// The ledger is deliberately outside files/ and .trash/, so it does not count
// toward usage and cannot be reached through the file API.
//
// Accounting model: UsedBytes = live usage (the walk of files/ and .trash/)
// plus, for every path with in-flight writes, the extra bytes those writes
// will add. Each path entry is
//
//	Counted = max(live(path), every outstanding target size)
//
// over an append-only Outstanding list: only a reservation's own commit or
// release removes its token, so a later write to the same path can never
// overwrite or delete an earlier in-flight reservation. The max (rather than
// chained deltas) makes the charge independent of which in-flight write lands
// first — the final content is the last writer's in unknown order, so the
// largest target in flight is the conservative (fail-closed) estimate. A
// reservation on a path that already has an entry builds on the entry's
// Counted, never on the handler's fresh stat, so two replacement generations
// do not subtract the same committed old bytes twice.
//
// Invariants (must hold after every ledger write):
//
//   - I1: UsedBytes = liveTotal + Σ over entries of (Counted - live(path));
//     a path with no in-flight writes carries no entry (contribution zero).
//   - I2: an outstanding token is removed only by its own successful commit
//     or release; no other operation may delete it.
//   - I3: Counted == max(live(path), all outstanding target sizes),
//     re-evaluated with a fresh stat on every reserve/commit/release/sync.
//   - I4: when in doubt, overstate usage (fail closed), never understate it.
//   - I5: a commit/release that cannot find its own token (a crash between
//     the object write and the ledger update) re-syncs and returns: no
//     panic, no negative usage, no dirty state.
const humanQuotaLedgerPath = ".control/quota.json"

const humanQuotaLedgerRetries = 8

type humanQuotaLedger struct {
	UsedBytes int64 `json:"usedBytes"`
	// New format: one entry per path with in-flight writes.
	Paths map[string]quotaLedgerPathEntry `json:"paths,omitempty"`
	// Legacy format (single reservation per path): migration input only.
	// No code path writes to it once the ledger is decoded.
	Reservations map[string]quotaReservation `json:"reservations,omitempty"`
}

// quotaLedgerPathEntry is the per-path accounting state: the bytes the path
// currently contributes to UsedBytes and the in-flight writes behind them.
type quotaLedgerPathEntry struct {
	Counted     int64              `json:"counted"`
	Outstanding []quotaReservation `json:"outstanding,omitempty"`
}

// quotaReservation is one in-flight write.
type quotaReservation struct {
	Token string `json:"token"`
	// BaseOldSize is diagnostic only (the actual old size the reserving
	// handler observed); it plays no part in the accounting.
	BaseOldSize int64 `json:"baseOldSize"`
	NewSize     int64 `json:"newSize"`
}

func newQuotaReservationToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func humanQuotaLedgerKey(username string) string {
	return humanAreaPrefix(username) + humanQuotaLedgerPath
}

// readQuotaLedger returns the current ledger and its ETag. os.ErrNotExist
// means the ledger has not been initialized yet (no quota write has happened
// since the feature was enabled or the human was created). A body in the
// legacy single-reservation-per-path format is upgraded in place before it is
// returned (one-time, lenient, fail-closed), so no caller can build on the
// old format.
func (h *humanFilesHandler) readQuotaLedger(ctx context.Context, username string) (humanQuotaLedger, string, error) {
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			return humanQuotaLedger{}, "", err
		}
		var ledger humanQuotaLedger
		if err := json.Unmarshal(data, &ledger); err != nil {
			return humanQuotaLedger{}, "", fmt.Errorf("decode quota ledger: %w", err)
		}
		meta, err := h.oss.StatMeta(ctx, key)
		if err != nil {
			return humanQuotaLedger{}, "", err
		}
		if meta.ETag == "" {
			return humanQuotaLedger{}, "", fmt.Errorf("quota ledger has no ETag")
		}
		if !quotaLedgerETagMatches(data, meta.ETag) {
			// The bytes and the ETag came from different object versions (a
			// concurrent reservation landed between GetObject and StatMeta).
			// Never bind stale contents to the newer ETag: retry the snapshot.
			continue
		}
		if len(ledger.Reservations) > 0 && len(ledger.Paths) == 0 {
			migrated, etag, ok := h.migrateQuotaLedger(ctx, username, ledger, meta.ETag)
			if !ok {
				continue // lost the migration CAS race; re-snapshot
			}
			return migrated, etag, nil
		}
		return ledger, meta.ETag, nil
	}
	return humanQuotaLedger{}, "", fmt.Errorf("quota ledger changed while taking a consistent snapshot")
}

// quotaLedgerETagMatches binds the ledger bytes to the ETag returned by the
// subsequent metadata read. The ledger is a small single-part object, so its
// S3 ETag is the MD5 of the body (MinIO/RustFS/S3 semantics). If a concurrent
// writer changes the body between GetObject and StatMeta, the hash and ETag
// disagree and the caller retries instead of CASing stale bytes onto the new
// version. This is the validate-and-retry snapshot required for the ledger.
func quotaLedgerETagMatches(data []byte, etag string) bool {
	sum := md5.Sum(data)
	expected := hex.EncodeToString(sum[:])
	return strings.EqualFold(strings.Trim(etag, `"`), expected)
}

// liveFileSize returns the current size of the file object at rel (0 when the
// object does not exist); other stat faults are returned so the caller can
// fail closed.
func (h *humanFilesHandler) liveFileSize(ctx context.Context, username, rel string) (int64, error) {
	meta, err := h.oss.StatMeta(ctx, humanFilesPrefix(username)+rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return meta.Size, nil
}

// migrateQuotaLedger upgrades a legacy ledger (one reservation per path) to
// the per-path outstanding-queue format, preserving every in-flight
// reservation: each becomes its path's sole outstanding entry, charged at
// max(live, reserved target) so an in-flight write keeps reserving after the
// migration (I4). UsedBytes is recomputed from I1. The write is a CAS on the
// snapshot's ETag; a lost race means the caller re-snapshots.
func (h *humanFilesHandler) migrateQuotaLedger(ctx context.Context, username string, legacy humanQuotaLedger, etag string) (humanQuotaLedger, string, bool) {
	liveTotal, err := h.usedBytes(ctx, username)
	if err != nil {
		return humanQuotaLedger{}, "", false
	}
	paths := make(map[string]quotaLedgerPathEntry, len(legacy.Reservations))
	extra := int64(0)
	for rel, r := range legacy.Reservations {
		live, err := h.liveFileSize(ctx, username, rel)
		if err != nil {
			return humanQuotaLedger{}, "", false // fail closed: I1 is unverifiable
		}
		counted := r.NewSize
		if live > counted {
			counted = live
		}
		paths[rel] = quotaLedgerPathEntry{
			Counted: counted,
			Outstanding: []quotaReservation{{
				Token:       r.Token,
				BaseOldSize: r.BaseOldSize,
				NewSize:     r.NewSize,
			}},
		}
		extra += counted - live
	}
	used := liveTotal + extra
	if used < 0 {
		used = 0
	}
	migrated := humanQuotaLedger{UsedBytes: used, Paths: paths}
	payload, err := json.Marshal(migrated)
	if err != nil {
		return humanQuotaLedger{}, "", false
	}
	if err := h.oss.PutObjectIfMatch(ctx, humanQuotaLedgerKey(username), payload, etag); err != nil {
		return humanQuotaLedger{}, "", false // concurrent writer; caller re-snapshots
	}
	fresh, etag, err := h.readQuotaLedger(ctx, username)
	if err != nil {
		return humanQuotaLedger{}, "", false
	}
	return fresh, etag, true
}

// ensureQuotaLedger lazily seeds the ledger from the actual object store. The
// If-None-Match:* create is the serialization point: two replicas may compute
// different initial snapshots, but only one creation wins and both then use
// the winner's ledger. The loser re-reads instead of overwriting.
func (h *humanFilesHandler) ensureQuotaLedger(ctx context.Context, username string) (humanQuotaLedger, string, error) {
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.readQuotaLedger(ctx, username)
		if err == nil {
			return ledger, etag, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return humanQuotaLedger{}, "", err
		}
		used, err := h.usedBytes(ctx, username)
		if err != nil {
			return humanQuotaLedger{}, "", err
		}
		data, err := json.Marshal(humanQuotaLedger{UsedBytes: used})
		if err != nil {
			return humanQuotaLedger{}, "", err
		}
		err = h.oss.PutObjectIfAbsent(ctx, key, data)
		if err == nil {
			continue // read back the created object and its ETag
		}
		if errors.Is(err, oss.ErrPreconditionFailed) {
			continue // another controller created it; read their ledger
		}
		return humanQuotaLedger{}, "", err
	}
	return humanQuotaLedger{}, "", fmt.Errorf("quota ledger initialization did not converge")
}

// reserveQuota atomically reserves the bytes an in-flight write will add. The
// returned token must be passed to commitQuota after the object write
// succeeds, or to releaseQuota when it fails. A nil quota means unlimited,
// but an existing ledger is still advanced: a later re-enable must not trust
// a ledger that predates writes made while the quota was off. No ledger is
// created while quota is disabled.
func (h *humanFilesHandler) reserveQuota(ctx context.Context, a *humanFileAccess, username, path string, oldSize, newSize int64) (string, int, string) {
	quota := a.human.Spec.WorkspaceQuotaBytes
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		var (
			ledger humanQuotaLedger
			etag   string
			err    error
		)
		if quota == nil {
			ledger, etag, err = h.readQuotaLedger(ctx, username)
			if errors.Is(err, os.ErrNotExist) {
				return "", 0, ""
			}
		} else {
			ledger, etag, err = h.ensureQuotaLedger(ctx, username)
		}
		if err != nil {
			return "", 500, "quota reservation failed"
		}
		entry := ledger.Paths[path]
		// With an existing entry the path is already charged at its Counted
		// size; the handler's freshly statted oldSize must not be subtracted
		// again (two in-flight replacements would double-release the same
		// committed bytes).
		countedOld := oldSize
		if _, ok := ledger.Paths[path]; ok {
			countedOld = entry.Counted
		}
		countedNew := countedOld
		if newSize > countedNew {
			countedNew = newSize
		}
		next := ledger.UsedBytes - countedOld + countedNew
		if quota != nil && next > *quota {
			return "", 413, fmt.Sprintf(
				"workspace quota of %d bytes exceeded (used %d, old %d, new %d)",
				*quota, ledger.UsedBytes, countedOld, newSize,
			)
		}
		if next < 0 {
			next = 0
		}
		token := ""
		if quota != nil {
			token = newQuotaReservationToken()
			if ledger.Paths == nil {
				ledger.Paths = make(map[string]quotaLedgerPathEntry)
			}
			// Append-only: an earlier in-flight reservation on this path
			// keeps reserving until its own commit/release removes it.
			entry.Outstanding = append(entry.Outstanding, quotaReservation{
				Token:       token,
				BaseOldSize: oldSize,
				NewSize:     newSize,
			})
			entry.Counted = countedNew
			ledger.Paths[path] = entry
		}
		ledger.UsedBytes = next
		payload, err := json.Marshal(ledger)
		if err != nil {
			return "", 500, "quota reservation failed"
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			return token, 0, ""
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return "", 500, "quota reservation failed"
		}
	}
	return "", 500, "quota reservation did not converge"
}

// commitQuota settles a completed reservation. The path's counted size is
// re-derived from a fresh stat and the in-flight reservations that remain, so
// the landing write's bytes are counted exactly once regardless of commit
// order.
func (h *humanFilesHandler) commitQuota(ctx context.Context, username, path, token string) {
	if token == "" {
		return
	}
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.readQuotaLedger(ctx, username)
		if err != nil {
			return
		}
		entry, ok := ledger.Paths[path]
		idx := -1
		for i, r := range entry.Outstanding {
			if r.Token == token {
				idx = i
				break
			}
		}
		if !ok || idx < 0 {
			// The token is already gone (a crash landed the object write
			// before the ledger update, or this is a duplicate commit):
			// recompute from the invariant so the live bytes count.
			_ = h.syncQuotaUsage(ctx, username)
			return
		}
		live, err := h.liveFileSize(ctx, username, path)
		if err != nil {
			// The object write just succeeded, so a stat fault here is a
			// transient backend problem; retry the whole cycle rather than
			// settle the entry on unknown live bytes (I4).
			continue
		}
		entry.Outstanding = append(entry.Outstanding[:idx], entry.Outstanding[idx+1:]...)
		countedNew := live
		for _, r := range entry.Outstanding {
			if r.NewSize > countedNew {
				countedNew = r.NewSize
			}
		}
		used := ledger.UsedBytes - entry.Counted + countedNew
		if used < 0 {
			used = 0
		}
		ledger.UsedBytes = used
		if len(entry.Outstanding) == 0 {
			delete(ledger.Paths, path)
			if len(ledger.Paths) == 0 {
				ledger.Paths = nil
			}
		} else {
			entry.Counted = countedNew
			ledger.Paths[path] = entry
		}
		payload, err := json.Marshal(ledger)
		if err != nil {
			return
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			_ = h.syncQuotaUsage(ctx, username)
			return
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return
		}
	}
}

// syncQuotaUsage re-derives the ledger from the invariants: live usage plus
// each path's (Counted - live). It never creates a ledger and never clears
// Outstanding (I2): an in-flight write keeps reserving until its own
// commit/release settles it. A stat fault on a tracked path aborts the whole
// recompute and returns the error without touching the ledger (I4, fail
// closed): writing the total without that path's contribution would drop the
// in-flight charge and understate usage — understating is the dangerous
// error, so the previous ledger (still charging the reservation) stands.
func (h *humanFilesHandler) syncQuotaUsage(ctx context.Context, username string) error {
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.readQuotaLedger(ctx, username)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		liveTotal, err := h.usedBytes(ctx, username)
		if err != nil {
			return err
		}
		extra := int64(0)
		changed := false
		for path, entry := range ledger.Paths {
			live, err := h.liveFileSize(ctx, username, path)
			if err != nil {
				// NotExist is already folded into live == 0 inside
				// liveFileSize; any other fault means this entry's charge
				// cannot be re-verified, and the total would understate
				// usage without it (I4). Abandon the recompute.
				return fmt.Errorf("quota ledger sync: stat %s: %w", path, err)
			}
			counted := live
			for _, r := range entry.Outstanding {
				if r.NewSize > counted {
					counted = r.NewSize
				}
			}
			if entry.Counted != counted {
				entry.Counted = counted
				ledger.Paths[path] = entry
				changed = true
			}
			extra += counted - live
		}
		used := liveTotal + extra
		if used < 0 {
			used = 0
		}
		if !changed && ledger.UsedBytes == used {
			return nil
		}
		ledger.UsedBytes = used
		payload, err := json.Marshal(ledger)
		if err != nil {
			return err
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			return nil
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return err
		}
	}
	return fmt.Errorf("quota ledger usage sync did not converge")
}

// reconcileQuotaLedger rebuilds the ledger's settled bytes from live usage.
// It is called when a quota is enabled: writes while unlimited advanced an
// existing ledger, but a workspace created without any quota may not have one
// yet, and old deployments may have stale state. In-flight reservations are
// preserved by sync's invariant-based recompute. Failure is fail-closed
// because accepting writes against an unknown ledger would defeat the quota.
func (h *humanFilesHandler) reconcileQuotaLedger(ctx context.Context, username string) error {
	if _, _, err := h.ensureQuotaLedger(ctx, username); err != nil {
		return err
	}
	return h.syncQuotaUsage(ctx, username)
}

// releaseQuota reverses a reservation after the file write failed. It never
// creates a ledger and never makes usage negative; a missing ledger or an
// unknown token means there was nothing to release.
func (h *humanFilesHandler) releaseQuota(ctx context.Context, username, path, token string) {
	if token == "" {
		return
	}
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.readQuotaLedger(ctx, username)
		if err != nil {
			return
		}
		entry, ok := ledger.Paths[path]
		idx := -1
		for i, r := range entry.Outstanding {
			if r.Token == token {
				idx = i
				break
			}
		}
		if !ok || idx < 0 {
			return // idempotent: already released, or never reserved
		}
		live, err := h.liveFileSize(ctx, username, path)
		if err != nil {
			continue
		}
		entry.Outstanding = append(entry.Outstanding[:idx], entry.Outstanding[idx+1:]...)
		countedNew := live
		for _, r := range entry.Outstanding {
			if r.NewSize > countedNew {
				countedNew = r.NewSize
			}
		}
		used := ledger.UsedBytes - entry.Counted + countedNew
		if used < 0 {
			used = 0
		}
		ledger.UsedBytes = used
		if len(entry.Outstanding) == 0 {
			delete(ledger.Paths, path)
			if len(ledger.Paths) == 0 {
				ledger.Paths = nil
			}
		} else {
			entry.Counted = countedNew
			ledger.Paths[path] = entry
		}
		payload, err := json.Marshal(ledger)
		if err != nil {
			return
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			_ = h.syncQuotaUsage(ctx, username)
			return
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return
		}
	}
}

// adjustQuotaLedger applies a net usage change for an operation that is not a
// file PUT (purge or overwrite-restore). It is a no-op when the ledger does
// not exist yet; the next quota write will seed it from live usage. In-flight
// reservations are preserved: the delta applies to the settled bytes only.
func (h *humanFilesHandler) adjustQuotaLedger(ctx context.Context, username string, delta int64) {
	if delta == 0 {
		return
	}
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.readQuotaLedger(ctx, username)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			return
		}
		next := ledger.UsedBytes + delta
		if next < 0 {
			next = 0
		}
		ledger.UsedBytes = next
		payload, err := json.Marshal(ledger)
		if err != nil {
			return
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			return
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return
		}
	}
}
