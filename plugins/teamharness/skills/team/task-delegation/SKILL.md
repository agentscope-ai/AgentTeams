---
name: teamharness-task-delegation
description: "Use when you act as Leader to turn ready Quick Task or Project Work state into Worker task instructions, send assignment messages, check submitted results, and define completion/blocker report contracts. Do not use to create projects, create rooms, or execute Worker tasks."
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
`delegate_task` again for that task. `create_quick_project` does not
auto-notify, so after it returns `ok: true`, send the assignment message
described in the Assignment Message section below.

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
6. `delegate_task` mentions the assigned Worker automatically after it
   succeeds — do not send a second mention.

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

It changes the project node status to indicate the node is delegated/assigned,
publishes the task directory to shared storage, and automatically sends the
Worker assignment notification with `m.mentions` in the Task room, returning
the Matrix `eventId` in the response.

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

`delegate_task` **automatically notifies the assigned Worker** in the Task
room: it validates room membership, sends a message with `m.mentions`
mentioning the Worker's full Matrix ID, and returns the Matrix `eventId`.
Do **not** send a second assignment message after `delegate_task` — that
duplicates the assignment and can trigger the Worker twice.

`create_quick_project` does **not** auto-notify. After it returns `ok: true`,
send a normal current-session reply in the Task room and mention the Worker:

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

If `effective` is false, do not accept the task. Tell the Worker what is
missing and wait for a corrected result.

If `effective` is true, retain the returned current `task.submission_id`,
return to `teamharness-project-management`, and pass that identity as
`submissionId` with an explicit boolean `accepted` decision. You may decide
only as the trusted Leader runtime; do not trust a payload role, and never
delegate acceptance or cancellation to the Worker. For every normal task that
already has a submission identity, omitting `submissionId` is an error for both
accept and cancel; only the documented no-identity legacy migration may omit it.

## Result Contract

Expect Worker results to contain:

```text
STATUS: SUCCESS
SUMMARY: Short summary

DELIVERABLES:
- shared/tasks/{task-id}/path
```

For report-style tasks, let the Worker write the full report directly to
`shared/tasks/{task-id}/result.md` before calling `submit_task`. The tool
records structured status in task metadata and does not create or rewrite
`result.md`. Do not treat `result.md` as only a short envelope when it is the
expected deliverable.

Accepted statuses are:

- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `REVISION_NEEDED`
- `BLOCKED`
- `INTERRUPTED`

Treat `INTERRUPTED` like `BLOCKED` at the terminal decision boundary: accepting
either status records the task and plan node as `blocked` and resolves the
continuation with `resolution: blocked`.

Submitting a result ends that Worker task. If more work is needed, create a new
project node and delegate a new task.

## Post-Action Notification

`delegate_task` **sends the Worker assignment automatically**: it validates
room membership, sends the assignment message with `m.mentions` in the Task
room, and returns the Matrix `eventId` in the response. No additional
assignment message is needed after a successful `delegate_task`.

`submit_task` and the `notificationNeeded` field are structured hints for the
completion report. When a Worker reports `TASK_COMPLETED` with a result path,
check the result and follow `teamharness-project-management` for acceptance or
rejection.

You should check `notificationNeeded` after accepting a task result to
determine whether a requester report or downstream notification is due. See
`teamharness-project-management` Post-Action Notification for the full
protocol.
