---
name: organization
description: Use before any Leader action that depends on current team topology, worker list, worker phase, runtime, room ID, Matrix ID, Team Admin, human identity, or lifecycle state. Always use this skill when assigning tasks, sending cross-room messages, recovering projects, handling heartbeat, waking/sleeping workers, or when any worker/room/identity value might be stale.
---

# Organization

Use this skill for current AgentTeams topology and runtime state.

## Source Of Truth

Use `hiclaw` CLI. Do not infer organization state from memory, old chat history, `SOUL.md`, or `AGENTS.md`.

Resolve the Team CR name before any team-scoped CLI query. The team name in `SOUL.md` may be the runtime/storage `teamName`, not the Kubernetes Team CR name accepted by `hiclaw --team`.

```bash
TEAM_CR="$(agt get workers "${AGENTTEAMS_WORKER_CR_NAME:-$AGENTTEAMS_WORKER_NAME}" -o json | jq -r '.team')"
agt get teams "$TEAM_CR" -o json
agt get workers --team "$TEAM_CR" -o json
agt worker status --team "$TEAM_CR"
```

Use the Team CR name for CLI filters and lifecycle commands. Use the returned `teamName` only as runtime/storage/display context. If `agt get workers --team <name>` returns no workers, re-check that you used the Team CR name from your own Worker metadata, not the runtime `teamName`.

## What To Read From CLI

- Team Room and Leader Room IDs
- Team Admin / human identity
- Worker names
- Worker full Matrix IDs
- Worker room IDs
- Worker phase and runtime state

Use the Team Room ID for normal task assignment notifications. Worker room IDs are for exceptional direct follow-up, not routine assignment.

## Lifecycle

Use lifecycle commands only after checking active project and task files.

```bash
agt worker ensure-ready --name <worker-name> --team "$TEAM_CR"
agt worker wake --name <worker-name> --team "$TEAM_CR"
agt worker sleep --name <worker-name> --team "$TEAM_CR"
```

If CLI output is missing a required room ID or Matrix ID, stop and report a metadata problem. Do not guess.
