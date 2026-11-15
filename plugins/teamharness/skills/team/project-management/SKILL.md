---
name: teamharness-project-management
description: "Use as Leader to start a complete task plan with start_project, accept checked submissions, and complete the plan with its final report. Also covers manual DAG and Loop lifecycle operations."
---

# Project Management

## Main tasks and subtasks

Keep Projects as optional organizational containers for larger work. You can
plan multiple main tasks within one Project. A main task with subtasks represents
the complete request you coordinate; a leaf task is an independently executable
Worker assignment. Set `parentTaskId` on each subtask to its main task's ID.
Keep `dependsOn` separate: it lists execution prerequisites, not parentage.

Use `plan_dag` to persist the full plan, including the main tasks and subtasks.
Delegate only ready leaf tasks. Do not delegate a coordinating parent to a
Worker. Keep its status open while any subtask is unfinished. After all subtasks
are accepted and you verify the overall deliverable, call `complete_project`
with `projectId` and `report` (your final user report). That operation closes
coordinating parents, saves result.md and notifies the requester. Completing one
subtask never completes the main task. Do not create a Project for each subtask.

Use this skill when maintaining durable project state.

A project owns the plan, context, dependencies, and accepted progress. Keep the
project plan separate from individual task execution logs.

Only advance a dependency after you accept the submitted result as the trusted
Leader runtime.
Do not use this skill for ordinary direct replies or lightweight one-off
actions.

## Scope

Use this skill when acting as Leader for:

- `projectflow` calls
- project creation
- quick single-task project creation
- DAG planning
- Loop planning
- ready-node resolution
- Loop iteration recording
- accepting checked Worker results into project progress
- project-level status reports

When owners and acceptance criteria are already known, use `start_project`
from this skill directly: it creates the room, validates the plan and performs
initial delegation, including its required decision classification. You do not
need to load roomflow or individual-delegation instructions first. Use
`teamharness-team-coordination` when task boundaries need analysis, and use
individual room/delegation skills when you choose the manual workflow.

## Project Files

Project files live under:

```text
shared/projects/{project-id}/
```

