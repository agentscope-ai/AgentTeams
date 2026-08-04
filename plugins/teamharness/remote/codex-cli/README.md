# AgentTeams Codex CLI Local Runtime

This package runs Codex CLI as a host-local TeamHarness remote member. It does
not create a Kubernetes Worker, add a CRD runtime, or copy Codex OAuth files.

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

## Safety

- Codex runs with `workspace-write` and `approvalPolicy=never`.
- App-server permission escalation requests are denied.
- The transient TeamHarness MCP registration is required, forces UTF-8 stdio,
  and exposes only `health`, `filesync`, `artifact`, and `taskflow`. Those four
  packaged tools are pre-approved; other MCP servers keep their own policy.
- Matrix and shared-storage credentials are forwarded by environment variable
  name. Their values are not written to the Codex command line or runtime file.
- `auth.json` is used only by the user's existing `CODEX_HOME`; it is never
  copied, mounted, logged, or packaged.
- Use a dedicated Git worktree when the source baseline must remain intact.
