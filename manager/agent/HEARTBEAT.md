## Manager Heartbeat Checklist

### 1. Read state.json

Read state.json (local only, no sync needed). If the file does not exist, initialize it first:

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh --action init
cat ~/state.json
```

The `active_tasks` field in state.json contains all in-progress tasks (both finite and infinite). No need to iterate over all meta.json files.

**Ensure admin notification channel is available** (used in Step 7):

1. Check `admin_dm_room_id` in state.json. If `null`, discover it now:
   - List joined rooms, find the DM room with exactly 2 members: you and `@${HICLAW_ADMIN_USER}:${HICLAW_MATRIX_DOMAIN}`
   - Persist it:
     ```bash
     bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
       --action set-admin-dm --room-id "<discovered-room-id>"
     ```
2. Verify the channel is resolvable:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
   ```
   If the output shows `"channel": "none"`, the admin DM room discovery above may have failed — retry or log a warning.

---

### 2. Check Status of Finite Tasks

Iterate over entries in `active_tasks` with `"type": "finite"`:

- Read `assigned_to`, `room_id`, and `project_room_id` (if present) from the entry
- Determine the target room: use `project_room_id` if available, otherwise use `room_id`
- **Before sending any message**, ensure the Worker's container is running:
  ```bash
  bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh \
    --action ensure-ready --worker {worker}
  ```
  The script outputs JSON with a `status` field:
  - `ready` — container was already running, proceed normally
  - `started` — container was stopped and has been woken up; **wait 30 seconds** for the Worker to initialize before sending the follow-up message
  - `recreated` — container was missing and has been recreated; **wait 60 seconds** before sending the follow-up message, and flag this anomaly for the admin report (Step 7)
  - `remote` — Worker is remotely deployed, assumed reachable
  - `failed` — could not start/recreate the container; **skip the follow-up message**, flag the anomaly for the admin report (Step 7), and suggest the admin intervene
- **Use the `message` tool** to send a follow-up to that room, with `user_id` set to the Worker's Matrix ID (`@{worker}:${HICLAW_MATRIX_DOMAIN}`):
  ```
  room_id: <room_id from state.json>
  user_id: @{worker}:${HICLAW_MATRIX_DOMAIN}
  message: @{worker}:{domain} How is your current task {task-id} going? Are you blocked on anything?
  ```
- Determine if the Worker is making normal progress based on their reply
- If the Worker has not responded (no response for more than one heartbeat cycle), flag the anomaly in the Room and notify the human admin (see Step 7)
- If the Worker has replied that the task is complete but meta.json has not been updated, proactively update meta.json (status → completed, fill in completed_at), and remove the entry from `active_tasks`:
  ```bash
  bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh --action complete --task-id {task-id}
  ```

---

### 2b. Check Team-Delegated Tasks

Iterate over entries in `active_tasks` that have a `delegated_to_team` field:

- These tasks are managed by Team Leaders, NOT individual workers
- Read `assigned_to` (the Team Leader name) and `room_id` (the Leader Room)
- **Ensure the Team Leader's container is running**:
  ```bash
  bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh \
    --action ensure-ready --worker {leader}
  ```
- **Use the `message` tool** to ask the Team Leader for a status update:
  ```
  room_id: <room_id from state.json>
  user_id: @{leader}:${HICLAW_MATRIX_DOMAIN}
  message: @{leader}:{domain} How is task {task-id} progressing? Any blockers from your team?
  ```
- **Do NOT contact team workers directly** — the Team Leader handles internal coordination
- If the Team Leader reports completion, process it the same as a regular worker completion
- If the Team Leader reports a blocker, escalate to admin (Step 7)

---

### 3. Check Infinite Task Timeouts

Iterate over entries in `active_tasks` with `"type": "infinite"`. For each entry:

```
Current UTC time = now

Conditions (both must be met):
  1. last_executed_at < next_scheduled_at (not yet executed this cycle)
     OR last_executed_at is null (never executed)
  2. now > next_scheduled_at + 30 minutes (overdue)
```

If conditions are met:

