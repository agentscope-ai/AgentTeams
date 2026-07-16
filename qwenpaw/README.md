# AgentTeams QwenPaw Worker Runtime

This directory contains the AgentTeams managed worker runtime for QwenPaw.

QwenPaw is integrated as a worker runtime through two pieces.

The worker daemon owns the managed-runtime lifecycle. The plugin adapter owns
the QwenPaw-specific mapping of TeamHarness assets and runtime wrappers. Keeping these two
pieces separate is the main boundary of this integration.

## 1. Worker Daemon

The worker daemon is the process started by the QwenPaw worker container. Its
job is to keep local QwenPaw runtime files aligned with the
controller-projected desired state.

### 1.1 Lifecycle

- Start from AgentTeams environment variables and worker arguments.
- Prepare the QwenPaw working directory and default workspace.
- Prepare image-bundled TeamHarness and WorkerFlow QwenPaw plugin adapters.
- Start the QwenPaw app process.
- Stop background tasks and the QwenPaw process on shutdown.

### 1.2 Storage Sync

- Restore worker and shared files from object storage on startup.
- Persist eligible local runtime changes back to object storage.
- Exclude credentials, shared projections, logs, tool results,
  media, file store, and other runtime cache paths from background push.

### 1.3 Desired-State Apply

- Pull and read `agents/{memberName}/runtime/runtime.yaml` from object
  storage.
- Run the desired-state apply loop every 5 seconds.
- Apply model, MCP, channel, TeamHarness prompt asset, and AgentSpec package changes from
  `runtime.yaml`.
- Apply `desired.model` to the active QwenPaw agent model.
- Apply `desired.mcpServers` to `config/mcporter.json` using the mcporter JSON
  shape, not as QwenPaw MCP clients.
- Apply `desired.channelPolicy` to QwenPaw Matrix access control.
- Apply `desired.channels.dingtalk` to the active QwenPaw DingTalk channel.
  When `streaming_enabled` is true, the worker requires a pre-created
  DingTalk streaming card template id and switches the current DingTalk runtime
  config to card streaming mode.
- Apply AgentSpec package changes inside the same desired-state loop, without
  restarting the pod.
- Apply AgentSpec package `config/` files to the active QwenPaw workspace root
  and package `skills/` to workspace skills, then reconcile QwenPaw `skill.json`.

`desired.mcpServers` and package `mcp.json` are intentionally separate:

- `desired.mcpServers` is controller-projected runtime config and updates
  `config/mcporter.json` for the `mcporter` CLI.
- Package `mcp.json` configures QwenPaw-native MCP clients. Its canonical shape
  matches mcporter config: `{"mcpServers": {"name": {...}}}`. Legacy package
  shapes (`{"clients": {...}}`, `{"mcp": {"clients": {...}}}`, and top-level
  server maps) are still read for compatibility.
  See `../docs/zh-cn/qwenpaw-mcp-json.md` for the user-facing package authoring
  guide.

### 1.4 Heartbeat

- Maintain local `heartbeat.json` for QwenPaw process/API reachability.
- Use QwenPaw's native `/api/version` endpoint as the process health probe.
- Report readiness once to `POST /api/v1/workers/{name}/ready` after QwenPaw
  becomes reachable.
- Report periodic heartbeat to `POST /api/v1/workers/{name}/heartbeat`.
- Use QwenPaw's `GET /api/agents/default/agent-status` runtime status API for
  `lastActiveAt`: running agents report the current heartbeat time, idle agents
  report the latest `last_finish_at` / `last_run_at` value.
- Discover controller reporting config from `AGENTTEAMS_CONTROLLER_URL`,
  `AGENTTEAMS_AUTH_TOKEN` or `AGENTTEAMS_AUTH_TOKEN_FILE`.
- Keep controller reporting as a bypass path: report failures are logged but do
  not block storage sync, desired-state apply, or QwenPaw runtime loops.

### 1.5 Matrix Channel Overlay

- Apply Matrix channel configuration and access control from `runtime.yaml`
  through the desired-state update loop.
- Keep runtime behavior fixes in the QwenPaw Matrix overlay, not in TeamHarness
  hooks.
- The overlay preserves QwenPaw Matrix support while adding AgentTeams startup
  readiness, first-sync stability, invite auto-join, and visible Matrix mention
  compatibility.

The daemon does not implement TeamHarness collaboration semantics. It triggers
runtime apply and storage persistence, then delegates TeamHarness-specific
QwenPaw integration to the adapter.

