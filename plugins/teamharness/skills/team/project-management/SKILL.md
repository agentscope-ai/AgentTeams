---
name: teamharness-project-management
description: "Use only after Project Work mode is selected, before TeamHarness projectflow calls, project DAG or Loop planning, ready-node resolution, iteration recording, project files, result acceptance, or project progress updates."
---

# Project Management

Use this skill when maintaining durable project state.

A project owns the plan, context, dependencies, and accepted progress. Keep the
project plan separate from individual task execution logs.

Only advance a dependency after the Leader accepts the submitted result.
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

Use `teamharness-team-coordination` first to decide task boundaries. Use
`teamharness-task-delegation` for individual task specs and Worker result
checks.

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

If `projectId` is omitted, TeamHarness derives one from `title` plus timestamp.
If the generated ID already exists, TeamHarness appends a numeric suffix.
Explicit `projectId` collisions are rejected.

Use `replyRoute` when the requester came from a runtime channel that may need a
cross-session final report. `channel`, `targetUser`, and `targetSession` must
come from the current message/session metadata or a trusted runtime query. Do
not guess them.

For DingTalk requester compatibility, `requester` may use
`dingtalk:{user_id}:{session_id}`; TeamHarness derives `reply_route` from that
value when `replyRoute` is not provided.

## Create Quick Project

Use `create_quick_project` only after Project Work mode is selected and the work
is clearly a single Worker task. It is a shortcut for:

```text
create_project + plan single-node DAG + delegate_task-compatible task spec
```

Determine the assignment `roomId` before calling this action. Use the Team Room
for Matrix DM-originated work. For DingTalk, Feishu, WeChat, or another
external requester channel, first call `roomflow` `create_task_room` with
`source` and the current channel metadata's stable `sourceRoomId`; use the
returned Matrix room id. The same `source` + `sourceRoomId` must map to one
assignment room and must be reused for later tasks from that external room.

Do not invent, transform, suffix, or specialize `sourceRoomId` by task, worker,
retry, or project. If the current DingTalk group already provided
`sourceRoomId: "uhrz6g=="`, pass exactly `"uhrz6g=="` for every later task from
that group. Values such as `"uhrz6g==-qa-task"` or `"uhrz6g==-dev-task"` are
wrong unless they came directly from trusted current channel metadata.

When reusing an external assignment room, call `roomflow` before creating or
delegating task state and pass the complete `invite` list for the Workers who
must receive work in that room. `roomflow` will return the existing Matrix room
and invite missing members.

Example assignment room setup for DingTalk:

```json
{
  "action": "create_task_room",
  "taskId": "demo-project-001",
  "name": "Demo Project",
  "source": "dingtalk",
  "sourceRoomId": "<exact current DingTalk group sourceRoomId>",
  "invite": ["@worker-a:matrix.local", "@worker-b:matrix.local"]
}
```

Example:

```json
{
  "action": "create_quick_project",
  "payload": {
    "title": "Write readiness note",
    "source": "matrix",
    "requester": "@admin:matrix.local",
    "assignedTo": "@worker-a:matrix.local",
    "roomId": "!team-room:matrix.local",
    "spec": "# Task\n\nWrite one concise readiness note.",
    "replyRoute": {
      "channel": "matrix",
      "targetUser": "@admin:matrix.local",
      "targetSession": "!team-room:matrix.local"
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
guess the project from the current room. First resolve the context from the task
id:

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

A Worker `SUCCESS` or `SUCCESS_WITH_NOTES` result is only a candidate result.
After `teamharness-task-delegation` checks the task and returns `effective:
true`, decide whether to accept the result.

To accept a result, call `accept_task_result`:

```json
{
  "action": "accept_task_result",
  "payload": {
    "projectId": "demo-project-001",
    "taskId": "demo-project-001-01",
    "resultStatus": "SUCCESS",
    "summary": "Completed the assigned work."
  }
}
```

`accept_task_result` updates the DAG or Loop node and records
`requester_report.pending` in ProjectMeta. Keep unresolved nodes in their current
state.

Then call `ready_nodes` and delegate any newly ready downstream node with
`teamharness-task-delegation`.

After the project state changes, close the loop with requester visibility:

1. Read `shared/projects/{project-id}/meta.json` and `plan.md`.
2. Build the requester-facing content with the Project Status Reports template
   below.
3. Use `teamharness-communication`
   `## Requester Report Delivery Protocol` to deliver the report to the
   requester recorded on the project.
4. After successful delivery, call `mark_requester_report_sent`.

Team Room coordination, Worker completion messages, downstream assignment
messages, and tool-call summaries do not count as the requester report. You may
batch several accepted task results and downstream delegations into one
requester report, but do not omit the report when accepted project state
changed.

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

**<localized next steps label>**:
1. <next DAG transition, next Loop decision, or requester action>

**<localized notes label>**: <blocker, risk, or decision needed; omit section if none>
````

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

## Current Boundary

TeamHarness v0.1 supports project creation, quick single-task project creation,
project context resolution, DAG planning, DAG ready-node resolution, Loop
planning, Loop ready-node resolution, Loop iteration recording, explicit result
acceptance, requester report clearing, `pause_project`, `resume_project`, and
`complete_project`. DAG and Loop task plans reject duplicate task ids, unknown
dependencies, and dependency cycles. Do not call unsupported `projectflow`
actions.
