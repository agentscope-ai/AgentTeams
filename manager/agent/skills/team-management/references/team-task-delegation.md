# Team Task Delegation

## When to Delegate to a Team

Delegate to a Team Leader when:
- The task semantically matches the team's name, description, Leader, or Worker roster
- The task is complex enough to benefit from decomposition
- Multiple workers with different skills are needed

Team matching is a Manager-side judgement based on the current Team data. The
Team registry and Team API do not define structured team-level
matching/filtering fields such as `domain`, `expertise`, or `capabilities`.
Worker-level `skills` can describe individual members, but Manager delegation is
not backed by a structured Team filter.

## Delegation Flow

```
Manager receives task from Admin
  ↓
Manager checks team info via `agt get team <TEAM_NAME>` and chooses a matching team
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

## Leader's Internal Handling

The Team Leader now supports two modes for handling delegated tasks:

### Simple Task Mode
For straightforward tasks that a single team worker can complete:
- Leader assigns directly to a worker
- Sub-task tracked in `teams/{team}/tasks/`
- Result aggregated and written back to `shared/tasks/{parent-task-id}/result.md`

### Project Mode (DAG)
For complex tasks requiring multiple workers with dependencies:
- Leader creates a team project with DAG-based task plan
- Tasks are orchestrated with parallel/serial execution based on dependencies
- `resolve-dag.sh` automatically identifies which tasks can run in parallel
- Result aggregated and written back to `shared/tasks/{parent-task-id}/result.md`

The Leader decides which mode to use based on task complexity. Manager does not need to specify the mode.

## Team Admin Direct Tasks

Team Admin can also assign tasks directly to the Team Leader via Leader DM. These tasks:
- Are handled entirely within the team's isolated storage (`teams/{team}/`)
- Do not have a parent task in `shared/tasks/`
- Completion is reported to Team Admin in Leader DM (not to Manager)
- Manager is not involved unless the Leader escalates a blocker

## Monitoring Delegated Tasks

During heartbeat, for tasks with `delegated_to_team`:
- Only @mention the Team Leader for status updates
- Do NOT contact team workers directly
- Trust the Leader to manage internal coordination

## Completion Flow

```
Leader aggregates team results → writes result.md
  ↓
Leader pushes to MinIO (shared/tasks/{parent-task-id}/)
  ↓
Leader @mentions Manager in Leader Room:
  "@manager:domain Task {task-id} complete. Outcome: SUCCESS"
  ↓
Manager processes completion (same as regular worker flow)
```

## Key Rules

1. **Never bypass the Leader** — all communication with team workers goes through the Leader
2. **One task per delegation** — don't assign multiple unrelated tasks simultaneously
3. **Trust the Leader's decomposition** — don't micromanage sub-task assignment or mode selection
4. **Escalation path** — if Leader reports BLOCKED, Manager escalates to Admin
5. **Team Admin tasks are independent** — Manager does not track or interfere with Team Admin-initiated tasks
