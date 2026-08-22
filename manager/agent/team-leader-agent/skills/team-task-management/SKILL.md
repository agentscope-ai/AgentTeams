---
name: team-task-management
description: Use when you need to assign tasks to team workers, track team task progress, find available workers in your team, or manage team-state.json.
---

# Team Task Management

Manage tasks within your team. You are the Team Leader — decompose tasks from Manager into sub-tasks and assign to your team workers.

## Task Lifecycle

```
Manager assigns task to you
  ↓
Decompose into sub-tasks
  ↓
find-team-worker.sh → check availability
  ↓
Create sub-task files + assign via @mention
  ↓
manage-team-state.sh --action add-finite
  ↓
Worker completes → @mentions you
  ↓
manage-team-state.sh --action complete
  ↓
All done → aggregate results → report to Manager
```

## Key Scripts

```bash
# Find available team workers
bash ~/skills/team-task-management/scripts/find-team-worker.sh

# Add a sub-task
bash ~/skills/team-task-management/scripts/manage-team-state.sh \
  --action add-finite --task-id sub-01 --title "Implement auth" \
  --assigned-to alice --room-id '!room:domain'

# Mark sub-task complete
bash ~/skills/team-task-management/scripts/manage-team-state.sh \
  --action complete --task-id sub-01

# List all active team tasks
bash ~/skills/team-task-management/scripts/manage-team-state.sh --action list
```

## Sub-Task Directory Convention

```
shared/tasks/{parent-task-id}/sub-tasks/
├── sub-01/
│   ├── meta.json
│   ├── spec.md
│   └── result.md
├── sub-02/
│   ├── meta.json
│   ├── spec.md
│   └── result.md
```

## References

| Topic | File |
|-------|------|
| Task creation & assignment | `references/finite-tasks.md` |
| Worker selection logic | `references/worker-selection.md` |
| State management | `references/state-management.md` |