TeamHarness writes CoPaw-compatible task runtime files:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
```

Write a final project result, when needed, to:

```text
shared/projects/{project-id}/result.md
```

You own this project result file as Leader. Build it from accepted project state
and accepted task deliverables; do not ask Workers to write or submit it as a
task deliverable.

Use `projectflow` to change `meta.json` and `plan.md`; do not hand-edit
project state unless a tool failure leaves no safe alternative.

## ID Rules

Use safe IDs only:

```text
[A-Za-z0-9][A-Za-z0-9._-]*
```

Do not use colons, slashes, spaces, or Matrix IDs as project or task IDs.

Suggested formats:

```text
projectId: {short-description}-YYYYMMDD-HHMMSS
taskId: {projectId}-{seq}
```

If a user or test provides explicit IDs, preserve them when they are already
safe.

## Tool Payload Rule

When calling `projectflow`, pass `payload` as a JSON object, not as a serialized
JSON string.

## Create Project

When API coordination is enabled and every execution task can be specified up
front, set `dispatchMode: "automatic"` on `start_project` and include `spec` and
`assignedTo` on every leaf. After you check and accept a result, the task system
dispatches newly ready leaves using the stored plan. Do not send duplicate
assignment messages or call `delegate_task` again. Submitted results do not
unlock dependencies. Use normal manual planning when later specs require a new
Human decision; do not invent those decisions to enable automatic dispatch.

Keep each leaf spec concise: state the requested behavior, exact acceptance
cases, required input paths and output paths. Reference accepted upstream
artifacts instead of copying their contents into every downstream spec. Do not
repeat task protocol, status report templates or CLI instructions in specs;
the runtime already supplies those. Scale the plan to the request rather than
expanding a small function into a long implementation document.

Add a dependency only when the task actually consumes that predecessor's
output or the requester requires the order. Role order alone is not a data
dependency. For example, implementation documentation and independent QA can
both use the accepted implementation in parallel; only make documentation wait
for QA when its requested content needs the QA result. Final acceptance still
requires all required branches to finish.

For a finite DAG whose initial owners and acceptance criteria are known, prefer
`start_project` in the source session. It reuses the existing room creation,
plan validation and delegation operations in one call; you do not send a
`PROJECT_REQUESTED` self-trigger for this path. Supply the normal
`create_project` fields, a safe explicit `projectId`, and `tasks`. Each initially
ready leaf needs `assignedTo` and a bounded `spec`; later tasks can wait for
their accepted inputs. Runtime-roster names or full Matrix IDs identify owners.

```json
{"action":"start_project","payload":{"projectId":"delivery-001","title":"Bounded delivery","source":"matrix","sourceRoomId":"!source:matrix.local","requester":"@requester:matrix.local","replyRoute":{"channel":"matrix","target_session":"!coordinator:matrix.local","target_user":"@coordinator:matrix.local"},"tasks":[{"taskId":"delivery-001-analysis","assignedTo":"@analyst:matrix.local","spec":"Describe the requested behavior and acceptance cases. Submit requirements.md under your task directory."},{"taskId":"delivery-001-implementation","assignedTo":"@developer:matrix.local","dependsOn":["delivery-001-analysis"]}]}}
```

After successful initial assignments, end the source turn with `NO_REPLY`.
Worker completion events wake you in the returned task room. An exact retry
reuses the plan and preserves accepted/assigned work. A partial failure returns
the room, project and assignment receipts; resolve the reported failure rather
than create a second plan. The existing explicit room/self-trigger flow remains
available when the request needs further planning in its task room.

Create the project before planning tasks:

```json
{
  "action": "create_project",
  "payload": {
    "projectId": "demo-project-001",
    "title": "Demo Project",
    "source": "dingtalk",
    "requester": "dingtalk:sender_001:aaaaaaaa",
    "replyRoute": {
      "channel": "dingtalk",
      "targetUser": "sender_001",
      "targetSession": "aaaaaaaa"
    }
  }
}
```

This creates `meta.json` and `plan.md`.
When the initial DAG is already known, include its `tasks` array in this
same `create_project` payload (using the `plan_dag` task fields). The tool
validates and saves the plan together, returning `readyNodes`; delegate those
nodes directly instead of calling `plan_dag` and `ready_nodes` again.

Keep intermediate results in the task room. `complete_project` sends the
verified final report to the requester automatically after committing terminal
state. When `requesterNotification.sent` is true, do not send it again or call
`mark_requester_report_sent`; delivery is already recorded.

`team_id` is stamped automatically from the runtime team configuration; pass
`payload.teamId` explicitly only when the project must belong to a different
team than the calling Worker's own team.

If `projectId` is omitted, TeamHarness derives one from `title` plus timestamp.
If the generated ID already exists, TeamHarness appends a numeric suffix.
Explicit `projectId` collisions are rejected.

Use `replyRoute` when the requester came from a runtime channel that may need a
cross-session final report. `channel`, `targetUser`, and `targetSession` must
come from the current message/session metadata or a trusted runtime query. Do
not guess them.

For Project Work that should proceed in a dedicated task room, do not call
`create_project` in the requester/source session. Use
`teamharness-communication` to send a `PROJECT_REQUESTED` self-trigger from the
current requester/source session to the same Leader in the target Matrix task
room first. After the task-room Leader session wakes up, call `create_project`
there and preserve the original requester route from the visible
`PROJECT_REQUESTED` request: keep the original `source`, `requester`,
`sourceRoomId`, and `replyRoute` so final reports can return to the
DingTalk group, Matrix DM, or other source room that made the request. Do not
invent missing `replyRoute` values from requester names or prose.

For DingTalk requester compatibility, `requester` may use
`dingtalk:{user_id}:{session_id}`; TeamHarness derives `reply_route` from that
value when `replyRoute` is not provided.

## Create Quick Project

Use `create_quick_project` only after Quick Task mode is selected and the
Leader is in the Matrix Task room that received the task request message. It is
a shortcut for:

```text
create_project + plan single-node DAG + assigned task spec
```

Do not call this in the requester/source session. If the current session is
still a DingTalk group, Matrix DM, Team room, or other source room, return to
`TEAMS.md`: create or reuse the task room with `teamharness-roomflow`, hand the
request to that room with `teamharness-communication`, and stop in the source
session.

Before calling this action in the Task room:

1. Use the current Matrix task room as `roomId`.
2. Preserve the original `source`, `requester`, `sourceRoomId`, and `replyRoute`
   from the task request message. Do not guess route values.
3. Choose the one Worker owner from the team roster.
4. Write a bounded task spec that includes the completion report instruction
   from `teamharness-task-delegation`.

`teamharness-roomflow` owns task-room creation, project-scoped reuse, source
metadata capture, and Worker invites. This skill only creates durable project
and task state after the Task room is already selected.

Example:

```json
{
  "action": "create_quick_project",
  "payload": {
    "title": "Write readiness note",
    "source": "matrix",
    "requester": "@admin:matrix.local",
    "sourceRoomId": "!admin-dm:matrix.local",
    "assignedTo": "@worker-a:matrix.local",
    "roomId": "!task-room:matrix.local",
    "spec": "# Task\n\nWrite one concise readiness note.",
    "replyRoute": {
      "channel": "matrix",
      "targetUser": "@admin:matrix.local",
      "targetSession": "!admin-dm:matrix.local"
    }
  }
}
```

This writes:

```text
shared/projects/{projectId}/meta.json
shared/projects/{projectId}/plan.md
shared/tasks/{taskId}/meta.json
shared/tasks/{taskId}/spec.md
```

It sets `mode: quick`, `plan_type: dag`, and the single task status to
`assigned`. It does not send the Matrix or external-channel assignment message;
send that message only after the tool returns `ok: true`.

## Plan DAG

Use DAG when the work is finite and dependencies are known enough to plan now.

Call `plan_dag` with the complete graph you want to keep:

```json
{
  "action": "plan_dag",
  "payload": {
    "projectId": "demo-project-001",
    "tasks": [
      {
        "taskId": "demo-project-001-01",
        "title": "Implement the worker-owned part",
        "assignedTo": "@worker-a:matrix.local",
        "dependsOn": []
      }
    ]
  }
}
```

For `assignedTo`, use the Worker Matrix ID or the stable runtime/member name
from `TEAMS.md`. Do not invent a Worker name.

`plan_dag` returns `readyNodes`. Delegate only ready nodes.

## Ready Nodes

Use `ready_nodes` when you need the next pending or assigned DAG nodes whose
dependencies are completed:

```json
{
  "action": "ready_nodes",
  "payload": {
    "projectId": "demo-project-001"
  }
}
```

Only nodes with dependencies marked `completed` unblock downstream work.

## Plan Loop

Use Loop when `teamharness-team-coordination` decided the work should repeat
until a stop condition, quality gate, evidence threshold, or maximum iteration
count.

Call `plan_loop` with the complete current-iteration plan you want to keep:

```json
{
  "action": "plan_loop",
  "payload": {
    "projectId": "demo-project-001",
    "goal": "Improve the result until tests pass",
    "stopCondition": "All target tests pass or maxIterations is reached",
    "iterationTemplate": "Inspect result, choose one bounded fix, run tests.",
    "maxIterations": 3,
    "currentIteration": 1,
    "tasks": [
      {
        "taskId": "demo-project-001-i001-01",
        "title": "Run iteration 1",
        "assignedTo": "@worker-a:matrix.local",
        "dependsOn": []
      }
    ]
  }
}
```

Required Loop inputs:

- `goal`
- `stopCondition`
- `iterationTemplate`
- `maxIterations`

Use `ready_loop_nodes` to find pending nodes in the current iteration whose
dependencies are completed:

```json
{
  "action": "ready_loop_nodes",
  "payload": {
    "projectId": "demo-project-001"
  }
}
```

After evaluating an iteration, use `record_loop_iteration`:

```json
{
  "action": "record_loop_iteration",
  "payload": {
    "projectId": "demo-project-001",
    "iteration": 1,
    "decision": "continue",
    "summary": "One target still fails.",
    "nextAction": "Plan the next fix pass."
  }
}
```

Allowed decisions are `continue`, `replan`, `ask_user`, `stop_success`, and
`stop_blocked`.

Do not pre-expand repeated Loop rounds into a large DAG. Plan the current
iteration, evaluate it, record the iteration decision, then decide whether to
continue, replan, ask the requester, stop successfully, or stop blocked.

## Resolve Project Context

When a Worker completion or blocker wakes the Leader in a fresh session, do not
guess the project from the current room. Start with `taskflow check_task` using
the task ID; its snapshot includes the task's project ID and submitted evidence.
Use `resolve_project` only if additional project context is actually missing:

```json
{
  "action": "resolve_project",
  "payload": {
    "taskId": "demo-project-001-01"
  }
}
```

`resolve_project` returns the ProjectMeta, TaskMeta, `replyRoute`, plan type, and
ready nodes that the Leader needs to resume normal project flow.

## Accepting Worker Results

Treat a Worker `SUCCESS` or `SUCCESS_WITH_NOTES` result as only a candidate.
After `teamharness-task-delegation` checks the task and returns `effective:
true`, copy its current `task.submission_id` and decide whether to accept the
result.

You may make this decision only when the runtime configuration identifies you
as the trusted Leader. Never rely on a payload `role` to override a Worker
runtime, and never let a Worker accept, reject, cancel, or resolve its own
continuation.

To accept a result, call `accept_task_result`:

```json
{
  "action": "accept_task_result",
  "payload": {
    "projectId": "demo-project-001",
    "taskId": "demo-project-001-01",
    "submissionId": "<current task.submission_id from check_task>",
    "accepted": true,
    "resultStatus": "SUCCESS",
    "summary": "Completed the assigned work."
  }
}
```

`accept_task_result` commits the terminal decision and returns the updated
project plus `readyNodes`/`readyLoopNodes`. Keep intermediate results in the
Task room; they do not trigger a Manager report. Reuse this returned snapshot
instead of reloading project files or querying readiness again.

For a normal task that already has a `submission_id`, you must pass that exact
value as `submissionId` to both acceptance and cancellation. Retry only with the
same `submissionId` and `accepted` decision; a missing or stale identity, or a
different terminal decision, is a conflict. The only acceptance exception is
the documented standalone migration of a persisted legacy submission with no
identity. Native CoPaw decisions do not perform that migration; first complete
the evidence-based Worker retry adoption, then use its generated identity.

To cancel a task, include a reason and, whenever the task has a submission
identity, keep the same `submissionId` fence. In the
CoPaw runtime call `projectflow` with `action: "cancel_task"`; with the
runtime-neutral standalone MCP call `taskflow` with `action: "cancel_task"`.
Both routes are trusted-Leader-only and resolve the existing continuation as
`cancelled` without rotating its `delivery_id`. The standalone MCP retains its
documented compatibility for cancelling legacy TaskMeta that has no identity;
do not invent or supply an unknown identity for that path.

Accepting a completed result does not publish the project artifact by default.
Write or update `shared/projects/{project-id}/result.md` as the Leader when a
project-level report file is needed. After the requester report message is sent
to a Matrix requester room, publish `shared/projects/{project-id}/result.md`
with `artifact publish_file` and set `parentEventId` to the report message id.
Use `publishArtifacts: true` only when you intentionally want an immediate
project artifact before the requester report.

Delegate downstream nodes from the acceptance response. When all required
work is accepted, write the final project summary and call `complete_project`.
For Matrix routes the tool commits state, publishes the full report and sends
a structured final event. Check `requesterNotification.sent`: when true,
report delivery is already recorded; do not resend or clear the marker again.
If delivery failed, resolve its explicit error and retry `complete_project`
idempotently. For legacy or non-Matrix routes without automatic delivery,
use the Requester Report Delivery Protocol once at final completion.

The standalone MCP records pending requester delivery in `requester_report`.
Native CoPaw `projectflow` does not invent a
`requester_report` marker when its runtime has not supplied one. An absent
marker is not permission to omit the report: deliver the verified final result
through the requester route once. In Agent API mode, check the durable delivery
receipt rather than treating queue admission as the Human's receipt.

Task room coordination, Worker completion messages, downstream assignment
messages, and tool-call summaries do not count as the requester report. You may
summarize accepted task results in the final requester report. Keep ordinary
intermediate changes in the Task room; do not wake a Manager to supervise each
step. Surface a blocker only when the requester must act, or progress when the
requester explicitly asks for it. The final report remains required.

## Project Status Reports

Build status report content here because this skill owns project execution
state. Use `teamharness-communication` only for destination selection, message
tool payloads, Matrix message formatting, and same-room versus cross-room
delivery.

Before writing a requester report:

1. Read `shared/projects/{project-id}/meta.json` and `plan.md`.
2. Identify the plan type from `meta.json` or `plan.md`.
3. For DAG, report dependency progress and the next ready nodes.
4. For Loop, report current iteration progress, the latest decision, and the
   next iteration path.
5. Write the report in the requester's language, then use
   `teamharness-communication` to deliver it to the requester recorded on the
   project.

Use stable state markers and localize the visible labels:

| State | Marker |
|---|---|
| Completed | `[done]` |
| In Progress | `[active]` |
| Pending | `[pending]` |
| Blocked | `[blocked]` |
| Revision | `[revision]` |

Keep the report envelope consistent across DAG and Loop:

````markdown
---

## <localized project status report heading>

**<localized project name label>**: <name>
**<localized project ID label>**: <project-id>
**<localized execution mode label>**: <DAG or Loop>
**<localized project status label>**: <marker + localized state label>
**<localized current focus label>**: <DAG wave/current node, or Loop iteration n / max and topic>

**<localized summary label>**: <1-3 sentences about what changed and what happens next>

**<localized task status label>**:
| <localized task ID label> | <localized task title label> | <localized owner label> | <localized status label> | <localized context label> |
|---|---|---|---|---|
| <task-id> | <title> | <worker> | <marker + localized state label> | <dependencies, iteration role, result note, or -> |

**<localized execution progress label>**:
```text
<DAG dependency flow or Loop iteration line; choose the matching shape below>
```

**<localized deliverables label>**:
- `<path>` - <what it contains>

For Matrix requester reports, this section may say the report file or artifact
was published to the requester room only after `artifact publish_file` succeeds.
For a non-Matrix channel, do not claim that a file was attached. Say the
files are available from the shared workspace or platform object-storage view;
do not say "attached", "uploaded to this chat", "click the file card", or
"view the file here".

**<localized next steps label>**:
1. <next DAG transition, next Loop decision, or requester action>

**<localized notes label>**: <blocker, risk, or decision needed; omit section if none>
````

In requester-facing reports, owner or executor cells are descriptive text.
Use plain Worker display names or role names such as `workeraa`, `workerbb`, or
`WorkerAA`. Do not prefix them with `@`, do not use full Matrix user ids, and do
not create Matrix mention links unless the report is intentionally asking that
member to respond or take action.

Use this execution progress shape for DAG reports:

```text
<task-id> [done] -> <task-id> [active] -> <task-id> [pending]
                                  ^
