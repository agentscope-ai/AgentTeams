---
name: teamharness-task-execution
description: "Use when a Worker receives TASK_ASSIGNED, acknowledges the task, works inside shared/tasks/{task-id}/, submits with taskflow submit_task, publishes deliverables through submit_task, and reports TASK_COMPLETED or blockers in the Task room."
---

# Task Execution

Use this skill when executing assigned work as a Worker or remote member.

Claim only tasks assigned to you. Read the task spec, keep deliverables in the
task directory, ask focused blocker questions, and submit a structured result.

Do not change project-level state or project result files.

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

If a task spec asks you to write or submit `shared/projects/...`, report that
boundary conflict to the Leader. Put Worker-owned deliverables under
`shared/tasks/{task-id}/...`; the Leader owns project-level reports.

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

Use meaningful filenames for deliverables. Prefer names that describe the
artifact's purpose, such as `analysis.md`, `trace-summary.log`, or
`patch-notes.md`; avoid vague names like `output.md` when there are multiple
files.

For report-style tasks whose expected output is `shared/tasks/{task-id}/result.md`,
write the full report content to that file before calling `submit_task`.
`submit_task` records structured status in task metadata and does not create or
rewrite `result.md`.

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
- `PARTIAL`
- `REVISION_NEEDED`
- `BLOCKED`
- `FAILED`

Submitting ends the task. Do not keep editing the old task after submission
unless the Leader assigns a new task.

If you already wrote a detailed `shared/tasks/{task-id}/result.md`, include that
path in `deliverables`; do not replace the report with only a short summary.
Do not include `shared/projects/...` paths in `deliverables`.

When Matrix room context is available, `submit_task` automatically publishes
`shared/tasks/{task-id}/result.md` and eligible deliverable files under the
current task directory as Matrix `m.file` events. Check `publishedArtifacts` in
the tool result for published, skipped, or failed artifact status. Do not upload
task artifacts manually with `message` just to make them appear in the room
file panel.

## Completion Message

`submit_task` automatically publishes the completion event to the Task room:
the first line is the contract below, and the event @mentions the Leader
and the human members of the team (the task initiator), so the requester is
routed with the same salience the leader is. After `submit_task` returns
`ok: true`, do not send another completion line.

The event first line carries one token per result status (code-generated):

```text
@leader-user:matrix.local TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md
@leader-user:matrix.local TASK_PARTIAL: demo-project-001-01 - <summary>
@leader-user:matrix.local TASK_REVISION_NEEDED: demo-project-001-01 - <summary>
@leader-user:matrix.local TASK_BLOCKED: demo-project-001-01 - <short blocker summary>
@leader-user:matrix.local TASK_FAILED: demo-project-001-01 - <summary>
```

If the task spec gives an exact completion line, the code event keeps that
line format; a short human-readable summary message may still follow in the
room but never replaces the event. Do not use `NO_REPLY` after successful
submission.

While the task is still in flight and you need a human decision (approval /
decision / escalation) instead of guessing, call `taskflow` with
`action: request_attention` (payload: `kind`, `question`). Do not rely on
the human noticing an ambient room message.
