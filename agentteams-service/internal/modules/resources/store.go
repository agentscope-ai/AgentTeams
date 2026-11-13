package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/audit"
	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/outbox"
)

// workerColumns lists the res.workers columns in DDL order
// (migrations/res/0001-workers.sql). The frozen DDL has 47 columns: the
// brief says 49, but the migration is authoritative and 58 §4.1 lists the
// same 47.
const workerColumns = "name, worker_name, team_slug, model, model_provider, subagent_model, " +
	"llm_stream_first_content_timeout, llm_stream_idle_timeout, " +
	"llm_max_retries, llm_backoff_base, llm_backoff_cap, llm_max_concurrent, " +
	"llm_max_qpm, llm_rate_limit_pause, llm_rate_limit_jitter, " +
	"llm_acquire_timeout, runtime, image, identity, soul, agents, skills_json, " +
	"remote_skills_json, mcp_servers_json, package_ref, expose_json, " +
	"channel_policy_json, resources_json, env_json, container_managed, " +
	"desired_state, observed_phase, observed_container_state, matrix_user_id, " +
	"room_id, last_active_at, agent_status, running_task_count, last_run_at, " +
	"last_finish_at, message, generation, spec_hash, version, created_at, " +
	"updated_at, deleted_at"

// workerSpecColumns are the columns that carry Spec (58 §4.1): everything
// between name and observed_phase, i.e. spec_hash/generation/version and
// the timestamps are excluded.
const workerSpecColumns = "worker_name, team_slug, model, model_provider, subagent_model, " +
	"llm_stream_first_content_timeout, llm_stream_idle_timeout, " +
	"llm_max_retries, llm_backoff_base, llm_backoff_cap, llm_max_concurrent, " +
	"llm_max_qpm, llm_rate_limit_pause, llm_rate_limit_jitter, " +
	"llm_acquire_timeout, runtime, image, identity, soul, agents, skills_json, " +
	"remote_skills_json, mcp_servers_json, package_ref, expose_json, " +
	"channel_policy_json, resources_json, env_json, container_managed, " +
	"desired_state"

// workerSpecAssignments is the UPDATE form of workerSpecColumns
// ("worker_name = ?, team_slug = ?, ..."). Deriving it from the one column
// list keeps INSERT, SELECT and UPDATE in lockstep — a literal assignment
// list would silently drift when a column is added.
//
// Real-DB note: a bare column list in SET is invalid; MySQL 8 / seekdb
// 5.7.25 reject it with error 1064. This shape has been verified against
// both engines.
var workerSpecAssignments = func() string {
	cols := strings.Split(workerSpecColumns, ", ")
	for i := range cols {
		cols[i] += " = ?"
	}
	return strings.Join(cols, ", ")
}()

const workerInsertPlaceholders = "?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?"

// workerInsertSQL inserts a full row. NULL is passed as nil for every
// nullable column.
const workerInsertSQL = "INSERT INTO workers (" + workerColumns + ") VALUES (" + workerInsertPlaceholders + ")"

// resurrectWorkerSQL is INSERT ... ON DUPLICATE KEY UPDATE. Every column is
// rewritten only when the existing row is soft-deleted; a live row is left
// untouched (RowsAffected 0 -> already_exists). This makes delete->create
// atomic without the old insert/delete/insert dance that deadlocked under
// same-name concurrency.
var resurrectWorkerSQL = buildResurrectWorkerSQL()

func buildResurrectWorkerSQL() string {
	assigns := make([]string, 0, 47)
	for _, raw := range strings.Split(workerColumns, ",") {
		col := strings.TrimSpace(raw)
		switch col {
		case "name":
			continue
		case "deleted_at":
			// Must stay last so the IF clauses above see the old value.
			assigns = append(assigns, col+" = IF("+col+" IS NOT NULL, NULL, "+col+")")
		default:
			assigns = append(assigns, col+" = IF(deleted_at IS NOT NULL, VALUES("+col+"), "+col+")")
		}
	}
	return workerInsertSQL + " ON DUPLICATE KEY UPDATE " + strings.Join(assigns, ", ")
}

const selectWorkerSQL = "SELECT " + workerColumns + " FROM workers WHERE name = ? AND deleted_at IS NULL"

