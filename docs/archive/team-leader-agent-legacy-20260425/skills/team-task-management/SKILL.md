---
name: team-task-management
description: Use when you need to assign finite tasks to team workers, track task progress, or manage team-state.json. Never do worker tasks yourself.
---

# Team Task Management

Manage individual tasks within your team. For complex multi-worker tasks with dependencies, use `team-project-management` instead.

## CRITICAL: You Are a Coordinator, Not an Executor

**NEVER write code, design APIs, create deliverables, or do any domain work yourself.**
If you catch yourself doing a worker's job — STOP and delegate instead.

## How to Assign a Task to a Worker

Follow these steps IN ORDER. Do NOT skip any step.

### Step 1: Create and publish spec.md

Resolve current team and worker metadata first:

```bash
hiclaw get teams <your-team-name> -o json
hiclaw get workers --team <your-team-name> -o json
```

Use the CLI output for the Team Room, Worker Room, worker name, and full worker Matrix ID. Do not use stale room IDs or worker IDs from memory, old chat history, or hand-written guesses.

Write the task spec under local team shared storage, then publish it. Workers read the synced local path `shared/tasks/<task-id>/spec.md`.

```bash
# Create spec locally in shared/tasks/
mkdir -p shared/tasks/st-01
cat > shared/tasks/st-01/spec.md << 'EOF'
# Task: Design API endpoints
(your task description here)
EOF

# Publish to team shared storage
mc cp shared/tasks/st-01/spec.md \
  ${HICLAW_STORAGE_PREFIX}/teams/<TEAM_NAME>/shared/tasks/st-01/spec.md

# Verify the Worker can pull it before you notify them
mc stat ${HICLAW_STORAGE_PREFIX}/teams/<TEAM_NAME>/shared/tasks/st-01/spec.md
```

Do not put remote storage paths in chat messages or task specs. Tell Workers only the local path: `shared/tasks/<task-id>/spec.md`.

### Step 2: Notify the worker

After the remote `spec.md` is verified, notify the worker.

- If you are already in the Team Room, reply directly in the current session.
- If you are in Leader DM or another room, use the `copaw channels send` template in `AGENTS.md` to send into the Team Room or the worker's room.
- In both cases, use the full worker Matrix ID from `hiclaw get workers --team <team> -o json` as the visible @mention.

Message template:

```text
@worker:domain New task [st-01]: Design API endpoints.
Please file-sync and read shared/tasks/st-01/spec.md.
@mention me when complete.
```

### Step 3: Track in team-state.json

```bash
bash ./skills/team-task-management/scripts/manage-team-state.sh \
  --action add-finite --task-id st-01 --title "Design API endpoints" \
  --assigned-to <worker-name> --room-id '<Team Room>' \
  --source team-admin --requester '<admin Matrix ID>'
```

## Task Sources

| Source | Channel | Report to |
|--------|---------|-----------|
| Manager | Leader Room @mention | Manager in Leader Room |
| Team Admin | Leader DM message | Team Admin in Leader DM |

## Key Scripts

```bash
# Track task state
bash ./skills/team-task-management/scripts/manage-team-state.sh \
  --action add-finite --task-id ID --title TITLE \
  --assigned-to WORKER --room-id ROOM --source SOURCE

# Mark task complete
bash ./skills/team-task-management/scripts/manage-team-state.sh \
  --action complete --task-id ID

# List active tasks
bash ./skills/team-task-management/scripts/manage-team-state.sh --action list
```

## References

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Assign a task or handle completion | `references/finite-tasks.md` |
| State management details | `references/state-management.md` |
