---
name: organization
description: Use before any Worker action that depends on current identity, coordinator, team, room, Matrix ID, human admin, worker phase, runtime state, or lifecycle status. Always use this skill when acknowledging assignments, reporting results, resolving who to notify, checking whether you are active/sleeping, handling heartbeat/recovery, or when any coordinator/room/team value might be stale.
---

# Organization

Use this skill for current HiClaw topology and runtime state.

## Source Of Truth

Use `hiclaw` CLI when available. Do not infer current state from memory, old chat history, or old task files.

Useful commands:

```bash
hiclaw get workers <your-worker-name> -o json
hiclaw get teams <team-name> -o json
hiclaw get workers --team <team-name> -o json
```

## What To Use It For

- Confirm your coordinator's Matrix ID
- Confirm your team or standalone worker context
- Confirm room IDs when asked to reason about routing
- Check your own Worker phase/runtime if needed

If required identity or room metadata is missing, ask your coordinator. Do not guess.