// selectWorkerForUpdateSQL locks the live row for update and returns the
// three columns UpdateSpec needs to decide conflict/generation/outbox.
const selectWorkerForUpdateSQL = "SELECT spec_hash, generation, version, created_at FROM workers WHERE name = ? AND deleted_at IS NULL FOR UPDATE"

// selectWorkerForDeleteSQL locks the live row and returns what SoftDelete
// needs: the optimistic-lock version, the unchanged generation for the
// deprovision idem key, and the pre-delete digest for the audit trail.
const selectWorkerForDeleteSQL = "SELECT version, generation, spec_hash, created_at FROM workers WHERE name = ? AND deleted_at IS NULL FOR UPDATE"

const listWorkersSQL = "SELECT " + workerColumns + " FROM workers WHERE deleted_at IS NULL"

const listWorkersOrderBy = " ORDER BY name ASC"

// updateWorkerSQLBumpGeneration is used when the canonical spec digest
// changed. generation+1 is expressed in SQL so the new generation is
// derived from the locked row, never from a client-supplied number.
var updateWorkerSQLBumpGeneration = "UPDATE workers SET " + workerSpecAssignments + ", spec_hash = ?, generation = generation + 1, version = version + 1, updated_at = ? WHERE name = ? AND version = ?"

// updateWorkerSQLKeepGeneration is used when the digest is unchanged:
// only version/updated_at move, and no outbox event is written.
var updateWorkerSQLKeepGeneration = "UPDATE workers SET " + workerSpecAssignments + ", spec_hash = ?, version = version + 1, updated_at = ? WHERE name = ? AND version = ?"

// updateWorkerStatusSQL writes observed_* only. It never touches
// generation/spec_hash/version and emits no outbox event (58 §4.1);
// generation <= ? discards out-of-order controller callbacks.
// COALESCE keeps the stored value when the callback does not report a field
// (nil): a partial status push must never silently clear matrix/room ids or
// task counters (58 §11 I17). A genuine clear is not a R1 use case.
const updateWorkerStatusSQL = "UPDATE workers SET observed_phase = COALESCE(?, observed_phase), observed_container_state = COALESCE(?, observed_container_state), matrix_user_id = COALESCE(?, matrix_user_id), room_id = COALESCE(?, room_id), message = COALESCE(?, message), last_active_at = COALESCE(?, last_active_at), agent_status = COALESCE(?, agent_status), running_task_count = COALESCE(?, running_task_count), last_run_at = COALESCE(?, last_run_at), last_finish_at = COALESCE(?, last_finish_at), updated_at = ? WHERE name = ? AND generation <= ? AND deleted_at IS NULL"

const softDeleteWorkerSQL = "UPDATE workers SET deleted_at = ?, version = version + 1, updated_at = ? WHERE name = ? AND version = ?"

// SQLStore is the MySQL implementation of Store.
//
// Transaction model (58 §4): res and ops are two schemas of the same MySQL
// instance. A *sql.Tx is bound to one connection, so every transaction is
// opened on the res pool (DB) and the ops tables are referenced by their
// qualified names ops.outbox / ops.audit_events. That is what lets the
// worker row and its outbox/audit rows commit or roll back together.
//
// Deployment prerequisite: the ResDSN account must hold INSERT/SELECT on
// the ops schema; the ops pool is opened separately by the kernel only for
// migrations/dispatcher-style work.
type SQLStore struct {
	DB  *sql.DB
	Now func() time.Time // nil → time.Now; all times are written in UTC
}

// NewSQLStore returns a Store backed by db (the res pool).
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

// Compile-time contract check.
var _ Store = (*SQLStore)(nil)

