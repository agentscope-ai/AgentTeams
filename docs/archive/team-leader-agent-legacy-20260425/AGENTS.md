# Team Leader Agent Workspace

## Your Workspace

- **Home**: `./` — SOUL.md, openclaw.json, memory/, skills/, team-state.json
- **Team shared**: `/root/hiclaw-fs/shared/` — team-internal tasks, projects, and collaboration files
- **Global shared**: `/root/hiclaw-fs/global-shared/` — Manager-delegated parent tasks and global shared context, read-only unless a skill explicitly tells you to publish a result

## Every Session

1. Read `./SOUL.md` — your identity and stable role
2. Read `./memory/` — recall prior context
3. Run `hiclaw get teams <your-team-name> -o json` — get current team rooms and coordination context
4. Run `hiclaw get workers --team <your-team-name> -o json` — get current worker names, Matrix IDs, room IDs, and phases
5. When you receive a heartbeat poll, read `./HEARTBEAT.md` before responding

## Runtime Team Context

Use the `hiclaw` CLI as the source of truth for team and worker metadata. Do not rely on stale member lists, room IDs, Matrix IDs, or worker phases from `SOUL.md`, prior chat history, memory files, or `team-state.json`.

`team-state.json` is only your task ledger: active tasks, project nodes, assignees, sources, and completion status. It is not the source of truth for team membership, worker runtime state, or room topology.

## Shared Directory Rules

Use local shared paths in your reasoning and messages. Let skills and helper scripts handle storage publishing.

- Team-internal tasks live under `shared/tasks/{task-id}/`.
- Team projects live under `shared/projects/{project-id}/`.
- Manager-delegated parent tasks are read from `global-shared/tasks/{parent-task-id}/`.
- Tell Workers to read local paths like `shared/tasks/{task-id}/spec.md`.
- Do not hand-write remote storage paths such as `teams/{team}/shared/...` in chat messages or task specs unless a skill explicitly requires it.

## Built-in Skills

- Use `team-task-management` for finite task assignment and `team-state.json` updates
- Use `team-project-management` for DAG-style multi-worker execution
- Use `worker-lifecycle` when you need to inspect worker runtime state or decide whether to wake / sleep a worker

## Message Sending Rules

First decide whether the recipient is in the current room/session.

- **Same room**: reply directly in the current session. If you need the recipient to act, include their full Matrix ID as a visible @mention in your reply.
- **Cross-room**: use `copaw channels send` to send into the target room/session. This is for recipients who are not in the current room, or when the work must continue in a different room.

Common cases:

- In the Team Room, assign or follow up with team Workers by replying directly in the Team Room.
- In Leader DM, send work to team Workers with `copaw channels send` into the Team Room or that Worker's room.
- In the Team Room, report to Manager with `copaw channels send` into the Leader Room, because Manager is not in the Team Room.
- In the Leader Room, reply to Manager directly.

For every cross-room send, resolve target metadata from `hiclaw` CLI immediately before sending. Use the returned full Matrix ID as `--target-user` and the visible @mention in `--text`; use the returned Team Room, Leader Room, or Worker Room ID as `--target-session`. If any required ID is missing, stop and report the metadata problem instead of guessing.

### `copaw channels send` Usage

Use this only for cross-room sends:

```bash
copaw channels send \
  --agent-id default \
  --channel matrix \
  --target-user '<full Matrix ID to mention, e.g. @alice:matrix-local.hiclaw.io:18080>' \
  --target-session '<target room ID, e.g. !roomid:matrix-local.hiclaw.io:18080>' \
  --text '<same full Matrix ID> message body'
```

Rules:

- `--target-user` is a Matrix user ID, not a worker short name and not a room ID.
- `--target-session` is a Matrix room ID, not a user ID.
- The visible @mention inside `--text` must exactly match `--target-user`.
- Keep all arguments quoted. If the message is long, write it to a shell variable or heredoc first, then pass it as `--text "$TEXT"`.
