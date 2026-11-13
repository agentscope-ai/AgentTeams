package resources

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/outbox"
)

var storeTestNow = time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

func newMockStore(t *testing.T) (*SQLStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &SQLStore{DB: db, Now: func() time.Time { return storeTestNow }}, mock
}

func timePtr(v time.Time) *time.Time { return &v }

func intPtr(v int) *int { return &v }

// sampleWorker returns a fully populated row. Tests override the fields
// they care about; SpecHash is set explicitly where a row is expected to
// round-trip.
func sampleWorker() *Worker {
	active := storeTestNow.Add(-time.Minute)
	run := storeTestNow.Add(-2 * time.Minute)
	finish := storeTestNow.Add(-time.Minute)
	return &Worker{
		Name:          "worker-a",
		WorkerName:    "Worker A",
		TeamSlug:      "team-a",
		Model:         "qwen-max",
		ModelProvider: "dashscope",
		SubagentModel: "qwen-plus",

		LLMStreamFirstContentTimeout: "300s",
		LLMStreamIdleTimeout:         "300s",
		LLMMaxRetries:                "3",
		LLMBackoffBase:               "1s",
		LLMBackoffCap:                "10s",
		LLMMaxConcurrent:             "2",
		LLMMaxQPM:                    "60",
		LLMRateLimitPause:            "5s",
		LLMRateLimitJitter:           "0.5",
		LLMAcquireTimeout:            "10s",

		Runtime:  "qwenpaw",
		Image:    "registry.example.test/worker:1",
		Identity: "identity-body",
		Soul:     "soul-body",
		Agents:   "agents-body",

		Skills:        []string{"skill-a", "skill-b"},
		RemoteSkills:  json.RawMessage(`[{"source":"https://example.test/skills"}]`),
		MCPServers:    json.RawMessage(`[{"name":"mcp-a"}]`),
		PackageRef:    "pkg:1",
		Expose:        json.RawMessage(`[{"port":8080}]`),
		ChannelPolicy: json.RawMessage(`{"dm":true}`),
		Resources:     json.RawMessage(`{"cpu":"1"}`),
		Env:           map[string]string{"LOG_LEVEL": "info"},

		ContainerManaged: true,
		DesiredState:     "Running",

		ObservedPhase:          "Pending",
		ObservedContainerState: "created",
		MatrixUserID:           "@bot:example.test",
		RoomID:                 "!room:example.test",
		LastActiveAt:           &active,
		AgentStatus:            "idle",
		RunningTaskCount:       intPtr(0),
		LastRunAt:              &run,
		LastFinishAt:           &finish,
		Message:                "ok",

		Generation: 1,
		Version:    1,
		CreatedAt:  storeTestNow,
		UpdatedAt:  storeTestNow,
	}
}

