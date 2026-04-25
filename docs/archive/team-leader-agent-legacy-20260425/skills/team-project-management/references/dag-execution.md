# DAG Execution

## Overview

After creating a project and filling in plan.md with the DAG task plan, follow this workflow to execute tasks respecting dependency order.

## Execution Loop

```
1. resolve-dag.sh --action ready → get unblocked pending tasks
2. For each ready task:
   a. Create task directory: shared/tasks/{task-id}/
   b. Write meta.json + spec.md
   c. Publish task files and verify spec.md exists in shared storage
   d. Update project plan: [ ] → [~]
   e. Register in team-state.json: manage-team-state.sh --action add-finite
   f. @mention worker in Team Room
3. Wait for worker completion
4. On completion:
   a. Refresh task directory from shared storage
   b. Read result.md
   c. Update project plan: [~] → [x] (or [!] if blocked, [→] if revision)
   d. Update team-state.json: manage-team-state.sh --action complete
   e. Publish updated Leader-owned files
   f. Go to step 1 (resolve next wave)
5. When all tasks [x] → aggregate results → complete project
```

## Step-by-Step

### Assign Ready Tasks

```bash
# Get ready tasks
READY=$(bash ./skills/team-project-management/scripts/resolve-dag.sh \
  --plan shared/projects/{project-id}/plan.md \
  --action ready)

# For each ready task, create task files
TASK_ID="st-01"
TASK_DIR="shared/tasks/${TASK_ID}"
mkdir -p "${TASK_DIR}"
```

Write `meta.json`:
```json
{
  "task_id": "st-01",
  "project_id": "tp-xxx",
  "task_title": "Design database schema",
  "assigned_to": "alice",
  "status": "assigned",
  "depends_on": [],
  "assigned_at": "ISO-8601"
}
```

Write `spec.md` with: task title, project context, deliverables, constraints, and the Task Directory Convention (worker runs file-sync, reads `shared/tasks/${TASK_ID}/spec.md`, keeps artifacts inside the task directory, and writes `result.md` when done).

Publish task files and verify `spec.md` before notifying the Worker:
```bash
mc cp ${TASK_DIR}/meta.json ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/${TASK_ID}/meta.json
mc cp ${TASK_DIR}/spec.md ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/${TASK_ID}/spec.md
mc stat ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/${TASK_ID}/spec.md
```

Update `shared/projects/{project-id}/plan.md` marker from `[ ]` to `[~]`. The project plan is Leader-owned; do not ask Workers to edit it.

@mention worker in Team Room:
```
@alice:{domain} New task [st-01]: Design database schema
Please file-sync and read shared/tasks/st-01/spec.md
@mention me when complete.
```

### Handle Completion

When worker @mentions you with completion:

1. Refresh from shared storage:
```bash
mc mirror ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/${TASK_ID}/ ${TASK_DIR}/ --overwrite
```

2. Read `result.md` for outcome status.

3. Based on outcome:

| Outcome | Action |
|---------|--------|
| `SUCCESS` | Update plan.md `[~]` → `[x]`, complete in state.json, resolve next wave |
| `SUCCESS_WITH_NOTES` | Same as SUCCESS, record notes |
| `REVISION_NEEDED` | Update plan.md `[~]` → `[→]`, create revision task |
| `BLOCKED` | Update plan.md `[~]` → `[!]`, escalate to Manager or Team Admin |

4. After marking `[x]`, immediately run:
```bash
bash ./skills/team-project-management/scripts/resolve-dag.sh \
  --plan shared/projects/{project-id}/plan.md \
  --action ready
```

5. Assign all newly unblocked tasks (they may be parallelizable).

### Parallel Execution

When `resolve-dag.sh --action ready` returns multiple tasks:
- Assign **all** of them simultaneously
- Workers execute in parallel
- As each completes, run `resolve-dag.sh --action ready` again to check for newly unblocked tasks

### Project Completion

When all tasks in plan.md are `[x]`:

1. Aggregate results from all task `result.md` files
2. Check `source` in project meta.json:
   - **source=manager**: Use the Manager parent task from `global-shared/tasks/{parent-task-id}/` and publish the aggregated result through the appropriate skill/helper, then @mention Manager in Leader Room
   - **source=team-admin**: Write summary in `shared/projects/{project-id}/result.md`, then @mention Team Admin in Leader DM
3. Update project meta.json: `status → completed`
4. Update team-state.json: `manage-team-state.sh --action complete-project --project-id P`
5. Publish updated Leader-owned files

## Heartbeat Integration

During heartbeat, for each active project:
1. Refresh `shared/projects/{project-id}/plan.md` if needed
2. Run `resolve-dag.sh --action ready`
3. If ready tasks exist but are not assigned → assign them (may have been missed)
4. If `[~]` tasks have been in-progress too long → follow up with worker
5. If worker unresponsive after 2 cycles → escalate based on source
