---
name: teamharness-task-execution
description: "Use when a TeamHarness Worker or remote member receives an assigned task, acknowledges it, executes it, submits a result, or reports blockers."
---

# Task Execution

Use this skill when executing assigned work as a Worker or remote member.

Claim only tasks assigned to you. Read the task spec, keep deliverables in the
task directory, ask focused blocker questions, and submit a structured result.

Do not change project-level state or project result files. The Leader owns
project state and acceptance.

Do not use this skill or taskflow for readiness checks, direct questions, or
explicit requests to reply with specific text. Answer those directly in the
current room.

## Task Directory

Your assigned task lives under:

```text
shared/tasks/{task-id}/
```

The Leader owns:

```text
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
```

You own:

```text
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/progress/
shared/tasks/{task-id}/result.md
```

Do not edit:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
shared/projects/{project-id}/result.md
```

If the task spec asks you to edit a project-level file, treat that as a blocker
and report it in the assignment room.

## Acknowledge

When you are mentioned with `TASK_ASSIGNED`, first pull or inspect the task
directory if needed, then acknowledge with `taskflow`:

```json
{
  "role": "worker",
  "action": "ack_task",
  "payload": {
    "taskId": "demo-project-001-01"
  }
}
```

If `ack_task` fails because the task is missing or assigned elsewhere, stop and
report the blocker to the Leader in the current room. Do not invent task files.

Read:

```text
shared/tasks/{task-id}/spec.md
```

before doing the work.

## Execute

Keep all deliverables under the task directory. If you need private notes, use:

```text
shared/tasks/{task-id}/workspace/
```

If blocked, submit a `BLOCKED` result instead of silently waiting.

## Submit

Submit with `taskflow`:

```json
{
  "role": "worker",
  "action": "submit_task",
  "payload": {
    "taskId": "demo-project-001-01",
    "status": "SUCCESS",
    "summary": "Completed the assigned work.",
    "deliverables": [
      "shared/tasks/demo-project-001-01/workspace/output.md"
    ]
  }
}
```

Use one of:

- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `REVISION_NEEDED`
- `BLOCKED`

Submitting ends the task. Do not keep editing the old task after submission
unless the Leader assigns a new task.

## Completion Message

After `submit_task` returns `ok: true`, send a normal text message in the
current assignment room and mention the Leader:

```text
@leader:matrix.local TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md
```

Use the Leader's exact Matrix user id or the resolvable mention provided in the
task assignment. Do not invent `@leader:matrix.local` if a real id is available.

If the task spec gives an exact completion line, preserve that line exactly and
include one short summary sentence. A tool call, tool-output thread, or
`result.md` file does not count as the completion message. Do not use
`NO_REPLY` after successful submission.

For blockers:

```text
@leader:matrix.local BLOCKED: demo-project-001-01 - <short blocker summary>
```
