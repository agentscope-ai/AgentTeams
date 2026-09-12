// Package audit implements the dual-layer audit trail of #1220 §8:
//
//	layer 1 — an immediate structured log line (always, even without storage);
//	layer 2 — an append-only daily JSONL object under audit/<YYYY-MM-DD>.jsonl,
//	  written with ETag-optimistic concurrency and bounded retries.
//
// Events never carry secret values: Before/After hold capability names (an
// open value set), and Detail is a controlled summary, not free text.
//
// Single-replica assumption: embedded and k8s deployments today run one
// controller replica, so layer-2 write contention only comes from
// in-process concurrency, which the client mutex serializes. A queryable
// audit store (or per-event objects) is #1220 §13 Q3.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Event is one auditable action (#1220 §8 event table, first rows:
// capability grant/revoke; the Action/Capability/Before/After fields are
// shaped for the follow-on consumers — approval_level changes, channel
// credential writes, external source adds/updates).
type Event struct {
	Who        string    `json:"who"`
	Role       string    `json:"role"` // admin | manager | team-leader | worker | human
	When       time.Time `json:"when"`
	Target     string    `json:"target,omitempty"`
	TargetTeam string    `json:"targetTeam,omitempty"`
	Action     string    `json:"action"`               // capability_grant | capability_revoke | ...
	Capability string    `json:"capability,omitempty"` // the capability named by this event
	Before     []string  `json:"before,omitempty"`     // never secret values
	After      []string  `json:"after,omitempty"`      // never secret values
	Detail     string    `json:"detail,omitempty"`     // controlled summary
}

// defaultRetryBackoffs are the layer-2 conflict retry delays (with up to
// 50% additive jitter). Exposed as fields so tests can shrink them.
var defaultRetryBackoffs = [3]time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}

// Client records events to both layers. Layer 2 runs only when a storage
// client was provided at construction.
type Client struct {
	sc            oss.StorageClient
	mu            sync.Mutex // serializes layer-2 writes (single-replica assumption)
	jitter        *rand.Rand
	retryBackoffs [3]time.Duration
}

// NewClient creates an audit Client. sc may be nil: layer 1 (log) still
// records, layer 2 is skipped (graceful degradation, e.g. in tests).
func NewClient(sc oss.StorageClient) *Client {
	return &Client{
		sc:            sc,
		jitter:        rand.New(rand.NewSource(time.Now().UnixNano())),
		retryBackoffs: defaultRetryBackoffs,
	}
}

// Record writes ev to both layers. Audit failures never propagate: the
// durable layer logs its errors and returns, so an audit outage can never
// break the mutating operation it documents.
func (c *Client) Record(ctx context.Context, ev Event) {
	if ev.When.IsZero() {
		ev.When = time.Now().UTC()
	}
	logger := log.FromContext(ctx)
	logger.Info("audit",
		"action", ev.Action,
		"who", ev.Who,
		"role", ev.Role,
		"target", ev.Target,
		"targetTeam", ev.TargetTeam,
		"capability", ev.Capability,
		"before", ev.Before,
		"after", ev.After,
		"detail", ev.Detail,
	)
	if c.sc == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.appendJSONL(ctx, ev); err != nil {
		logger.Error(err, "audit: durable layer failed; event logged only", "action", ev.Action, "who", ev.Who)
	}
}

const jsonlPrefix = "audit/"

// appendJSONL appends ev as one JSON line to audit/<YYYY-MM-DD>.jsonl
// (UTC date, #1220 §8 object form). Read-modify-write with ETag-optimistic
// concurrency: conflicting writers (a second controller process, or a
// manual edit) are retried up to len(retryBackoffs) times.
func (c *Client) appendJSONL(ctx context.Context, ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal event: %w", err)
	}

	key := jsonlPrefix + ev.When.UTC().Format("2006-01-02") + ".jsonl"
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := c.retryBackoffs[attempt-1]
			delay += time.Duration(c.jitter.Int63n(int64(delay/2) + 1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		var existing []byte
		var etag string
		data, gerr := c.sc.GetObject(ctx, key)
		switch {
		case gerr == nil:
			existing = data
			if meta, merr := c.sc.StatMeta(ctx, key); merr == nil {
				etag = meta.ETag
			}
		case errors.Is(gerr, os.ErrNotExist):
			// New day / new object.
		default:
			return fmt.Errorf("audit: get %s: %w", key, gerr)
		}

		content := make([]byte, 0, len(existing)+len(line)+1)
		content = append(content, existing...)
		content = append(content, line...)
		content = append(content, '\n')

		if etag == "" {
			// Either a fresh object or a backend that reports no ETag: a
			// conditional write is impossible, so a plain put is the best
			// available. (MinIO reports MD5 ETags for single-part objects,
			// so the conditional path is the norm in practice.)
			if err := c.sc.PutObject(ctx, key, content); err != nil {
				return fmt.Errorf("audit: put %s: %w", key, err)
			}
			return nil
		}
		if err := c.sc.PutObjectIfMatch(ctx, key, content, etag); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) && attempt < len(c.retryBackoffs) {
				continue // someone wrote concurrently; re-read and retry
			}
			return fmt.Errorf("audit: put-if-match %s: %w", key, err)
		}
		return nil
	}
}
