---
name: team-management
description: Use when admin requests creating a team, importing a team, managing team composition, adding/removing workers from a team, or delegating tasks to a Team Leader.
---

# Team Management

A Team consists of 1 Team Leader + N Workers. The Team Leader is a special Worker with management skills that handles task decomposition and assignment within the team. Manager delegates tasks to Team Leaders, not directly to team workers.

## Quick Create (2 steps)

```bash
# 1. Create team via hiclaw CLI
hiclaw create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --leader-model <MODEL> \
  --workers <w1>,<w2>

# 2. @mention the Leader in Leader Room to assign task
```

After creation, the Leader is online in the Leader Room (Manager + Global Admin + Leader). @mention the Leader there to delegate the task — the Leader will decompose it and coordinate with team workers in the Team Room.

> Full workflow: read `references/create-team.md`

If admin asks for CPU or memory requests/limits, use a YAML Team manifest with `leader.resources` and/or `workers[].resources`, then apply it with `hiclaw apply -f`. The simple `hiclaw create team` / `hiclaw update team` flags do not expose resource tuning. Changing member resources recreates the affected member container, so confirm the team is not mid-task.

## Gotchas

- **Team Leader is a Worker container** — same runtime, but with team-leader-agent skills instead of worker-agent skills
- **Team workers only talk to their Leader** — their groupAllowFrom has [Leader, Team Admin], NOT Manager
- **Manager only talks to Team Leader** — never @mention team workers directly
- **Team Room includes Team Admin** — it's Leader + Team Admin + all team workers (no Global Admin unless they are Team Admin)
- **Leader Room is standard 3-party** — Manager + Global Admin + Leader (same as regular worker room)
- **Leader DM is Team Admin ↔ Leader** — for team-level management
- **Team Admin defaults to Global Admin** — if `--team-admin` not specified
- **Delegated tasks use `--delegated-to-team`** — so heartbeat knows to check with Leader, not workers
- **Controller forces `runtime: copaw` for all team members** — omit runtime from team creation

## Operation Reference

| Admin wants to... | Read | Command |
|---|---|---|
| Create a new team | `references/create-team.md` | `hiclaw create team` |
| Understand team lifecycle | `references/team-lifecycle.md` | — |
| Delegate task to team | `references/team-task-delegation.md` | — |
| Add/remove worker from team | `references/team-lifecycle.md` | `hiclaw get team` |
| Delete a team's containers | `references/team-lifecycle.md` | `scripts/lifecycle-worker.sh` (per worker) |
