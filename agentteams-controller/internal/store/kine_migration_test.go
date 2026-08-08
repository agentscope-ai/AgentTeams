package store

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates a file with the given content, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFileOrFail reads a file, failing the test on error.
func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestEnsureKineDBMigration_OldDBOnly(t *testing.T) {
	dataDir := t.TempDir()

	// Old database with WAL and SHM sidecars present, new DB absent.
	writeFile(t, filepath.Join(dataDir, "hiclaw.db"), "old-data")
	writeFile(t, filepath.Join(dataDir, "hiclaw.db-wal"), "old-wal")
	writeFile(t, filepath.Join(dataDir, "hiclaw.db-shm"), "old-shm")

	migrated, err := ensureKineDBMigration(dataDir)
	if err != nil {
		t.Fatalf("ensureKineDBMigration returned error: %v", err)
	}
	if !migrated {
		t.Fatal("expected migration to run (old DB present, new DB absent)")
	}

	// New DB and sidecars must exist with old content.
	if got := readFileOrFail(t, filepath.Join(dataDir, "agentteams.db")); got != "old-data" {
		t.Fatalf("agentteams.db content = %q, want %q", got, "old-data")
	}
	if got := readFileOrFail(t, filepath.Join(dataDir, "agentteams.db-wal")); got != "old-wal" {
		t.Fatalf("agentteams.db-wal content = %q, want %q", got, "old-wal")
	}
	if got := readFileOrFail(t, filepath.Join(dataDir, "agentteams.db-shm")); got != "old-shm" {
		t.Fatalf("agentteams.db-shm content = %q, want %q", got, "old-shm")
	}

	// Old DB must be preserved (copy, not rename) for rollback.
	if got := readFileOrFail(t, filepath.Join(dataDir, "hiclaw.db")); got != "old-data" {
		t.Fatalf("hiclaw.db content = %q, want %q (old DB should be preserved)", got, "old-data")
	}
}

func TestEnsureKineDBMigration_NewDBExistsSkips(t *testing.T) {
	dataDir := t.TempDir()

	// Both old and new DB present: user may already have migrated manually.
	writeFile(t, filepath.Join(dataDir, "hiclaw.db"), "old-data")
	writeFile(t, filepath.Join(dataDir, "agentteams.db"), "new-data")

	migrated, err := ensureKineDBMigration(dataDir)
	if err != nil {
		t.Fatalf("ensureKineDBMigration returned error: %v", err)
	}
	if migrated {
		t.Fatal("expected migration to be skipped when new DB already exists")
	}

	// New DB content must NOT be overwritten by old DB.
	if got := readFileOrFail(t, filepath.Join(dataDir, "agentteams.db")); got != "new-data" {
		t.Fatalf("agentteams.db content = %q, want %q (existing new DB must not be overwritten)", got, "new-data")
	}
}

func TestEnsureKineDBMigration_NoOldDB(t *testing.T) {
	dataDir := t.TempDir()

	// Fresh install: no old DB, no new DB.
	migrated, err := ensureKineDBMigration(dataDir)
	if err != nil {
		t.Fatalf("ensureKineDBMigration returned error: %v", err)
	}
	if migrated {
		t.Fatal("expected no migration when old DB is absent")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "agentteams.db")); !os.IsNotExist(err) {
		t.Fatal("agentteams.db should not be created when there is nothing to migrate")
	}
}

func TestEnsureKineDBMigration_OldDBSidecarOnly(t *testing.T) {
	dataDir := t.TempDir()

	// Old DB missing but sidecar files exist (unusual): should not error, no migration.
	writeFile(t, filepath.Join(dataDir, "hiclaw.db-wal"), "orphan-wal")

	migrated, err := ensureKineDBMigration(dataDir)
	if err != nil {
		t.Fatalf("ensureKineDBMigration returned error: %v", err)
	}
	if migrated {
		t.Fatal("expected no migration when old DB file itself is absent")
	}
}
