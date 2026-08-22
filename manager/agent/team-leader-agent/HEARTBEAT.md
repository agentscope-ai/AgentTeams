# Team Leader Heartbeat Checklist

Run this checklist periodically (every ~15 minutes) to ensure team tasks are progressing.

## Step 1: Read team-state.json

```bash
bash ~/skills/team-task-management/scripts/manage-team-state.sh --action list
```

If no active tasks, heartbeat is done — no action needed.

## Step 2: Check Active Sub-Tasks

For each active sub-task in team-state.json:

1. Check if the assigned worker's container is running
2. If the worker has not responded in a while, send a follow-up:
   ```
   @{worker}:{domain} How is sub-task {sub-id} going? Are you blocked on anything?
   ```
3. If worker reports completion but team-state.json not updated, proactively update it

## Step 3: Check for Stalled Tasks

If a sub-task has been in-progress for an unusually long time:
- @mention the worker asking for status
- If no response after 2 heartbeat cycles, escalate to Manager:
  ```
  @manager:{domain} Worker {name} is unresponsive on sub-task {sub-id} of task {task-id}. May need intervention.
  ```

## Step 4: Report to Manager (if issues found)

Only @mention Manager if:
- A worker is unresponsive or blocked
- A sub-task failed and needs escalation
- All sub-tasks completed (send completion report)

If everything is healthy, do NOT message Manager — silence means all is well.
