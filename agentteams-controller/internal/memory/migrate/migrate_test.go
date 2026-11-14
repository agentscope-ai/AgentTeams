package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "memory-meta.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}

var expectedTables = []string{
	"memory_anchors",
	"memory_promotions",
	"memory_exports",
	"knowledge_nodes",
	"knowledge_edges",
	"memory_outbox",
	"storage_refs",
	"memory_export_jobs",
	"schema_version",
}

// tableNames lists the user tables, skipping SQLite-internal ones
// (e.g. sqlite_sequence once AUTOINCREMENT has been used).
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// versionRows lists the applied migration file names in version order.
func versionRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT version, name FROM schema_version ORDER BY version")
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var (
			v    int
			name string
		)
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// sameSorted reports whether a and b hold the same elements, ignoring
// order: the schema query returns names in SQLite's own order, so the
// comparison must not depend on the slice order.
func sameSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func TestMigrateEmptyDatabase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	v, err := Migrate(ctx, db, ProfileLite)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v != 8 {
		t.Fatalf("version = %d, want 8", v)
	}
	if got := tableNames(t, db); !sameSorted(got, expectedTables) {
		t.Fatalf("tables = %v, want %v", got, expectedTables)
	}
	rows := versionRows(t, db)
	if len(rows) != 8 || rows[0] != "0001_memory_anchors.sql" || rows[7] != "0008_memory_export_jobs.sql" {
		t.Fatalf("version rows = %v", rows)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	v1, err := Migrate(ctx, db, ProfileLite)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	names1 := tableNames(t, db)
	rows1 := versionRows(t, db)

	v2, err := Migrate(ctx, db, ProfileLite)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if v2 != v1 {
		t.Fatalf("version moved %d -> %d on re-run", v1, v2)
	}
	if !sameSorted(tableNames(t, db), names1) {
		t.Fatal("tables changed on re-run")
	}
	if !sameSorted(versionRows(t, db), rows1) {
		t.Fatal("version rows changed on re-run")
	}
}

func TestMigrateAppliesInVersionOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Deliberately out of order; the run must apply 1, 2, 3.
	ms := []Migration{
		{Version: 3, Name: "0003_c.sql", SQL: "CREATE TABLE m3 (id BIGINT AUTO_INCREMENT PRIMARY KEY)"},
		{Version: 1, Name: "0001_a.sql", SQL: "CREATE TABLE m1 (id BIGINT AUTO_INCREMENT PRIMARY KEY)"},
		{Version: 2, Name: "0002_b.sql", SQL: "CREATE TABLE m2 (id BIGINT AUTO_INCREMENT PRIMARY KEY)"},
	}
	v, err := MigrateWith(ctx, db, ProfileLite, ms)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v != 3 {
		t.Fatalf("version = %d, want 3", v)
	}
	if got := versionRows(t, db); len(got) != 3 || got[0] != "0001_a.sql" || got[1] != "0002_b.sql" || got[2] != "0003_c.sql" {
		t.Fatalf("version rows = %v", got)
	}

	// Re-running with only old versions is a no-op.
	v2, err := MigrateWith(ctx, db, ProfileLite, ms[:2])
	if err != nil {
		t.Fatalf("no-op migrate: %v", err)
	}
	if v2 != 3 {
		t.Fatalf("version = %d, want 3", v2)
	}

	// A duplicated version is rejected.
	if _, err := MigrateWith(ctx, db, ProfileLite, []Migration{
		{Version: 4, Name: "0004_a.sql", SQL: "CREATE TABLE m4 (id BIGINT AUTO_INCREMENT PRIMARY KEY)"},
		{Version: 4, Name: "0004_b.sql", SQL: "CREATE TABLE m4b (id BIGINT AUTO_INCREMENT PRIMARY KEY)"},
	}); err == nil {
		t.Fatal("a duplicated version must be rejected")
	}
}

func TestBrokenMigrationRollsBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	good := Migration{
		Version: 1,
		Name:    "0001_good.sql",
		SQL:     "CREATE TABLE good_table (\n  id BIGINT AUTO_INCREMENT PRIMARY KEY,\n  owner_type VARCHAR(16) NOT NULL\n)",
	}
	// Two primary keys: parses fine, invalid schema.
	bad := Migration{
		Version: 2,
		Name:    "0002_bad.sql",
		SQL:     "CREATE TABLE broken_table (\n  id BIGINT AUTO_INCREMENT PRIMARY KEY,\n  id BIGINT AUTO_INCREMENT PRIMARY KEY\n)",
	}

	if _, err := MigrateWith(ctx, db, ProfileLite, []Migration{good, bad}); err == nil {
		t.Fatal("the broken migration must fail")
	}

	// The version did not advance.
	v, err := CurrentVersion(ctx, db, ProfileLite)
	if err != nil {
		t.Fatalf("current version: %v", err)
	}
	if v != 0 {
		t.Fatalf("version = %d after a failed batch, want 0", v)
	}
	// Nothing from the batch survived.
	names := strings.Join(tableNames(t, db), ",")
	if strings.Contains(names, "good_table") || strings.Contains(names, "broken_table") {
		t.Fatalf("tables after rollback = %v, want none of the batch", names)
	}

	// A clean run after the failure still works.
	if v, err := Migrate(ctx, db, ProfileLite); err != nil || v != 8 {
		t.Fatalf("recovery migrate: v=%d err=%v", v, err)
	}
}

