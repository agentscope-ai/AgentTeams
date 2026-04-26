---
name: task-management
description: Use before any taskflow call or Leader workflow involving projects, DAG planning, task creation, task assignment, Worker completion, blockers, revisions, project recovery, heartbeat DAG checks, result aggregation, or team-state updates. Always use this skill when the request mentions tasks, projects, DAG, dependencies, completion, result.md, BLOCKED, REVISION_NEEDED, ready tasks, or assigning Workers.
---

# Task Management

You are the Leader. Coordinate work; do not do Worker domain work.

## Ownership

Leader owns:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
shared/projects/{project-id}/result.md
team-state.json
```

Worker owns:

```text
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/<deliverables>
```

Worker submits the final task status through `taskflow(action=submit_task)`, which writes `shared/tasks/{task-id}/result.md` in the standard format.

Every external request becomes a Project first. A Task is only a single-Worker execution unit derived from a project `plan.md` DAG item.

Use `taskflow` for project graph and task state transitions. Use `filesync` separately before reading remote changes and after publishing local changes. `taskflow` never pulls or pushes files for you.

Current `taskflow` only completes successful DAG tasks. For `REVISION_NEEDED` or `BLOCKED` results, stop and decide the next Leader action; do not call `complete_task`.

Do not create bare tasks directly from Manager or Team Admin requests.
Do not create or ask Workers to create task-level `plan.md`.
Do not ask Workers to edit project-level `plan.md` or `meta.json`.
Do not use old shell scripts under `skills/task-management/scripts`; task state changes go through `taskflow`.

## Create A Project

Use projects for all incoming work, even if the project has only one Worker task.

Set the project requester from the current notification message `sender`:

- Record the original `sender` as `requester`.
- Set `source` from that sender's role, such as `team-admin` or `manager`.
- Later reports go back to the recorded requester. Do not infer Manager source from the fact that the work is a project.

1. Refresh organization state with `organization` skill.
2. Create project files with `taskflow(action, payload)`:

   ```json
   {
     "action": "create_project",
     "payload": {
       "projectId": "{project-id}",
       "title": "<project title>",
       "source": "<manager|team-admin>",
       "requester": "<requester Matrix ID or task ID>"
     }
   }
   ```

3. Add one DAG item per Worker execution unit:

   ```json
   {
     "action": "add_tasks",
     "payload": {
       "projectId": "{project-id}",
       "tasks": [
         {
           "taskId": "st-01",
           "title": "<task title>",
           "assignedTo": "@worker:domain",
           "dependsOn": []
         },
         {
           "taskId": "st-02",
           "title": "<task title>",
           "assignedTo": "@worker:domain",
           "dependsOn": ["st-01"]
         }
       ]
     }
   }
   ```

   A simple one-Worker request still uses a one-item DAG.

4. Publish project files with `file-sharing` skill:

   ```json
   {
     "action": "push",
     "path": "shared/projects/{project-id}/"
   }
   ```

5. Call `taskflow` with `action=ready_tasks`, then assign each ready DAG item as a Worker task.

## Assign A DAG Task

Assign only tasks that came from `shared/projects/{project-id}/plan.md`.

Task assignment is team-room visible. Use the team room as the assignment `roomId` and @mention the assigned Worker in the message. Do not send normal task assignments to a Worker's private room.

1. Create one task directory per DAG item with `taskflow` and `action=assign_task`. Do not reuse the project ID as a task ID for multiple Workers:

   ```json
   {
     "action": "assign_task",
     "payload": {
       "projectId": "{project-id}",
       "taskId": "st-01",
       "roomId": "room:!team-room:domain",
       "spec": "# Task: <title>\n\n## Context\n...\n\n## Expected Result\nKeep deliverables under shared/tasks/st-01/ and submit status, summary, and deliverables through taskflow(action=submit_task)."
     }
   }
   ```

   This writes:

   ```text
   shared/tasks/{task-id}/meta.json
   shared/tasks/{task-id}/spec.md
   ```

   It also updates the project plan marker from `[ ]` to `[~]`.

2. Publish and verify task files plus updated project plan with `file-sharing` skill.
3. Notify Worker in the team room with `communication` skill:

   ```text
   @worker:domain New task [st-01]: <title>
   Please pull shared/tasks/st-01/ with filesync, then read shared/tasks/st-01/spec.md.
   @mention me when complete.
   ```

4. Update `team-state.json` only as an index if needed. Do not treat it as the source of truth.

## Handle Completion

When Worker reports completion:

1. Pull `shared/tasks/{task-id}/` with `filesync(action="pull")`.
2. Verify `shared/tasks/{task-id}/result.md` with `filesync(action="stat")`.
3. Read `shared/tasks/{task-id}/result.md`. It should contain `STATUS: <value>`, generated by Worker-side `taskflow`.
4. If `STATUS` is `SUCCESS` or `SUCCESS_WITH_NOTES`, complete the DAG item:

   ```json
   {
     "action": "complete_task",
     "payload": {
       "projectId": "{project-id}",
       "taskId": "{task-id}"
     }
   }
   ```

5. If `STATUS` is `REVISION_NEEDED` or `BLOCKED`, do not call `complete_task`; record the blocker or revision need, then decide whether to ask a question, assign a new DAG task, or escalate.
6. Publish updated `shared/projects/{project-id}/plan.md` after any Leader-owned change.
7. Assign any `readyTasks` returned by `taskflow`, or call `taskflow` with `action=ready_tasks`.
8. Update `team-state.json` only as an index if needed.
9. Report outcome to the original requester when the project or requested milestone is complete.

## Project Flow

Project flow is the only external request flow.

1. Create the project.
2. Add and validate DAG tasks with `taskflow` and `action=add_tasks`.
3. Publish project files with `filesync(action="push", path="shared/projects/{project-id}/")`.
4. Assign ready tasks as DAG tasks under `shared/tasks/{task-id}/` with `taskflow` and `action=assign_task`.
5. On completion, call `taskflow` with `action=complete_task`, publish the project plan, then assign the next ready wave.

Use `file-sharing` to publish project files after `taskflow` changes them. Do not use a project-creation script to hide file ownership or storage behavior.

## Source-Aware Finalization

For Manager-sourced tasks, read Manager parent input from:

```text
global-shared/tasks/{parent-task-id}/
```

When the team finishes, aggregate Worker results and report to the original requester recorded on the project:

- `source=team-admin`: report to Team Admin in Leader DM.
- `source=manager`: report to Manager in Leader Room, using the Manager parent task from `global-shared/...` when present.

Project finalization is still Leader-owned: write `shared/projects/{project-id}/result.md`, update project `meta.json` status if needed, publish with `filesync`, and report. Use a helper or explicit skill instruction to publish the final result; do not hand-write remote storage paths in chat.

## References

Read the relevant reference only when needed:

- `references/dag-tasks.md` — assigning and completing one Worker task derived from a project DAG
- `references/dag-execution.md` — executing a project plan with task dependencies
