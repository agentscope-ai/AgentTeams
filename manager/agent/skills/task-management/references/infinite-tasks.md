# Infinite Task Workflow

For recurring/scheduled tasks that repeat on a cron schedule with no natural end.

## Creating an infinite task

1. Create task directory with `meta.json` and `spec.md`:
   - `meta.json`: type "infinite", status "active" (never "completed"), plus `schedule` (5-field cron) and `timezone` (tz database name)
   - `spec.md`: task spec including per-run execution guidelines

2. Push to MinIO.

3. Add to state.json:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
     --action add-infinite --task-id {task-id} --title "{title}" \
     --assigned-to {worker} --room-id {room-id} \
     --schedule "{cron}" --timezone "{tz}" --next-scheduled-at "{ISO-8601}"
   ```

## Triggering execution

Infinite tasks are triggered **exclusively by heartbeat** when `now > next_scheduled_at + 30min` and `last_executed_at < next_scheduled_at`. See HEARTBEAT.md Step 3.

**HARD RULE:** Never paste the trigger text only in an admin DM reply. Workers cannot see the admin DM. Heartbeat (or any dispatch) must send the trigger into the Worker's room using the runtime protocol in HEARTBEAT.md Step 3.

Before sending, use `hiclaw get workers -o json` for `room_id` and `hiclaw get managers -o json | jq -r '.managers[0].runtime'` for runtime:

- **`openclaw`:** **message** tool with `channel=matrix`, `target=room:<ROOM_ID>`, and body:
  `@{worker}:{domain} Execute recurring task {task-id}: {title}. Report back with "executed" when done.`

- **`copaw`:** `copaw channels send --agent-id default --channel matrix --target-session "<ROOM_ID>" --target-user "@{worker}:${HICLAW_MATRIX_DOMAIN}"` with `--text` set to that same body (quoted for the shell).

## Recording execution completion

When a Worker reports `executed`, **only** update state.json:

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
  --action executed --task-id {task-id} --next-scheduled-at "{new-ISO-8601}"
```

**CRITICAL: Do NOT @mention the Worker after recording execution.** "Recording completion" and "triggering next execution" are completely independent. Recording happens when Worker reports back. Triggering happens later during heartbeat when the schedule says it's time. If you @mention the Worker here, you create a rapid-fire loop: Worker executes → reports → you trigger → Worker executes → ... burning tokens continuously.
