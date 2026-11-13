package idem

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

var idemNow = time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

func newTestStore(t *testing.T) (*SQLStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &SQLStore{DB: db, Now: func() time.Time { return idemNow }}, mock
}

// encodedBody mirrors Put's storage encoding: the response bytes are
// wrapped in a JSON string so MySQL/seekdb cannot normalise them.
func encodedBody(t *testing.T, body []byte) []byte {
	t.Helper()
	b, err := json.Marshal(string(body))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestGetHit(t *testing.T) {
	store, mock := newTestStore(t)
	expires := idemNow.Add(24 * time.Hour)
	// Whitespace and key order must survive verbatim (a JSON object column
	// would normalise them; the column stores a JSON string instead).
	body := []byte("{\n  \"b\": 2,\n  \"a\": 1\n}")
	mock.ExpectQuery(regexp.QuoteMeta(selectSQL)).
		WithArgs(scopedKey("alice", "key-1"), idemNow).
		WillReturnRows(sqlmock.NewRows([]string{"worker_name", "request_hash", "response_status", "response_body", "created_at", "expires_at"}).
			AddRow("worker-a", "hash-a", 201, encodedBody(t, body), idemNow, expires))

	rec, err := store.Get(context.Background(), "alice", "key-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.WorkerName != "worker-a" || rec.Status != 201 {
		t.Errorf("Get record = %+v", rec)
	}
	if string(rec.Body) != string(body) {
		t.Errorf("Get body = %q, want byte-identical %q", rec.Body, body)
	}
	if !rec.CreatedAt.Equal(idemNow) || !rec.ExpiresAt.Equal(expires) {
		t.Errorf("Get timestamps = %v/%v", rec.CreatedAt, rec.ExpiresAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGetMissAndExpired(t *testing.T) {
	store, mock := newTestStore(t)
	// Expiry is enforced by the query (expires_at > ?), so an expired row
	// is indistinguishable from a missing one.
	if !strings.Contains(selectSQL, "expires_at > ?") {
		t.Fatalf("Get SQL must filter expiry in SQL: %s", selectSQL)
	}
	mock.ExpectQuery(regexp.QuoteMeta(selectSQL)).
		WithArgs(scopedKey("alice", "expired-key"), idemNow).
		WillReturnRows(sqlmock.NewRows([]string{"worker_name", "request_hash", "response_status", "response_body", "created_at", "expires_at"}))

	_, err := store.Get(context.Background(), "alice", "expired-key")
	if !errors.Is(err, resources.ErrIdemMiss) {
		t.Fatalf("Get expired error = %v, want ErrIdemMiss", err)
	}
}

func TestGetError(t *testing.T) {
	store, mock := newTestStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(selectSQL)).
		WithArgs(scopedKey("alice", "key-1"), idemNow).
		WillReturnError(errors.New("db down"))

	_, err := store.Get(context.Background(), "alice", "key-1")
	if err == nil || errors.Is(err, resources.ErrIdemMiss) {
		t.Fatalf("Get driver error = %v, want non-miss error", err)
	}
}

func TestPut(t *testing.T) {
	store, mock := newTestStore(t)
	body := []byte("{\n  \"name\": \"worker-a\"\n}")
	mock.ExpectExec(regexp.QuoteMeta(putSQL)).
		WithArgs(scopedKey("alice", "key-1"), "alice", "hash-a", "worker-a", 201, encodedBody(t, body), idemNow, idemNow.Add(24*time.Hour)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Put(context.Background(), "alice", "key-1", "hash-a", "worker-a", 201, body, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPutUpsertShapeAndEmptyBody(t *testing.T) {
	// The same key concurrent write is last-writer-wins.
	if !strings.Contains(putSQL, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("Put SQL must upsert: %s", putSQL)
	}
	store, mock := newTestStore(t)
	mock.ExpectExec(regexp.QuoteMeta(putSQL)).
		WithArgs(scopedKey("alice", "key-2"), "alice", "", "worker-b", 409, nil, idemNow, idemNow.Add(time.Hour)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Put(context.Background(), "alice", "key-2", "", "worker-b", 409, nil, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPutError(t *testing.T) {
	store, mock := newTestStore(t)
	mock.ExpectExec(regexp.QuoteMeta(putSQL)).
		WillReturnError(errors.New("db down"))

	if err := store.Put(context.Background(), "alice", "key-1", "hash-a", "worker-a", 201, []byte(`{}`), time.Hour); err == nil {
		t.Fatal("Put: want error, got nil")
	}
}

func TestDeleteExpired(t *testing.T) {
	store, mock := newTestStore(t)
	mock.ExpectExec(regexp.QuoteMeta(deleteExpiredSQL)).
		WithArgs(idemNow).
		WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := store.DeleteExpired(context.Background())
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
