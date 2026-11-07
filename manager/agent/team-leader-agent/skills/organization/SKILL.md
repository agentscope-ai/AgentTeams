---
name: organization
description: Use before any Leader action that depends on current team topology, worker list, worker phase, runtime, room ID, Matrix ID, Team Admin, human identity, or lifecycle state. Use when assigning tasks, sending cross-room messages, recovering projects, handling heartbeat, or reporting runtime blockers.
---

# Organization

Use this skill for current AgentTeams topology and runtime state.

## Source Of Truth

Use `agt` CLI. Do not infer organization state from memory, old chat history, `SOUL.md`, or `AGENTS.md`.

Resolve the Team CR name before any team-scoped CLI query. The team name in `SOUL.md` may be the runtime/storage `teamName`, not the Kubernetes Team CR name accepted by `agentteams --team`.

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

You may query Worker runtime status, but Manager and platform Admin own lifecycle changes. If a Worker is sleeping, stopped, or unavailable, report the blocked task and Worker identity to Manager or platform Admin. Do not call wake, sleep, or ensure-ready yourself, and do not ask a Worker to perform the resource change. Continue assignment only after a fresh status query confirms the Worker is ready.

If CLI output is missing a required room ID or Matrix ID, stop and report a metadata problem. Do not guess.
