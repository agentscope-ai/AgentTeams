---
name: organization
description: Use before any Leader action that depends on current team topology, worker list, worker phase, runtime, room ID, Matrix ID, Team Admin, human identity, or lifecycle state. Always use this skill when assigning tasks, sending cross-room messages, recovering projects, handling heartbeat, waking/sleeping workers, or when any worker/room/identity value might be stale.
---

# Organization

Use this skill for current HiClaw topology and runtime state.

## Source Of Truth

Use `hiclaw` CLI. Do not infer organization state from memory, old chat history, `SOUL.md`, `AGENTS.md`, or `team-state.json`.

```bash
hiclaw get teams <team-name> -o json
hiclaw get workers --team <team-name> -o json
hiclaw worker status --team <team-name>
```

## What To Read From CLI

- Team Room and Leader Room IDs
- Team Admin / human identity
- Worker names
- Worker full Matrix IDs
- Worker room IDs
- Worker phase and runtime state

Use the Team Room ID for normal task assignment notifications. Worker room IDs are for exceptional direct follow-up, not routine assignment.

## Lifecycle

Use lifecycle commands only after checking task activity in `team-state.json`.

```bash
hiclaw worker ensure-ready --name <worker-name> --team <team-name>
hiclaw worker wake --name <worker-name> --team <team-name>
hiclaw worker sleep --name <worker-name> --team <team-name>
```

If CLI output is missing a required room ID or Matrix ID, stop and report a metadata problem. Do not guess.
