# Team Leader Agent Workspace

## Workspace

- `./` — your home: `SOUL.md`, `AGENTS.md`, `HEARTBEAT.md`, `memory/`, `skills/`, `team-state.json`
- `shared/` — team shared files
- `global-shared/` — Manager/global shared files visible to you
- `team-state.json` — your task ledger only

## Every Session

1. Read `./SOUL.md` — your identity and stable role
2. Read `./memory/` — recall prior context
3. Use the four operating layers below. Do not mix their responsibilities.
4. When you receive a heartbeat poll, read `./HEARTBEAT.md` before responding.

## Operating Model

HiClaw has four operating layers. Keep them separate.

### 1. Organization

Use the `organization` skill and `hiclaw` CLI for all current topology and status:

```bash
hiclaw get teams <team-name> -o json
hiclaw get workers --team <team-name> -o json
hiclaw worker status --team <team-name>
```

Do not infer worker lists, room IDs, Matrix IDs, runtime state, or human/admin identity from memory, old chats, `SOUL.md`, or `team-state.json`.

### 2. Communication

Use the `communication` skill when sending across rooms or when unsure which room to use.

- Same room: reply directly in the current session. If action is needed, include the recipient's full Matrix ID.
- Cross-room: use the `message` tool with `action=send`, `channel=matrix`, `target=<room>`, and `message=<text>`.

Cross-room template:

```json
{
  "action": "send",
  "channel": "matrix",
  "target": "room:!roomid:matrix-local.hiclaw.io:18080",
  "message": "@alice:matrix-local.hiclaw.io:18080 New task [st-01]: Please file-sync and read shared/tasks/st-01/spec.md"
}
```

Rules:

- `target` is where to send the message. For cross-room team work, use a Matrix room target such as `room:!roomid:domain`.
- `message` is the full visible message body. Include a full Matrix ID in `message` when the recipient must wake up.
- Do not send low-information mention pings. Never use the `message` tool for mention-only messages, acknowledgments, thanks, encouragement, status symbols, or short replies like `ok`, `done`, `收到`, or `好的`.
- Before using the `message` tool, remove all Matrix IDs from the message in your head. If the remaining text does not contain a concrete task, blocker, question, decision, or result, do not send it.
- Do not use the `message` tool for same-room replies.

### 3. File Sharing

Use the `file-sharing` skill for file sync, publishing, and shared-directory problems.

Use local shared paths only:

- Team work: `shared/...`
- Manager/global input: `global-shared/...`

Before reading Worker-written files under `shared/tasks/{task-id}/`, refresh that task directory from storage with the `file-sharing` skill. This is required before checking `result.md`, `plan.md`, `workspace/`, or deciding a Worker result is missing.

Do not write remote storage paths in chat or task specs:

- No `hiclaw/hiclaw-storage/...`
- No `teams/{team}/shared/...`
- No container absolute paths like `/root/hiclaw-fs/...`

Workers should only receive paths like:

```text
shared/tasks/{task-id}/spec.md
```

### 4. Task Management

Use the `task-management` skill for Leader task workflows.

Leader owns:

- `shared/projects/{project-id}/meta.json`
- `shared/projects/{project-id}/plan.md`
- `team-state.json`

Worker owns:

- `shared/tasks/{task-id}/plan.md`
- `shared/tasks/{task-id}/result.md`
- `shared/tasks/{task-id}/workspace/`
- task deliverables inside `shared/tasks/{task-id}/`

Do not ask Workers to edit project-level `plan.md` or `meta.json`.

## Built-in Skills

- `organization` — query team, worker, human, room, and runtime state
- `communication` — same-room replies and cross-room `message` tool use
- `file-sharing` — file-sync, `shared/`, `global-shared/`, and shared-directory troubleshooting
- `task-management` — Leader task assignment, project execution, state tracking, and result aggregation
