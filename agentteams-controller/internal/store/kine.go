package store

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/k3s-io/kine/pkg/endpoint"
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
		cfg.DataDir = "/data/agentteams-controller"
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:2379"
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data dir %s: %w", cfg.DataDir, err)
	}

	// v1.2.0 renamed the kine database from hiclaw.db to agentteams.db without
	// migrating existing data. Detect a legacy DB and copy it to the new name
	// (copy, not rename, so the old file is preserved as a rollback).
	migrated, err := ensureKineDBMigration(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate legacy kine database: %w", err)
	}
	if migrated {
		log.Printf("kine database migrated from hiclaw.db to agentteams.db in %s", cfg.DataDir)
	}

	dbPath := filepath.Join(cfg.DataDir, "agentteams.db")
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

// ensureKineDBMigration copies a legacy hiclaw.db (plus its SQLite WAL/SHM
// sidecar files) to agentteams.db when the legacy DB exists and the new DB
// does not. It is idempotent: if agentteams.db already exists, nothing is
// touched. Returns true when a migration was performed.
func ensureKineDBMigration(dataDir string) (bool, error) {
	oldDB := filepath.Join(dataDir, "hiclaw.db")
	newDB := filepath.Join(dataDir, "agentteams.db")

	if _, err := os.Stat(oldDB); err != nil {
		if os.IsNotExist(err) {
			// No legacy DB: fresh install, nothing to migrate.
			return false, nil
		}
		return false, fmt.Errorf("stat legacy db %s: %w", oldDB, err)
	}

	if _, err := os.Stat(newDB); err == nil {
		// New DB already present: user may have migrated manually. Do not
		// overwrite it.
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat new db %s: %w", newDB, err)
	}

	if err := copyFile(oldDB, newDB); err != nil {
		return false, fmt.Errorf("copy %s to %s: %w", oldDB, newDB, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		oldSidecar := oldDB + suffix
		newSidecar := newDB + suffix
		if _, err := os.Stat(oldSidecar); err == nil {
			if err := copyFile(oldSidecar, newSidecar); err != nil {
				return false, fmt.Errorf("copy %s to %s: %w", oldSidecar, newSidecar, err)
			}
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", oldSidecar, err)
		}
	}
	return true, nil
}

// copyFile copies a file, preserving its permission bits.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
