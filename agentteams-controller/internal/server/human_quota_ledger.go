package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
	UsedBytes int64 `json:"usedBytes"`
}

func humanQuotaLedgerKey(username string) string {
	return humanAreaPrefix(username) + humanQuotaLedgerPath
}

// readQuotaLedger returns the current ledger and its ETag. os.ErrNotExist
// means the ledger has not been initialized yet (no quota write has happened
// since the feature was enabled or the human was created).
func (h *humanFilesHandler) readQuotaLedger(ctx context.Context, username string) (humanQuotaLedger, string, error) {
	key := humanQuotaLedgerKey(username)
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
	return ledger, meta.ETag, nil
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
// A nil quota means unlimited and performs no ledger I/O.
func (h *humanFilesHandler) reserveQuota(ctx context.Context, a *humanFileAccess, username string, oldSize, newSize int64) (int64, int, string) {
	quota := a.human.Spec.WorkspaceQuotaBytes
	if quota == nil {
		return 0, 0, ""
	}
	key := humanQuotaLedgerKey(username)
	for attempt := 0; attempt < humanQuotaLedgerRetries; attempt++ {
		ledger, etag, err := h.ensureQuotaLedger(ctx, username)
		if err != nil {
			return 0, 500, "quota reservation failed"
		}
		next := ledger.UsedBytes - oldSize + newSize
		if next > *quota {
			return 0, 413, fmt.Sprintf(
				"workspace quota of %d bytes exceeded (used %d, old %d, new %d)",
				*quota, ledger.UsedBytes, oldSize, newSize,
			)
		}
		payload, err := json.Marshal(humanQuotaLedger{UsedBytes: next})
		if err != nil {
			return 0, 500, "quota reservation failed"
		}
		if err := h.oss.PutObjectIfMatch(ctx, key, payload, etag); err == nil {
			return newSize - oldSize, 0, ""
		} else if !errors.Is(err, oss.ErrPreconditionFailed) {
			return 0, 500, "quota reservation failed"
		}
		// Another replica changed the ledger between read and CAS; retry
		// against its version.
	}
	return 0, 500, "quota reservation did not converge"
}

// releaseQuota reverses a reservation after the file write failed. It never
// creates a ledger and never makes usage negative; a missing ledger means
// there was no quota reservation to release.
func (h *humanFilesHandler) releaseQuota(ctx context.Context, username string, delta int64) {
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
		next := ledger.UsedBytes - delta
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
