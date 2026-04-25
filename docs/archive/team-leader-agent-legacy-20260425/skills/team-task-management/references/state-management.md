# State Management (Team Scope)

## team-state.json

Location: `./team-state.json`

Schema (same as Manager's state.json):
```json
{
  "team_id": "alpha-team",
  "active_tasks": [
    {
      "task_id": "sub-01",
      "title": "Implement auth",
      "type": "finite",
      "assigned_to": "alice",
      "room_id": "!room:domain"
    }
  ],
  "updated_at": "ISO"
}
```

## Operations

```bash
# Initialize
manage-team-state.sh --action init

# Add sub-task
manage-team-state.sh --action add-finite \
  --task-id sub-01 --title "Implement auth" \
  --assigned-to alice --room-id '!room:domain'

# Complete sub-task
manage-team-state.sh --action complete --task-id sub-01

# List active
manage-team-state.sh --action list
```

## Rules

- Always use `manage-team-state.sh` — never edit JSON manually
- Script handles atomicity (tmp+mv pattern)
- Duplicate adds are silently skipped (idempotent)