### 1.6 DingTalk Streaming Card Mode

The worker consumes `desired.channels.dingtalk` from `runtime.yaml`. When
`streaming_enabled: true`, the worker requires `client_id`, `client_secret`,
`robot_code`, and `card_template_id`. The card template must be created and
published in DingTalk Open Platform before the runtime config is applied.

Enabling DingTalk streaming changes the current runtime card configuration to
the configured streaming template id. Existing DingTalk card templates are not
deleted from DingTalk, but the runtime stops using the previous custom template
while streaming remains enabled. If streaming is later disabled and a custom
card is needed again, configure that card template explicitly.

`filter_thinking` and `filter_tool_messages` remain independent of the
streaming switch: streaming controls the card transport, while those filters
control whether reasoning and tool progress are shown by the underlying
QwenPaw DingTalk channel.

## 2. Plugin Adapter

The default plugin adapters live under `plugins/teamharness/adapters/qwenpaw/`
and `plugins/workerflow/adapters/qwenpaw/`. They are installed into an
image-local QwenPaw working directory during Docker build, then prepared in the
runtime QwenPaw working directory on startup and `runtime.yaml` reapply.

### 2.1 Plugin Package

- Package TeamHarness prompts, skills, MCP assets, and WorkerFlow assets into
  QwenPaw plugins.
- Install the packages through `qwenpaw plugin install --force` during Docker
  image build.
- Keep the QwenPaw plugin ids as `teamharness` and `workerflow`.

### 2.2 TeamHarness Assets

- Merge TeamHarness role prompts into workspace `TEAMS.md`.
- Register TeamHarness skills for QwenPaw.
- Register the TeamHarness MCP server for QwenPaw.

### 2.3 Runtime Wrappers

- Install `TEAMS.md` into each QwenPaw agent workspace and include it in the
  prompt file list.
- Redact sensitive tool output through the QwenPaw adapter sanitizer.
- Keep the sanitizer adapter-private. TeamHarness base plugin does not define
  runtime-neutral top-level hooks.

## Image Integration Tests

The image integration tests live under `qwenpaw/tests/integration/`. They use a
real QwenPaw worker image and a real MinIO container, but they do not call a
paid model API unless the real-model test is explicitly selected.

Enable the image tests with:

```bash
AGENTTEAMS_QWENPAW_IMAGE_E2E=1 qwenpaw/tests/integration/test-worker-daemon.sh
AGENTTEAMS_QWENPAW_IMAGE_E2E=1 qwenpaw/tests/integration/test-sync-minio.sh
AGENTTEAMS_QWENPAW_IMAGE_E2E=1 qwenpaw/tests/integration/test-update-runtime-config.sh
```

Set `AGENTTEAMS_QWENPAW_IMAGE=agentteams/qwenpaw-worker:<tag>` to reuse an existing
local image. Without that override, the scripts build a fresh image.

- `test-worker-daemon.sh` verifies image startup, entrypoint wiring, QwenPaw API
  readiness, TeamHarness adapter health, and local heartbeat readiness.
- `test-sync-minio.sh` verifies startup mirror and background push boundaries
  against MinIO.
- `test-update-runtime-config.sh` verifies `runtime.yaml` hot apply for model,
  MCP, Matrix channel policy, and AgentSpec package changes without restarting
  the worker container.

### Real Image And Model Test

`qwenpaw/tests/integration/test-real-image-model-api.sh` is the narrow
end-to-end tracer bullet for this runtime. It builds the QwenPaw worker image,
starts a real MinIO-backed worker container, writes controller-style
`runtime.yaml`, applies the model provider through the worker update loop, and
calls a real model through QwenPaw.

It is opt-in because it builds Docker images and calls a paid model API:

```bash
AGENTTEAMS_QWENPAW_REAL_MODEL_E2E=1 qwenpaw/tests/integration/test-real-image-model-api.sh
```

By default the test reads `AGENTTEAMS_LLM_API_KEY`, `AGENTTEAMS_OPENAI_BASE_URL`, and
`AGENTTEAMS_DEFAULT_MODEL` from `${AGENTTEAMS_ENV_FILE:-$HOME/agentteams-manager.env}`.
`AGENTTEAMS_QWENPAW_REAL_MODEL_API_KEY`,
`AGENTTEAMS_QWENPAW_REAL_MODEL_BASE_URL`, and `AGENTTEAMS_QWENPAW_REAL_MODEL` can be
set only when a test run needs an explicit override.
