# Team Lifecycle

## Team States

- **Active**: Leader and workers are running, team is operational
- **Degraded**: Some workers stopped or unavailable, Leader still running
- **Stopped**: All containers stopped (can be restarted)

## Adding a Worker to an Existing Team

1. Write SOUL.md for the new worker
2. Run `create-worker.sh --name <NEW_WORKER> --role worker --team <TEAM> --team-leader <LEADER>`
3. Invite new worker to Team Room
4. Add new worker to Leader's `groupAllowFrom`
5. Update `teams-registry.json` via `manage-teams-registry.sh --action add-worker`
6. Notify Leader to `file-sync`

## Removing a Worker from a Team

1. Stop the worker container
2. Remove worker from Team Room
3. Remove worker from Leader's `groupAllowFrom`
4. Update `teams-registry.json` via `manage-teams-registry.sh --action remove-worker`
5. Push updated Leader `openclaw.json` to MinIO
6. Notify Leader to `file-sync`

## Deleting a Team

Order matters — delete from inside out:

1. Stop all team worker containers
2. Stop Leader container
3. Remove Team Room (or leave it as archive)
4. Remove from `teams-registry.json`
5. Remove `team_id` from workers in `workers-registry.json`

## Task Delegation to Teams

When Manager receives a task matching a team's domain:

1. Use `manage-state.sh --action add-finite --delegated-to-team <TEAM>` to track
2. @mention the Team Leader in the Leader Room with the task
3. Team Leader handles decomposition and assignment internally
4. Manager only checks with Team Leader for progress (never team workers)
