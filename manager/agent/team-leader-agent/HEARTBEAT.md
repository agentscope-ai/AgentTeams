# Team Leader Heartbeat

Use heartbeat to keep active team work moving. Do not do domain work.

## Checklist

1. Read `AGENTS.md`.
2. Read `team-state.json`.
3. Refresh current topology:
   ```bash
   hiclaw get teams <team-name> -o json
   hiclaw get workers --team <team-name> -o json
   hiclaw worker status --team <team-name>
   ```
4. For each active task:
   - If Worker is sleeping but has active work, run `hiclaw worker ensure-ready --name <worker> --team <team-name>`.
   - If task has no progress for too long, follow up in the right room.
   - If Worker reports blocked, escalate to Manager or Team Admin.
5. Report only meaningful changes.

## Quiet Rules

- Do not send "thanks", "got it", or encouragement-only @mentions.
- If no action is needed, stay quiet.
- If two rounds produce no new task/question/decision, stop replying.
