# Task Lifecycle (Assign → Complete → Next)

## Assign a Task (Step 2)

### 2a-pre. Check cross-project CRD dependencies

If this project has a federated Project CR with `spec.dependsOn`, fetch it before any assignment:

```bash
curl -s -H "Authorization: Bearer ${AGENTTEAMS_AUTH_TOKEN}" \
  "${AGENTTEAMS_CONTROLLER_URL}/api/v1/projects/${PROJECT_ID}"
```

When `dependencies` contains any entry with `"satisfied": false`, **do not assign tasks** — notify admin in DM which upstream project is still blocking (include `project` + `phase`). Retry after the dependency reaches `Ready`, `Completed`, or `Archived`. Skip this gate when the project has no CR or no `dependsOn`.

### 2a. Determine if Multi-Phase Collaboration

Before creating task files, check if this is a **multi-phase collaborative project**:
- Does the project have multiple phases to be executed by different workers?
- Is there a handoff pattern (Worker A → Worker B → Worker C)?

If YES, you MUST include the following in every task spec:

```markdown
## ⚠️ Multi-Phase Collaboration Protocol

This is a multi-phase collaborative project. When you complete your assigned phase, you MUST:
1. Report completion with **@manager:{domain} PHASE{N}_DONE** (use your phase number)
2. Include a brief summary of what was done
3. Wait for Manager to assign the next phase to the next worker

**DO NOT** post completion without @mentioning Manager — your phase completion triggers the next worker's assignment.
```

This ensures workers in collaborative projects @mention Manager on phase completion, preventing workflow stalls.

### 2b. Create task files

```bash
TASK_ID="task-$(date +%Y%m%d-%H%M%S)"
mkdir -p /root/hiclaw-fs/shared/tasks/${TASK_ID}
```

Write `meta.json`:
```json
{
  "task_id": "<task-id>",
  "project_id": "<project-id>",
  "task_title": "<title>",
  "assigned_to": "<worker-name>",
  "room_id": "<project-room-id>",
  "status": "assigned",
  "depends_on": [],
  "assigned_at": "<ISO-8601>"
}
```

Write `spec.md` with: task title, project context, deliverables, constraints, and the Task Directory Convention:
- Worker creates `plan.md` before starting
- All artifacts stay in the task directory
- Worker writes `result.md` when done
- Worker pushes with: `mc mirror ... --overwrite --exclude "spec.md" --exclude "base/"` (spec.md and base/ are Manager-owned)

### 2c. Sync to MinIO

```bash
mc cp /root/hiclaw-fs/shared/tasks/${TASK_ID}/meta.json ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/${TASK_ID}/meta.json
mc cp /root/hiclaw-fs/shared/tasks/${TASK_ID}/spec.md ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/${TASK_ID}/spec.md
```

### 2d. Update plan.md

Change `[ ]` to `[~]` for the task. Sync plan.md to MinIO.

### 2e. @mention Worker in Project Room

Adapt language to admin's preferred language:
```
@{worker}:{domain} New task [{task-id}]: {task title}

{2-3 sentence summary}

Full spec: ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/spec.md

Please file-sync, read the spec, create plan.md before starting. @mention me when complete.
```

---

## Handle Completion (Step 3)

### 3a. Parse task outcome

Pull task directory from MinIO, then read `result.md` for the Outcome status: `SUCCESS`, `SUCCESS_WITH_NOTES`, `REVISION_NEEDED`, or `BLOCKED`.

### 3b. REVISION_NEEDED → Trigger revision

1. Find revision target in plan.md (`On REVISION_NEEDED:` directive)
2. Identify who revises (`return to {task-id}` → original assignee, `reassign to @{worker}` → specified worker)
3. Create revision task: `meta.json` with `is_revision_for` and `triggered_by` fields, `spec.md` referencing the feedback source
4. Push to MinIO, add revision task to plan.md
5. @mention the worker in project room
6. **Do NOT proceed to next phase** until revision is complete

### 3c. BLOCKED → Handle blocker

See `references/plan-changes.md` Step 4.

### 3d. SUCCESS / SUCCESS_WITH_NOTES

1. Update `meta.json`: `status → completed`, fill `completed_at`
2. Sync to MinIO
3. Update plan.md: `[~]` → `[x]`, add Change Log entry
4. If `SUCCESS_WITH_NOTES`, record notes for reference
5. Notify admin about completion:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
   ```
   Send `[Project Task Completed] {project-title} — {task-id}: {task title} by {worker}. {summary}` to resolved channel. Read SOUL.md first for persona and language.
6. Proceed to find next tasks (3e)

### 3e. Find next tasks

Read plan.md, find `[ ]` tasks whose dependencies are all `[x]`. For each newly unblocked task, go to Step 2.

If the same Worker has another task ready, assign immediately — they're available and context-fresh.

### 3f. All tasks complete

**Mandatory — always execute, including in YOLO mode.**

1. Update meta.json: `status → completed`
2. Update plan.md Status to "completed"
3. Sync to MinIO
4. Post completion summary in project room, @mention admin
5. Update `memory/YYYY-MM-DD.md` with project outcome