func TestStandardProfileShapesToMemSchema(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(ms) != 8 {
		t.Fatalf("embedded migrations = %d, want 8", len(ms))
	}

	for _, p := range []Profile{ProfileStandard, ProfileScale} {
		stmts, err := ShapeStatements(ms[0].SQL, p)
		if err != nil {
			t.Fatalf("%s: shape: %v", p.Name, err)
		}
		if len(stmts) != 1 {
			t.Fatalf("%s: 0001 shaped to %d statements, want 1", p.Name, len(stmts))
		}
		s := stmts[0]
		if !strings.Contains(s, "CREATE TABLE mem.memory_anchors (") {
			t.Fatalf("%s: table not qualified: %s", p.Name, head(s))
		}
		// The SQL family keeps the canonical DDL verbatim.
		if !strings.Contains(s, "BIGINT AUTO_INCREMENT PRIMARY KEY") ||
			!strings.Contains(s, "UNIQUE KEY uq_anchor (owner_type, owner_id)") ||
			strings.Contains(s, "IF NOT EXISTS") {
			t.Fatalf("%s: dialect mangled:\n%s", p.Name, s)
		}
	}

	// standard and scale are the same shape (same DDL, same schema).
	a, err := ShapeStatements(ms[0].SQL, ProfileStandard)
	if err != nil {
		t.Fatalf("shape standard: %v", err)
	}
	b, err := ShapeStatements(ms[0].SQL, ProfileScale)
	if err != nil {
		t.Fatalf("shape scale: %v", err)
	}
	if a[0] != b[0] {
		t.Fatalf("standard and scale shapes diverge:\n%s\nvs\n%s", a[0], b[0])
	}
}

func TestLiteProfileShapesToSQLite(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	stmts, err := ShapeStatements(ms[0].SQL, ProfileLite)
	if err != nil {
		t.Fatalf("shape 0001: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("0001 shaped to %d statements, want table + unique index", len(stmts))
	}
	if !strings.Contains(stmts[0], "CREATE TABLE IF NOT EXISTS memory_anchors (") ||
		!strings.Contains(stmts[0], "id INTEGER PRIMARY KEY AUTOINCREMENT") ||
		strings.Contains(stmts[0], "AUTO_INCREMENT") ||
		strings.Contains(stmts[0], "VARCHAR") {
		t.Fatalf("0001 table shape wrong:\n%s", stmts[0])
	}
	if stmts[1] != "CREATE UNIQUE INDEX IF NOT EXISTS uq_anchor ON memory_anchors (owner_type, owner_id)" {
		t.Fatalf("0001 index shape wrong:\n%s", stmts[1])
	}

	// 0004 lifts idx_path and keeps the composite primary key inline.
	stmts4, err := ShapeStatements(ms[3].SQL, ProfileLite)
	if err != nil {
		t.Fatalf("shape 0004: %v", err)
	}
	if len(stmts4) != 2 {
		t.Fatalf("0004 shaped to %d statements, want 2", len(stmts4))
	}
	if !strings.Contains(stmts4[0], "PRIMARY KEY (anchor_id, node_id)") {
		t.Fatalf("0004 lost its primary key:\n%s", stmts4[0])
	}
	if stmts4[1] != "CREATE INDEX IF NOT EXISTS idx_path ON knowledge_nodes (anchor_id, path)" {
		t.Fatalf("0004 index shape wrong:\n%s", stmts4[1])
	}
}

func TestSplitStatements(t *testing.T) {
	in := "-- header comment\nCREATE TABLE a (x INT); -- trailing\nCREATE TABLE b (y INT);\n"
	out := splitStatements(in)
	if len(out) != 2 {
		t.Fatalf("split = %v", out)
	}
	if !strings.Contains(out[0], "CREATE TABLE a") || !strings.Contains(out[1], "CREATE TABLE b") {
		t.Fatalf("split = %v", out)
	}
}
