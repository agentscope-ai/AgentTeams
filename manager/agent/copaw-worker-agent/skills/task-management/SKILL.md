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

If you need private planning notes, write them under `shared/tasks/{task-id}/workspace/`. Do not create shared task-level `plan.md`.

Do not edit project-level `shared/projects/{project-id}/plan.md` or `meta.json` unless the task spec explicitly tells you to.

## Execution Flow

1. Pull `shared/tasks/{task-id}/` with the `filesync` tool. This is mandatory whenever the task references `shared/...`; do it before checking whether `spec.md` exists.
2. Read `shared/tasks/{task-id}/meta.json` and `shared/tasks/{task-id}/spec.md`.
3. Acknowledge the task locally with `taskflow`:

   ```json
   {
     "action": "ack_task",
     "payload": {
       "taskId": "{task-id}"
     }
   }
   ```

4. Execute the task.
5. Keep deliverables inside `shared/tasks/{task-id}/`.
6. Push after meaningful updates:

   ```json
   {
     "action": "push",
     "payload": {
       "path": "shared/tasks/{task-id}/",
       "exclude": ["spec.md", "meta.json", "base/"]
     }
   }
   ```

7. Submit the task result with `taskflow`. This writes `shared/tasks/{task-id}/result.md` and marks local task state submitted:

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

   Use `SUCCESS`, `SUCCESS_WITH_NOTES`, `REVISION_NEEDED`, or `BLOCKED` for `status`.

8. Push `shared/tasks/{task-id}/` again. Keep excluding coordinator-owned inputs (`spec.md`, `meta.json`, `base/`) unless the spec explicitly asks otherwise.
9. Verify `shared/tasks/{task-id}/result.md` with `filesync(action="stat")`.
10. @mention your coordinator with completion only after `stat` returns `ok=true`:

   ```text
   @coordinator:domain TASK_COMPLETED: {task-id} - <short outcome>. Result: shared/tasks/{task-id}/result.md
   ```

## Blocked

If blocked, submit a `BLOCKED` result before you @mention your coordinator:

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

Then push `shared/tasks/{task-id}/`, verify `result.md`, and @mention:

```text
@coordinator:domain BLOCKED: {task-id} - <what is blocking you>
```

Do not invent missing task files, project plans, or shared directories.

## Progress

Progress notes are optional unless the task spec asks for them. If you write progress, put it under:

```text
shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

Progress updates that require no decision should not @mention anyone.
