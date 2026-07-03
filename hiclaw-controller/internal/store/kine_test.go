package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestCheckSQLiteIntegrity_MissingFile verifies that a not-yet-created DB
// (fresh volume / first boot) is treated as a no-op, not a failure — kine
// itself is responsible for creating the file.
func TestCheckSQLiteIntegrity_MissingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hiclaw.db")

	if err := checkSQLiteIntegrity(dbPath); err != nil {
		t.Fatalf("expected no error for missing file (fresh volume), got: %v", err)
	}
}

// TestCheckSQLiteIntegrity_ValidDB verifies that a well-formed SQLite DB
// passes the pre-flight check.
func TestCheckSQLiteIntegrity_ValidDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hiclaw.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t (k TEXT PRIMARY KEY, v BLOB)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO t (k, v) VALUES ('a', 'b')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := checkSQLiteIntegrity(dbPath); err != nil {
		t.Fatalf("expected no error for a valid DB, got: %v", err)
	}
}

// TestCheckSQLiteIntegrity_CorruptedDB verifies the core S3b requirement:
// a corrupted DB causes checkSQLiteIntegrity to return a loud error and NOT
// silently succeed (which would let kine fall back to treating it as fresh).
//
// This mirrors the manual corruption drill described in
// hiclaw-controller/preflight.sh and docs/implementation-milestone-1.md
// Step 3: copy the DB, truncate it, expect refusal.
func TestCheckSQLiteIntegrity_CorruptedDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hiclaw.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t (k TEXT PRIMARY KEY, v BLOB)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Insert enough rows to force the DB past a single page so truncation
	// below actually chops through live data rather than trailing free space.
	for i := 0; i < 500; i++ {
		if _, err := db.Exec("INSERT INTO t (k, v) VALUES (?, ?)", i, strings.Repeat("x", 200)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Truncate to roughly half the file to guarantee mid-page corruption.
	corruptSize := info.Size() / 2
	if err := os.Truncate(dbPath, corruptSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	err = checkSQLiteIntegrity(dbPath)
	if err == nil {
		t.Fatal("expected checkSQLiteIntegrity to fail loudly on a truncated/corrupted DB, got nil error")
	}
	if !strings.Contains(err.Error(), "integrity pre-flight FAILED") {
		t.Fatalf("expected error to be the loud pre-flight failure, got: %v", err)
	}
}
