# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **Worker port exposure CLI**: Encode `--expose` values as numeric ports and reject invalid or out-of-range inputs before create, update, or apply requests reach the Controller.
- **QwenPaw MCP policy startup convergence**: Persist built-in plugin MCP policies before runtime desired-state reloads so a replacement QwenPaw workspace cannot retain the pre-policy interactive approval handler.
- **QwenPaw Team policy and runtime-aware acceptance**: Merge Team and Worker channel-policy overrides into QwenPaw `runtime.yaml`, wait for public plugin/API state before integration assertions, and verify prompt/config files from the runtime location that consumes them.
- **QwenPaw 2.0 tool execution**: Preserve QwenPaw's asynchronous tool-result stream while sanitizing output, and allow only the built-in TeamHarness and Workerflow MCP drivers to run without an unavailable interactive approval prompt.
- **QwenPaw package and Team storage access**: Preserve referenced members' effective Team name across independent Worker reconciles, update MinIO policies without detaching active Workers, revoke access on detach, and grant Workers read-only access to centrally uploaded AgentSpec packages.
- **QwenPaw inline prompt compatibility**: Project Worker identity, SOUL, and AGENTS content through runtime desired state and apply it to both the native QwenPaw workspace and the Worker root storage contract.
- **Integration failure diagnostics**: Export AgentTeams container state and timestamped logs with CI artifacts, classify QwenPaw startup errors without exposing their message contents, and stop the TeamHarness shard after startup failures instead of hiding the cause behind cascading timeouts.
- **QwenPaw local startup readiness**: Select and propagate the QwenPaw Worker image across install backends, then wait for the runtime-owned `runtime/runtime.yaml` object before creating a local Docker Worker.
- **QwenPaw 2.0 runtime compatibility**: Adapt the QwenPaw Worker image, custom Matrix Channel, and native plugins to QwenPaw 2.0.1 startup and schema contracts.
- **Manager diagnostic loops**: Manager prompts and Worker lifecycle guidance stop repeated no-op troubleshooting commands and treat a missing Worker in `agt get workers` as the deletion boundary instead of looping on Matrix room probes. ([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**Bug 修复**

- **Worker 端口暴露 CLI**：将 `--expose` 参数编码为数值端口，并在创建、更新或应用请求到达 Controller 前拒绝无效或越界输入。
- **Manager 诊断循环**：Manager 提示和 Worker 生命周期指引会停止重复执行无效果的排障命令，并以 `agt get workers` 不再列出目标 Worker 作为删除完成边界，避免继续循环探测 Matrix Room。([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**Change list / 变更列表**

- `90c9fd4f` fix(manager): stop repeated diagnostic loops (#975)
