---
name: team-management
description: Use when admin requests creating a team, importing a team, managing team composition, adding/removing workers from a team, or delegating tasks to a Team Leader.
---

# Team Management

A Team consists of 1 Team Leader + N Workers. The Team Leader is a special Worker with management skills that handles task decomposition and assignment within the team. Manager delegates tasks to Team Leaders, not directly to team workers.

## ⚠️ Informational Queries — Read This First

When the admin asks about team structure, relationships, or status (e.g., "what's the relationship between Team X, Leader Y, and Worker Z?", "how are my teams organized?", "explain the team topology"):

1. **DO NOT create or modify teams.** Answer the question directly using `agt get teams -o json` and `agt get workers -o json` to gather facts.
2. **DO NOT interpret a question as a creation request.** Only create teams when the admin explicitly asks to create a new team ("create a team", "set up a team", "build a team").
3. **Check before creating.** Run `agt get teams -o json` first to confirm the target team name does not already exist. If the lookup fails, do not create the Team.

If the admin's intent is ambiguous, ask: "Do you want me to create a new team, or are you asking about the existing team structure?"

## Quick Create

```bash
# 1. Create each Worker CR with worker-management, then reference them
agt create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --workers <w1>,<w2>

# 2. @mention the Leader in Leader Room to assign task
```

**Pre-flight check before creation:**

```bash
# Verify no duplicate team exists
EXISTING=$(agt get teams -o json | jq -r '.teams[] | select(.name == "<team-name>") | .name')
if [ -n "$EXISTING" ]; then
  echo "Team already exists: $EXISTING. Skipping creation."
  # Report existing team info to admin instead of creating duplicate
fi
```

The Team command fails if a referenced Worker does not exist. Configure model, runtime, image, resources, identity, skills, MCP, channel policy, and lifecycle on each Worker CR. After Team reconciliation, @mention the Leader in the Leader Room; the Leader will coordinate referenced workers in the Team Room.

> Full workflow: read `references/create-team.md`

If admin asks for CPU or memory requests/limits, update `Worker.spec.resources`. Changing resources recreates that Worker's container, so confirm it is not mid-task.

## Gotchas

- **Team Leader is a Worker container** — same runtime, but with team-leader-agent skills instead of worker-agent skills
- **Team workers only talk to their Leader** — their groupAllowFrom has [Leader, Team Admin], NOT Manager
- **Manager only talks to Team Leader** — never @mention team workers directly
- **Team Room includes Team Admin** — it's Leader + Team Admin + all team workers (no Global Admin unless they are Team Admin)
- **Leader Room is standard 3-party** — Manager + Global Admin + Leader (same as regular worker room)
- **Leader DM is Team Admin ↔ Leader** — for team-level management
- **Team Admin defaults to Global Admin** — if `--team-admin` not specified
- **Delegated tasks use `--delegated-to-team`** — so heartbeat knows to check with Leader, not workers
- **Team never owns Worker runtime configuration or lifecycle** — update the referenced Worker CR directly
- **Never create duplicate teams** — always check `agt get teams -o json` before creating; informational queries about team structure do NOT warrant team creation

## Operation Reference

| Admin wants to... | Read | Command |
|---|---|---|
| Create a new team | `references/create-team.md` | `agt create team` |
| Understand team lifecycle | `references/team-lifecycle.md` | — |
| Delegate task to team | `references/team-task-delegation.md` | — |
| Add/remove worker from team | `references/team-lifecycle.md` | `agt get team` |
| Stop/delete a member Worker | `references/team-lifecycle.md` | `scripts/lifecycle-worker.sh` (per worker) |
