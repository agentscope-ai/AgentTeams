---
name: task-management
description: Use when assigning Worker tasks, running team projects, tracking task state, or aggregating results.
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
shared/tasks/{task-id}/plan.md
shared/tasks/{task-id}/result.md
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/<deliverables>
```

Do not ask Workers to edit project-level `plan.md` or `meta.json`.

## Assign A Finite Task

1. Refresh organization state with `organization` skill.
2. Create task directory:

   ```bash
   TASK_ID="st-01"
   TASK_DIR="shared/tasks/${TASK_ID}"
   mkdir -p "$TASK_DIR"
   ```

3. Write Leader-owned task files:

   ```text
   shared/tasks/{task-id}/meta.json
   shared/tasks/{task-id}/spec.md
   ```

4. Publish and verify task files with `file-sharing` skill.
5. Notify Worker with `communication` skill:

   ```text
   @worker:domain New task [st-01]: <title>
   Please file-sync and read shared/tasks/st-01/spec.md.
   Create plan.md inside the task directory before starting.
   @mention me when complete.
   ```

6. Register task:

   ```bash
   bash ./skills/task-management/scripts/manage-team-state.sh \
     --action add-finite \
     --task-id "$TASK_ID" \
     --title "<title>" \
     --assigned-to "<worker>" \
     --room-id "<team-room>" \
     --source "<manager|team-admin>"
   ```

## Handle Completion

When Worker reports completion:

1. Refresh `shared/tasks/{task-id}/` from storage with the `file-sharing` skill.
2. Read `shared/tasks/{task-id}/result.md`.
3. Update `team-state.json`:

   ```bash
   bash ./skills/task-management/scripts/manage-team-state.sh \
     --action complete \
     --task-id "$TASK_ID"
   ```

4. If this task belongs to a project, update `shared/projects/{project-id}/plan.md`.
5. Resolve next ready tasks if needed.
6. Report outcome to the original requester.

## Project Flow

Use projects only when a task needs multiple Workers or dependencies.

1. Create `shared/projects/{project-id}/meta.json`.
2. Create `shared/projects/{project-id}/plan.md`.
3. Validate DAG:

   ```bash
   bash ./skills/task-management/scripts/resolve-dag.sh \
     --plan "shared/projects/{project-id}/plan.md" \
     --action validate
   ```

4. Assign ready tasks as finite tasks under `shared/tasks/{task-id}/`.
5. On completion, update project plan and resolve the next ready tasks:

   ```bash
   bash ./skills/task-management/scripts/resolve-dag.sh \
     --plan "shared/projects/{project-id}/plan.md" \
     --action ready
   ```

## Manager-Sourced Tasks

Read Manager parent input from:

```text
global-shared/tasks/{parent-task-id}/
```

When the team finishes, aggregate Worker results and report to Manager. Use a helper or explicit skill instruction to publish the final result; do not hand-write remote storage paths in chat.
