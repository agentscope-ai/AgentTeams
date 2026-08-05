# AgentTeams Codex CLI Local Runtime

This package provides one role-neutral Codex app-server core plus host-local
Worker and Manager bridges. It does not add a Controller runtime or copy Codex
OAuth files.

## Runtime snapshot

Use the existing `MemberRuntimeConfig` mapping; a complete non-secret example
is available at `examples/runtime.yaml`. The bridge consumes only the
non-secret identity, routing, and model fields; secrets are environment-only.

```yaml
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
team:
  name: demo-team
  teamRoomId: "!team:matrix.local"
  leaderMatrixUserId: "@leader:matrix.local"
member:
  name: codex-worker
  runtimeName: codex-worker
  role: remote-member
  runtime: codex-cli
  matrixUserId: "@codex-worker:matrix.local"
desired:
  model: {}
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
```

Set runtime secrets without putting them in the file:

```powershell
$env:AGENTTEAMS_MATRIX_URL = "http://localhost:8008"
$env:AGENTTEAMS_WORKER_MATRIX_TOKEN = "<Matrix access token>"
```

Then validate and run:

```powershell
python run.py doctor --runtime-config runtime.yaml --workspace C:\src\project
python run.py doctor --runtime-config runtime.yaml --workspace C:\src\project --skip-matrix
python run.py run --runtime-config runtime.yaml --workspace C:\src\project
```

Omit `desired.model.model` to use the model selected by the installed Codex
CLI. `--skip-matrix` validates the local CLI, login, workspace, and plugin
assets before Matrix credentials are available.

The bridge reacts only to joined-room `TASK_ASSIGNED: <task-id>` events that
mention its full Matrix user id. State under
`~/.agentteams/codex-worker/<member>/` contains only the Matrix sync cursor,
bounded event ids, and task-to-Codex-thread ids.

## Manager runner

The QwenPaw TeamHarness plugin can delegate Manager replies to the same Codex
core. Give the Manager process and host runner the same random capability
token, then enable the plugin middleware:

```powershell
$env:AGENTTEAMS_CODEX_MANAGER_ENABLED = "true"
$env:AGENTTEAMS_CODEX_BROKER_TOKEN = "<random capability token>"
python manager_run.py --broker-url http://127.0.0.1:8080 `
  --workspace C:\src\manager-workspace
```

The runner polls `/teamharness/codex/executions/lease`, preserves one Codex
thread per QwenPaw session, and returns completion to the originating reply.
Add `--mcp-server <teamharness>/mcp/server.py` when the host runner has the
Manager's TeamHarness environment. Manager executions use `read-only` and
`approvalPolicy=never`; TeamHarness mutations remain explicit MCP calls.

## Safety

- Codex runs with `workspace-write` and `approvalPolicy=never`.
- App-server permission escalation requests are denied.
- The Codex child receives an allowlisted OS/proxy/login environment. Matrix,
  storage, GitHub, and cloud credentials are not inherited.
- A loopback-only MCP capability proxy owns TeamHarness credentials. Codex gets
  a random short-lived capability token and only the role's approved MCP tools.
- Worker MCP exposes `health`, `filesync`, `artifact`, and `taskflow`; Manager
  additionally exposes coordination tools such as `message` and `projectflow`.
- `auth.json` is used only by the user's existing `CODEX_HOME`; it is never
  copied, mounted, logged, or packaged.
- Use a dedicated Git worktree when the source baseline must remain intact.
