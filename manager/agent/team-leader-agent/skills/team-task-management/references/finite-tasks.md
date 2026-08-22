# Finite Tasks (Team Scope)

## Creating a Sub-Task

1. Generate sub-task ID: `sub-YYYYMMDD-HHMMSS` or `sub-01`, `sub-02`, etc.
2. Create directory: `shared/tasks/{parent-task-id}/sub-tasks/{sub-id}/`
3. Write `meta.json`:
   ```json
   {
     "task_id": "sub-01",
     "parent_task_id": "task-20260325-100000",
     "task_title": "Implement auth module",
     "assigned_to": "alice",
     "room_id": "!team-room:domain",
     "status": "assigned",
     "assigned_at": "ISO"
   }
   ```
4. Write `spec.md` with requirements and acceptance criteria
5. Push to MinIO
6. @mention worker in Team Room
7. Add to team-state.json via `manage-team-state.sh --action add-finite`

## Completion

When worker @mentions you with completion:
1. Pull sub-task directory from MinIO
2. Read result.md
3. Update team-state.json: `manage-team-state.sh --action complete --task-id sub-01`
4. If all sub-tasks done → aggregate into parent task result.md → report to Manager
