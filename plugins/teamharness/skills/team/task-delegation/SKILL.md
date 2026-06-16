---
name: teamharness-task-delegation
description: "Use only after Project Work mode is selected, when a TeamHarness Leader delegates ready project nodes, writes task specs, checks Worker results, and routes completion or blocker messages."
---

# Task Delegation

Use this skill when acting as Leader to create and delegate task specs.

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

Within Project Work, do not create bare tasks directly from a user request.
First create or update a project DAG, then delegate a ready project node
returned by `projectflow` `readyNodes`, or create/update a Loop and delegate a
ready project node returned by `readyLoopNodes`.

For a single-task Project Work item, `projectflow` `create_quick_project` may be
used instead. It already writes `shared/tasks/{task-id}/meta.json`,
`shared/tasks/{task-id}/spec.md`, and marks the task `assigned`; do not call
`delegate_task` again for that task. After it returns `ok: true`, send the same
assignment message you would send after `delegate_task`.

Before delegation:

1. Read `TEAMS.md` for the Worker Matrix ID and Team Room ID.
2. Confirm the node came from `readyNodes` or `readyLoopNodes`.
3. Write a bounded task spec through `taskflow`. The spec must include the
   completion report instruction below.
4. Mention the assigned Worker in the assignment room only after
   `delegate_task` succeeds. Use the Team Room for Matrix DM-originated work,
   or the assignment room returned by `roomflow` for external requester channels.
   For external channels, `roomflow` must receive the stable `sourceRoomId` from
   the current channel metadata so repeated tasks from that source room reuse
   one Matrix assignment room.
5. Do not create task-specific or worker-specific `sourceRoomId` values. Reuse
   the exact external `sourceRoomId` from the current requester room. Before
   sending the assignment message, call `roomflow` with that same `sourceRoomId`
   and include the assigned Worker in `invite`; include any other Workers who
   need to observe or work in the same assignment room.

## Delegate Task

Call `taskflow` with `role: "leader"` and pass `payload` as an object:

```json
{
  "role": "leader",
  "action": "delegate_task",
  "payload": {
    "projectId": "demo-project-001",
    "taskId": "demo-project-001-01",
    "roomId": "room:!team-room:matrix.local",
    "spec": "# Task demo-project-001-01\n\n## Context\nExplain why this task exists.\n\n## Expected Result\nCreate deliverables under shared/tasks/demo-project-001-01/ and submit a result with STATUS, SUMMARY, and DELIVERABLES.\n\n## Acceptance Criteria\n- The result addresses the task scope.\n- Deliverables are listed in result.md.\n\n## Completion Report\nAfter `taskflow submit_task` returns `ok: true`, reply in the current assignment room and mention the Leader Matrix user from `TEAMS.md`:\n\n<Leader Matrix user from TEAMS.md> TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md\n\nDo not use `NO_REPLY` after a successful task submission.\n"
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
room and mention the Leader Matrix user from `TEAMS.md`:

<Leader Matrix user from TEAMS.md> TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md

Do not use `NO_REPLY` after a successful task submission.
```

If the project node already contains a custom completion line, preserve it and
still make the Leader mention requirement explicit.

## Assignment Message

After `delegate_task` returns `ok: true`, use `teamharness-communication` to
mention the Worker in the assignment room:

```text
@worker-a:matrix.local TASK_ASSIGNED: demo-project-001-01 - Please start this task. Spec: shared/tasks/demo-project-001-01/spec.md
```

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

Accepted statuses are:

- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `REVISION_NEEDED`
- `BLOCKED`

Submitting a result ends that Worker task. If more work is needed, create a new
project node and delegate a new task.