// Create inserts the worker row plus its provision outbox event and allow
// audit event in one transaction (58 §4.2). Defaults: generation 1,
// version 1, observed_phase "Pending", timestamps = now.
func (s *SQLStore) Create(ctx context.Context, w *Worker, a *AuditMeta) (*Worker, error) {
	if w == nil {
		return nil, errors.New("resources: create: nil worker")
	}
	now := s.now()
	w.Generation = 1
	w.Version = 1
	if w.ObservedPhase == "" {
		w.ObservedPhase = "Pending"
	}
	w.CreatedAt = now
	w.UpdatedAt = now

	spec := w.Spec()
	hash, err := SpecHash(spec)
	if err != nil {
		return nil, fmt.Errorf("resources: create worker %q: %w", w.Name, err)
	}
	w.SpecHash = hash
	wire, err := WireSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("resources: create worker %q: %w", w.Name, err)
	}
	meta := auditMeta(a)

	args, aerr := insertWorkerArgs(w)
	if aerr != nil {
		return nil, fmt.Errorf("resources: create worker %q: %w", w.Name, aerr)
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, ierr := tx.ExecContext(ctx, resurrectWorkerSQL, args...)
		if ierr != nil {
			return createWriteError(w.Name, ierr)
		}
		n, rerr := res.RowsAffected()
		if rerr != nil {
			return fmt.Errorf("resources: create worker %q rows affected: %w", w.Name, rerr)
		}
		if n == 0 {
			// The row exists and is live: the ON DUPLICATE clause was a no-op.
			return fmt.Errorf("resources: create worker %q: %w", w.Name, ErrAlreadyExists)
		}
		if err := outbox.Enqueue(ctx, tx, outbox.Event{
			WorkerName:  w.Name,
			Generation:  w.Generation,
			EventType:   "provision",
			Incarnation: w.CreatedAt.UTC().Format(time.RFC3339Nano),
			Payload: map[string]any{
				"name":        w.Name,
				"generation":  w.Generation,
				"incarnation": w.CreatedAt.UTC().Format(time.RFC3339Nano),
				"spec":        wire,
			},
		}, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{
			RequestID:       meta.RequestID,
			ActorType:       meta.ActorType,
			ActorID:         meta.ActorID,
			OnBehalfOfType:  meta.OnBehalfOfType,
			OnBehalfOfID:    meta.OnBehalfOfID,
			TeamUID:         meta.TeamUID,
			ProjectUID:      meta.ProjectUID,
			TaskID:          meta.TaskID,
			Action:          meta.Action,
			Decision:        "allow",
			AfterDigest:     hash,
			TokenJTI:        meta.TokenJTI,
			Client:          meta.Client,
			ResourceVersion: meta.ResourceVersion,
		}, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Get reads one live worker by name.
func (s *SQLStore) Get(ctx context.Context, name string) (*Worker, error) {
	w, err := scanWorker(s.DB.QueryRowContext(ctx, selectWorkerSQL, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("resources: get worker %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("resources: get worker %q: %w", name, err)
	}
	return w, nil
}

// List returns live workers in name ascending order (58 §1.2: no
// pagination). The result is never nil.
func (s *SQLStore) List(ctx context.Context, f ListFilter) ([]*Worker, error) {
	q := listWorkersSQL
	args := make([]any, 0, 2)
	if f.TeamSlug != "" {
		q += " AND team_slug = ?"
		args = append(args, f.TeamSlug)
	}
	if f.ObservedPhase != "" {
		q += " AND observed_phase = ?"
		args = append(args, f.ObservedPhase)
	}
	q += listWorkersOrderBy

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("resources: list workers: %w", err)
	}
	defer rows.Close()

	out := make([]*Worker, 0, 16)
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("resources: list workers: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resources: list workers: %w", err)
	}
	return out, nil
}

// UpdateSpec applies spec under the version optimistic lock. The
// generation advances iff the canonical spec digest changed, and only then
// is an 'update' outbox event enqueued. An audit event is recorded for
// every successful update (the allow decision), with the digest pair
// before/after.
func (s *SQLStore) UpdateSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error) {
	return s.updateSpec(ctx, name, spec, expectedVersion, a, true)
}

// SyncSpec applies spec like UpdateSpec but enqueues a generation-only
// "sync" event (no spec). See the Store interface comment.
func (s *SQLStore) SyncSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta) (*Worker, error) {
	return s.updateSpec(ctx, name, spec, expectedVersion, a, false)
}

