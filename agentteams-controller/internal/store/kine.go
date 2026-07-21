package store

import (
	"context"
	"fmt"
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

	dbPath, err := agentTeamsDBPath(cfg.DataDir)
	if err != nil {
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

func agentTeamsDBPath(dataDir string) (string, error) {
	current := filepath.Join(dataDir, "agentteams.db")
	legacy := filepath.Join(dataDir, "hiclaw.db")

	if _, err := os.Stat(current); err == nil {
		return current, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat AgentTeams database: %w", err)
	}

	if _, err := os.Stat(legacy); err == nil {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			oldPath := legacy + suffix
			newPath := current + suffix
			if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("migrate legacy database %s: %w", oldPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat legacy database: %w", err)
	}

	return current, nil
}
