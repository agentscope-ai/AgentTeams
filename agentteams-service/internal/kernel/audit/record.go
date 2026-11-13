// Package audit writes ops.audit_events rows inside the caller's business
// transaction (58 §2/§4.3; 15 号审计模型). Only the service-originated
// decision stream lives here; controller/agent events are not recorded.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Event is one service audit record. Empty optional fields are stored as
// NULL; Decision defaults to "allow" because handlers call Record only for
// the decision they actually took.
type Event struct {
	RequestID       string
	ActorType       string
	ActorID         string
	OnBehalfOfType  string
	OnBehalfOfID    string
	TeamUID         string
	ProjectUID      string
	TaskID          string
	Action          string
	Decision        string
	DenyReason      string
	BeforeDigest    string
	AfterDigest     string
	TokenJTI        string
	Client          string
	ResourceVersion *int64
}

// sourceService is frozen by 58 §4.3 (the only source this service writes).
const sourceService = "service"

// insertSQL names every column. trace_id is inserted as NULL: the service
// audit model does not carry a trace id yet (58 §4.3 keeps it nullable).
const insertSQL = "INSERT INTO ops.audit_events (request_id, trace_id, actor_type, actor_id, on_behalf_of_type, on_behalf_of_id, team_uid, project_uid, task_id, action, resource_version, decision, deny_reason, before_digest, after_digest, source, token_jti, client, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

// Record inserts one audit row through tx so it commits or rolls back with
// the mutation it describes.
func Record(ctx context.Context, tx *sql.Tx, e Event, now time.Time) error {
	if tx == nil {
		return errors.New("audit: nil transaction")
	}
	decision := e.Decision
	if decision == "" {
		decision = "allow"
	}
	// Every string is cut to its column width (utf8mb4 characters, not
	// bytes). These values include client-controlled input (X-Request-Id,
	// User-Agent); without the cut a long header would make the business
	// transaction fail with 1406 and turn a create into a 500.
	if _, err := tx.ExecContext(ctx, insertSQL,
		cut(e.RequestID, 128),
		nil, // trace_id
		cut(e.ActorType, 16),
		cut(e.ActorID, 128),
		nullableCut(e.OnBehalfOfType, 16),
		nullableCut(e.OnBehalfOfID, 128),
		nullableCut(e.TeamUID, 26),
		nullableCut(e.ProjectUID, 26),
		nullableCut(e.TaskID, 64),
		cut(e.Action, 128),
		nullableInt64(e.ResourceVersion),
		cut(decision, 32),
		nullableCut(e.DenyReason, 255),
		nullableCut(e.BeforeDigest, 64),
		nullableCut(e.AfterDigest, 64),
		sourceService,
		nullableCut(e.TokenJTI, 128),
		nullableCut(e.Client, 191),
		now.UTC(),
	); err != nil {
		return fmt.Errorf("audit: record %q for request %q: %w", e.Action, e.RequestID, err)
	}
	return nil
}

// nullableCut is nullable over a rune-truncated value.
func nullableCut(s string, max int) any {
	if s == "" {
		return nil
	}
	return cut(s, max)
}

// cut truncates s to at most max UTF-8 characters (the DDL widths are
// character counts, not bytes).
func cut(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// nullableInt64 maps a nil pointer to SQL NULL.
func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