func (s *SQLStore) updateSpec(ctx context.Context, name string, spec Spec, expectedVersion int64, a *AuditMeta, emitSpec bool) (*Worker, error) {
	now := s.now()
	canonical, err := CanonicalSpecJSON(spec)
	if err != nil {
		return nil, fmt.Errorf("resources: update worker %q: %w", name, err)
	}
	newHash := hashCanonicalSpec(canonical)
	wire, err := WireSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("resources: update worker %q: %w", name, err)
	}
	meta := auditMeta(a)

	var updated *Worker
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			oldHash    string
			generation int64
			version    int64
			createdAt  time.Time
		)
		err := tx.QueryRowContext(ctx, selectWorkerForUpdateSQL, name).
			Scan(&oldHash, &generation, &version, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resources: update worker %q: %w", name, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("resources: lock worker %q: %w", name, err)
		}
		if version != expectedVersion {
			return fmt.Errorf("resources: update worker %q: %w", name, ErrConflict)
		}

		changed := newHash != oldHash
		specArgs, err := workerSpecArgs(spec)
		if err != nil {
			return fmt.Errorf("resources: update worker %q: %w", name, err)
		}
		args := make([]any, 0, len(specArgs)+4)
		args = append(args, specArgs...)
		args = append(args, newHash, now, name, expectedVersion)

		updateSQL := updateWorkerSQLKeepGeneration
		if changed {
			updateSQL = updateWorkerSQLBumpGeneration
		}
		res, err := tx.ExecContext(ctx, updateSQL, args...)
		if err != nil {
			return fmt.Errorf("resources: update worker %q: %w", name, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("resources: update worker %q rows affected: %w", name, err)
		}
		if affected == 0 {
			return fmt.Errorf("resources: update worker %q: %w", name, ErrConflict)
		}

		if changed {
			incarnation := createdAt.UTC().Format(time.RFC3339Nano)
			eventType := "update"
			payload := map[string]any{
				"name":        name,
				"generation":  generation + 1,
				"incarnation": incarnation,
			}
			if emitSpec {
				payload["spec"] = wire
			} else {
				// Controller already applied the write; only refresh the
				// applied generation/incarnation bookkeeping.
				eventType = "sync"
			}
			if err := outbox.Enqueue(ctx, tx, outbox.Event{
				WorkerName:  name,
				Generation:  generation + 1,
				EventType:   eventType,
				Incarnation: incarnation,
				Payload:     payload,
			}, now); err != nil {
				return err
			}
		}

		if err := audit.Record(ctx, tx, audit.Event{
			RequestID:       meta.RequestID,
			ActorType:       meta.ActorType,
			ActorID:         meta.ActorID,
			OnBehalfOfType:  meta.OnBehalfOfType,
			OnBehalfOfID:    meta.OnBehalfOfID,
			TeamUID:         meta.TeamUID,
			ProjectUID:      meta.ProjectUID,
			TaskID:          meta.TaskID,
			Action:          meta.Action,
			Decision:        "allow",
			BeforeDigest:    oldHash,
			AfterDigest:     newHash,
			TokenJTI:        meta.TokenJTI,
			Client:          meta.Client,
			ResourceVersion: meta.ResourceVersion,
		}, now); err != nil {
			return err
		}

		w, err := scanWorker(tx.QueryRowContext(ctx, selectWorkerSQL, name))
		if err != nil {
			return fmt.Errorf("resources: reload worker %q: %w", name, err)
		}
		updated = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdateStatus writes the controller-reported observed_* fields only. It
// never touches generation/spec_hash/version, emits no outbox event, and
// discards callbacks whose generation is older than the stored one.
// It returns whether a row was updated.
func (s *SQLStore) UpdateStatus(ctx context.Context, name string, o Observed, generation int64) (bool, error) {
	res, err := s.DB.ExecContext(ctx, updateWorkerStatusSQL,
		nullableString(o.Phase),
		nullableString(o.ContainerState),
		nullableString(o.MatrixUserID),
		nullableString(o.RoomID),
		nullableString(o.Message),
		nullableTime(o.LastActiveAt),
		nullableString(o.AgentStatus),
		nullableInt(o.RunningTaskCount),
		nullableTime(o.LastRunAt),
		nullableTime(o.LastFinishAt),
		s.now(),
		name,
		generation,
	)
	if err != nil {
		return false, fmt.Errorf("resources: update status for worker %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resources: update status for worker %q rows affected: %w", name, err)
	}
	return affected == 1, nil
}

// SoftDelete stamps deleted_at and bumps version under the optimistic
// lock, then writes the deprovision outbox event and the allow audit
// event in the same transaction. The generation is unchanged (58 §4.2).
func (s *SQLStore) SoftDelete(ctx context.Context, name string, expectedVersion int64, a *AuditMeta) error {
	now := s.now()
	meta := auditMeta(a)

	return s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			version    int64
			generation int64
			specHash   string
			createdAt  time.Time
		)
		err := tx.QueryRowContext(ctx, selectWorkerForDeleteSQL, name).
			Scan(&version, &generation, &specHash, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resources: delete worker %q: %w", name, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("resources: lock worker %q: %w", name, err)
		}
		if version != expectedVersion {
			return fmt.Errorf("resources: delete worker %q: %w", name, ErrConflict)
		}

		res, err := tx.ExecContext(ctx, softDeleteWorkerSQL, now, now, name, expectedVersion)
		if err != nil {
			return fmt.Errorf("resources: delete worker %q: %w", name, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("resources: delete worker %q rows affected: %w", name, err)
		}
		if affected == 0 {
			return fmt.Errorf("resources: delete worker %q: %w", name, ErrConflict)
		}

		if err := outbox.Enqueue(ctx, tx, outbox.Event{
			WorkerName:  name,
			Generation:  generation,
			EventType:   "deprovision",
			Incarnation: createdAt.UTC().Format(time.RFC3339Nano),
			Payload: map[string]any{
				"name":        name,
				"generation":  generation,
				"incarnation": createdAt.UTC().Format(time.RFC3339Nano),
			},
		}, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{
			RequestID:       meta.RequestID,
			ActorType:       meta.ActorType,
			ActorID:         meta.ActorID,
			OnBehalfOfType:  meta.OnBehalfOfType,
			OnBehalfOfID:    meta.OnBehalfOfID,
			TeamUID:         meta.TeamUID,
			ProjectUID:      meta.ProjectUID,
			TaskID:          meta.TaskID,
			Action:          meta.Action,
			Decision:        "allow",
			BeforeDigest:    specHash,
			TokenJTI:        meta.TokenJTI,
			Client:          meta.Client,
			ResourceVersion: meta.ResourceVersion,
		}, now); err != nil {
			return err
		}
		return nil
	})
}

