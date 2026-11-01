# Changelog

## 0.2.0 (2026-10-28)

### Added

- **A · cancel/stop**: `POST /api/qwen-code-bridge/dispatch/{task_id}/cancel`
  (alias `POST .../stop`) with optional body
  `{"reason","grace_s":10,"wait":false}`. Success: `202` with
  `{"ok", "task_id", "previous_state", "state", "pid",
  "signalled": "SIGTERM"|null, "already_terminal": false}`; idempotent
  cancel on a terminal task returns `200` with `"already_terminal": true`.
  Errors: `404 task_not_found`, `409 pid_mismatch` (identity check failed,
  no signal sent), `409 not_cancellable` (protected pid).
- **A · `cancelling` state**: running task → `cancelling` after SIGTERM,
  final `cancelled` written by the watchdog with
  `death_reason="cancelled_by_user"` (a cancel request overrides
  timeout/done exit classification).
- **A · task JSON fields**: `cancel_requested_at`, `cancel_reason`,
  `signals: [{"ts","signal","pid"}]`, `cancelled_at`.
- **A · safety rules**: only the child pid itself is signalled (no
  killpg/negative pid/pkill); `/proc/<pid>/cmdline` must name both the CLI
  executable and the task session_id; pid ≤ 1 / own pid / parent pid are
  refused; SIGKILL only after the grace window (default 10s, cap 60s),
  both signals recorded in `signals[]`; per-task asyncio.Lock, no double
  signal. Pending tasks (no pid) cancel directly to `cancelled` with
  `death_reason="cancelled_before_spawn"`; the spawn path re-checks under
  the same lock and never starts a cancelled task.
- **B1**: dispatch budget defaults to 7200s when not passed (the
  `timeout_s` body field is the spec-name alias of `timeout` and wins when
  both are given); the task JSON records `timeout_s`; the watchdog budget
  is always bounded (no more None-unbounded path).
- **B2**: `GET /dispatch/{task_id}/log?tail=N` (default 200, cap 2000,
  `tail=0` = full) returns `{"task_id","log","truncated","total_lines",
  "tail"}`.
- **B3**: `GET /dispatch/{task_id}/progress?limit=N` (default 200, cap
  2000) additionally returns the last stream-json events plus
  `{"events","truncated","total_events","limit"}`.
- **B4**: on plugin load, `dispatch/*.json` tasks left `running`/
  `cancelling` with a dead child pid are finalized (`failed` +
  `death_reason="orphaned_on_reload"`; `cancelled` for cancelling).
  Idempotent; never kills live processes; fresh or terminating tasks are
  left alone.
- **B5**: `GET /api/qwen-code-bridge/version` → `{"plugin", "started_at",
  "python", "qwen_cli", "state_dir"}`.
- **B6**: `scripts/package.sh` builds a distributable zip (excludes
  `__pycache__`, `tests`, `state`, `web-shell/dist`, `*.pyc`, `*.log`).

### Changed

- `DEFAULT_DISPATCH_TIMEOUT` 3600 → 7200 (2h wall budget by default).
- Millforge mapping: `cancelling` → running (agent tier), `cancelled` →
  cancelled (agent tier), so a user-initiated stop does not page the
  human tier.

## 0.1.0 (baseline)

- Management surface (probe/settings/install) ported from QwenPaw PR
  8156; session-injection middleware; gateless dispatch with watchdog,
  progress tracking, human/agent interaction, wake queue, sidebar chat
  sync, and the 千锻 (millforge) run projection.
