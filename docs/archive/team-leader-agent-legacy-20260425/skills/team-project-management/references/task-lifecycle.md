# Task Lifecycle (within Team Projects)

## Task Directory Convention

All team project tasks live under:
```
shared/tasks/{task-id}/
├── meta.json      # Task metadata (Leader-owned)
├── spec.md        # Requirements (Leader-owned, read-only to workers)
├── result.md      # Outcome (Worker-written)
├── plan.md        # Execution plan (Worker-written)
├── base/          # Reference files (Leader-maintained, read-only to workers)
└── workspace/     # Shared workspace (Worker + Leader)
```

## Assign a Task

### 1. Create task files

```bash
TASK_ID="st-01"
TASK_DIR="shared/tasks/${TASK_ID}"
mkdir -p "${TASK_DIR}"
```

Write `meta.json`:
```json
{
  "task_id": "st-01",
  "project_id": "tp-20260331-100000",
  "task_title": "Design database schema",
  "assigned_to": "alice",
  "status": "assigned",
  "depends_on": [],
  "assigned_at": "2026-03-31T10:00:00Z"
}
```

Write `spec.md` with:
- Task title and project context
- Deliverables and acceptance criteria
- Constraints and references
- Task Directory Convention reminder:
  - Run file-sync before reading the task
  - Read `shared/tasks/{task-id}/spec.md`
  - Create `plan.md` before starting
  - Keep all artifacts inside `shared/tasks/{task-id}/`
  - Write `result.md` when done and push the task directory with the file-sync helper

### 2. Publish task files

```bash
mc cp ${TASK_DIR}/meta.json ${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/tasks/${TASK_ID}/meta.json
mc cp ${TASK_DIR}/spec.md ${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/tasks/${TASK_ID}/spec.md
mc stat ${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/tasks/${TASK_ID}/spec.md
```

Do not send the remote storage path to Workers. They only need `shared/tasks/${TASK_ID}/spec.md`.

### 3. Update project plan

Change `[ ]` to `[~]` for this task in `shared/projects/{project-id}/plan.md`. The project plan is Leader-owned; do not ask Workers to edit it.

### 4. Register in state

```bash
bash ./skills/team-task-management/scripts/manage-team-state.sh \
  --action add-finite --task-id st-01 --title "Design database schema" \
  --assigned-to alice --room-id '!teamroom:domain' \
  --source manager --parent-task-id task-xxx
```

### 5. @mention Worker

In Team Room, or with `copaw channels send` only if you are currently in a different room:
```
@alice:{domain} New task [st-01]: Design database schema
Please file-sync and read shared/tasks/st-01/spec.md
Create plan.md inside the task directory before starting. @mention me when complete.
```

## Handle Completion

### 1. Refresh results

```bash
mc mirror ${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/tasks/${TASK_ID}/ ${TASK_DIR}/ --overwrite
```

### 2. Read result.md

Check the `Outcome` → `Status` field.

### 3. SUCCESS / SUCCESS_WITH_NOTES

1. Update `shared/tasks/${TASK_ID}/meta.json`: `status → completed`, fill `completed_at`
2. Update `shared/projects/{project-id}/plan.md`: `[~]` → `[x]`, add Change Log entry
3. Complete in state: `manage-team-state.sh --action complete --task-id st-01`
4. Publish updated Leader-owned files
5. Run `resolve-dag.sh --action ready` to find next tasks

### 4. REVISION_NEEDED

1. Update plan.md: `[~]` → `[→]`
2. Create revision task with `is_revision_for` and `triggered_by` in meta.json
3. Assign revision to original worker (or as specified)
4. **Do NOT proceed to dependent tasks** until revision completes

### 5. BLOCKED

1. Update plan.md: `[~]` → `[!]`
2. Escalate based on source:
   - source=manager → @mention Manager in Leader Room
   - source=team-admin → @mention Team Admin in Leader DM

## result.md Format

```markdown
# Task Result: {title}

**Task ID**: {task-id}
**Completed**: {ISO datetime}

## Outcome

**Status**: SUCCESS | SUCCESS_WITH_NOTES | REVISION_NEEDED | BLOCKED

## Summary

{Brief summary of what was done}

## Deliverables

{List of completed deliverables}

## Notes

{Any notes, issues, or suggestions}
```
