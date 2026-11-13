// Forward-only migration runner for the res / ops schemas (58 §4, 55 §6).
//
// Design (O4, R1-2):
//   - No external migration tool. Each frozen .sql is a single DDL
//     statement (CREATE TABLE IF NOT EXISTS), applied in lexical NNN order.
//   - Cross-instance serialization via the kernel:upgrade row in
//     ops.upgrade_leases (58 §4.6; the kernel is the only lease owner,
//     55 号): INSERT to acquire, steal-if-expired with an optimistic
//     version check, DELETE only while still the holder.
//   - Per-schema bookkeeping in schema_migrations (runner-internal, not
//     part of the frozen DDL).
//   - No bootstrap CREATE DATABASE: the res/ops schemas are pre-created by
//     the installer; the runner works inside the schemas only.
//   - MySQL DDL auto-commits, so the DDL and its bookkeeping INSERT are not
//     one atomic step. That is safe because every R1 statement is
//     idempotent (IF NOT EXISTS): a crash between the two re-applies the
//     statement as a no-op and records it.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/migrations"
)

// UpgradeLeaseScope is the only upgrade lease the kernel holds.
const UpgradeLeaseScope = "kernel:upgrade"

// leaseTableDDL is byte-identical to ops/0004-upgrade-leases.sql. The first
// instance to start applies it out-of-band because the lease that
// serializes migration runs must exist before any migration can run; the
// versioned 0004 application afterwards is a no-op.
const leaseTableDDL = `CREATE TABLE IF NOT EXISTS upgrade_leases (
  scope VARCHAR(128) NOT NULL PRIMARY KEY,
  holder_id VARCHAR(128) NOT NULL,
  lease_until DATETIME(6) NOT NULL,
  version BIGINT NOT NULL DEFAULT 1,
  updated_at DATETIME(6) NOT NULL
)`

// bookkeepingDDL is the runner-internal version table, created per schema.
const bookkeepingDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version VARCHAR(16) NOT NULL PRIMARY KEY,
  filename VARCHAR(255) NOT NULL,
  applied_at DATETIME(6) NOT NULL
)`

// Runner applies the forward-only migrations for the res and ops schemas.
type Runner struct {
	Res    *sql.DB
	Ops    *sql.DB
	Holder string // lease holder identity (service instance)
	Now    func() time.Time
	Log    *slog.Logger
}

// Migrate opens its own connections (the long-lived service pools are not
// touched) and runs the migration flow.
func Migrate(ctx context.Context, resDSN, opsDSN string, holder string, lg *slog.Logger) error {
	resDB, err := Open(ctx, resDSN)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer resDB.Close()
	opsDB, err := Open(ctx, opsDSN)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer opsDB.Close()
	return (&Runner{Res: resDB, Ops: opsDB, Holder: holder, Now: time.Now, Log: lg}).Migrate(ctx)
}

// Migrate runs the whole flow. It is safe to call on every boot: already
// applied versions are no-ops.
func (r *Runner) Migrate(ctx context.Context) error {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Holder == "" {
		r.Holder = "agentteams-service"
	}

	if _, err := r.Ops.ExecContext(ctx, leaseTableDDL); err != nil {
		return fmt.Errorf("migrate: ensure upgrade_leases: %w", err)
	}

	version, err := r.acquireLease(ctx)
	if err != nil {
		return err
	}
	defer r.releaseLease(ctx, version)

	if err := r.applySchema(ctx, "res", r.Res, migrations.ResSQL); err != nil {
		return fmt.Errorf("migrate: res: %w", err)
	}
	if err := r.applySchema(ctx, "ops", r.Ops, migrations.OpsSQL); err != nil {
		return fmt.Errorf("migrate: ops: %w", err)
	}
	return nil
}

// acquireLease takes the kernel:upgrade lease. A successful INSERT means
// the lease was free; on duplicate key the existing row is checked —
// expired leases are stolen under an optimistic version check, live ones
// yield "another instance upgrading".
func (r *Runner) acquireLease(ctx context.Context) (int64, error) {
	now := r.Now()
	until := now.Add(config.LeaseTTL)

	_, err := r.Ops.ExecContext(ctx,
		"INSERT INTO upgrade_leases (scope, holder_id, lease_until, version, updated_at) VALUES (?, ?, ?, 1, ?)",
		UpgradeLeaseScope, r.Holder, until, now)
	if err == nil {
		return 1, nil
	}
	// *mysql.MySQLError: Error/Is use pointer receivers in
	// go-sql-driver v1.8.1, so the value type does not implement error
	// and errors.As would panic on a value target.
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlErrDuplicateEntry {
		return 0, fmt.Errorf("migrate: acquire lease: %w", err)
	}

	var heldUntil time.Time
	var current int64
	if err := r.Ops.QueryRowContext(ctx,
		"SELECT lease_until, version FROM upgrade_leases WHERE scope = ?", UpgradeLeaseScope,
	).Scan(&heldUntil, &current); err != nil {
		return 0, fmt.Errorf("migrate: read lease: %w", err)
	}
	if heldUntil.After(now) {
		return 0, fmt.Errorf("migrate: another instance is upgrading (lease %q held until %s)",
			UpgradeLeaseScope, heldUntil.UTC().Format(time.RFC3339))
	}

	res, err := r.Ops.ExecContext(ctx,
		"UPDATE upgrade_leases SET holder_id = ?, lease_until = ?, version = version + 1, updated_at = ? WHERE scope = ? AND version = ?",
		r.Holder, until, now, UpgradeLeaseScope, current)
	if err != nil {
		return 0, fmt.Errorf("migrate: steal expired lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errors.New("migrate: lost lease race (another instance stole the expired lease)")
	}
	return current + 1, nil
}

// releaseLease drops the lease only while we still hold it (holder +
// version match) — a stolen lease is never deleted.
func (r *Runner) releaseLease(ctx context.Context, version int64) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PingTimeout)
	defer cancel()
	if _, err := r.Ops.ExecContext(rctx,
		"DELETE FROM upgrade_leases WHERE scope = ? AND holder_id = ? AND version = ?",
		UpgradeLeaseScope, r.Holder, version); err != nil {
		r.Log.Warn("migration lease release failed", "scope", UpgradeLeaseScope, "err", err)
	}
}

// applySchema ensures the bookkeeping table, then applies every embedded
// migration that has not been recorded yet, in NNN order.
func (r *Runner) applySchema(ctx context.Context, name string, pool *sql.DB, fsys embed.FS) error {
	if _, err := pool.ExecContext(ctx, bookkeepingDDL); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	var applied map[string]bool
	rows, err := pool.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	applied = make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	files, err := migrations.SQLFiles(fsys, name)
	if err != nil {
		return err
	}
	for _, f := range files {
		v := migrations.Version(f)
		if applied[v] {
			continue
		}
		if err := r.applyOne(ctx, pool, fsys, name, f, v); err != nil {
			return err
		}
		r.Log.Info("migration applied", "schema", name, "version", v, "file", f)
	}
	return nil
}

// applyOne runs one migration statement and records its version. See the
// package doc for the DDL auto-commit note.
func (r *Runner) applyOne(ctx context.Context, pool *sql.DB, fsys embed.FS, schema, file, version string) error {
	stmt, err := migrations.SQL(fsys, schema, file)
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply %s: %w", file, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, string(stmt)); err != nil {
		return fmt.Errorf("apply %s: %w", file, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, filename, applied_at) VALUES (?, ?, ?)",
		version, file, r.Now()); err != nil {
		return fmt.Errorf("record %s: %w", file, err)
	}
	return tx.Commit()
}

const mysqlErrDuplicateEntry = 1062
