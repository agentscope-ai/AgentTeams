---
name: team-management
description: Use when admin requests creating a team, importing a team, managing team composition, adding/removing workers from a team, or delegating tasks to a Team Leader.
---

# Team Management

A Team consists of 1 Team Leader + N Workers. The Team Leader is a special Worker with management skills that handles task decomposition and assignment within the team. Manager delegates tasks to Team Leaders, not directly to team workers.

## Quick Create (3 steps)

```bash
# 1. Write Leader SOUL.md (REQUIRED)
mkdir -p /root/hiclaw-fs/agents/<LEADER_NAME>
# Write SOUL.md with team management focus (see references/create-team.md)

# 2. Write Worker SOUL.md(s) (REQUIRED for each worker)
mkdir -p /root/hiclaw-fs/agents/<WORKER_NAME>
# Write SOUL.md with domain-specific role

# 3. Run create-team script
bash /opt/hiclaw/agent/skills/team-management/scripts/create-team.sh \
  --name <TEAM_NAME> --leader <LEADER_NAME> --workers <w1>,<w2> \
  [--team-admin <HUMAN_NAME>] [--team-admin-matrix-id <@user:domain>] \
  [--worker-skills <s1,s2>:<s3,s4>] [--worker-mcp-servers <m1>:<m2>]
```

> Full workflow: read `references/create-team.md`

## Gotchas

- **Team Leader is a Worker container** — same runtime, but with team-leader-agent skills instead of worker-agent skills
- **Team workers only talk to their Leader** — their groupAllowFrom has [Leader, Team Admin], NOT Manager
- **Manager only talks to Team Leader** — never @mention team workers directly
- **Team Room includes Team Admin** — it's Leader + Team Admin + all team workers (no Global Admin unless they are Team Admin)
- **Leader Room is standard 3-party** — Manager + Global Admin + Leader (same as regular worker room)
- **Leader DM is Team Admin ↔ Leader** — for team-level management
- **Team Admin defaults to Global Admin** — if `--team-admin` not specified
- **Delegated tasks use `--delegated-to-team`** — so heartbeat knows to check with Leader, not workers

## Operation Reference

| Admin wants to... | Read | Key script |
|---|---|---|
| Create a new team | `references/create-team.md` | `scripts/create-team.sh` |
| Understand team lifecycle | `references/team-lifecycle.md` | — |
| Delegate task to team | `references/team-task-delegation.md` | — |
| Add/remove worker from team | `references/team-lifecycle.md` | `scripts/manage-teams-registry.sh` |