<task-id> [done] -----------------|
```

Use the DAG progress section to show the dependency path that changed, newly
unblocked nodes, or the next ready wave. If the graph is large, show only the
relevant changed path and summarize the rest in the task table.

Use this execution progress shape for Loop reports:

```text
Iteration 1 [done] -> Iteration 2 [active] -> Iteration 3 [pending] ... Max <n>
                       Current: <current iteration topic>
                       Decision: <continue, replan, ask_user, stop_success, stop_blocked, or pending evaluation>
```

Use the Loop progress section to show current iteration number, maximum
iterations, current iteration topic, and the latest Leader decision. Do not
present future Loop rounds as if they were pre-planned DAG tasks. Show only the
current iteration's task statuses in the task table unless prior iteration
context is necessary.

For intermediate updates, include the Project header, summary, task status
table, and next steps. Omit the deliverables section unless new deliverables
are available, and omit the execution progress section only when neither DAG
flow nor Loop iteration state changed.

After the requester report is sent, clear the pending flag:

```json
{
  "action": "mark_requester_report_sent",
  "payload": {
    "projectId": "demo-project-001"
  }
}
```

## Post-Action Notification

State-mutating `projectflow` and `taskflow` operations return a
`notificationNeeded` field when they succeed. This field is a hint — the tool
does not send any message automatically. You must act on it as Leader.

When `notificationNeeded` is present in the tool result:

1. Check `notificationNeeded.targetRoom`. If it matches the current session
   room, a direct reply in the current session is sufficient — no cross-room
   message is needed.
2. If `targetRoom` is a different room, use the `message` tool to send a brief
   status update to that room. Use `m.notice`-style concise format:
   `[ProjectFlow] {event}: {summary}`.
3. If `notificationNeeded.replyRoute` is present and the event is
   `complete_project` and `requesterNotification.sent` is not true, this is the final requester report
   trigger — follow the Requester Report Delivery Protocol in
   `teamharness-communication`.
4. Do not send a notification if the tool returned `ok: false`.
5. Do not send duplicate notifications for the same event within a single
   Leader turn — one notification per state change is sufficient.

The `notificationNeeded` field contains:

```json
{
  "event": "create_project",
  "projectId": "demo-project-001",
  "summary": "create_project: Demo Project",
  "targetRoom": "!task-room:matrix.local",
  "replyRoute": { "channel": "matrix", "targetSession": "..." }
}
```

`targetRoom` and `replyRoute` may be absent when no room context is available
(for example, a bare `create_project` before any task room exists). In that
case, skip the notification — the assignment message or requester report will
carry the project context later.

## Pause / Resume / Complete

Use these when a human asks the Leader to pause, resume, or complete a
project (or when the Leader decides a project should stop dispatching new
work). A Controller API also implements these as structured endpoints; this
skill documents the MCP-native path a Leader uses in conversation.

Call `pause_project` to stop new task dispatch. In-flight tasks are NOT
cancelled — they keep running and their completion reports still arrive. Do
not delegate new ready nodes while paused.

```json
{
  "action": "pause_project",
  "payload": {
    "projectId": "demo-project-001"
  }
}
```

Call `resume_project` to put a paused project back to `active`:

```json
{
  "action": "resume_project",
  "payload": {
    "projectId": "demo-project-001"
  }
}
```

Call `complete_project` only when every task has reached a terminal status
(completed / revision / blocked / cancelled) and no work remains — a
completed project is terminal. Pass `publishArtifacts: true` only when you
intentionally want an immediate project artifact before the requester report
(see Accepting Worker Results):

```json
{
  "action": "complete_project",
  "payload": {
    "projectId": "demo-project-001",
    "publishArtifacts": false
  }
}
```

`pause_project` / `resume_project` / `complete_project` change project status
only; they do not cancel in-flight tasks or delete task state. If a task must
be cancelled, use `taskflow cancel_task` (Leader only) with a reason.

## Current Boundary

TeamHarness v0.1 supports project creation, quick single-task project creation,
project context resolution, DAG planning, DAG ready-node resolution, Loop
planning, Loop ready-node resolution, Loop iteration recording, explicit result
acceptance, requester report clearing, `pause_project`, `resume_project`, and
`complete_project`. DAG and Loop task plans reject duplicate task ids, unknown
dependencies, and dependency cycles. Do not call unsupported `projectflow`
actions.
