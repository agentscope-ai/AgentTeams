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
const humanQuotaLedgerPath = ".control/quota.json"

const humanQuotaLedgerRetries = 8

type humanQuotaLedger struct {
	UsedBytes    int64                       `json:"usedBytes"`
	Reservations map[string]quotaReservation `json:"reservations,omitempty"`
}

// quotaReservation ties an in-flight write to the file version it observed.
// Two replacements of the same path must subtract the old size only once;
// the later reservation inherits the earlier reservation's target size and
// the original base size so a release can restore the right value.
type quotaReservation struct {
	Token       string `json:"token"`
	BaseOldSize int64  `json:"baseOldSize"`
	NewSize     int64  `json:"newSize"`
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
// since the feature was enabled or the human was created).
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
		if quotaLedgerETagMatches(data, meta.ETag) {
			return ledger, meta.ETag, nil
		}
		// The bytes and the ETag came from different object versions (a
		// concurrent reservation landed between GetObject and StatMeta).
		// Never bind stale contents to the newer ETag: retry the snapshot.
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

// reserveQuota atomically reserves the byte delta of a write. The returned
// delta must be passed to releaseQuota if the subsequent object write fails.
// A nil quota means unlimited, but an existing ledger is still advanced: a
// later re-enable must not trust a ledger that predates writes made while the
// quota was off. No ledger is created while quota is disabled.
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
		effectiveOld := oldSize
		baseOld := oldSize
		if prior, ok := ledger.Reservations[path]; ok {
			effectiveOld = prior.NewSize
			baseOld = prior.BaseOldSize
		}
		next := ledger.UsedBytes - effectiveOld + newSize
		if quota != nil && next > *quota {
			return "", 413, fmt.Sprintf(
				"workspace quota of %d bytes exceeded (used %d, old %d, new %d)",
				*quota, ledger.UsedBytes, effectiveOld, newSize,
			)
		}
		if next < 0 {
			next = 0
		}
		token := ""
		if quota != nil {
			token = newQuotaReservationToken()
			if ledger.Reservations == nil {
				ledger.Reservations = map[string]quotaReservation{}
			}
			ledger.Reservations[path] = quotaReservation{
				Token:       token,
				BaseOldSize: baseOld,
				NewSize:     newSize,
			}
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

// commitQuota removes a completed reservation. The ledger already carries the
// reserved size, so a successful object write needs no further accounting.
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
		prior, ok := ledger.Reservations[path]
		if !ok {
			// A superseded commit can still be the last write for this path;
			// recompute from live usage so the final size is counted.
			_ = h.syncQuotaUsage(ctx, username)
			return
		}
		if prior.Token != token {
			return // a newer reservation owns the path
		}
		delete(ledger.Reservations, path)
		if len(ledger.Reservations) == 0 {
			ledger.Reservations = nil
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

// syncQuotaUsage derives the ledger value from live usage plus every active
// reservation delta. It never creates a ledger. A reservation whose object PUT
// already landed may be counted conservatively until its commit removes it;
// this is safer than under-counting an in-flight write.
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
		live, err := h.usedBytes(ctx, username)
		if err != nil {
			return err
		}
		used := live
		for _, reservation := range ledger.Reservations {
			used += reservation.NewSize - reservation.BaseOldSize
		}
		if used < 0 {
			used = 0
		}
		if ledger.UsedBytes == used {
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

// reconcileQuotaLedger rebuilds the reservation ledger from live usage. It is
// called when a quota is enabled: writes while unlimited advanced an existing
// ledger, but a workspace created without any quota may not have one yet, and
// old deployments may have stale state. Failure is fail-closed because
// accepting writes against an unknown ledger would defeat the quota.
func (h *humanFilesHandler) reconcileQuotaLedger(ctx context.Context, username string) error {
	if _, _, err := h.ensureQuotaLedger(ctx, username); err != nil {
		return err
	}
	return h.syncQuotaUsage(ctx, username)
}

// releaseQuota reverses a reservation after the file write failed. It never
// creates a ledger and never makes usage negative; a missing ledger means
// there was no quota reservation to release.
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
		prior, ok := ledger.Reservations[path]
		if !ok || prior.Token != token {
			return
		}
		delete(ledger.Reservations, path)
		if len(ledger.Reservations) == 0 {
			ledger.Reservations = nil
		}
		next := ledger.UsedBytes - prior.NewSize + prior.BaseOldSize
		if next < 0 {
			next = 0
		}
		ledger.UsedBytes = next
		payload, err := json.Marshal(ledger)
		if err != nil {
			return
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			_ = h.syncQuotaUsage(ctx, username)
			return
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, oss.ErrPreconditionFailed) {
			return
		}
	}
}

// adjustQuotaLedger applies a net usage change for an operation that is not a
// file PUT (purge or overwrite-restore). It is a no-op when the ledger does
// not exist yet; the next quota write will seed it from live usage.
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
		payload, err := json.Marshal(humanQuotaLedger{UsedBytes: next})
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
