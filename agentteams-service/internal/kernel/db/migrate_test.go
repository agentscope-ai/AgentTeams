package db

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/migrations"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixedNow pins Runner.Now so lease timestamps are deterministic.
var fixedNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// fixedNowFn returns fixedNow for Runner.Now.
func fixedNowFn() time.Time { return fixedNow }

// migrationFile is the frozen naming rule: NNNN-<slug>.sql.
var migrationFile = regexp.MustCompile(`^\d{4}-.+\.sql$`)

// Exact lease/bookkeeping statements pinned from migrate.go (a contract
// change in the runner breaks these tests on purpose).
const (
	leaseTableSQL   = "CREATE TABLE IF NOT EXISTS upgrade_leases"
	insertLeaseSQL  = "INSERT INTO upgrade_leases (scope, holder_id, lease_until, version, updated_at) VALUES (?, ?, ?, 1, ?)"
	readLeaseSQL    = "SELECT lease_until, version FROM upgrade_leases WHERE scope = ?"
	stealLeaseSQL   = "UPDATE upgrade_leases SET holder_id = ?, lease_until = ?, version = version + 1, updated_at = ? WHERE scope = ? AND version = ?"
	releaseLeaseSQL = "DELETE FROM upgrade_leases WHERE scope = ? AND holder_id = ? AND version = ?"
	appliedSQL      = "SELECT version FROM schema_migrations"
	recordSQL       = "INSERT INTO schema_migrations (version, filename, applied_at) VALUES (?, ?, ?)"
)

func TestMigrationsEmbedded(t *testing.T) {
	res, err := migrations.SQLFiles(migrations.ResSQL, "res")
	if err != nil {
		t.Fatalf("SQLFiles res: %v", err)
	}
	ops, err := migrations.SQLFiles(migrations.OpsSQL, "ops")
	if err != nil {
		t.Fatalf("SQLFiles ops: %v", err)
	}
	for _, ns := range [][]string{res, ops} {
		for _, f := range ns {
			if !migrationFile.MatchString(f) {
				t.Errorf("file %q does not match NNNN-<slug>.sql", f)
			}
		}
		for i := 1; i < len(ns); i++ {
			if !(ns[i-1] < ns[i]) {
				t.Errorf("files not in apply order: %v", ns)
			}
		}
	}
	if len(res) != 1 || res[0] != "0001-workers.sql" {
		t.Errorf("res files = %v, want [0001-workers.sql]", res)
	}
	if len(ops) != 5 {
		t.Errorf("ops files = %v, want 5", ops)
	}

	if _, err := migrations.SQL(migrations.ResSQL, "res", "0001-workers.sql"); err != nil {
		t.Errorf("migrations.SQL: %v", err)
	}
	if v := migrations.Version("0001-workers.sql"); v != "0001" {
		t.Errorf("Version(0001-workers.sql) = %q, want 0001", v)
	}
	if _, err := migrations.SQLFiles(migrations.ResSQL, "nope"); err == nil {
		t.Error("SQLFiles(unknown schema): want error, got nil")
	}

	// The deprecated 0001_bootstrap.sql (goose era) stays on disk for
	// reference but is embedded nowhere: O4 never issues CREATE DATABASE.
	if _, err := migrations.ResSQL.Open("0001_bootstrap.sql"); err == nil {
		t.Error("0001_bootstrap.sql must not be embedded in res")
	}
	if _, err := migrations.OpsSQL.Open("0001_bootstrap.sql"); err == nil {
		t.Error("0001_bootstrap.sql must not be embedded in ops")
	}
}

// freshNamespace scripts a not-yet-migrated schema: bookkeeping table,
// empty version read, then one transaction per frozen file.
func freshNamespace(mock sqlmock.Sqlmock, tables ...string) {
	mock.ExpectExec(`(?i)CREATE TABLE IF NOT EXISTS schema_migrations`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(appliedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}))
	for _, table := range tables {
		mock.ExpectBegin()
		mock.ExpectExec(`(?i)CREATE TABLE IF NOT EXISTS ` + regexp.QuoteMeta(table)).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta(recordSQL)).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
	}
}

func freshOpsNamespace(mock sqlmock.Sqlmock) {
	freshNamespace(mock, "outbox", "audit_events", "idempotency_keys", "upgrade_leases")
	mock.ExpectBegin()
	mock.ExpectExec("(?i)ALTER TABLE idempotency_keys").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(recordSQL)).
		WillReturnResult(sqlmock.NewResult(5, 1))
	mock.ExpectCommit()
}

func newTestRunner(resDB, opsDB *sql.DB) *Runner {
	return &Runner{Res: resDB, Ops: opsDB, Holder: "test-holder", Now: fixedNowFn, Log: testLogger()}
}

