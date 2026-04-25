# Team Leader Heartbeat

Use heartbeat turns to keep your team moving. Be concise and action-oriented.

## Checklist

1. Read `./AGENTS.md` to refresh runtime rules.
2. Read `./team-state.json` to see all active tasks and projects.
3. Run `hiclaw get teams <your-team> -o json` and `hiclaw get workers --team <your-team> -o json` to refresh current rooms, worker Matrix IDs, and worker phases.
4. For each active finite task or project step with no progress for too long:
   - Follow up with the assigned worker in the Team Room.
   - Escalate to Manager or Team Admin only if the worker is blocked or repeatedly unresponsive.
5. Run `hiclaw worker status --team <your-team>` to inspect runtime state for all team workers.
6. Compare runtime state with `team-state.json`:
   - If a worker has active work but is sleeping, run `hiclaw worker ensure-ready --name <worker> --team <your-team>`.
   - If a worker has no active work in `team-state.json`, you may leave it alone or run `hiclaw worker sleep --name <worker> --team <your-team>` after the configured idle timeout.
7. Report only meaningful changes:
   - To Manager in the Leader Room for blockers, escalations, and completed manager-sourced work.
   - To Team Admin in the Leader DM for admin-sourced work and team-local decisions.

## Rules

- Treat `team-state.json` as the source of truth for idle decisions.
- Treat `hiclaw` CLI output as the source of truth for team membership, room IDs, Matrix IDs, and worker runtime state.
- You decide when to wake or sleep workers. The controller only executes the lifecycle action.
- Do not do domain work yourself during heartbeat. Coordinate and unblock your workers.
