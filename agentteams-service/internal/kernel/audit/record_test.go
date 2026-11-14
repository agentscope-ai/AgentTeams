package audit

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

var auditNow = time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

func TestRecordAllow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rv := int64(4)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WithArgs(
		"req-1",
		nil, // trace_id
		"human",
		"human-1",
		"team",
		"team-1",
		"team-uid",
		"project-uid",
		"task-1",
		"worker.update",
		rv,
		"allow",
		nil,
		"before-digest",
		"after-digest",
		"service",
		"jti-1",
		"client-1",
		auditNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Record(context.Background(), tx, Event{
		RequestID:       "req-1",
		ActorType:       "human",
		ActorID:         "human-1",
		OnBehalfOfType:  "team",
		OnBehalfOfID:    "team-1",
		TeamUID:         "team-uid",
		ProjectUID:      "project-uid",
		TaskID:          "task-1",
		Action:          "worker.update",
		ResourceVersion: &rv,
		BeforeDigest:    "before-digest",
		AfterDigest:     "after-digest",
		TokenJTI:        "jti-1",
		Client:          "client-1",
	}, auditNow)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordDefaultsDecisionAndNullOptionals(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WithArgs(
		"req-2",
		nil,
		"system",
		"service-account",
		nil,
		nil,
		nil,
		nil,
		nil,
		"worker.create",
		nil,
		"allow",
		nil,
		nil,
		nil,
		"service",
		nil,
		nil,
		auditNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Record(context.Background(), tx, Event{
		RequestID: "req-2",
		ActorType: "system",
		ActorID:   "service-account",
		Action:    "worker.create",
	}, auditNow)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordDeny(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WithArgs(
		"req-3", nil, "worker", "worker-a", nil, nil, nil, nil, nil,
		"worker.delete", nil, "deny", "forbidden", nil, nil, "service", nil, nil, auditNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Record(context.Background(), tx, Event{
		RequestID:  "req-3",
		ActorType:  "worker",
		ActorID:    "worker-a",
		Action:     "worker.delete",
		Decision:   "deny",
		DenyReason: "forbidden",
	}, auditNow)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordNilTransaction(t *testing.T) {
	err := Record(context.Background(), nil, Event{RequestID: "req-4", Action: "worker.create"}, auditNow)
	if err == nil || !strings.Contains(err.Error(), "nil transaction") {
		t.Fatalf("Record(nil tx) = %v, want nil transaction error", err)
	}
}

func TestRecordExecError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertSQL)).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = Record(context.Background(), tx, Event{RequestID: "req-5", ActorType: "human", ActorID: "u", Action: "worker.create"}, auditNow)
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("Record(exec error) = %v, want record error", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
