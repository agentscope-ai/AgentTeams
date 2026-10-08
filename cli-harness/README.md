# AgentTeams CLI-harness Worker

One worker image that turns headless CLI coding agents into AgentTeams
Worker runtimes. AgentTeams provides everything a CLI lacks — Matrix
transport, MinIO workspace sync, provider credentials — and the CLI acts as
the agent loop.

## Supported runtimes

| Runtime       | CLI binary | npm package                    | Headless call |
|---------------|------------|--------------------------------|---------------|
| `atomcode`    | `atomcode` | varies by release              | `atomcode -p <task>` |
| `codex`       | `codex`    | `@openai/codex`                | `codex exec <task>` |
| `claude-code` | `claude`   | `@anthropic-ai/claude-code`    | `claude -p <task>` |
| `kimi-code`   | `kimi`     | `@moonshot-ai/kimi-code`       | `kimi -p <task>` |
| `pi`          | `pi`       | `@mariozechner/pi-coding-agent`| `pi -p <task>` |

DeepSeek Harness is served by the dedicated upstream `deepseek-harness` runtime
(its own image and release cadence), so it is deliberately a controller-level
runtime here. The `dsh` adapter still ships in the image registry for
`AGENTTEAMS_CLI_ADAPTER=dsh` on custom images.

All controller-level runtimes share this image. The controller injects
`AGENTTEAMS_WORKER_RUNTIME` into the container; the Python worker selects the
matching adapter from `AGENTTEAMS_WORKER_RUNTIME` (or `AGENTTEAMS_CLI_ADAPTER`
for backends that cannot inject env, e.g. SandboxClaim).

## Architecture

```
Matrix room ──► cli-harness-worker (Python)
                 ├─ sync.py     MinIO mirror (agents/<name>/ ⇄ workspace)
                 ├─ bridge.py   openclaw.json → provider env + prompt context
                 ├─ matrix.py   receive tasks / send replies (matrix-nio)
                 └─ adapters/   one adapter per CLI (headless subprocess)
```

Message contract:

- Own room (`AGENTTEAMS_WORKER_ROOM_ID`): every non-self message is a task.
- Other rooms (team rooms): only @mentions are tasks.
- Each task runs the CLI once in headless mode inside the workspace
  (`HOME`), output is chunked and sent back to the room.

## Environment variables

| Variable | Purpose |
|----------|---------|
| `AGENTTEAMS_WORKER_RUNTIME` | adapter selection (injected by controller) |
| `AGENTTEAMS_CLI_ADAPTER` | adapter override / fallback |
| `AGENTTEAMS_CLI_MODEL` | model override passed as `--model` |
| `AGENTTEAMS_CLI_EXTRA_ARGS` | extra argv appended to every CLI run (flag-drift escape hatch) |

## Build

```bash
make build-cli-harness-worker
# local tag: agentteams/cli-harness-worker:$(VERSION)
```

## Development

```bash
pip install -e .
pip install pytest
python -m pytest tests/ -q
```
