---
name: worker-lifecycle
description: Use when you need to inspect team worker runtime state or decide whether to wake, sleep, or ensure-ready a worker. Team Leaders must use team-state.json as the source of truth for idle decisions.
---

# Worker Lifecycle

Use this skill when you need to manage worker runtime state inside your team.

## Principles

- You may manage only workers in your own team.
- Use `team-state.json` to decide whether a worker is idle.
- Use `hiclaw worker status --team <team>` to inspect runtime state before taking action.
- Prefer `ensure-ready` when a worker has active work and might be sleeping.

## Commands

```bash
# List runtime state for all workers in your team
hiclaw worker status --team <team-name>

# Wake a worker that should resume work
hiclaw worker wake --name <worker-name> --team <team-name>

# Ensure a worker is ready before or after task assignment
hiclaw worker ensure-ready --name <worker-name> --team <team-name>

# Sleep a worker only after team-state.json shows no active work for that worker
hiclaw worker sleep --name <worker-name> --team <team-name>
```

## Decision Guide

1. Read `./team-state.json`.
2. Check whether the worker still owns any active task or in-progress project node.
3. Run `hiclaw worker status --team <team-name>`.
4. Choose the action:
   - Active task + worker not running: `ensure-ready`
   - Active task + worker already running: no lifecycle action needed
   - No active task + idle timeout reached: `sleep`
   - New task assigned to a sleeping worker: `wake` or `ensure-ready`

## Escalation

- If lifecycle commands fail repeatedly, report the failure to Manager.
- If the worker is idle but you are unsure whether it should be stopped, stay conservative and leave it running.
