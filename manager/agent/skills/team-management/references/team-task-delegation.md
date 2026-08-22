# Team Task Delegation

## When to Delegate to a Team

Delegate to a Team Leader when:
- The task matches the team's domain/expertise
- The task is complex enough to benefit from decomposition
- Multiple workers with different skills are needed

## Delegation Flow

```
Manager receives task from Admin
  ↓
Manager checks teams-registry.json for matching team
  ↓
Manager creates task: shared/tasks/{task-id}/
  - meta.json: assigned_to = leader name
  - spec.md: full task requirements
  ↓
Manager pushes to MinIO
  ↓
Manager adds to state.json:
  manage-state.sh --action add-finite \
    --task-id T --title TITLE \
    --assigned-to <LEADER> --room-id <LEADER_ROOM> \
    --delegated-to-team <TEAM>
  ↓
Manager @mentions Leader in Leader Room:
  "@leader:domain New task [task-id]: title.
   Pull spec: shared/tasks/{task-id}/spec.md
   Decompose and assign to your team. @mention me when complete."
```

## Monitoring Delegated Tasks

During heartbeat, for tasks with `delegated_to_team`:
- Only @mention the Team Leader for status updates
- Do NOT contact team workers directly
- Trust the Leader to manage internal coordination

## Completion Flow

```
Leader aggregates team results → writes result.md
  ↓
Leader pushes to MinIO
  ↓
Leader @mentions Manager in Leader Room:
  "@manager:domain Task {task-id} complete. Outcome: SUCCESS"
  ↓
Manager processes completion (same as regular worker flow)
```

## Key Rules

1. **Never bypass the Leader** — all communication with team workers goes through the Leader
2. **One task per delegation** — don't assign multiple unrelated tasks simultaneously
3. **Trust the Leader's decomposition** — don't micromanage sub-task assignment
4. **Escalation path** — if Leader reports BLOCKED, Manager escalates to Admin