// withTx runs fn in a transaction on the res pool and commits on success.
// Any error rolls back; a commit error is reported after a best-effort
// rollback.
func (s *SQLStore) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("resources: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("resources: commit transaction: %w", err)
	}
	return nil
}

// now returns the store clock in UTC.
func (s *SQLStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// auditMeta dereferences the request audit context; a nil *AuditMeta is
// treated as an empty one so repository calls never panic.
func auditMeta(a *AuditMeta) AuditMeta {
	if a == nil {
		return AuditMeta{}
	}
	return *a
}

// isDuplicateKey reports MySQL error 1062 (duplicate entry), which on
// Create means the worker name already exists.
func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// isFieldTooLong reports MySQL error 1406 (data too long for column). The
// handler maps it to 400 instead of leaking a 500 for a request that can
// never succeed.
func isFieldTooLong(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1406
}

// createWriteError maps an INSERT failure to the store's sentinels.
func createWriteError(name string, err error) error {
	switch {
	case isDuplicateKey(err):
		return fmt.Errorf("resources: create worker %q: %w", name, ErrAlreadyExists)
	case isFieldTooLong(err):
		return fmt.Errorf("resources: insert worker %q: %w", name, ErrFieldTooLong)
	default:
		return fmt.Errorf("resources: insert worker %q: %w", name, err)
	}
}

// insertWorkerArgs builds the 47 INSERT arguments in workerColumns order.
func insertWorkerArgs(w *Worker) ([]any, error) {
	specArgs, err := workerSpecArgs(w.Spec())
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, 47)
	args = append(args, w.Name)
	args = append(args, specArgs...)
	args = append(args,
		w.ObservedPhase,
		nullableString(w.ObservedContainerState),
		nullableString(w.MatrixUserID),
		nullableString(w.RoomID),
		nullableTime(w.LastActiveAt),
		nullableString(w.AgentStatus),
		nullableInt(w.RunningTaskCount),
		nullableTime(w.LastRunAt),
		nullableTime(w.LastFinishAt),
		nullableString(w.Message),
		w.Generation,
		w.SpecHash,
		w.Version,
		w.CreatedAt.UTC(),
		w.UpdatedAt.UTC(),
		nullableTime(w.DeletedAt),
	)
	return args, nil
}

