# Task Completion Notification (submit_task) + Lifecycle Attention Events

> **Status**: Implemented in `plugins/teamharness/mcp/server.py` (branch
> `fix/task-completion-notification`).
> **Scope**: The TeamHarness MCP `taskflow` tool — the task lifecycle
> executed by Worker/Leader runtimes. The Manager-runtime hook variant
> (`copaw_worker.hooks.tools.taskflow`) is out of scope: the Manager
> coordinates tasks but is not a task executor, so its `submit_task`
> path is not the production completion route.
>
> **v2 (PR review 2026-09-05, design issue #1229)** extends the original
> completion line into a full lifecycle attention model:
> per-status first-line tokens, `@initiator` (human members) routing,
> P0 sync-before-notify ordering with a retryable failure, a new
> `request_attention` action for in-flight human decisions, and a
> code-level `PROJECT_COMPLETED` event on `complete_project`.

## Problem

The taskflow MCP layer is asymmetric:

- `delegate_task` **atomically** records the assignment in task state and
  publishes the assignment to the task room with `m.mentions`
  (`_send_delegate_notification`, stable txn `delegate-<task-id>`,
  recorded `eventId`, reuse-on-retry).
- `submit_task` records the terminal state and auto-publishes result
  artifacts as `m.file` events (`_publish_task_artifacts`), but the
  completion *message* is only a `notificationNeeded` **hint** — the
  Worker's LLM must self-remember to send
  `@leader TASK_COMPLETED: <task-id> - Result: shared/tasks/<task-id>/result.md`.

Real deployments (Node1, multi-turn sessions) show the Worker forgetting
that line after context compaction. Consequences:

- The Leader receives no wake signal: `check_task` is poll-based, and
  `m.file` artifact events carry no mention, so the Leader is not
  triggered by the artifacts alone.
- Downstream tasks stay stuck in `waiting` until the Leader happens to
  poll or a human pokes the room.

## Design

Mirror the existing delegate pattern, same file, same send path:

| Concern | `delegate_task` (existing) | `submit_task` (this change) |
|:--|:--|:--|
| Send helper | `_send_delegate_notification` | `_send_task_completion_notification` |
| Matrix path | HTTP PUT `/rooms/{room}/send/m.room.message/{txn}` (same as message tool) | identical |
| Credentials | `AGENTTEAMS_MATRIX_URL` + `AGENTTEAMS_WORKER_MATRIX_TOKEN` | identical (Worker's own token — the sender *is* the Worker) |
| Stable txn | `delegate-<task-id>` | `submit-<task-id>-<status>` (status-scoped: a same-status retry de-duplicates; a changed status produces a new event) |
| Mention | assignee mxid | **leader + human members** mxids (resolved from runtime config — the `@initiator` routing, see below) |
| Recorded event | task state `eventId` | task state `completionEventId` |
| Retry | reuse recorded `eventId` | reuse recorded `completionEventId` |
| Failure | task never marked `assigned` (hard fail) | **best-effort**: submission proceeds, response reports `sent: false` + error |

Message contract (first line parseable by leader-side prompts; mirrors
the task-execution skill). Every accepted result status gets its own
first-line token so a leader prompt can branch on the line itself:

```
@leader TASK_COMPLETED: <task-id> - Result: shared/tasks/<task-id>/result.md
- Worker: @worker:matrix.local
<summary preview, ≤500 chars>
```

```
@leader TASK_PARTIAL: <task-id> - <summary>
- Worker: @worker:matrix.local
- Status: PARTIAL
```

```
@leader TASK_REVISION_NEEDED: <task-id> - <summary>
- Worker: @worker:matrix.local
- Status: REVISION_NEEDED
```

```
@leader TASK_BLOCKED: <task-id> - <short blocker summary>
- Worker: @worker:matrix.local
- Status: BLOCKED
```

```
@leader TASK_FAILED: <task-id> - <summary>
- Worker: @worker:matrix.local
- Status: FAILED
```

`SUCCESS` / `SUCCESS_WITH_NOTES` keep the `TASK_COMPLETED` token and the
`Result:` line (no `- Status:` line — the token already says it); every
other token carries the `- Status:` line. `submit_task` validates the
submitted status against the accepted set and rejects unknown values
with a clear error instead of rendering a generic line.

### Leader resolution

`_team_leader_matrix_id()` reads the runtime config the controller
projects into the Worker: `team.members[]` with
`role ∈ {team_leader, teamleader, leader}` → `matrixUserId` (same role
normalization as `_roomflow_room_meta`). Empty result → notification
skipped with a `skipped` reason (standalone runs).

### Human resolution (@initiator)

`_team_human_matrix_ids()` reads the same roster: every member whose
normalized role is neither leader nor worker, plus the `team.admin`
entry. These are the human users of the team (the task initiator
included) and are mentioned alongside the leader in completion and
attention events. Humans are not membership-checked (only the leader
is): a human who is not in the room simply cannot see the room event,
which is the correct Matrix semantics. Empty list → leader-only
mentions (standalone runs unchanged).

### Membership guard

`_validate_assignee_membership(room_id, leader)` is reused as-is: when
the Matrix env is configured, the leader must be a joined member of the
task room; otherwise the send is skipped (reason recorded) instead of
producing an error event for a user who cannot receive it.

### Idempotency (status-scoped, v2)

- Stable txn `submit-<task-id>-<status>`: Matrix de-duplicates a
  redelivered identical PUT for the *same status*.
- `completionEventId` **and** `completionEventStatus` are persisted in
  task state after the first success. A later resubmit with the **same**
  status returns the recorded event with `reused: true` and performs no
  HTTP call. A resubmit with a **changed** status invalidates the
  recorded pair and sends a fresh event (different txn), so a worker
  that first reports `BLOCKED` and later `SUCCESS` wakes the leader
  again instead of being silently absorbed by the reuse branch.
- Tasks recorded before the upgrade (no `completionEventStatus`) keep
  the old behavior: any resubmit reuses the recorded event.

### P0 ordering: sync first, then notify (v2)

The completion event is an *attention signal*, not a receipt. The
submit sequence is: local state → publish artifacts → **sync shared
storage → only then notify**.

- **Sync failure** → `ok: false`, `retryable: true`, **no notification
  at all** (the field is withheld from the response). The local task
  state is already `submitted`, so the retry is idempotent: the event
  is sent exactly once, on the first sync that succeeds. This closes
  the "leader told done, artifacts unreachable" window — the leader
  cannot be woken by an event whose artifacts it cannot read.
- **Notification-level failure** (no room, no leader, no Matrix env,
  membership missing, HTTP error) stays best-effort: it returns
  `{"sent": false, "skipped"?: true, "error": "..."}` and the submit
  still completes: `ok: true`, `status: "submitted"`, artifacts
  published, state synced.
- The existing `notificationNeeded` hint is **kept** — it also drives
  the requester reply-route report, which the code-level line
  intentionally does not cover (different room/audience).

## Lifecycle attention events (v2, issue #1229)

### `request_attention` (new taskflow action)

In-flight human decisions are currently "ambient room chat" — a worker
that needs approval/decision/escalation pokes the group and hopes a
human notices. The new action makes that a first-class, idempotent,
auditable event:

- **Roles**: worker / leader / remote-member. Terminal tasks are
  rejected (`_require_task_mutable`).
- **Payload**: `kind` ∈ `approval | decision | escalation | other`,
  `question` (required, ≤500 chars), optional `resolved: true` to
  close it without a result.
- **Contract line**: `@leader ATTENTION_<KIND>: <task-id> - <question>`
  + `- Worker:` line; mentions leader + human members.
- **State**: appends an `attention` record to task meta
  (`kind / question / attempt / requestedAt / resolved / eventId?`).
  Re-requesting the same `kind` while an unresolved record exists
  reuses the recorded event (no duplicate ping); a new kind or a new
  attempt number gets a fresh event (txn
  `attention-<task-id>-<kind>-<attempt>`).
- **Sync-first** like submit: a failed sync withholds the notification
  and returns a retryable failure.
- **Resolution**: `accept_task_result` marks **all** unresolved
  attention records on the task `resolved: true` (the leader's decision
  closed the loop), or an explicit `resolved: true` call closes a
  record early.

Routing-salience note: v1 delivers all attention in the task room
(room @mentions). A dedicated DM step for humans (higher salience) is
recorded as a follow-up in the PR, not in this change.

### `PROJECT_COMPLETED` on `complete_project` (v2)

`complete_project` previously only wrote state; a finished project
waited for the next incident to surface. It now sends a best-effort
room event before the state write:

- **Contract line**: `@leader PROJECT_COMPLETED: <project-id> -
  Project completed: <title>`; mentions leader + human members.
- Room resolution: first task `room_id` in the plan, falling back to
  the project `source_room_id` when it is a Matrix room.
- **Idempotent**: `projectCompletionEventId` is persisted on the
  project state (txn `project-<project-id>-success`); a retried
  `complete_project` reuses the recorded event.
- Never blocks the terminal project write (same best-effort guards as
  completion events).

## Changes

| File | Change |
|:--|:--|
| `plugins/teamharness/mcp/server.py` | v1: `_team_leader_matrix_id()`, `_send_task_completion_notification()`, `_task_completion_notification()`; `submit_task` branch adds `notification` to the response. v2: `_TASK_COMPLETION_EVENT_TOKENS` + per-status first-line rendering + status-scoped txn + `completionEventStatus` (status-scoped reuse); `_team_human_matrix_ids()` @initiator mentions; `submit_task` status validation + sync-before-notify ordering (retryable failure withholds the notification); new `request_attention` action + `_send_attention_notification()` (idempotent per kind, terminal guard, sync-first); `accept_task_result` auto-resolves outstanding attention; `_send_project_completion_notification()` + idempotent `projectCompletionEventId` on `complete_project` |
| `plugins/teamharness/skills/team/task-execution/SKILL.md` | contract section rewritten: code-generated per-status event lines (worker no longer hand-sends the completion line), status list extended to the full accepted set, `request_attention` documented as the in-flight decision path |
| `plugins/tests/teamharness/mcp/tools/test-taskflow.rb` | runtime config gains the team roster; fake Matrix server gains a `submit-` fault-injection branch; `mc` shim gains a `TEAMHARNESS_TEST_FAIL_SYNC_TASK` hook; new assertions (see below); context file-event selection made mxcUri-based instead of positional (the last event is no longer guaranteed to be a file event) |

## Tests (contract, `test-taskflow.rb`)

1. **Send + content**: exactly one `submit-t-001` message event;
   `m.mentions.user_ids` contains the leader; body carries the contract
   line, `- Worker:` line, and the summary; auth = Worker token.
2. **Persistence**: task state `completionEventId` equals the response
   `notification.eventId`.
3. **Retry**: resubmit with the same payload returns
   `notification.reused: true` with the same `eventId` and sends no
   second event.
4. **BLOCKED**: `BLOCKED: <task-id> - <summary>` line, no
   `TASK_COMPLETED` text.
5. **Failure**: forced HTTP 500 on the `submit-` txn → submit still
   `ok: true` / `submitted`, `notification.sent: false` with the HTTP
   error, no `completionEventId` persisted.

v2 additions (issue #1229):

6. **P0 ordering**: `mc` shim forced to fail for one task → submit
   returns `ok: false` / `retryable: true` with **no `notification`
   field**, local state still `submitted`; the idempotent retry after
   storage recovery sends the event exactly once.
7. **Per-status token + @initiator**: `PARTIAL` / `FAILED` /
   `REVISION_NEEDED` each render their own first-line token +
   `- Status:` line, and the event mentions both the leader and the
   human roster member; `SUCCESS` keeps the `Result:` line and carries
   no `- Status:` line.
8. **Status validation**: submit with an unknown status is rejected
   (`invalid status`) and the bad value is not persisted.
9. **Changed-status resubmit**: `FAILED` → `SUCCESS` resubmit sends a
   second, distinct event (no silent reuse); a same-status resubmit
   reuses the recorded event.
10. **request_attention**: in-flight `approval` ping sends the
    `ATTENTION_APPROVAL` line with leader + human mentions; an
    unresolved same-kind repeat is idempotent (no second event); a
    different kind is not reused; a terminal (cancelled) task is
    rejected; `accept_task_result` resolves the outstanding records.
11. **PROJECT_COMPLETED**: `complete_project` sends the
    `PROJECT_COMPLETED` line with leader + human mentions; a retried
    `complete_project` reuses the recorded event.

## Open questions

1. **Should `check_task`'s polling be removed from leader prompts?** The
   auto-notification makes blind polling redundant; keep it as a
   reconciliation path (cheap) until the notification is proven in
   production.
2. **Manager-runtime taskflow**: the same asymmetry exists in
   `copaw_worker.hooks.tools.taskflow` (its `delegate_task` notifies, its
   `submit_task` does not). Not addressed here — the Manager is not a
   task executor; revisit if a deployment ever makes it one.
