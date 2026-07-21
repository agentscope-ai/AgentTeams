# Create a Project

## Step 0: Check YOLO mode (decides Step 1c behaviour)

**Run this first — before drafting anything:**

```bash
if [ "${AGENTTEAMS_YOLO:-}" = "1" ] || [ -f ~/yolo-mode ]; then
    echo "YOLO=ON"   # admin unavailable, you auto-confirm in Step 1c
else
    echo "YOLO=OFF"  # admin will reply "confirm"; you wait in Step 1c
fi
```

The result determines Step 1c. **Do not skip this step** — silently waiting for an unreachable admin stalls the entire project indefinitely.

## Step 1a: Analyze and decompose

Break the project goal into phases and tasks. For each task identify:
- Clear title and deliverable
- Which Worker role is best suited
- Dependencies on other tasks
- Expected output format

## Step 1b: Create project via script

```bash
PROJECT_ID="proj-$(date +%Y%m%d-%H%M%S)"

bash /opt/agentteams/agent/skills/project-management/scripts/create-project.sh \
  --id "${PROJECT_ID}" \
  --title "<title>" \
  --workers "worker1,worker2,worker3"
```

The script handles: directory creation, meta.json, placeholder plan.md, Matrix room creation (with admin + all workers invited), Manager groupAllowFrom update, and MinIO sync.

After the script, **fill in the full plan.md** with phases, tasks, and assignments (see `references/plan-format.md` for format).

## Step 1c: Present plan (and confirm)

### If YOLO=OFF (Step 0)

Post plan.md content in **DM with human admin** (not project room) asking for confirmation:

```
I've drafted the project plan for "<title>". Please review and confirm to start:

[paste plan.md content]

If you'd like changes, let me know. Otherwise, reply "confirm" to begin.
```

Wait for confirmation before proceeding.

### If YOLO=ON (Step 0) — MANDATORY auto-confirm

The admin is unavailable and **cannot be reached**. Asking for confirmation will stall the project indefinitely — no human will ever reply.

**Do all of the following in the same turn**, before sending any DM:
1. Update `meta.json`: `"status": "planning" → "active"`, set `confirmed_at` to current ISO timestamp
2. Sync to MinIO (see Step 1d, item 2)
3. Then post a single DM informing admin (no question, just a notice): `"Auto-confirmed plan for <title> (YOLO mode). Starting Phase 1 now."`
4. Proceed immediately to Step 1d items 3–5 in the **same** turn

Never post a "please confirm" message in YOLO mode — that is a hard rule, not a guideline.

## Step 1d: After confirmation

1. Update meta.json: `"status": "planning" → "active"`, set `confirmed_at`
2. Sync to MinIO: `mc mirror /root/agentteams-fs/shared/projects/${PROJECT_ID}/ ${AGENTTEAMS_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/ --overwrite`
3. Verify admin is in the project room — if not, invite immediately
4. Post the project plan in the project room
5. Assign the first task(s) — see `references/task-lifecycle.md`
