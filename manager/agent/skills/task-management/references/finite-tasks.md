# Finite Task Workflow

## Choosing task type

- **Finite** — clear end state. Worker delivers result, it's done. Examples: "implement login page", "fix bug #123", "write a report".
- **Infinite** — repeats on schedule, no natural end. See `references/infinite-tasks.md`.

**Rule**: if the request contains a recurring schedule or implies ongoing monitoring, use infinite. Everything else is finite.

## Assigning a finite task

### Structured intake (before generating the task ID)

When a human hands over a finite task, walk the request through this fill-in-the-blanks skeleton before you create anything. Fill each field by best guess from what the human already said; ask **at most one** clarifying question for the fields you genuinely cannot infer; then confirm the filled-in skeleton back to the human in **one message** before dispatching. This skeleton doubles as the schema behind the future dashboard task form.

```
Deliverable:          {what "done" produces — a file, a PR, a report, a decision}
Acceptance criteria:  {how you or the human will know it's done}
Target team/worker:   {which team or worker should do this — infer from worker-selection.md if not stated}
Priority:             {normal | high | urgent — default normal if unstated}
Due:                  {date/time, or "none" if open-ended}
```

Rules:
- Best-guess first. Only ask a clarifying question when a field is truly ambiguous (e.g. deliverable format is unclear, or no team/worker fits) — never ask about fields you can reasonably infer.
- One question maximum, and only if needed. Bundle it into a single message rather than asking field-by-field.
- Confirm once: after filling the skeleton (with or without the human's answer to your one question), send back the completed skeleton in one message so the human can correct it before work starts, then proceed to step 1 below.
- The confirmed skeleton's `Deliverable` and `Acceptance criteria` become the basis for `spec.md` (step 2); `Target team/worker` drives `assigned_to`/`room_id` (steps 2–5); `Priority` and `Due` are recorded in `meta.json` alongside `type` and `status`.

1. Generate task ID: `task-YYYYMMDD-HHMMSS`
2. Create task directory and files:
   ```bash
   mkdir -p /root/hiclaw-fs/shared/tasks/{task-id}
   ```
   Write `meta.json` (type: "finite", status: "assigned") and `spec.md` (requirements, acceptance criteria, context).

3. Push to MinIO **immediately** — Worker cannot file-sync until files are in MinIO:
   ```bash
   mc cp /root/hiclaw-fs/shared/tasks/{task-id}/meta.json ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/meta.json
   mc cp /root/hiclaw-fs/shared/tasks/{task-id}/spec.md ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/spec.md
   ```
   **Verify the push succeeded** (non-zero exit = retry). Do NOT proceed to step 4 until files are confirmed in MinIO.

4. Notify Worker in their Room (never in admin DM):

   **HARD RULE:** Do **not** put @worker task-assignment text in your admin DM reply. Workers cannot read the admin DM. The admin DM reply must only confirm to the admin (for example: assigned `{task-id}` to `{worker}`). The full dispatch with @mention MUST go to the Worker's Matrix room using the helper below.

   a) Get the Worker's `room_id` from `hiclaw get workers -o json` (`.roomID` / room field for that worker).

   b) Compose the body the Worker must receive (full Matrix @mention so they wake):
   ```
   @{worker}:{domain} New task [{task-id}]: {title}. Use your file-sync skill to pull the spec: shared/tasks/{task-id}/spec.md. @mention me when complete.
   ```

   c) Send via the task dispatch helper (handles OpenClaw vs CoPaw automatically — do **not** call `hiclaw get managers` for runtime):
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/send-task-message.sh \
     --room "<ROOM_ID>" \
     --worker "{worker}" \
     --text '@{worker}:{domain} New task [{task-id}]: {title}. Use your file-sync skill to pull the spec: shared/tasks/{task-id}/spec.md. @mention me when complete.'
   ```
   - **CoPaw:** the script runs `copaw channels send` and exits 0.
   - **OpenClaw:** the script prints the target room + body and exits 2 — deliver that text with the **message** tool (`channel=matrix`, `target=room:<ROOM_ID>`).

5. **MANDATORY — Add to state.json** (this step is NOT optional, even for coordination, research, or management tasks):
   ```bash
   # manage-state.sh delegates to hiclaw manager-state when available
   bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
     --action add-finite --task-id {task-id} --title "{title}" \
     --assigned-to {worker} --room-id {room-id}
   ```
   If task belongs to a project, append `--project-room-id {project-room-id}`.
   **WARNING**: Skipping this step causes the Worker to be auto-stopped by idle timeout. Every task assigned to a Worker MUST be registered here.

## On completion

1. Pull task directory from MinIO (Worker has pushed results):
   ```bash
   mc mirror ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/ /root/hiclaw-fs/shared/tasks/{task-id}/ --overwrite
   ```
2. Update `meta.json`: status=completed, fill completed_at. Push back to MinIO.
3. Remove from state.json:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
     --action complete --task-id {task-id}
   ```
4. Log to `memory/YYYY-MM-DD.md`.
5. Notify admin — read SOUL.md first for persona/language, then resolve channel:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
   ```
   Then send admin notification using `resolve-notify-channel.sh` output:
   - **`openclaw`:** If `channel` is not `"none"`, use the **message** tool with the resolved `channel` and `target`.
   - **`copaw`:** If `channel` is not `"none"`, use **`copaw channels send`** with the resolved channel and target. If you are **in an admin DM session** for this turn, put the completion text in your **final reply only** (see copaw-manager-agent AGENTS.md).

   - If `channel` is `"none"`: the admin DM room is not yet cached. Discover it now — list joined rooms, find the DM room with exactly 2 members (you and admin), then persist:
     ```bash
     bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
       --action set-admin-dm --room-id "<discovered-room-id>"
     ```
     After persisting, retry `resolve-notify-channel.sh` and send the notification. If discovery fails, log a warning and move on — heartbeat will catch up.

## Task directory layout

```
shared/tasks/{task-id}/
├── meta.json     # Manager-maintained
├── spec.md       # Manager-written
├── base/         # Manager-maintained reference files (Workers must not overwrite)
├── plan.md       # Worker-written execution plan
├── result.md     # Worker-written final result
└── *             # Intermediate artifacts
```
