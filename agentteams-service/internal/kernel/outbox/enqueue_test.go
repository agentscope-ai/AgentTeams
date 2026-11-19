package outbox

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

var enqueueNow = time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

func TestIdemKeyBounded(t *testing.T) {
	name := strings.Repeat("w", 253)
	key := IdemKey(name, 9223372036854775807, "deprovision", "")
	// 2 ("w:") + 32 (sha256 prefix) + 1 + 19 (max int64) + 1 + 11.
	if len(key) != 66 {
		t.Fatalf("len(IdemKey) = %d, want 66 (%q)", len(key), key)
	}
	if len(key) > 191 {
		t.Fatalf("IdemKey must fit VARCHAR(191), got %d", len(key))
	}
	if !strings.HasPrefix(key, "w:") {
		t.Fatalf("IdemKey = %q, want w: prefix", key)
	}
	if strings.Count(key, ":") != 3 {
		t.Fatalf("IdemKey = %q, want w:<hash>:<generation>:<type>", key)
	}
	if strings.Contains(key, name) {
		t.Fatal("IdemKey must not embed the raw worker name (VARCHAR(191) budget)")
	}
	if !strings.HasSuffix(key, ":9223372036854775807:deprovision") {
		t.Fatalf("IdemKey = %q, want generation/event suffix", key)
	}
}

// TestIdemKeyDistinguishesIncarnation pins the delete→create case: the same
// (name, generation, event type) in a new worker life must not collide with
// the deleted life's outbox row.
func TestIdemKeyDistinguishesIncarnation(t *testing.T) {
	a := IdemKey("worker-a", 1, "provision", "2026-11-13T00:00:00Z")
	b := IdemKey("worker-a", 1, "provision", "2026-11-13T01:00:00Z")
	if a == b {
		t.Fatalf("different incarnations share idem key %q", a)
	}
	if a == IdemKey("worker-a", 1, "provision", "") {
		t.Fatalf("incarnated key must differ from the legacy empty key")
	}
	if len(a) > 191 {
		t.Fatalf("len(IdemKey) = %d, want <= 191", len(a))
	}
}

func TestIdemKeyDistinguishesTuple(t *testing.T) {
	base := IdemKey("worker-a", 1, "provision", "")
	if base != IdemKey("worker-a", 1, "provision", "") {
		t.Fatal("IdemKey is not deterministic")
	}
	for _, other := range []string{
		IdemKey("worker-b", 1, "provision", ""),
		IdemKey("worker-a", 2, "provision", ""),
		IdemKey("worker-a", 1, "update", ""),
	} {
		if base == other {
			t.Fatalf("IdemKey collision: %q == %q", base, other)
		}
	}
}

func TestNewEventID(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id, err := newEventID()
		if err != nil {
			t.Fatalf("newEventID: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("len(eventID) = %d, want 32", len(id))
		}
		if strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("eventID = %q, want lowercase hex", id)
		}
		if seen[id] {
			t.Fatalf("duplicate eventID %q", id)
		}
		seen[id] = true
	}
}

func TestEnqueue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WithArgs(
		sqlmock.AnyArg(),
		IdemKey("worker-a", 2, "update", ""),
		"worker-a",
		int64(2),
		"update",
		"controller",
		[]byte(`{"name":"worker-a"}`),
		"pending",
		int64(0),
		enqueueNow,
		enqueueNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Enqueue(context.Background(), tx, Event{
		WorkerName: "worker-a",
		Generation: 2,
		EventType:  "update",
		Payload:    map[string]string{"name": "worker-a"},
	}, enqueueNow)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestEnqueueCustomDestination(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WithArgs(
		sqlmock.AnyArg(),
		IdemKey("worker-a", 1, "provision", ""),
		"worker-a",
		int64(1),
		"provision",
		"controller-b",
		[]byte(`{"name":"worker-a"}`),
		"pending",
		int64(0),
		enqueueNow,
		enqueueNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Enqueue(context.Background(), tx, Event{
		WorkerName:  "worker-a",
		Generation:  1,
		EventType:   "provision",
		Payload:     map[string]string{"name": "worker-a"},
		Destination: "controller-b",
	}, enqueueNow)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestEnqueueNilTransaction(t *testing.T) {
	err := Enqueue(context.Background(), nil, Event{WorkerName: "worker-a", Generation: 1, EventType: "provision"}, enqueueNow)
	if err == nil || !strings.Contains(err.Error(), "nil transaction") {
		t.Fatalf("Enqueue(nil tx) = %v, want nil transaction error", err)
	}
}

func TestEnqueueMarshalError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectRollback()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Enqueue(context.Background(), tx, Event{
		WorkerName: "worker-a",
		Generation: 1,
		EventType:  "provision",
		Payload:    func() {},
	}, enqueueNow)
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("Enqueue(unmarshalable payload) = %v, want marshal error", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestEnqueueExecError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).
		WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Enqueue(context.Background(), tx, Event{
		WorkerName: "worker-a",
		Generation: 1,
		EventType:  "provision",
		Payload:    map[string]int{"n": 1},
	}, enqueueNow)
	if err == nil || !strings.Contains(err.Error(), "enqueue") {
		t.Fatalf("Enqueue(exec error) = %v, want enqueue error", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
