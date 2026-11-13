package outbox

// The ops.outbox dispatcher (58 §4.2, 54 §4.4).
//
// It polls ops.outbox for pending/failed events, hands each one to the
// controller adapter and records the outcome, so a business transaction and
// its controller instruction never diverge: the row was enqueued in the
// business transaction (enqueue.go), delivery is retried here with
// exponential backoff, and an exhausted retry ends in the dead state instead
// of being silently dropped (54 §4.4).
//
// Single-instance design: R1 runs exactly one dispatcher, so events are
// claimed implicitly by the SELECT and a duplicated delivery can only come
// from a crash between the controller call and the status update. The
// controller is idempotent on (name, generation), which makes that
// redelivery safe. Multi-instance claiming (FOR UPDATE SKIP LOCKED or a
// lease on ops.outbox) is deliberately deferred to R2: seekdb 5.7 does not
// support SKIP LOCKED, and R1 has a single service instance by design.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/adapter"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
)

const (
	// eventProvision, eventUpdate and eventDeprovision are the frozen
	// ops.outbox.event_type values (58 §4.2).
	eventProvision   = "provision"
	eventUpdate      = "update"
	eventSync        = "sync"
	eventDeprovision = "deprovision"

	// statusPending, statusPublished, statusFailed and statusDead are the
	// frozen ops.outbox.status values (58 §4.2).
	statusPending   = "pending"
	statusPublished = "published"
	statusFailed    = "failed"
	statusDead      = "dead"

	// defaultOutboxBatch bounds one poll; a full batch is drained before
	// the next tick.
	defaultOutboxBatch = 20
)

// EventClient is the delivery surface the dispatcher needs. *adapter.Client
// is the production implementation.
type EventClient interface {
	Provision(ctx context.Context, name string, generation int64, spec json.RawMessage, incarnation string) error
	Deprovision(ctx context.Context, name string, generation int64, incarnation string) error
}

// Compile-time contract: the controller adapter satisfies EventClient.
var _ EventClient = (*adapter.Client)(nil)

// Dispatcher delivers ops.outbox events to the controller.
type Dispatcher struct {
	// DB is the ops pool (the dispatcher reads and writes ops.outbox
	// outside any business transaction).
	DB *sql.DB
	// Client is the controller adapter; nil is a wiring error.
	Client EventClient
	// Interval is the poll period; 0 -> config.DefaultOutboxInterval.
	Interval time.Duration
	// Now is the dispatcher clock (attempt timestamps and backoff); nil ->
	// time.Now. All times are UTC.
	Now func() time.Time
	// Log is the dispatcher logger; nil -> slog.Default().
	Log *slog.Logger
	// MaxAttempts is the dead-letter threshold; 0 ->
	// config.MaxOutboxAttempts.
	MaxAttempts int
	// Batch is the per-tick fetch size; 0 -> defaultOutboxBatch.
	Batch int
}

// Run ticks immediately, then on every Interval until ctx is cancelled.
// Cancellation is a clean shutdown: Run returns without an error.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval())
	defer ticker.Stop()

	d.runTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runTick(ctx)
		}
	}
}

// runTick runs one Tick and logs the outcome. A cancelled context is not an
// error worth a log line: the tick was interrupted by shutdown.
func (d *Dispatcher) runTick(ctx context.Context) {
	published, err := d.Tick(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		d.logger().Error("outbox: dispatch tick failed", "err", err)
		return
	}
	if published > 0 {
		d.logger().Info("outbox: dispatched events", "published", published)
	}
}

// pendingEvent is one selected ops.outbox row.
type pendingEvent struct {
	id         int64
	workerName string
	generation int64
	eventType  string
	payload    []byte
	attempts   int64
}

const pendingColumns = "id, worker_name, generation, event_type, payload_json, attempts"