1. **Ensure the Worker's container is running** before triggering:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh \
     --action ensure-ready --worker {worker}
   ```
   If `status` is `failed`, skip the trigger and flag the anomaly for the admin report (Step 7). If `started` or `recreated`, wait for the Worker to initialize (30s / 60s respectively).

2. Read `room_id` from the entry and **use the `message` tool** to trigger execution:
```
room_id: <room_id from state.json>
user_id: @{worker}:${HICLAW_MATRIX_DOMAIN}
message: @{worker}:{domain} It's time to run your scheduled task {task-id} "{task-title}". Please execute it now and report back with the keyword "executed".
```

**Note**: Infinite tasks are never removed from active_tasks. After the Worker reports `executed`, **only** update `last_executed_at` and `next_scheduled_at` — do NOT @mention the Worker again:
```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
  --action executed --task-id {task-id} --next-scheduled-at "{new-ISO-8601}"
```

**⚠️ CRITICAL**: Triggering and recording are independent actions. Heartbeat triggers execution when the schedule says it's time. Recording happens when the Worker reports back. Never re-trigger a Worker immediately after recording — the next execution will be triggered by a future heartbeat when `next_scheduled_at` is due.

---

### 4. Project Progress Monitoring

Scan plan.md for all active projects under /root/hiclaw-fs/shared/projects/:

```bash
for meta in /root/hiclaw-fs/shared/projects/*/meta.json; do
  cat "$meta"
done
```

- Filter projects with `"status": "active"`
- For each active project, read `project_room_id` from meta.json, then read plan.md and find tasks marked as `[~]` (in progress)
- If the responsible Worker has had no activity during this heartbeat cycle, **ensure the Worker's container is running first** (`lifecycle-worker.sh --action ensure-ready --worker {worker}`), then **use the `message` tool** to send a follow-up to the project room:
  ```
  room_id: <project_room_id from meta.json>
  user_id: @{worker}:${HICLAW_MATRIX_DOMAIN}
  message: @{worker}:{domain} Any progress on your current task {task-id} "{title}"? Please let us know if you're blocked.
  ```
- If a Worker has reported task completion in the project room but plan.md has not been updated yet, handle it immediately (see the project management section in AGENTS.md)

---

### 5. Capacity Assessment

- Count the number of `type=finite` entries in state.json (finite tasks in progress) and identify idle Workers with no assigned tasks (neither finite nor infinite)
- If Workers are insufficient, check in with the human admin about whether new Workers need to be created
- If Workers are idle, suggest reassigning tasks

---

### 6. Worker Container Lifecycle Management

Only execute when the container API is available (check first):

```bash
bash -c 'source /opt/hiclaw/scripts/lib/container-api.sh && container_api_available && echo available'
```

If the output is `available`, proceed with the following steps:

1. Sync status:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action sync-status
   ```

2. Detect idle Workers and auto-stop those that have exceeded the timeout:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action check-idle
   ```
   For each Worker that was auto-stopped, look up the Worker's `room_id` from `workers-registry.json` and **use the `message` tool** to log:
   ```
   room_id: <Worker's room_id from workers-registry.json>
   message: Worker <name> container has been automatically paused due to idle timeout. It will be automatically resumed when a task is assigned.
   ```

---

### 7. Report to Admin

**All heartbeat findings MUST be sent to the admin via the `message` tool** (not as a reply in the current heartbeat context).

- If all Workers are healthy and there are no pending items: HEARTBEAT_OK (no message needed)
- Otherwise, **read SOUL.md first** — use the identity, personality, and **user's preferred language** defined there when composing the report. Report in that language and tone.
- Resolve the notification channel:
  ```bash
  bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
  ```
  The script outputs JSON with `channel`, `target`, and `via` fields. Use the `message` tool with those values:
  - If `channel` is not `"none"`: send `[Heartbeat Report] <summarize findings and recommended actions, in SOUL.md persona and language>` to the resolved `target`.
  - If `channel` is `"none"`: admin DM room has not been discovered yet — attempt discovery now (see Step 1), then retry.