// TestMigrateFresh runs the full first-boot flow on two empty schemas.
func TestMigrateFresh(t *testing.T) {
	resDB, resMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer opsDB.Close()

	// ops: ensure lease table, acquire via INSERT (free → version 1),
	// apply the five frozen files, release the lease.
	opsMock.ExpectExec(`(?i)` + regexp.QuoteMeta(leaseTableSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectExec(regexp.QuoteMeta(insertLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	freshOpsNamespace(opsMock)
	opsMock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// res: apply the single frozen file.
	freshNamespace(resMock, "workers")

	if err := newTestRunner(resDB, opsDB).Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := resMock.ExpectationsWereMet(); err != nil {
		t.Errorf("res expectations: %v", err)
	}
	if err := opsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("ops expectations: %v", err)
	}
}

// TestMigrateIdempotent: a fully migrated schema re-applies nothing.
func TestMigrateIdempotent(t *testing.T) {
	resDB, resMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer opsDB.Close()

	opsMock.ExpectExec(`(?i)` + regexp.QuoteMeta(leaseTableSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectExec(regexp.QuoteMeta(insertLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	opsMock.ExpectExec(`(?i)CREATE TABLE IF NOT EXISTS schema_migrations`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectQuery(regexp.QuoteMeta(appliedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).
			AddRow("0001").AddRow("0002").AddRow("0003").AddRow("0004").AddRow("0024"))
	opsMock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	resMock.ExpectExec(`(?i)CREATE TABLE IF NOT EXISTS schema_migrations`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	resMock.ExpectQuery(regexp.QuoteMeta(appliedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("0001"))

	if err := newTestRunner(resDB, opsDB).Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// ExpectationsWereMet also proves no DDL ran beyond the scripted set.
	if err := resMock.ExpectationsWereMet(); err != nil {
		t.Errorf("res expectations: %v", err)
	}
	if err := opsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("ops expectations: %v", err)
	}
}

func duplicateLeaseError() error {
	return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'kernel:upgrade' for key 'upgrade_leases.PRIMARY'"}
}

// TestMigrateLeaseHeld: a live lease held by another instance blocks.
func TestMigrateLeaseHeld(t *testing.T) {
	resDB, resMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer opsDB.Close()

	opsMock.ExpectExec(`(?i)` + regexp.QuoteMeta(leaseTableSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectExec(regexp.QuoteMeta(insertLeaseSQL)).
		WillReturnError(duplicateLeaseError())
	// The lease is still live (until > now): no steal, no release.
	opsMock.ExpectQuery(regexp.QuoteMeta(readLeaseSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"lease_until", "version"}).
			AddRow(fixedNow.Add(20*time.Second), int64(3)))

	err = newTestRunner(resDB, opsDB).Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another instance is upgrading") {
		t.Fatalf("Migrate with live lease = %v, want 'another instance is upgrading'", err)
	}
	if err := opsMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ops expectations: %v", err)
	}
	if err := resMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("res must be untouched: %v", err)
	}
}

// TestMigrateStealExpiredLease: an expired lease is stolen under the
// optimistic version check and migration proceeds on version+1.
func TestMigrateStealExpiredLease(t *testing.T) {
	resDB, resMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer opsDB.Close()

	opsMock.ExpectExec(`(?i)` + regexp.QuoteMeta(leaseTableSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectExec(regexp.QuoteMeta(insertLeaseSQL)).
		WillReturnError(duplicateLeaseError())
	opsMock.ExpectQuery(regexp.QuoteMeta(readLeaseSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"lease_until", "version"}).
			AddRow(fixedNow.Add(-time.Second), int64(3)))
	opsMock.ExpectExec(regexp.QuoteMeta(stealLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	freshOpsNamespace(opsMock)
	opsMock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	freshNamespace(resMock, "workers")

	if err := newTestRunner(resDB, opsDB).Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := resMock.ExpectationsWereMet(); err != nil {
		t.Errorf("res expectations: %v", err)
	}
	if err := opsMock.ExpectationsWereMet(); err != nil {
		t.Errorf("ops expectations: %v", err)
	}
}

// TestMigrateLostLeaseRace: the steal UPDATE matched 0 rows (a third
// instance won the race) → fail instead of migrating concurrently.
func TestMigrateLostLeaseRace(t *testing.T) {
	resDB, resMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer resDB.Close()
	opsDB, opsMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer opsDB.Close()

	opsMock.ExpectExec(`(?i)` + regexp.QuoteMeta(leaseTableSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	opsMock.ExpectExec(regexp.QuoteMeta(insertLeaseSQL)).
		WillReturnError(duplicateLeaseError())
	opsMock.ExpectQuery(regexp.QuoteMeta(readLeaseSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"lease_until", "version"}).
			AddRow(fixedNow.Add(-time.Second), int64(3)))
	opsMock.ExpectExec(regexp.QuoteMeta(stealLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = newTestRunner(resDB, opsDB).Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lost lease race") {
		t.Fatalf("Migrate with lost steal = %v, want 'lost lease race'", err)
	}
	if err := opsMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ops expectations: %v", err)
	}
	if err := resMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("res must be untouched: %v", err)
	}
}