// selectPendingSQL fetches deliverable events in id order. next_attempt_at
// NULL means "never attempted"; a future value is a backoff not yet due.
// No FOR UPDATE SKIP LOCKED: seekdb 5.7 lacks it and R1 is single-instance
// (see the package comment).
const selectPendingSQL = "SELECT " + pendingColumns + " FROM ops.outbox WHERE status IN ('" + statusPending + "', '" + statusFailed + "') AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY id LIMIT ?"

const (
	markPublishedSQL = "UPDATE ops.outbox SET status = '" + statusPublished + "', attempts = attempts + 1, published_at = ?, updated_at = ? WHERE id = ?"
	markFailedSQL    = "UPDATE ops.outbox SET status = '" + statusFailed + "', attempts = attempts + 1, next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ?"
	markDeadSQL      = "UPDATE ops.outbox SET status = '" + statusDead + "', attempts = attempts + 1, last_error = ?, updated_at = ? WHERE id = ?"
)

// maxLastError is the frozen ops.outbox.last_error width (VARCHAR(1024)).
const maxLastError = 1024

// Tick delivers at most Batch events and reports how many were published.
// One failing delivery is recorded (failed or dead) and never aborts the
// rest of the batch; a bookkeeping failure is returned because the state on
// disk is then unknown.
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	if d == nil || d.DB == nil {
		return 0, errors.New("outbox: dispatcher has no ops pool")
	}
	if d.Client == nil {
		return 0, errors.New("outbox: dispatcher has no event client")
	}
	now := d.now()
	rows, err := d.DB.QueryContext(ctx, selectPendingSQL, now, int64(d.batch()))
	if err != nil {
		return 0, fmt.Errorf("outbox: select deliverable events: %w", err)
	}

	var batch []pendingEvent
	for rows.Next() {
		var ev pendingEvent
		if err := rows.Scan(&ev.id, &ev.workerName, &ev.generation, &ev.eventType, &ev.payload, &ev.attempts); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("outbox: scan deliverable event: %w", err)
		}
		batch = append(batch, ev)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("outbox: read deliverable events: %w", err)
	}
	// Close before the updates: the pool must not keep the result set (and
	// its connection) held while the dispatcher writes outcomes back.
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("outbox: close deliverable events: %w", err)
	}

	published := 0
	for _, ev := range batch {
		ok, err := d.dispatch(ctx, ev, now)
		if err != nil {
			return published, err
		}
		if ok {
			published++
		}
	}
	return published, nil
}

// dispatch delivers one event and records the outcome. The returned bool is
// "published"; the error is reserved for a failed status update, because
// that is the only failure the caller must surface.
func (d *Dispatcher) dispatch(ctx context.Context, ev pendingEvent, now time.Time) (bool, error) {
	switch ev.eventType {
	case eventProvision, eventUpdate, eventSync, eventDeprovision:
	default:
		// An unknown event type can never become deliverable: dead-letter it
		// immediately instead of burning the retry budget.
		return false, d.markDead(ctx, ev, now, fmt.Errorf("unknown event type %q", ev.eventType))
	}

	payload, err := decodePayload(ev.payload)
	if err != nil {
		// A payload that does not decode is equally undeliverable (it cannot
		// turn into a valid controller request).
		return false, d.markDead(ctx, ev, now, err)
	}

	deliverErr := d.deliver(ctx, ev, payload)
	if deliverErr == nil {
		return true, d.markPublished(ctx, ev, now)
	}
	// Deprovisioning a worker the controller no longer knows has already
	// achieved the event's goal (idempotent delete).
	if errors.Is(deliverErr, adapter.ErrNotFound) && ev.eventType == eventDeprovision {
		return true, d.markPublished(ctx, ev, now)
	}

	attempts := ev.attempts + 1
	if attempts >= int64(d.maxAttempts()) {
		d.logger().Warn("outbox: event exhausted its retries",
			"id", ev.id, "worker", ev.workerName, "event", ev.eventType, "attempts", attempts, "err", deliverErr)
		return false, d.markDead(ctx, ev, now, deliverErr)
	}
	d.logger().Warn("outbox: event delivery failed; retrying",
		"id", ev.id, "worker", ev.workerName, "event", ev.eventType, "attempts", attempts,
		"next_attempt_at", now.Add(backoff(attempts)), "err", deliverErr)
	return false, d.markFailed(ctx, ev, now, attempts, deliverErr)
}

