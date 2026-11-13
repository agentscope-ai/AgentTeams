//go:build integration

// Package integration exercises the R1 store against a REAL MySQL-protocol
// database (MySQL 8 or standalone seekdb / OceanBase MySQL). It is the layer
// sqlmock cannot cover: DDL syntax and types (JSON, DATETIME(6), CHAR(64)),
// cross-schema writes in one transaction, unique keys, optimistic locking
// and out-of-order status guards.
//
// Run:
//
//	AGENTTEAMS_TEST_RES_DSN='root:pw@tcp(127.0.0.1:13306)/res?parseTime=true&loc=UTC' \
//	AGENTTEAMS_TEST_OPS_DSN='root:pw@tcp(127.0.0.1:13306)/ops?parseTime=true&loc=UTC' \
//	go test -tags integration -count=1 ./test/integration/...
//
// Without the two DSNs every test is skipped, so a plain `go test ./...`
// stays green on a machine with no database.
package integration

import (
	"context"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/db"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/idem"
	klog "github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/log"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/modules/resources"
)

type harness struct {
	res   *sql.DB
	ops   *sql.DB
	store *resources.SQLStore
	idem  *idem.SQLStore
}

// newHarness connects, migrates (idempotent) and truncates the business
// tables. Each test gets a clean slate.
func newHarness(t *testing.T) *harness {
	t.Helper()
	resDSN, opsDSN := os.Getenv("AGENTTEAMS_TEST_RES_DSN"), os.Getenv("AGENTTEAMS_TEST_OPS_DSN")
	if resDSN == "" || opsDSN == "" {
		t.Skip("integration: AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN not set")
	}
	ctx := context.Background()
	lg, err := klog.New("error", io.Discard)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	if err := db.Migrate(ctx, resDSN, opsDSN, "it-harness", lg); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	res, err := db.Open(ctx, resDSN)
	if err != nil {
		t.Fatalf("open res: %v", err)
	}
	ops, err := db.Open(ctx, opsDSN)
	if err != nil {
		res.Close()
		t.Fatalf("open ops: %v", err)
	}
	t.Cleanup(func() { res.Close(); ops.Close() })

	// ops tables are schema-qualified: the res pool's default database is
	// res, so an unqualified name would look for res.outbox.
	for _, stmt := range []string{
		"DELETE FROM workers",
		"DELETE FROM ops.outbox",
		"DELETE FROM ops.audit_events",
		"DELETE FROM ops.idempotency_keys",
	} {
		if _, err := res.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("truncate %q: %v", stmt, err)
		}
	}
	return &harness{res: res, ops: ops, store: resources.NewSQLStore(res), idem: idem.NewSQLStore(res)}
}

func (h *harness) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := h.res.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func worker(name string) *resources.Worker {
	return &resources.Worker{
		Name:             name,
		WorkerName:       name + "-display",
		Model:            "qwen3-coder",
		Runtime:          "qwenpaw",
		Skills:           []string{"s1", "s2"},
		Env:              map[string]string{"FOO": "bar"},
		MCPServers:       []byte(`[{"name":"m1","url":"https://mcp.example.com"}]`),
		Expose:           []byte(`[{"port":8080,"domain":"w.example.com"}]`),
		ContainerManaged: true,
		DesiredState:     "Running",
	}
}

func meta(action string) *resources.AuditMeta {
	return &resources.AuditMeta{
		RequestID: "req-1",
		ActorType: "admin",
		ActorID:   "admin",
		Action:    action,
		TokenJTI:  "digest-1",
		Client:    "it",
	}
}

func TestCreateGetListAndCrossSchemaTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.store.Create(ctx, worker("w1"), meta("worker.create"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Generation != 1 || created.Version != 1 || created.ObservedPhase != "Pending" {
		t.Fatalf("create defaults: gen=%d ver=%d phase=%q", created.Generation, created.Version, created.ObservedPhase)
	}
	if len(created.SpecHash) != 64 {
		t.Fatalf("spec_hash = %q, want 64 hex chars", created.SpecHash)
	}
	if h.count(t, "ops.outbox") != 1 || h.count(t, "ops.audit_events") != 1 {
		t.Fatalf("outbox=%d audit=%d, want 1/1 (same transaction)", h.count(t, "ops.outbox"), h.count(t, "ops.audit_events"))
	}
	// The adapter payload speaks the controller API (camelCase), never the
	// storage schema (58 §11 I12).
	var payload []byte
	if err := h.res.QueryRow("SELECT payload_json FROM ops.outbox WHERE worker_name = 'w1'").Scan(&payload); err != nil {
		t.Fatalf("outbox payload: %v", err)
	}
	if !strings.Contains(string(payload), `"workerName"`) || strings.Contains(string(payload), `"worker_name"`) {
		t.Fatalf("outbox spec payload is not API-shaped: %s", payload)
	}

	got, err := h.store.Get(ctx, "w1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "qwen3-coder" || len(got.Skills) != 2 || got.Env["FOO"] != "bar" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.CreatedAt.IsZero() || !strings.Contains(string(got.Expose), "8080") {
		t.Fatalf("json/time round trip: created=%v expose=%s", got.CreatedAt, got.Expose)
	}

	if _, err := h.store.Create(ctx, worker("w1"), meta("worker.create")); err == nil ||
		!strings.Contains(err.Error(), resources.ErrAlreadyExists.Error()) {
		t.Fatalf("duplicate create = %v, want ErrAlreadyExists", err)
	}
	if h.count(t, "ops.outbox") != 1 {
		t.Fatalf("duplicate create leaked an outbox row")
	}

	rows, err := h.store.List(ctx, resources.ListFilter{})
	if err != nil || len(rows) != 1 || rows[0].Name != "w1" {
		t.Fatalf("list = %v, %v", rows, err)
	}
	empty, err := h.store.List(ctx, resources.ListFilter{TeamSlug: "nope"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, %v (want non-nil empty)", empty, err)
	}
	if _, err := h.store.Get(ctx, "missing"); err == nil ||
		!strings.Contains(err.Error(), resources.ErrNotFound.Error()) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
}

func TestUpdateSpecGenerationAndOutbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("w2"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}

	spec := worker("w2").Spec()
	spec.Model = "qwen3-max"
	updated, err := h.store.UpdateSpec(ctx, "w2", spec, 1, meta("worker.update"))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Generation != 2 || updated.Version != 2 || updated.Model != "qwen3-max" {
		t.Fatalf("update: gen=%d ver=%d model=%q", updated.Generation, updated.Version, updated.Model)
	}
	if h.count(t, "ops.outbox") != 2 {
		t.Fatalf("outbox=%d, want provision+update", h.count(t, "ops.outbox"))
	}

	// Same spec again: version moves, generation and outbox do not.
	same, err := h.store.UpdateSpec(ctx, "w2", spec, 2, meta("worker.update"))
	if err != nil {
		t.Fatalf("update same: %v", err)
	}
	if same.Generation != 2 || same.Version != 3 {
		t.Fatalf("same-spec update: gen=%d ver=%d, want 2/3", same.Generation, same.Version)
	}
	if h.count(t, "ops.outbox") != 2 {
		t.Fatalf("unchanged spec wrote an outbox event")
	}

	if _, err := h.store.UpdateSpec(ctx, "w2", spec, 99, meta("worker.update")); err == nil ||
		!strings.Contains(err.Error(), resources.ErrConflict.Error()) {
		t.Fatalf("stale version = %v, want ErrConflict", err)
	}
}

// TestSyncSpecEmitsGenerationOnlyEvent pins the write-through contract: the
// proxied PUT already applied the change in the controller, so the service
// must bump its generation but enqueue a "sync" event WITHOUT the spec
// (sending the partial service view back would revert env/resources).
func TestSyncSpecEmitsGenerationOnlyEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("sync-w"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}
	spec := worker("sync-w").Spec()
	spec.Model = "m-sync"
	updated, err := h.store.SyncSpec(ctx, "sync-w", spec, 1, meta("worker.proxied_update"))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if updated.Generation != 2 || updated.Version != 2 {
		t.Fatalf("gen/ver = %d/%d, want 2/2", updated.Generation, updated.Version)
	}
	var eventType string
	var payload []byte
	if err := h.res.QueryRow("SELECT event_type, payload_json FROM ops.outbox WHERE worker_name='sync-w' AND event_type='sync'").Scan(&eventType, &payload); err != nil {
		t.Fatalf("sync outbox row: %v", err)
	}
	if strings.Contains(string(payload), `"spec"`) {
		t.Fatalf("sync payload must not carry a spec: %s", payload)
	}
	if !strings.Contains(string(payload), `"incarnation"`) {
		t.Fatalf("sync payload must carry the incarnation: %s", payload)
	}
}