// workerSpecArgs builds the 30 spec column arguments in workerSpecColumns
// order.
func workerSpecArgs(spec Spec) ([]any, error) {
	skills, err := skillsArg(spec.Skills)
	if err != nil {
		return nil, fmt.Errorf("encode skills: %w", err)
	}
	env, err := envArg(spec.Env)
	if err != nil {
		return nil, fmt.Errorf("encode env: %w", err)
	}
	return []any{
		nullableString(spec.WorkerName),
		nullableString(spec.TeamSlug),
		nullableString(spec.Model),
		nullableString(spec.ModelProvider),
		nullableString(spec.SubagentModel),

		nullableString(spec.LLMStreamFirstContentTimeout),
		nullableString(spec.LLMStreamIdleTimeout),
		nullableString(spec.LLMMaxRetries),
		nullableString(spec.LLMBackoffBase),
		nullableString(spec.LLMBackoffCap),
		nullableString(spec.LLMMaxConcurrent),
		nullableString(spec.LLMMaxQPM),
		nullableString(spec.LLMRateLimitPause),
		nullableString(spec.LLMRateLimitJitter),
		nullableString(spec.LLMAcquireTimeout),

		nullableString(spec.Runtime),
		nullableString(spec.Image),
		nullableString(spec.Identity),
		nullableString(spec.Soul),
		nullableString(spec.Agents),

		skills,
		jsonArg(spec.RemoteSkills),
		jsonArg(spec.MCPServers),
		nullableString(spec.PackageRef),
		jsonArg(spec.Expose),
		jsonArg(spec.ChannelPolicy),
		jsonArg(spec.Resources),
		env,

		spec.ContainerManaged,
		nullableString(spec.DesiredState),
	}, nil
}

