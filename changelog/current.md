# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**What's New**

- **Codex CLI remote Worker adapter**: Add a host-local TeamHarness remote-member bridge that drives `codex app-server` with an existing ChatGPT OAuth login, preserves task-scoped Codex threads, denies workspace permission expansion, and reuses TeamHarness Matrix/task/file contracts without adding a new controller-managed runtime.
- **Shared Codex execution adapter**: Reuse one host-local `codex app-server` core for Worker and opt-in QwenPaw Manager roles, add authenticated Manager lease/completion endpoints and session recovery, isolate AgentTeams credentials from the Codex child, route role-scoped TeamHarness tools through a loopback capability proxy, and let Managers delegate, check, and cancel tasks with Worker-compatible Matrix notifications.
- **QwenPaw 2.0 runtime unification**: Migrate the Manager container from copaw 1.0.2 to QwenPaw 2.0.1 on a single venv, register projectflow/taskflow/message/filesync tools through a QwenPaw plugin instead of monkey-patching CoPawAgent, replace the physical Matrix channel overlay with the QwenPaw plugin system, read Matrix credentials directly from agent.json so the manager tools work without importing copaw at runtime, align CMS observability packages and env vars with the Worker image, inject session-file privacy policy into prompt files, set approval_level=AUTO in the agent template, bridge YOLO mode to Qwenpaw approval_level=OFF, disable the built-in QA Agent, replace start-copaw-manager.sh with start-qwenpaw-manager.sh, add explicit qwenpaw Manager and Worker runtime values alongside copaw while keeping user-facing installer defaults and image pulls on CoPaw until the QwenPaw release, make task assignment state and Matrix notification atomic with room membership validation and m.mentions delivery, preserve m.mentions metadata in streamed/edit events, make the TeamHarness MCP `delegate_task` path atomic too (validate assignee room membership — strictly `join`, not `invite` — prepare → stable-txn notification → commit assigned + event_id; the initial file publish gates the notification and the assigned/eventId commit gates success, both returning a retryable failure so an idempotent retry finishes the sync instead of reporting success with stale shared storage), and migrate a legacy Worker `.copaw` working dir to `.qwenpaw` on the qwenpaw_worker startup path only (idempotent; the migration follows the target runtime — an explicitly configured copaw Worker keeps `.copaw` and never migrates before a switch).
- **Custom model capability overrides**: `AGENTTEAMS_MODEL_VISION` and `AGENTTEAMS_MODEL_REASONING` env vars let deployments override vision and reasoning capabilities for custom models not in the built-in presets table (e.g. local multimodal models like `qwen3.6-27b-fp8`).

**Bug Fixes**

- **CoPaw Team assignment handoff**: `taskflow(delegate_task)` sends the Worker assignment automatically with `m.mentions` in the Team Room (atomic pending → prepared → assigned state, stable Matrix txn_id for idempotent retries, normalize Worker aliases from the Team roster, refresh Controller-managed runtime context every minute, and reroute assignment replies from non-Team rooms to the Team Room). ([#1120](https://github.com/agentscope-ai/AgentTeams/pull/1120), [#1095](https://github.com/agentscope-ai/AgentTeams/pull/1095))
- **Docker Worker ServiceAccount token rotation**: Project short-lived tokens into per-Worker Docker volumes, refresh the token file atomically without recreating running Workers, and remove the credential volume with the Worker.
- **Worker port exposure CLI**: Encode `--expose` values as numeric ports and reject invalid or out-of-range inputs before create, update, or apply requests reach the Controller.
- **QwenPaw MCP policy startup convergence**: Persist built-in plugin MCP policies before runtime desired-state reloads so a replacement QwenPaw workspace cannot retain the pre-policy interactive approval handler.
- **QwenPaw Team policy and runtime-aware acceptance**: Merge Team and Worker channel-policy overrides into QwenPaw `runtime.yaml`, wait for public plugin/API state before integration assertions, and verify prompt/config files from the runtime location that consumes them.
- **QwenPaw 2.0 tool execution**: Preserve QwenPaw's asynchronous tool-result stream while sanitizing output, and allow only the built-in TeamHarness and Workerflow MCP drivers to run without an unavailable interactive approval prompt.
- **QwenPaw package and Team storage access**: Preserve referenced members' effective Team name across independent Worker reconciles, update MinIO policies without detaching active Workers, revoke access on detach, and grant Workers read-only access to centrally uploaded AgentSpec packages.
- **QwenPaw inline prompt compatibility**: Project Worker identity, SOUL, and AGENTS content through runtime desired state and apply it to both the native QwenPaw workspace and the Worker root storage contract.
- **Integration failure diagnostics**: Export AgentTeams container state and timestamped logs with CI artifacts, classify QwenPaw startup errors without exposing their message contents, and stop the TeamHarness shard after startup failures instead of hiding the cause behind cascading timeouts.
- **QwenPaw local startup readiness**: Select and propagate the QwenPaw Worker image across install backends, then wait for the runtime-owned `runtime/runtime.yaml` object before creating a local Docker Worker.
- **QwenPaw Manager remote Matrix discovery**: Use `AGENTTEAMS_MATRIX_URL` instead of a loopback-only URL when detecting Manager DM rooms in split-container and cluster deployments.
- **QwenPaw package reproducibility**: Normalize staged timestamps and strip ZIP metadata so repeated TeamHarness QwenPaw plugin builds are byte-for-byte identical.
- **QwenPaw 2.0 runtime compatibility**: Adapt the QwenPaw Worker image, custom Matrix Channel, and native plugins to QwenPaw 2.0.1 startup and schema contracts.
- **Multimodal model image support**: Add `supports_multimodal` and `supports_image` to `agents.defaults` in generated openclaw.json when the selected model supports image input, so QwenPaw does not strip images at the framework layer. Fixes #931.
- **Install script model env passthrough**: Pass custom model override env vars to the Controller container so that `AGENTTEAMS_MODEL_VISION` and related settings actually reach the config generator.
- **Bridge model capability propagation**: Propagate model `input` modalities from openclaw.json through `_write_providers_json()` so QwenPaw's `ModelInfo` receives `supports_image`/`supports_video`/`supports_multimodal` flags instead of relying on fail-open defaults.
- **Manager diagnostic loops**: Manager prompts and Worker lifecycle guidance stop repeated no-op troubleshooting commands and treat a missing Worker in `agt get workers` as the deletion boundary instead of looping on Matrix room probes. ([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**Bug 修复**

- **CoPaw Team 任务分配交接**：由 `taskflow(delegate_task)` 返回必须执行的 Team Room `message` 动作，根据 Team roster 规范化 Worker 别名，每分钟刷新 Controller 管理的运行时上下文，并将非 Team Room 中的任务分配回复重定向到 Team Room。([#1120](https://github.com/agentscope-ai/AgentTeams/pull/1120))
- **Worker 端口暴露 CLI**：将 `--expose` 参数编码为数值端口，并在创建、更新或应用请求到达 Controller 前拒绝无效或越界输入。
- **Manager 诊断循环**：Manager 提示和 Worker 生命周期指引会停止重复执行无效果的排障命令，并以 `agt get workers` 不再列出目标 Worker 作为删除完成边界，避免继续循环探测 Matrix Room。([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**Change list / 变更列表**

- `90c9fd4f` fix(manager): stop repeated diagnostic loops (#975)
