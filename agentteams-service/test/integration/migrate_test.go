//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/db"
)

// TestMigrateReappliesMissingRecord pins the crash window documented in
// 58 O4: if a DDL committed but its schema_migrations row did not, the next
// boot re-runs the same file and must succeed because every statement is
// idempotent. A future version written by a newer binary must be ignored,
// not treated as an error (rolling upgrades).
func TestMigrateReappliesMissingRecord(t *testing.T) {
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if resDSN == "" || opsDSN == "" {
		t.Skip("integration: AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN not set")
	}
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.res.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = '0001'"); err != nil {
		t.Fatalf("delete res record: %v", err)
	}
	if _, err := h.ops.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = '0001'"); err != nil {
		t.Fatalf("delete ops record: %v", err)
	}
	// A newer binary may have recorded a version this build does not embed.
	if _, err := h.res.ExecContext(ctx, "INSERT INTO schema_migrations (version, filename, applied_at) VALUES ('9999', 'future.sql', NOW(6))"); err != nil {
		t.Fatalf("insert future version: %v", err)
	}

	lg := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := db.Migrate(ctx, resDSN, opsDSN, "it-reapply", lg); err != nil {
		t.Fatalf("Migrate after a lost record: %v", err)
	}

	var n int
	if err := h.res.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = '0001'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("res 0001 record = %d err=%v, want 1", n, err)
	}
	if err := h.ops.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = '0001'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("ops 0001 record = %d err=%v, want 1", n, err)
	}
	if err := h.res.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = '9999'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("future version = %d err=%v, want preserved", n, err)
	}
	// The re-applied DDL left usable tables.
	if err := h.res.QueryRowContext(ctx, "SELECT COUNT(*) FROM workers").Scan(&n); err != nil {
		t.Fatalf("workers after reapply: %v", err)
	}
	if err := h.ops.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox").Scan(&n); err != nil {
		t.Fatalf("outbox after reapply: %v", err)
	}
}
