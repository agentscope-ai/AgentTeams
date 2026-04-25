---
name: team-task-coordination
description: Coordinate access to team task directories using .processing marker files. Use before accessing a worker's task workspace to prevent conflicts when both Leader and Worker might modify files simultaneously.
---

# Team Task Coordination

Prevents conflicts when Leader and Workers access the same task directory under `shared/tasks/`.

## The `.processing` Marker

Location: `shared/tasks/{task-id}/.processing`

```json
{
  "processor": "leader-name",
  "started_at": "2026-03-31T10:30:00Z",
  "expires_at": "2026-03-31T10:45:00Z"
}
```

Auto-expires after 15 minutes (default).

## Protocol

1. Refresh the task directory first
2. Check marker: `check-processing-marker.sh <task-id>`
   - Exit 0: safe to proceed
   - Exit 1: processing in progress, wait
3. Create marker: `create-processing-marker.sh <task-id> <your-name>`
4. Perform modifications
5. Remove marker: `remove-processing-marker.sh <task-id>`
6. Publish the task directory if you changed files

## Scripts

```bash
# Check if safe to modify
bash ./skills/team-task-coordination/scripts/check-processing-marker.sh <task-id>

# Create marker before modifying
bash ./skills/team-task-coordination/scripts/create-processing-marker.sh <task-id> <processor-name> [timeout-mins]

# Remove marker after done
bash ./skills/team-task-coordination/scripts/remove-processing-marker.sh <task-id>
```