// payload is the event body written by enqueue.go: the worker identity plus
// (for provision/update) the canonical spec. worker_name/generation are
// taken from the row, not the payload, so a malformed body cannot retarget
// the delivery.
type payload struct {
	Name        string          `json:"name"`
	Generation  int64           `json:"generation"`
	Incarnation string          `json:"incarnation,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
}

func decodePayload(raw []byte) (payload, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return payload{}, fmt.Errorf("decode payload: %w", err)
	}
	return p, nil
}

// deliver performs the controller call for one event.
func (d *Dispatcher) deliver(ctx context.Context, ev pendingEvent, p payload) error {
	switch ev.eventType {
	case eventProvision, eventUpdate:
		return d.Client.Provision(ctx, ev.workerName, ev.generation, p.Spec, p.Incarnation)
	case eventSync:
		// Generation/incarnation bookkeeping only: the controller already
		// applied the proxied write, so no spec may be sent.
		return d.Client.Provision(ctx, ev.workerName, ev.generation, nil, p.Incarnation)
	case eventDeprovision:
		return d.Client.Deprovision(ctx, ev.workerName, ev.generation, p.Incarnation)
	default:
		return fmt.Errorf("unknown event type %q", ev.eventType)
	}
}

func (d *Dispatcher) markPublished(ctx context.Context, ev pendingEvent, now time.Time) error {
	if _, err := d.DB.ExecContext(ctx, markPublishedSQL, now, now, ev.id); err != nil {
		return fmt.Errorf("outbox: mark event %d published: %w", ev.id, err)
	}
	return nil
}

func (d *Dispatcher) markFailed(ctx context.Context, ev pendingEvent, now time.Time, attempts int64, cause error) error {
	next := now.Add(backoff(attempts))
	if _, err := d.DB.ExecContext(ctx, markFailedSQL, next, truncateLastError(cause), now, ev.id); err != nil {
		return fmt.Errorf("outbox: mark event %d failed: %w", ev.id, err)
	}
	return nil
}

func (d *Dispatcher) markDead(ctx context.Context, ev pendingEvent, now time.Time, cause error) error {
	if _, err := d.DB.ExecContext(ctx, markDeadSQL, truncateLastError(cause), now, ev.id); err != nil {
		return fmt.Errorf("outbox: mark event %d dead: %w", ev.id, err)
	}
	return nil
}

// backoff returns the delay after the n-th failed attempt:
// OutboxBackoffBase * 2^(n-1), capped at OutboxBackoffCap (54 §4.4). The
// base and the cap come from the kernel config so there is one numeric
// source for the retry policy.
func backoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := config.OutboxBackoffBase
	for i := int64(1); i < attempt; i++ {
		if d >= config.OutboxBackoffCap/2 {
			return config.OutboxBackoffCap
		}
		d *= 2
	}
	if d > config.OutboxBackoffCap {
		return config.OutboxBackoffCap
	}
	return d
}

// truncateLastError bounds the message at the frozen last_error width
// (VARCHAR(1024) counts characters, so the truncation is rune-based).
func truncateLastError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= maxLastError {
		return msg
	}
	runes := []rune(msg)
	if len(runes) <= maxLastError {
		return msg
	}
	return string(runes[:maxLastError])
}

// --- configured defaults ---

func (d *Dispatcher) interval() time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return config.DefaultOutboxInterval
}

func (d *Dispatcher) maxAttempts() int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return config.MaxOutboxAttempts
}

func (d *Dispatcher) batch() int {
	if d.Batch > 0 {
		return d.Batch
	}
	return defaultOutboxBatch
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