// nullableString maps an empty string to SQL NULL.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableTime maps a nil timestamp to SQL NULL, otherwise stores UTC.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// nullableInt maps a nil int pointer to SQL NULL.
func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// jsonArg maps an empty RawMessage to SQL NULL; MySQL validates the JSON
// text on insert.
func jsonArg(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// skillsArg encodes the skills array, or NULL when it is empty.
func skillsArg(skills []string) (any, error) {
	if len(skills) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(skills)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// envArg encodes env, or NULL when it is empty.
func envArg(env map[string]string) (any, error) {
	if len(env) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanWorker maps one res.workers row. Destinations follow workerColumns
// order exactly.
func scanWorker(sc rowScanner) (*Worker, error) {
	var (
		w Worker

		workerName    sql.NullString
		teamSlug      sql.NullString
		model         sql.NullString
		modelProvider sql.NullString
		subagentModel sql.NullString

		llmStreamFirstContentTimeout sql.NullString
		llmStreamIdleTimeout         sql.NullString
		llmMaxRetries                sql.NullString
		llmBackoffBase               sql.NullString
		llmBackoffCap                sql.NullString
		llmMaxConcurrent             sql.NullString
		llmMaxQPM                    sql.NullString
		llmRateLimitPause            sql.NullString
		llmRateLimitJitter           sql.NullString
		llmAcquireTimeout            sql.NullString

		runtime  sql.NullString
		image    sql.NullString
		identity sql.NullString
		soul     sql.NullString
		agents   sql.NullString

		skillsRaw        []byte
		remoteSkillsRaw  []byte
		mcpServersRaw    []byte
		packageRef       sql.NullString
		exposeRaw        []byte
		channelPolicyRaw []byte
		resourcesRaw     []byte
		envRaw           []byte

		desiredState sql.NullString

		observedPhase          sql.NullString
		observedContainerState sql.NullString
		matrixUserID           sql.NullString
		roomID                 sql.NullString
		lastActiveAt           sql.NullTime
		agentStatus            sql.NullString
		runningTaskCount       sql.NullInt64
		lastRunAt              sql.NullTime
		lastFinishAt           sql.NullTime
		message                sql.NullString

		generation int64
		specHash   string
		version    int64
		createdAt  time.Time
		updatedAt  time.Time
		deletedAt  sql.NullTime
	)

	if err := sc.Scan(
		&w.Name,
		&workerName,
		&teamSlug,
		&model,
		&modelProvider,
		&subagentModel,

		&llmStreamFirstContentTimeout,
		&llmStreamIdleTimeout,
		&llmMaxRetries,
		&llmBackoffBase,
		&llmBackoffCap,
		&llmMaxConcurrent,
		&llmMaxQPM,
		&llmRateLimitPause,
		&llmRateLimitJitter,
		&llmAcquireTimeout,

		&runtime,
		&image,
		&identity,
		&soul,
		&agents,

		&skillsRaw,
		&remoteSkillsRaw,
		&mcpServersRaw,
		&packageRef,
		&exposeRaw,
		&channelPolicyRaw,
		&resourcesRaw,
		&envRaw,

		&w.ContainerManaged,
		&desiredState,

		&observedPhase,
		&observedContainerState,
		&matrixUserID,
		&roomID,
		&lastActiveAt,
		&agentStatus,
		&runningTaskCount,
		&lastRunAt,
		&lastFinishAt,
		&message,

		&generation,
		&specHash,
		&version,
		&createdAt,
		&updatedAt,
		&deletedAt,
	); err != nil {
		return nil, err
	}

	w.WorkerName = workerName.String
	w.TeamSlug = teamSlug.String
	w.Model = model.String
	w.ModelProvider = modelProvider.String
	w.SubagentModel = subagentModel.String

	w.LLMStreamFirstContentTimeout = llmStreamFirstContentTimeout.String
	w.LLMStreamIdleTimeout = llmStreamIdleTimeout.String
	w.LLMMaxRetries = llmMaxRetries.String
	w.LLMBackoffBase = llmBackoffBase.String
	w.LLMBackoffCap = llmBackoffCap.String
	w.LLMMaxConcurrent = llmMaxConcurrent.String
	w.LLMMaxQPM = llmMaxQPM.String
	w.LLMRateLimitPause = llmRateLimitPause.String
	w.LLMRateLimitJitter = llmRateLimitJitter.String
	w.LLMAcquireTimeout = llmAcquireTimeout.String

	w.Runtime = runtime.String
	w.Image = image.String
	w.Identity = identity.String
	w.Soul = soul.String
	w.Agents = agents.String

	if len(skillsRaw) > 0 {
		if err := json.Unmarshal(skillsRaw, &w.Skills); err != nil {
			return nil, fmt.Errorf("workers.skills_json: %w", err)
		}
	}
	w.RemoteSkills = rawJSON(remoteSkillsRaw)
	w.MCPServers = rawJSON(mcpServersRaw)
	w.PackageRef = packageRef.String
	w.Expose = rawJSON(exposeRaw)
	w.ChannelPolicy = rawJSON(channelPolicyRaw)
	w.Resources = rawJSON(resourcesRaw)
	if len(envRaw) > 0 {
		if err := json.Unmarshal(envRaw, &w.Env); err != nil {
			return nil, fmt.Errorf("workers.env_json: %w", err)
		}
	}

	w.DesiredState = desiredState.String

	w.ObservedPhase = observedPhase.String
	w.ObservedContainerState = observedContainerState.String
	w.MatrixUserID = matrixUserID.String
	w.RoomID = roomID.String
	w.LastActiveAt = nullTimePtr(lastActiveAt)
	w.AgentStatus = agentStatus.String
	w.RunningTaskCount = nullIntPtr(runningTaskCount)
	w.LastRunAt = nullTimePtr(lastRunAt)
	w.LastFinishAt = nullTimePtr(lastFinishAt)
	w.Message = message.String

	w.Generation = generation
	w.SpecHash = specHash
	w.Version = version
	w.CreatedAt = createdAt.UTC()
	w.UpdatedAt = updatedAt.UTC()
	w.DeletedAt = nullTimePtr(deletedAt)

	return &w, nil
}

// rawJSON copies a nullable JSON column into a RawMessage.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// nullTimePtr converts a nullable DATETIME(6) into a UTC *time.Time.
func nullTimePtr(ns sql.NullTime) *time.Time {
	if !ns.Valid {
		return nil
	}
	t := ns.Time.UTC()
	return &t
}

// nullIntPtr converts a nullable INT into a *int.
func nullIntPtr(ns sql.NullInt64) *int {
	if !ns.Valid {
		return nil
	}
	v := int(ns.Int64)
	return &v
}
