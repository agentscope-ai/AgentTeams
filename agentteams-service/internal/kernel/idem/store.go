// Package idem stores first responses for the Idempotency-Key header in
// ops.idempotency_keys. Get replays only unexpired records; Put
// writes/overwrites with a caller-supplied TTL (24h in R1). Expired rows
// are deleted by the outbox scanner, not here.
package idem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

// SQLStore is the MySQL implementation of resources.IdemStore.
type SQLStore struct {
	DB  *sql.DB
	Now func() time.Time // nil → time.Now; all times are stored in UTC
}

// NewSQLStore returns a store using DB.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

// Compile-time contract check (resources does not import idem, so no cycle).
var _ resources.IdemStore = (*SQLStore)(nil)

const selectSQL = "SELECT worker_name, request_hash, response_status, response_body, created_at, expires_at FROM ops.idempotency_keys WHERE idem_key = ? AND expires_at > ?"

// scopedKey binds the client key to the authenticated principal. The stored
// idem_key is a hash so the 128-char column cannot overflow and two callers
// can never share a replay row.
func scopedKey(principal, key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(principal+"\x00"+key)))
}

// Get returns the first response for key. A missing or expired row is
// resources.ErrIdemMiss (expiry is enforced in SQL, so an expired row never
// leaves the database).
func (s *SQLStore) Get(ctx context.Context, principal, key string) (*resources.IdemRecord, error) {
	var (
		rec  resources.IdemRecord
		body []byte
	)
	err := s.DB.QueryRowContext(ctx, selectSQL, scopedKey(principal, key), s.now()).
		Scan(&rec.WorkerName, &rec.RequestHash, &rec.Status, &body, &rec.CreatedAt, &rec.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("idem: key %q: %w", key, resources.ErrIdemMiss)
	}
	if err != nil {
		return nil, fmt.Errorf("idem: get key %q: %w", key, err)
	}
	// response_body holds the opaque first response encoded as a JSON
	// string: MySQL/seekdb normalize a JSON object (whitespace/key order),
	// which would break byte-identical replay, while a JSON string round
	// trips the exact bytes.
	if len(body) > 0 {
		var s string
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("idem: decode body for key %q: %w", key, err)
		}
		rec.Body = []byte(s)
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.ExpiresAt = rec.ExpiresAt.UTC()
	return &rec, nil
}

// putSQL upserts the record. A concurrent duplicate for the same key is
// resolved last-writer-wins: MySQL serialises the INSERT ... ON DUPLICATE
// KEY UPDATE, and the later row overwrites worker/status/body/expires_at.
// created_at keeps the first write's timestamp.
const putSQL = "INSERT INTO ops.idempotency_keys (idem_key, principal_id, request_hash, worker_name, response_status, response_body, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE principal_id = VALUES(principal_id), request_hash = VALUES(request_hash), worker_name = VALUES(worker_name), response_status = VALUES(response_status), response_body = VALUES(response_body), expires_at = VALUES(expires_at)"

// Put stores body as the first response for key, expiring after ttl. A
// non-empty body is stored as a JSON string (json.Marshal(string(body))):
// MySQL/seekdb normalise a JSON object's whitespace/key order, while a JSON
// string round-trips the exact bytes. An empty body is stored as SQL NULL
// (response_body is a nullable JSON column).
func (s *SQLStore) Put(ctx context.Context, principal, key, requestHash, workerName string, status int, body []byte, ttl time.Duration) error {
	now := s.now()
	var bodyArg any
	if len(body) > 0 {
		raw, err := json.Marshal(string(body))
		if err != nil {
			return fmt.Errorf("idem: encode body for key %q: %w", key, err)
		}
		bodyArg = raw
	}
	if _, err := s.DB.ExecContext(ctx, putSQL, scopedKey(principal, key), principal, requestHash, workerName, status, bodyArg, now, now.Add(ttl).UTC()); err != nil {
		return fmt.Errorf("idem: put key %q: %w", key, err)
	}
	return nil
}

// deleteExpiredSQL drops rows past their TTL. The cleanup rides
// the outbox scanner cadence (no separate background job); callers invoke it
// on the same interval.
const deleteExpiredSQL = "DELETE FROM ops.idempotency_keys WHERE expires_at <= ?"

// DeleteExpired removes expired idempotency rows and reports how many it
// deleted. It is safe to call concurrently with Put/Get: an expired row is
// already invisible to Get.
func (s *SQLStore) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, deleteExpiredSQL, s.now())
	if err != nil {
		return 0, fmt.Errorf("idem: delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("idem: delete expired rows affected: %w", err)
	}
	return n, nil
}

// now returns the store clock truncated to UTC.
func (s *SQLStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
