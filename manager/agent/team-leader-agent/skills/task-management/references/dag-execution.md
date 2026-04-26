# DAG Execution

Use this reference after creating a project and filling `plan.md` with a DAG task plan.

## Execution Loop

```text
1. Run `taskflow` with `action=ready_tasks` to get unblocked pending tasks.
2. For each ready DAG task:
   a. Run `taskflow` with `action=assign_task`.
   b. Confirm meta.json and spec.md were written.
   c. Publish task files and verify spec.md exists in shared storage.
   d. Publish the updated project plan marker: [ ] -> [~].
   e. Update team-state.json as an index if needed.
   f. @mention the Worker in the Team Room.
3. Wait for Worker completion.
4. On completion:
   a. Refresh task directory from shared storage.
   b. Read result.md.
   c. Run `taskflow` with `action=complete_task` only for successful results.
   d. Update team-state.json as an index if needed.
   e. Publish updated Leader-owned files.
   f. Assign the next ready wave returned by taskflow.
5. When all tasks are [x], aggregate results and complete the project.
```

## Assign Ready Tasks

```json
{
  "action": "ready_tasks",
  "payload": {
    "projectId": "{project-id}"
  }
}
```

For each ready task:

```json
{
  "action": "assign_task",
  "payload": {
    "projectId": "{project-id}",
    "taskId": "st-01",
    "roomId": "room:!team-room:domain",
    "spec": "# Task: Design database schema\n\n## Context\n...\n\n## Deliverables\n..."
  }
}
```

`spec` should include:

- task title
- project context
- deliverables
- constraints
- task directory convention: Worker pulls `shared/tasks/{task-id}/` with `filesync`, reads `shared/tasks/{task-id}/spec.md`, keeps artifacts inside the task directory, submits status/summary/deliverables through `taskflow(action=submit_task)`, pushes the task directory, and verifies `result.md` before reporting completion

Publish task files with the `filesync` tool before notifying the Worker. Leader writes only `meta.json` and `spec.md` inside task directories; do not create task-level `plan.md`.

`taskflow` with `action=assign_task` updates `shared/projects/{project-id}/plan.md` marker from `[ ]` to `[~]`. Publish the project plan after assignment. The project plan is Leader-owned; do not ask Workers to edit it.

Message template:

```text
@alice:{domain} New task [st-01]: Design database schema
Please pull shared/tasks/st-01/ with filesync, then read shared/tasks/st-01/spec.md.
@mention me when complete.
```

## Handle Completion

When a Worker @mentions you with completion:

1. Pull the task directory with `filesync(action="pull")`.
2. Verify `shared/tasks/{task-id}/result.md` with `filesync(action="stat")`.
3. Read `shared/tasks/{task-id}/result.md`. Worker-side `taskflow` should generate the `STATUS: <value>` line.
4. Interpret the outcome:

| Outcome | Action |
|---|---|
| `SUCCESS` | Call `taskflow` with `action=complete_task`, publish plan.md, resolve next wave |
| `SUCCESS_WITH_NOTES` | Same as `SUCCESS`, and record notes |
| `REVISION_NEEDED` | Do not call `complete_task`; create/request a revision path |
| `BLOCKED` | Do not call `complete_task`; escalate to Manager or Team Admin |

5. For `SUCCESS` or `SUCCESS_WITH_NOTES`, run:

   ```json
   {
     "action": "complete_task",
     "payload": {
       "projectId": "{project-id}",
       "taskId": "{task-id}"
     }
   }
   ```

6. Assign all newly unblocked tasks returned in `readyTasks`.

## Parallel Execution

When `taskflow` with `action=ready_tasks` returns multiple tasks:

- Assign all of them.
- Workers execute in parallel.
- As each completes, use `readyTasks` from `taskflow` with `action=complete_task` or run `taskflow` with `action=ready_tasks` again to check for newly unblocked tasks.

## Project Completion

When all tasks in `plan.md` are `[x]`:

1. Aggregate results from all task `result.md` files.
2. Check `source` and `requester` in project `meta.json`. The requester must be the original notification message `sender`, not an inference from task content:
   - `source=manager`: use the Manager parent task from `global-shared/tasks/{parent-task-id}/`, publish the aggregated result through the appropriate helper, then @mention Manager in Leader Room.
   - `source=team-admin`: write summary in `shared/projects/{project-id}/result.md`, then @mention Team Admin in Leader DM.
3. Update project `meta.json`: `status` -> `completed`. Project finalization is Leader-owned; `taskflow` currently manages DAG tasks, not project completion.
4. Update `team-state.json` only as an index if needed.
5. Publish updated Leader-owned files.

## Heartbeat Integration

During heartbeat, for each active project:

1. Pull `shared/projects/{project-id}/` with `filesync` before inspecting the project.
2. Run `taskflow` with `action=ready_tasks`.
3. If ready tasks exist but are not assigned, assign them.
4. If `[~]` tasks have been in progress too long, follow up with the Worker.
5. If the Worker remains unresponsive after two cycles, escalate based on source.
