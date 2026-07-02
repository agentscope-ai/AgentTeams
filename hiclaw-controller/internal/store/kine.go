package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// go-sqlite3 is already an indirect dependency (pulled in transitively by
	// kine's own sqlite driver usage) and the controller binary is already
	// built with CGO_ENABLED=1 (see hiclaw-controller/Dockerfile), so it is
	// safe to import directly here for the pre-flight integrity check below —
	// this avoids depending on a `sqlite3` CLI binary being present in the
	// final embedded image at all.
	"github.com/k3s-io/kine/pkg/endpoint"
	_ "github.com/mattn/go-sqlite3"
)

// Config holds kine/store configuration.
type Config struct {
	// DataDir is the directory for SQLite database.
	DataDir string
	// ListenAddress for the kine etcd-compatible endpoint.
	ListenAddress string
	// KubeMode: "embedded" (default, kine+SQLite) or "incluster" (real K8s API).
	KubeMode string
}

// KineServer wraps a running kine instance.
type KineServer struct {
	ETCDConfig endpoint.ETCDConfig
}

// StartKine starts an embedded kine server backed by SQLite.
// Returns ETCDConfig that can be used to connect via client-go.
func StartKine(ctx context.Context, cfg Config) (*KineServer, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "/data/hiclaw-controller"
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:2379"
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data dir %s: %w", cfg.DataDir, err)
	}

	dbPath := filepath.Join(cfg.DataDir, "hiclaw.db")

	// Pre-flight integrity check: refuse to start kine (and therefore the
	// embedded apiserver + controller) against a corrupted SQLite DB rather
	// than silently falling back to an empty/fresh one, which would look like
	// a working cluster while having quietly discarded every CR. See plan
	// §7 (Step 3, S3b).
	if err := checkSQLiteIntegrity(dbPath); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("sqlite://%s?_journal=WAL&cache=shared&_busy_timeout=30000", dbPath)

	etcdCfg, err := endpoint.Listen(ctx, endpoint.Config{
		Listener:       cfg.ListenAddress,
		Endpoint:       dsn,
		NotifyInterval: time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start kine: %w", err)
	}

	return &KineServer{ETCDConfig: etcdCfg}, nil
}

// checkSQLiteIntegrity runs `PRAGMA integrity_check` against the kine SQLite
// database BEFORE kine (and thus the embedded apiserver) starts.
//
// If the file does not exist yet (first boot, fresh volume), this is a no-op —
// kine will create it. If the file exists but is corrupted, this returns a
// loud, unmistakable error that aborts startup entirely; there is no silent
// fallback to a fresh/empty database, because that would masquerade as a
// healthy cluster while having discarded every CR (Worker/Team/Human/Manager)
// on disk.
func checkSQLiteIntegrity(dbPath string) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		// Fresh volume / first boot — nothing to check yet.
		return nil
	} else if err != nil {
		return fmt.Errorf("kine integrity pre-flight: stat %s: %w", dbPath, err)
	}

	// Open read-only, single connection: this check must not create the file
	// if it somehow got deleted between the Stat above and here, and must not
	// race kine's own WAL-mode connection.
	roDSN := fmt.Sprintf("file:%s?mode=ro&_query_only=1", dbPath)
	db, err := sql.Open("sqlite3", roDSN)
	if err != nil {
		return fmt.Errorf("kine integrity pre-flight: open %s: %w", dbPath, err)
	}
	defer db.Close()

	row := db.QueryRow("PRAGMA integrity_check;")
	var result string
	if err := row.Scan(&result); err != nil {
		logKineCorruption(dbPath, fmt.Sprintf("PRAGMA integrity_check query failed: %v", err))
		return fmt.Errorf("kine SQLite integrity pre-flight FAILED for %s: query error: %w (refusing to start — see log line above)", dbPath, err)
	}

	if result != "ok" {
		logKineCorruption(dbPath, result)
		return fmt.Errorf("kine SQLite integrity pre-flight FAILED for %s: %s (refusing to start — no silent fallback to an empty DB; restore from backup or remove the file to intentionally re-initialize)", dbPath, result)
	}

	return nil
}

// logKineCorruption emits an unmistakable, greppable log line on stderr so a
// corrupted kine DB is impossible to miss in `docker logs` / supervisord logs,
// independent of whatever logging framework the caller uses.
func logKineCorruption(dbPath, detail string) {
	fmt.Fprintf(os.Stderr,
		"\n"+
			"########################################################################\n"+
			"# KINE SQLITE INTEGRITY CHECK FAILED — REFUSING TO START\n"+
			"# db_path=%s\n"+
			"# detail=%s\n"+
			"# The controller will NOT fall back to a fresh/empty database. Restore\n"+
			"# %s from backup, or intentionally remove it to re-initialize the\n"+
			"# cluster state from scratch.\n"+
			"########################################################################\n\n",
		dbPath, detail, dbPath)
}
