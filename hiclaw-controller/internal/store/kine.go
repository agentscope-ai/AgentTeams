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
		cfg.DataDir = "/data/hiclaw-controller"
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:2379"
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data dir %s: %w", cfg.DataDir, err)
	}

	dbPath := filepath.Join(cfg.DataDir, "hiclaw.db")
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
