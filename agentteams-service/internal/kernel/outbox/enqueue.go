// Package outbox writes ops.outbox rows inside the caller's business
// transaction and, in dispatcher.go, delivers them to the
// controller with retry/backoff/dead-lettering.
package outbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Event is one pending controller instruction.
type Event struct {
	WorkerName  string
	Generation  int64
	EventType   string // provision | update | deprovision
	Payload     any
	Destination string // empty → "controller"
	// Incarnation distinguishes two lives of the same worker name. A
	// soft-deleted row frees its name, so delete→create legitimately
	// re-emits (name, 1, provision); without this component the unique
	// idem_key would reject the second life. Callers pass the row's
	// created_at; empty keeps the pre-incarnation key shape.
	Incarnation string
}

// IdemKey returns the ops.outbox.idem_key for (worker, generation, event
// type) plus the worker incarnation.
//
// Deviation from the documented key shape: the contract describes the key
// as name+generation+event_type. A 253-character worker name plus
// generation and event type reaches 284 characters, overflowing the
// frozen idem_key VARCHAR(191). We therefore hash the name to 32 hex
// characters:
//
//	"w:" + sha256hex(name)[:32] + ":" + generation + ":" + event_type
//	    (+ ":" + sha256hex(incarnation)[:16] when incarnation != "")
//
// which is at most 2+32+1+20+1+32+1+16 = 105 characters. The tuple keeps
// the (name, generation, event type) idempotency semantics for one
// incarnation; the incarnation component is what lets delete→create emit a
// fresh (name, 1, provision) without colliding with the deleted life.
func IdemKey(workerName string, generation int64, eventType, incarnation string) string {
	sum := sha256.Sum256([]byte(workerName))
	key := "w:" + hex.EncodeToString(sum[:])[:32] + ":" + strconv.FormatInt(generation, 10) + ":" + eventType
	if incarnation != "" {
		inc := sha256.Sum256([]byte(incarnation))
		key += ":" + hex.EncodeToString(inc[:])[:16]
	}
	return key
}

// insertSQL inserts the outbox row with the column set the dispatcher
// consumes. status/attempts start at pending/0; next_attempt_at,
// last_error and published_at stay NULL.
const insertSQL = "INSERT INTO ops.outbox (event_id, idem_key, worker_name, generation, event_type, destination, payload_json, status, attempts, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

// Enqueue inserts one outbox row through tx. The caller owns the
// transaction: a business row and its outbox event commit or roll back
// together.
func Enqueue(ctx context.Context, tx *sql.Tx, ev Event, now time.Time) error {
	if tx == nil {
		return errors.New("outbox: nil transaction")
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload for worker %q: %w", ev.WorkerName, err)
	}
	eventID, err := newEventID()
	if err != nil {
		return err
	}
	destination := ev.Destination
	if destination == "" {
		destination = "controller"
	}
	ts := now.UTC()
	if _, err := tx.ExecContext(ctx, insertSQL,
		eventID,
		IdemKey(ev.WorkerName, ev.Generation, ev.EventType, ev.Incarnation),
		ev.WorkerName,
		ev.Generation,
		ev.EventType,
		destination,
		payload,
		"pending",
		int64(0),
		ts,
		ts,
	); err != nil {
		return fmt.Errorf("outbox: enqueue %s for worker %q generation %d: %w", ev.EventType, ev.WorkerName, ev.Generation, err)
	}
	return nil
}

// newEventID returns 16 crypto/rand bytes as 32 hex characters
// (ops.outbox.event_id VARCHAR(64), unique).
func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("outbox: read random event id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