func TestUpdateStatusGuardAndImmutability(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	created, err := h.store.Create(ctx, worker("w3"), meta("worker.create"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ok, err := h.store.UpdateStatus(ctx, "w3", resources.Observed{Phase: "Running", ContainerState: "running"}, 0)
	if err != nil || ok {
		t.Fatalf("stale status gen=0 applied: ok=%v err=%v", ok, err)
	}
	ok, err = h.store.UpdateStatus(ctx, "w3", resources.Observed{Phase: "Running", ContainerState: "running"}, 1)
	if err != nil || !ok {
		t.Fatalf("status gen=1 not applied: ok=%v err=%v", ok, err)
	}
	got, err := h.store.Get(ctx, "w3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ObservedPhase != "Running" || got.Generation != created.Generation || got.Version != created.Version || got.SpecHash != created.SpecHash {
		t.Fatalf("status update leaked into spec: phase=%q gen=%d ver=%d", got.ObservedPhase, got.Generation, got.Version)
	}
	if h.count(t, "ops.outbox") != 1 {
		t.Fatalf("status update wrote an outbox event")
	}
}

// TestUpdateStatusPartialDoesNotClear pins the COALESCE update: a callback
// that reports only phase must not wipe matrix/room/task fields.
func TestUpdateStatusPartialDoesNotClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("partial-w"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := h.store.UpdateStatus(ctx, "partial-w", resources.Observed{
		Phase: "Running", MatrixUserID: "@partial:mtx", RoomID: "!room:mtx", AgentStatus: "idle",
	}, 1); err != nil || !ok {
		t.Fatalf("first status: ok=%v err=%v", ok, err)
	}
	if ok, err := h.store.UpdateStatus(ctx, "partial-w", resources.Observed{Phase: "Sleeping"}, 1); err != nil || !ok {
		t.Fatalf("partial status: ok=%v err=%v", ok, err)
	}
	got, err := h.store.Get(ctx, "partial-w")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ObservedPhase != "Sleeping" {
		t.Fatalf("phase = %q, want Sleeping", got.ObservedPhase)
	}
	if got.MatrixUserID != "@partial:mtx" || got.RoomID != "!room:mtx" || got.AgentStatus != "idle" {
		t.Fatalf("partial update cleared fields: matrix=%q room=%q agent=%q", got.MatrixUserID, got.RoomID, got.AgentStatus)
	}
}

func TestSoftDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("w4"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.store.SoftDelete(ctx, "w4", 1, meta("worker.delete")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := h.store.Get(ctx, "w4"); err == nil {
		t.Fatalf("deleted worker still readable")
	}
	if h.count(t, "ops.outbox") != 2 {
		t.Fatalf("outbox=%d, want provision+deprovision", h.count(t, "ops.outbox"))
	}
	if err := h.store.SoftDelete(ctx, "w4", 1, meta("worker.delete")); err == nil ||
		!strings.Contains(err.Error(), resources.ErrNotFound.Error()) {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

func TestIdempotencyStore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.idem.Get(ctx, "k1"); err == nil ||
		!strings.Contains(err.Error(), resources.ErrIdemMiss.Error()) {
		t.Fatalf("get missing = %v, want ErrIdemMiss", err)
	}
	if err := h.idem.Put(ctx, "k1", "w1", 201, []byte(`{"ok":true}`), 24*time.Hour); err != nil {
		t.Fatalf("put: %v", err)
	}
	rec, err := h.idem.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Status != 201 || string(rec.Body) != `{"ok":true}` || rec.WorkerName != "w1" {
		t.Fatalf("record = %+v", rec)
	}
	// Negative TTL stores a row that is already expired.
	if err := h.idem.Put(ctx, "k2", "w2", 409, []byte("{}"), -time.Hour); err != nil {
		t.Fatalf("put expired: %v", err)
	}
	if _, err := h.idem.Get(ctx, "k2"); err == nil {
		t.Fatalf("expired key was served")
	}
}

// TestRecreateAfterSoftDelete pins 58 §1.1's delete→create behaviour on a
// real engine: a soft-deleted row still owns the name primary key, so the
// create reclaims it and installs a fresh generation-1 incarnation.
func TestRecreateAfterSoftDelete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Create(ctx, worker("w7"), meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.store.SoftDelete(ctx, "w7", 1, meta("worker.delete")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	recreated, err := h.store.Create(ctx, worker("w7"), meta("worker.create"))
	if err != nil {
		t.Fatalf("recreate after soft delete: %v", err)
	}
	if recreated.Generation != 1 || recreated.Version != 1 {
		t.Fatalf("recreated gen/ver = %d/%d, want 1/1", recreated.Generation, recreated.Version)
	}
	got, err := h.store.Get(ctx, "w7")
	if err != nil {
		t.Fatalf("get after recreate: %v", err)
	}
	if got.DeletedAt != nil || got.Model != "qwen3-coder" {
		t.Fatalf("recreated row = %+v", got)
	}
	// provision, deprovision, provision.
	if n := h.count(t, "ops.outbox"); n != 3 {
		t.Fatalf("outbox rows = %d, want 3", n)
	}
}

func TestSchemaTypes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// DATETIME(6) keeps microseconds; JSON columns accept objects.
	fixed := time.Date(2026, 11, 13, 0, 0, 0, 123456000, time.UTC)
	w := worker("w5")
	w.CreatedAt = fixed // overwritten by Create; assert after.
	h.store.Now = func() time.Time { return fixed }
	if _, err := h.store.Create(ctx, w, meta("worker.create")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := h.store.Get(ctx, "w5")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CreatedAt.Nanosecond()/1000 != 123456 {
		t.Fatalf("DATETIME(6) lost microseconds: %v", got.CreatedAt)
	}
	var table, typ string
	if err := h.res.QueryRow("SELECT table_name, column_type FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'workers' AND column_name = 'spec_hash'").Scan(&table, &typ); err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if table != "workers" || !strings.HasPrefix(strings.ToLower(typ), "char(64)") {
		t.Fatalf("spec_hash column = %q.%q, want workers.char(64)", table, typ)
	}
}
