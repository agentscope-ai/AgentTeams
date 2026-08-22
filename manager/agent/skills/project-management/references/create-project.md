# Create a Project

## Step 1a: Analyze and decompose

Break the project goal into phases and tasks. For each task identify:
- Clear title and deliverable
- Which Worker role is best suited
- Dependencies on other tasks
- Expected output format

## Step 1b: Create project via script

```bash
PROJECT_ID="proj-$(date +%Y%m%d-%H%M%S)"

bash /opt/hiclaw/agent/skills/project-management/scripts/create-project.sh \
  --id "${PROJECT_ID}" \
  --title "<title>" \
  --workers "worker1,worker2,worker3"
```

The script handles: directory creation, meta.json, placeholder plan.md, Matrix room creation (with admin + all workers invited), Manager groupAllowFrom update, and MinIO sync.

After the script, **fill in the full plan.md** with phases, tasks, and assignments (see `references/plan-format.md` for format).

## Step 1c: Present plan for confirmation

Post plan.md content in **DM with human admin** (not project room) asking for confirmation:

```
I've drafted the project plan for "<title>". Please review and confirm to start:

[paste plan.md content]

If you'd like changes, let me know. Otherwise, reply "confirm" to begin.
```

Wait for confirmation before proceeding.

> **YOLO mode**: The admin is unavailable and cannot be reached. Do NOT wait — auto-confirm immediately, update meta.json `status → active`, set `confirmed_at`, and proceed to Step 1d in the same turn.

## Step 1d: After confirmation

1. Update meta.json: `"status": "planning" → "active"`, set `confirmed_at`
2. Sync to MinIO: `mc mirror /root/hiclaw-fs/shared/projects/${PROJECT_ID}/ ${HICLAW_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/ --overwrite`
3. Verify admin is in the project room — if not, invite immediately
4. Post the project plan in the project room
5. Assign the first task(s) — see `references/task-lifecycle.md`
