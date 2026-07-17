---
name: teamharness-task-delegation
description: "Use when a Leader turns ready Quick Task or Project Work state into Worker task instructions, sends assignment messages, checks submitted results, and defines completion/blocker report contracts. Do not use to create projects, create rooms, or execute Worker tasks."
---

# Task Delegation

Use this skill when acting as Leader to create Project Work task specs, send
assignment messages, and check submitted Worker results.

Each delegated task should have a task id, owner, scope, expected deliverables,
acceptance criteria, and blocker reporting path. Write task instructions to the
shared task directory before asking the owner to execute.

A submitted result is only a candidate result until accepted by the Leader.
Do not use this skill to turn ordinary conversation into tasks.

## Scope

Use this skill for:

- `taskflow` calls as Leader
- converting a ready project node into a Worker task spec
- checking a submitted Worker result
- routing task assignment and completion messages

Use `teamharness-project-management` to create projects, plan DAG or Loop work,
resolve ready nodes, record Loop iteration decisions, and accept results into
project progress.

## Delegate Only Ready Nodes

Use `delegate_task` only for Project Work nodes that are ready in project
state. Do not create bare tasks directly from a user request. First create or
update a project DAG, then delegate a ready project node returned by
`projectflow` `readyNodes`, or create/update a Loop and delegate a ready
project node returned by `readyLoopNodes`.

For Quick Task, `projectflow` `create_quick_project` is used instead. It
already writes `shared/tasks/{task-id}/meta.json`,
`shared/tasks/{task-id}/spec.md`, and marks the task `assigned`; do not call
`delegate_task` again for that task. After it returns `ok: true`, send the same
assignment message you would send after `delegate_task`.

Before delegation:

1. Resolve the Worker Matrix ID or stable member name from the team roster.
2. Confirm the node came from `readyNodes` or `readyLoopNodes`.
3. Write a bounded task spec through `taskflow`. The spec must include the
   completion report instruction below.
4. Keep Worker deliverables under `shared/tasks/{task-id}/...`. Do not ask a
   Worker to write or submit `shared/projects/...`; project reports are Leader
   owned.
5. Use the current Matrix Task room for the assignment. Do not fall back to
   the requester/source session.
6. Mention the assigned Worker in the Task room only after
   `delegate_task` succeeds.

`teamharness-roomflow` owns task-room creation, reuse, external source binding,
and Worker invites before Project Work reaches this skill.

## Delegate Task

Call `taskflow` with `role: "leader"` and pass `payload` as an object:

```json
{
  "role": "leader",
  "action": "delegate_task",
  "payload": {
    "projectId": "demo-project-001",
    "taskId": "demo-project-001-01",
    "roomId": "room:!task-room:matrix.local",
    "spec": "# Task demo-project-001-01\n\n## Context\nExplain why this task exists.\n\n## Expected Result\nCreate deliverables under shared/tasks/demo-project-001-01/ and submit a result with STATUS, SUMMARY, and DELIVERABLES.\n\n## Acceptance Criteria\n- The result addresses the task scope.\n- Deliverables are listed in result.md.\n\n## Completion Report\nAfter `taskflow submit_task` returns `ok: true`, reply in the current Task room and mention the exact Leader Matrix user from this task context:\n\n<Leader Matrix user> TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md\n\nDo not use `NO_REPLY` after a successful task submission.\n"
  }
}
```

`delegate_task` writes:

```text
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
```

It also changes the project node status to `assigned`.

## Task Spec Completion Report

Every delegated task spec must include this final instruction, with the task id
and result path adjusted for the actual task:

```text
## Completion Report

After `taskflow submit_task` returns `ok: true`, reply in the current assignment
room and mention the exact Leader Matrix user from this task context:

<Leader Matrix user> TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md

Do not use `NO_REPLY` after a successful task submission.
```

If the project node already contains a custom completion line, preserve it and
still make the Leader mention requirement explicit.

## Assignment Message

After `delegate_task` or `create_quick_project` returns `ok: true`, send a
normal current-session reply in the Task room and mention the Worker:

```text
@worker-a:matrix.local TASK_ASSIGNED: demo-project-001-01 - Please start this task. Spec: shared/tasks/demo-project-001-01/spec.md
```

Do not use the `message` tool for this same-room assignment. The direct Task
room reply is the trigger; using `message` plus a direct mention can trigger the
Worker twice.

Do not ask the Worker to edit project files. Do not ask several Workers to own
the same task directory.

## Check Submitted Task

When a Worker reports completion or blocker status, call:

```json
{
  "role": "leader",
  "action": "check_task",
  "payload": {
    "taskId": "demo-project-001-01"
  }
}
```

`check_task` auto-pulls the task directory, validates result metadata, and runs
artifact verification (`verification.verified`, `failedClaims`). A `SUCCESS`
status alone is not enough to accept a task.

**Deliverable sources (TeamHarness vs Manager):** `check_task` verifies paths
stored in task metadata from `submit_task` (`deliverables` / `verifiable_claims`).
It does not parse the `result.md` `DELIVERABLES:` or `## Deliverables` section.
OpenClaw/CoPaw Managers instead use `verify-output.sh`, which reads those headings
from `result.md`. Workers should still list deliverables in `result.md` for human
readability, but TeamHarness acceptance follows metadata, not a re-parse of that
section.

If `effective` is false, do not accept the task. Inspect `validationErrors` and
`failedClaims`, tell the Worker what is missing, and wait for a corrected result.

If `effective` is true, return to `teamharness-project-management` and decide
whether to accept the result into project progress.

## Result Contract

Worker results should contain:

```text
STATUS: SUCCESS
SUMMARY: Short summary

DELIVERABLES:
- shared/tasks/{task-id}/path
```

For report-style tasks, the Worker may write the full report directly to
`shared/tasks/{task-id}/result.md` before calling `submit_task`. The tool
records structured status in task metadata and does not create or rewrite
`result.md`. Do not treat `result.md` as only a short envelope when it is the
expected deliverable.

Accepted statuses are:

- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `REVISION_NEEDED`
- `BLOCKED`

Submitting a result ends that Worker task. If more work is needed, create a new
project node and delegate a new task.

## Post-Action Notification

`delegate_task` and `submit_task` return a `notificationNeeded` field when they
succeed. This field is a structured hint — the tool does not send any message
automatically.

For `delegate_task`: the assignment message in the Task room (see Assignment
Message above) already serves as the notification. No additional cross-room
notification is needed unless the `notificationNeeded.targetRoom` differs from
the current Task room.

For `submit_task`: the Worker completion message in the Task room already serves
as the notification to the Leader. The `notificationNeeded` field confirms the
target room for this report.

The Leader should check `notificationNeeded` after accepting a task result to
determine whether a requester report or downstream notification is due. See
`teamharness-project-management` Post-Action Notification for the full
protocol.
