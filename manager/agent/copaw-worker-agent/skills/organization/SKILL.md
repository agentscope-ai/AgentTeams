---
name: organization
description: Use only when you need to look up team topology, worker phase, runtime state, or identity that is NOT available from the current message context. Do not use for standard task flows — the coordinator is the message sender, and the task room is in meta.json.room_id.
---

# Organization

Use this skill for current AgentTeams topology and runtime state.

## Source Of Truth

Use `hiclaw` CLI when available. Do not infer current state from memory, old chat history, or old task files.

Useful commands:

```bash
agt get workers "${AGENTTEAMS_WORKER_CR_NAME:-$AGENTTEAMS_WORKER_NAME}" -o json
TEAM_CR="$(agt get workers "${AGENTTEAMS_WORKER_CR_NAME:-$AGENTTEAMS_WORKER_NAME}" -o json | jq -r '.team')"
agt get workers --team "$TEAM_CR" -o json
```

Use the Team CR name from your own Worker metadata for team-scoped CLI filters. Do not use a runtime/storage `teamName` from prompts, task files, or old chat as `--team`. If team-scoped queries are denied, ask your coordinator instead of guessing.

## What To Use It For

- Confirm your coordinator's Matrix ID
- Confirm your team or standalone worker context
- Confirm room IDs when asked to reason about routing
- Check your own Worker phase/runtime if needed

Do not use your Worker profile room or private room as the delivery target for a task result. Task completion routing comes from `shared/tasks/{task-id}/meta.json.room_id`.

If required identity or room metadata is missing, ask your coordinator. Do not guess.
