---
name: task-management
description: Use before any Worker taskflow call or assigned-task workflow, including reading task state, acknowledging a task, executing a task, tracking progress, handling blockers/questions, submitting structured results, or reporting completion. Always use this skill when the message mentions assigned task, task ID, shared/tasks, spec.md, meta.json, result.md, deliverables, BLOCKED, REVISION_NEEDED, SUCCESS, submit_task, or ack_task.
---

# Task Management

You are a Worker. Execute only your assigned task.

## Task Directory

All work for a task stays under:

```text
shared/tasks/{task-id}/
```

Your coordinator creates:

```text
shared/tasks/{task-id}/spec.md
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/base/
```

You own:

```text
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/progress/
shared/tasks/{task-id}/<deliverables>
```

`taskflow` owns `shared/tasks/{task-id}/result.md` and `meta.json`. Do not hand-edit either file. You submit task results through `taskflow` with `action=submit_task`; it writes the standard `result.md` protocol for you.

`ack_task` and `submit_task` only succeed when your Matrix identity matches `meta.json.assigned_to`. If either action reports that the task is assigned to someone else, stop and report the assignment mismatch to your coordinator.

If you need private planning notes, write them under `shared/tasks/{task-id}/workspace/`. Do not create shared task-level `plan.md`.

Do not edit project-level `shared/projects/{project-id}/plan.md` or `meta.json` unless the task spec explicitly tells you to.

## Execution Flow

1. In the current room, directly say that you received the message before task acceptance work starts.
2. Accept the task with `taskflow`. This single call pulls the task directory from storage, reads `spec.md` and `meta.json`, acknowledges the task, and pushes the acknowledged status back to storage. The response contains the spec content:

   ```json
   {
     "action": "ack_task",
     "payload": {
       "taskId": "{task-id}"
     }
   }
   ```

   The response `spec` field contains the full task spec. Read it from the response instead of calling `read_file` separately.

   `meta.json.room_id` is the task's assignment and delivery room. Use it only when cross-room delivery is truly needed. If it is missing, stop and report a blocker in the current session instead of guessing another room.

3. Execute the task. Keep deliverables inside `shared/tasks/{task-id}/`.
4. Submit the task result with `taskflow`. This writes `shared/tasks/{task-id}/result.md`, marks local task state submitted, pushes the task directory to storage, and verifies `result.md` exists on storage:

   ```json
   {
     "action": "submit_task",
     "payload": {
       "taskId": "{task-id}",
       "status": "SUCCESS",
       "summary": "<one paragraph summary>",
       "deliverables": [
         "shared/tasks/{task-id}/workspace/<file>"
       ]
     }
   }
   ```

   Use `SUCCESS`, `SUCCESS_WITH_NOTES`, `REVISION_NEEDED`, or `BLOCKED` for normal task execution status.

   Submitting a result ends this Worker task. If more work is needed after `REVISION_NEEDED` or `BLOCKED`, wait for your coordinator to assign a new task; do not resume or rewrite the submitted task on your own.

5. In the current room, directly @mention your coordinator with completion:

   ```text
   @coordinator:domain TASK_COMPLETED: {task-id} - <short outcome>. Result: shared/tasks/{task-id}/result.md
   ```

   Do not look up your Worker profile room or private room as a fallback. The task directory is the source of truth if you ever need to verify the assignment room.

## Blocked

If blocked, submit a `BLOCKED` result. `submit_task` automatically pushes and verifies:

```json
{
  "action": "submit_task",
  "payload": {
    "taskId": "{task-id}",
    "status": "BLOCKED",
    "summary": "<what is blocking you>",
    "deliverables": []
  }
}
```

Then @mention your coordinator:

```text
@coordinator:domain BLOCKED: {task-id} - <what is blocking you>
```

Do not invent missing task files, project plans, or shared directories.

## Progress

Progress notes are optional unless the task spec asks for them. If you write progress, put it under:

```text
shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

For a long-running step, include a UTC line such as `- Expected next update: 2026-06-19T12:30:00Z` so heartbeat does not treat unchanged progress as suspected stale work before that time.

Progress updates that require no decision should not @mention anyone.