// prepareCreate returns the input worker (defaults cleared) and the row
// Create is expected to write.
func prepareCreate(t *testing.T) (*Worker, *Worker) {
	t.Helper()
	w := sampleWorker()
	w.Generation = 0
	w.Version = 0
	w.ObservedPhase = ""
	w.CreatedAt = time.Time{}
	w.UpdatedAt = time.Time{}
	w.DeletedAt = nil
	hash, err := SpecHash(w.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	want := *w
	want.Generation = 1
	want.Version = 1
	want.ObservedPhase = "Pending"
	want.SpecHash = hash
	want.CreatedAt = storeTestNow
	want.UpdatedAt = storeTestNow
	return w, &want
}

func strV(s string) driver.Value {
	if s == "" {
		return nil
	}
	return s
}

func rawV(raw json.RawMessage) driver.Value {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func timeV(t *time.Time) driver.Value {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func intV(p *int) driver.Value {
	if p == nil {
		return nil
	}
	return int64(*p)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// workerSpecValues mirrors workerSpecArgs for assertions.
func workerSpecValues(spec Spec) []driver.Value {
	var skills driver.Value
	if len(spec.Skills) > 0 {
		skills = mustJSON(spec.Skills)
	}
	var env driver.Value
	if len(spec.Env) > 0 {
		env = mustJSON(spec.Env)
	}
	return []driver.Value{
		strV(spec.WorkerName),
		strV(spec.TeamSlug),
		strV(spec.Model),
		strV(spec.ModelProvider),
		strV(spec.SubagentModel),

		strV(spec.LLMStreamFirstContentTimeout),
		strV(spec.LLMStreamIdleTimeout),
		strV(spec.LLMMaxRetries),
		strV(spec.LLMBackoffBase),
		strV(spec.LLMBackoffCap),
		strV(spec.LLMMaxConcurrent),
		strV(spec.LLMMaxQPM),
		strV(spec.LLMRateLimitPause),
		strV(spec.LLMRateLimitJitter),
		strV(spec.LLMAcquireTimeout),

		strV(spec.Runtime),
		strV(spec.Image),
		strV(spec.Identity),
		strV(spec.Soul),
		strV(spec.Agents),

		skills,
		rawV(spec.RemoteSkills),
		rawV(spec.MCPServers),
		strV(spec.PackageRef),
		rawV(spec.Expose),
		rawV(spec.ChannelPolicy),
		rawV(spec.Resources),
		env,

		spec.ContainerManaged,
		strV(spec.DesiredState),
	}
}

// workerInsertValues mirrors insertWorkerArgs for assertions.
func workerInsertValues(w *Worker) []driver.Value {
	vals := make([]driver.Value, 0, 47)
	vals = append(vals, w.Name)
	vals = append(vals, workerSpecValues(w.Spec())...)
	vals = append(vals,
		w.ObservedPhase,
		strV(w.ObservedContainerState),
		strV(w.MatrixUserID),
		strV(w.RoomID),
		timeV(w.LastActiveAt),
		strV(w.AgentStatus),
		intV(w.RunningTaskCount),
		timeV(w.LastRunAt),
		timeV(w.LastFinishAt),
		strV(w.Message),
		w.Generation,
		w.SpecHash,
		w.Version,
		w.CreatedAt,
		w.UpdatedAt,
		timeV(w.DeletedAt),
	)
	return vals
}

func workerRows(w *Worker) *sqlmock.Rows {
	return sqlmock.NewRows(strings.Split(workerColumns, ", ")).AddRow(workerInsertValues(w)...)
}

func workerColumnNames() []string {
	return strings.Split(workerColumns, ", ")
}

// hexID matches a 32-character lowercase hex event id.
type hexID struct{}

func (hexID) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok || len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// payloadCheck matches an outbox payload_json argument: it must carry
// name and the expected generation, and spec iff hasSpec.
type payloadCheck struct {
	generation float64
	hasSpec    bool
}

func (c payloadCheck) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	if !ok {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	if _, ok := m["name"]; !ok {
		return false
	}
	gen, ok := m["generation"].(float64)
	if !ok || gen != c.generation {
		return false
	}
	_, hasSpec := m["spec"]
	return hasSpec == c.hasSpec
}

func TestSQLStoreCreate(t *testing.T) {
	store, mock := newMockStore(t)
	w, want := prepareCreate(t)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(resurrectWorkerSQL)).
		WithArgs(workerInsertValues(want)...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.outbox")).
		WithArgs(
			hexID{},
			outbox.IdemKey("worker-a", 1, "provision", storeTestNow.UTC().Format(time.RFC3339Nano)),
			"worker-a",
			int64(1),
			"provision",
			"controller",
			payloadCheck{generation: 1, hasSpec: true},
			"pending",
			int64(0),
			storeTestNow,
			storeTestNow,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.audit_events")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := store.Create(context.Background(), w, &AuditMeta{
		RequestID: "req-1",
		ActorType: "human",
		ActorID:   "human-1",
		Action:    "worker.create",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Generation != 1 || got.Version != 1 {
		t.Errorf("Create generation/version = %d/%d, want 1/1", got.Generation, got.Version)
	}
	if got.ObservedPhase != "Pending" {
		t.Errorf("Create observed_phase = %q, want Pending", got.ObservedPhase)
	}
	if got.SpecHash != want.SpecHash {
		t.Errorf("Create spec_hash = %q, want %q", got.SpecHash, want.SpecHash)
	}
	if !got.CreatedAt.Equal(storeTestNow) || !got.UpdatedAt.Equal(storeTestNow) {
		t.Errorf("Create timestamps = %v/%v, want %v", got.CreatedAt, got.UpdatedAt, storeTestNow)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreCreateRollbackOnOutboxFailure(t *testing.T) {
	store, mock := newMockStore(t)
	w, want := prepareCreate(t)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(resurrectWorkerSQL)).
		WithArgs(workerInsertValues(want)...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.outbox")).
		WillReturnError(errors.New("outbox unavailable"))
	mock.ExpectRollback()

	if _, err := store.Create(context.Background(), w, &AuditMeta{Action: "worker.create"}); err == nil {
		t.Fatal("Create: want error when outbox insert fails, got nil")
	} else if !strings.Contains(err.Error(), "outbox") {
		t.Errorf("Create error = %v, want outbox context", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreCreateDuplicate(t *testing.T) {
	store, mock := newMockStore(t)
	w, want := prepareCreate(t)

	mock.ExpectBegin()
	// A live row owns the name: the ON DUPLICATE clause is a no-op
	// (RowsAffected 0) and the create fails with ErrAlreadyExists.
	mock.ExpectExec(regexp.QuoteMeta(resurrectWorkerSQL)).
		WithArgs(workerInsertValues(want)...).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err := store.Create(context.Background(), w, &AuditMeta{Action: "worker.create"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Create duplicate error = %v, want ErrAlreadyExists", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestSQLStoreCreateResurrectsSoftDeleted pins the delete→create path: a
// soft-deleted row still owns the primary key, so the create reclaims it and
// inserts the new incarnation in the same transaction.
func TestSQLStoreCreateResurrectsSoftDeleted(t *testing.T) {
	store, mock := newMockStore(t)
	w, want := prepareCreate(t)

	mock.ExpectBegin()
	// The name is owned by a soft-deleted row: ON DUPLICATE rewrites it
	// (RowsAffected 2), which is the resurrection.
	mock.ExpectExec(regexp.QuoteMeta(resurrectWorkerSQL)).
		WithArgs(workerInsertValues(want)...).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.outbox")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.audit_events")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if _, err := store.Create(context.Background(), w, &AuditMeta{Action: "worker.create"}); err != nil {
		t.Fatalf("Create after soft delete = %v, want success", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreGet(t *testing.T) {
	store, mock := newMockStore(t)
	want := sampleWorker()
	hash, err := SpecHash(want.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	want.SpecHash = hash
	want.Generation = 4
	want.Version = 9

	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerSQL)).
		WithArgs("worker-a").
		WillReturnRows(workerRows(want))

	got, err := store.Get(context.Background(), "worker-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != want.Name || got.WorkerName != want.WorkerName || got.Model != want.Model {
		t.Errorf("Get row = %+v, want %+v", got, want)
	}
	if len(got.Skills) != 2 || got.Skills[0] != "skill-a" {
		t.Errorf("Get skills = %v", got.Skills)
	}
	if string(got.MCPServers) != string(want.MCPServers) {
		t.Errorf("Get mcp_servers = %s", got.MCPServers)
	}
	if got.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Get env = %v", got.Env)
	}
	if got.LastActiveAt == nil || !got.LastActiveAt.Equal(*want.LastActiveAt) {
		t.Errorf("Get last_active_at = %v", got.LastActiveAt)
	}
	if got.RunningTaskCount == nil || *got.RunningTaskCount != 0 {
		t.Errorf("Get running_task_count = %v", got.RunningTaskCount)
	}
	if got.Generation != 4 || got.Version != 9 || got.SpecHash != hash {
		t.Errorf("Get generation/version/hash = %d/%d/%s", got.Generation, got.Version, got.SpecHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreGetNullColumns(t *testing.T) {
	store, mock := newMockStore(t)
	want := sampleWorker()
	want.SpecHash = "h"
	want.Skills = nil
	want.RemoteSkills = nil
	want.MCPServers = nil
	want.Expose = nil
	want.ChannelPolicy = nil
	want.Resources = nil
	want.Env = nil
	want.ObservedContainerState = ""
	want.MatrixUserID = ""
	want.RoomID = ""
	want.LastActiveAt = nil
	want.AgentStatus = ""
	want.RunningTaskCount = nil
	want.LastRunAt = nil
	want.LastFinishAt = nil
	want.Message = ""

	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerSQL)).
		WithArgs("worker-a").
		WillReturnRows(workerRows(want))

	got, err := store.Get(context.Background(), "worker-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Skills != nil || got.Env != nil || got.RemoteSkills != nil || got.MCPServers != nil {
		t.Errorf("Get nullable JSON fields = %v/%v/%s/%s, want nil", got.Skills, got.Env, got.RemoteSkills, got.MCPServers)
	}
	if got.LastActiveAt != nil || got.RunningTaskCount != nil || got.LastRunAt != nil || got.LastFinishAt != nil {
		t.Errorf("Get nullable observed fields not nil: %+v", got)
	}
	if got.DeletedAt != nil {
		t.Errorf("Get deleted_at = %v, want nil", got.DeletedAt)
	}
}

func TestSQLStoreGetNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerSQL)).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows(workerColumnNames()))

	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
}

func TestSQLStoreList(t *testing.T) {
	store, mock := newMockStore(t)
	a := sampleWorker()
	a.SpecHash = "hash-a"
	b := sampleWorker()
	b.Name = "worker-b"
	b.TeamSlug = "team-b"
	b.ObservedPhase = "Running"
	b.SpecHash = "hash-b"

	mock.ExpectQuery(regexp.QuoteMeta(listWorkersSQL + listWorkersOrderBy)).
		WillReturnRows(workerRows(a).AddRow(workerInsertValues(b)...))

	got, err := store.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Name != "worker-a" || got[1].Name != "worker-b" {
		t.Fatalf("List names = %v, want [worker-a worker-b]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreListFilters(t *testing.T) {
	store, mock := newMockStore(t)
	a := sampleWorker()
	a.SpecHash = "hash-a"

	wantSQL := listWorkersSQL + " AND team_slug = ? AND observed_phase = ?" + listWorkersOrderBy
	mock.ExpectQuery(regexp.QuoteMeta(wantSQL)).
		WithArgs("team-a", "Running").
		WillReturnRows(workerRows(a))

	if _, err := store.List(context.Background(), ListFilter{TeamSlug: "team-a", ObservedPhase: "Running"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreListEmptyIsNonNil(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(listWorkersSQL + listWorkersOrderBy)).
		WillReturnRows(sqlmock.NewRows(workerColumnNames()))

	got, err := store.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("List returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("List = %v, want empty", got)
	}
}

func TestSQLStoreUpdateSpecChanged(t *testing.T) {
	store, mock := newMockStore(t)
	old := sampleWorker()
	old.Generation = 3
	old.Version = 7
	oldHash, err := SpecHash(old.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	old.SpecHash = oldHash

	spec := old.Spec()
	spec.Model = "qwen-plus"
	newHash, err := SpecHash(spec)
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	if newHash == oldHash {
		t.Fatal("test setup: model change must change the digest")
	}

	want := *old
	want.Model = "qwen-plus"
	want.Generation = 4
	want.Version = 8
	want.SpecHash = newHash
	want.UpdatedAt = storeTestNow

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForUpdateSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"spec_hash", "generation", "version", "created_at"}).
			AddRow(oldHash, int64(3), int64(7), storeTestNow))
	updateArgs := append(workerSpecValues(spec), newHash, storeTestNow, "worker-a", int64(7))
	mock.ExpectExec(regexp.QuoteMeta(updateSpecSETPrefix) + ".*").
		WithArgs(updateArgs...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.outbox")).
		WithArgs(
			hexID{},
			outbox.IdemKey("worker-a", 4, "update", storeTestNow.UTC().Format(time.RFC3339Nano)),
			"worker-a",
			int64(4),
			"update",
			"controller",
			payloadCheck{generation: 4, hasSpec: true},
			"pending",
			int64(0),
			storeTestNow,
			storeTestNow,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.audit_events")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerSQL)).
		WithArgs("worker-a").
		WillReturnRows(workerRows(&want))
	mock.ExpectCommit()

	got, err := store.UpdateSpec(context.Background(), "worker-a", spec, 7, &AuditMeta{Action: "worker.update"})
	if err != nil {
		t.Fatalf("UpdateSpec: %v", err)
	}
	if got.Generation != 4 || got.Version != 8 {
		t.Errorf("UpdateSpec generation/version = %d/%d, want 4/8", got.Generation, got.Version)
	}
	if got.Model != "qwen-plus" || got.SpecHash != newHash {
		t.Errorf("UpdateSpec model/hash = %q/%q", got.Model, got.SpecHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreUpdateSpecUnchanged(t *testing.T) {
	store, mock := newMockStore(t)
	old := sampleWorker()
	old.Generation = 3
	old.Version = 7
	hash, err := SpecHash(old.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	old.SpecHash = hash

	spec := old.Spec()
	want := *old
	want.Version = 8
	want.UpdatedAt = storeTestNow

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForUpdateSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"spec_hash", "generation", "version", "created_at"}).
			AddRow(hash, int64(3), int64(7), storeTestNow))
	updateArgs := append(workerSpecValues(spec), hash, storeTestNow, "worker-a", int64(7))
	mock.ExpectExec(regexp.QuoteMeta(updateSpecSETPrefix) + ".*").
		WithArgs(updateArgs...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No ops.outbox expectation: an unchanged spec must not enqueue.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.audit_events")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerSQL)).
		WithArgs("worker-a").
		WillReturnRows(workerRows(&want))
	mock.ExpectCommit()

	got, err := store.UpdateSpec(context.Background(), "worker-a", spec, 7, &AuditMeta{Action: "worker.update"})
	if err != nil {
		t.Fatalf("UpdateSpec: %v", err)
	}
	if got.Generation != 3 || got.Version != 8 {
		t.Errorf("UpdateSpec generation/version = %d/%d, want 3/8", got.Generation, got.Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// updateSpecSETPrefix is deliberately independent of the SQL constants: the
// first implementation embedded the bare column list (worker_name, team_slug,
// ...) and sqlmock matched that same wrong constant, so the missing "= ?"
// only surfaced on a real database (MySQL 8 / seekdb 5.7.25: error 1064).
// A literal prefix makes the mock fail if the SET clause regresses again.
const updateSpecSETPrefix = "UPDATE workers SET worker_name = ?, team_slug = ?, model = ?"

// TestUpdateSpecSQLAssignments pins the real-DB-verified UPDATE shape: every
// spec column is assigned (col = ?), in workerSpecColumns order, and the
// placeholder count equals len(specArgs)+4 for both statement variants
// (spec_hash / updated_at / name / version are the four trailing args;
// the generation bump adds no placeholder).
func TestUpdateSpecSQLAssignments(t *testing.T) {
	specCols := strings.Split(workerSpecColumns, ", ")
	specArgs, err := workerSpecArgs(Spec{})
	if err != nil {
		t.Fatalf("workerSpecArgs: %v", err)
	}
	if len(specArgs) != len(specCols) {
		t.Fatalf("workerSpecArgs = %d args, workerSpecColumns = %d columns", len(specArgs), len(specCols))
	}
	cases := []struct {
		name      string
		sql       string
		extraArgs int
		tail      string
	}{
		{
			name:      "bump-generation",
			sql:       updateWorkerSQLBumpGeneration,
			extraArgs: 4,
			tail:      ", spec_hash = ?, generation = generation + 1, version = version + 1, updated_at = ? WHERE name = ? AND version = ?",
		},
		{
			name:      "keep-generation",
			sql:       updateWorkerSQLKeepGeneration,
			extraArgs: 4,
			tail:      ", spec_hash = ?, version = version + 1, updated_at = ? WHERE name = ? AND version = ?",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, "SET worker_name = ?") {
				t.Fatalf("SET must assign columns: %s", tc.sql)
			}
			const prefix = "UPDATE workers SET "
			if !strings.HasPrefix(tc.sql, prefix) {
				t.Fatalf("unexpected UPDATE prefix: %s", tc.sql)
			}
			rest := strings.TrimPrefix(tc.sql, prefix)
			end := strings.Index(rest, ", spec_hash = ?")
			if end < 0 {
				t.Fatalf("missing spec_hash assignment: %s", tc.sql)
			}
			assignments := strings.Split(rest[:end], ", ")
			if len(assignments) != len(specCols) {
				t.Fatalf("assignments = %d, want %d: %v", len(assignments), len(specCols), assignments)
			}
			for i, col := range specCols {
				if want := col + " = ?"; assignments[i] != want {
					t.Errorf("assignment %d = %q, want %q", i, assignments[i], want)
				}
			}
			if got, want := strings.Count(tc.sql, "?"), len(specArgs)+tc.extraArgs; got != want {
				t.Errorf("placeholders = %d, want len(specArgs)+%d = %d", got, tc.extraArgs, want)
			}
			if !strings.HasSuffix(tc.sql, tc.tail) {
				t.Errorf("UPDATE tail = %q, want %q", tc.sql, tc.tail)
			}
		})
	}
}

func TestSQLStoreUpdateSpecVersionConflict(t *testing.T) {
	store, mock := newMockStore(t)
	old := sampleWorker()
	old.Generation = 3
	old.Version = 7
	hash, err := SpecHash(old.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForUpdateSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"spec_hash", "generation", "version", "created_at"}).
			AddRow(hash, int64(3), int64(6), storeTestNow))
	mock.ExpectRollback()

	_, err = store.UpdateSpec(context.Background(), "worker-a", old.Spec(), 7, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateSpec conflict error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreUpdateSpecNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForUpdateSQL)).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{"spec_hash", "generation", "version", "created_at"}))
	mock.ExpectRollback()

	_, err := store.UpdateSpec(context.Background(), "missing", Spec{}, 1, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSpec missing error = %v, want ErrNotFound", err)
	}
}

func TestSQLStoreUpdateSpecRowsAffectedZero(t *testing.T) {
	store, mock := newMockStore(t)
	old := sampleWorker()
	old.Generation = 3
	old.Version = 7
	hash, err := SpecHash(old.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	spec := old.Spec()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForUpdateSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"spec_hash", "generation", "version", "created_at"}).
			AddRow(hash, int64(3), int64(7), storeTestNow))
	mock.ExpectExec(regexp.QuoteMeta(updateSpecSETPrefix) + ".*").
		WithArgs(append(workerSpecValues(spec), hash, storeTestNow, "worker-a", int64(7))...).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err = store.UpdateSpec(context.Background(), "worker-a", spec, 7, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateSpec rows=0 error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreUpdateStatus(t *testing.T) {
	// Shape contract: observed columns only, generation gate in WHERE.
	if !strings.Contains(updateWorkerStatusSQL, "generation <= ?") {
		t.Fatalf("UpdateStatus SQL must gate on generation <= ?, got %s", updateWorkerStatusSQL)
	}
	if !strings.Contains(updateWorkerStatusSQL, "deleted_at IS NULL") {
		t.Fatalf("UpdateStatus SQL must filter deleted rows, got %s", updateWorkerStatusSQL)
	}
	if strings.Contains(updateWorkerStatusSQL, "spec_hash") {
		t.Fatalf("UpdateStatus SQL must not touch spec_hash, got %s", updateWorkerStatusSQL)
	}
	if strings.Contains(updateWorkerStatusSQL, "generation = ?") {
		t.Fatalf("UpdateStatus SQL must not assign generation, got %s", updateWorkerStatusSQL)
	}

	store, mock := newMockStore(t)
	o := Observed{
		Phase:            "Running",
		ContainerState:   "running",
		MatrixUserID:     "@bot:example.test",
		RoomID:           "!room:example.test",
		Message:          "up",
		LastActiveAt:     timePtr(storeTestNow),
		AgentStatus:      "busy",
		RunningTaskCount: intPtr(2),
		LastRunAt:        timePtr(storeTestNow),
		LastFinishAt:     timePtr(storeTestNow),
	}
	mock.ExpectExec(regexp.QuoteMeta(updateWorkerStatusSQL)).
		WithArgs(
			"Running",
			"running",
			"@bot:example.test",
			"!room:example.test",
			"up",
			storeTestNow,
			"busy",
			int64(2),
			storeTestNow,
			storeTestNow,
			storeTestNow,
			"worker-a",
			int64(3),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	updated, err := store.UpdateStatus(context.Background(), "worker-a", o, 3)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !updated {
		t.Fatal("UpdateStatus = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreUpdateStatusStaleGeneration(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec(regexp.QuoteMeta(updateWorkerStatusSQL)).
		WithArgs("Pending", nil, nil, nil, nil, nil, nil, nil, nil, nil, storeTestNow, "worker-a", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	updated, err := store.UpdateStatus(context.Background(), "worker-a", Observed{Phase: "Pending"}, 1)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if updated {
		t.Fatal("UpdateStatus = true for affected rows 0, want false")
	}
}

func TestSQLStoreSoftDelete(t *testing.T) {
	store, mock := newMockStore(t)
	old := sampleWorker()
	old.Generation = 5
	old.Version = 3
	hash, err := SpecHash(old.Spec())
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	old.SpecHash = hash

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForDeleteSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"version", "generation", "spec_hash", "created_at"}).
			AddRow(int64(3), int64(5), hash, storeTestNow))
	mock.ExpectExec(regexp.QuoteMeta(softDeleteWorkerSQL)).
		WithArgs(storeTestNow, storeTestNow, "worker-a", int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.outbox")).
		WithArgs(
			hexID{},
			outbox.IdemKey("worker-a", 5, "deprovision", storeTestNow.UTC().Format(time.RFC3339Nano)),
			"worker-a",
			int64(5),
			"deprovision",
			"controller",
			payloadCheck{generation: 5, hasSpec: false},
			"pending",
			int64(0),
			storeTestNow,
			storeTestNow,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ops.audit_events")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.SoftDelete(context.Background(), "worker-a", 3, &AuditMeta{Action: "worker.delete"}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLStoreSoftDeleteConflict(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForDeleteSQL)).
		WithArgs("worker-a").
		WillReturnRows(sqlmock.NewRows([]string{"version", "generation", "spec_hash", "created_at"}).
			AddRow(int64(4), int64(5), "hash", storeTestNow))
	mock.ExpectRollback()

	err := store.SoftDelete(context.Background(), "worker-a", 3, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SoftDelete conflict error = %v, want ErrConflict", err)
	}
}

func TestSQLStoreSoftDeleteNotFound(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectWorkerForDeleteSQL)).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{"version", "generation", "spec_hash", "created_at"}))
	mock.ExpectRollback()

	err := store.SoftDelete(context.Background(), "missing", 1, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SoftDelete missing error = %v, want ErrNotFound", err)
	}
}
