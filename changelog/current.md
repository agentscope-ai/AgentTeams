# Changelog (Unreleased)

Target release: `v1.2.1`

Comparison baseline: `v1.2.0`

Record release-facing changes here before the next release.

---

**What's New**

- feat(controller): enforce cluster-capped ephemeral storage for DeepAgents Runner sandboxes ([c8f9de94](https://github.com/agentscope-ai/AgentTeams/commit/c8f9de9474dc94fb4ca1ef4745cc830597e07859), [6f8bf982](https://github.com/agentscope-ai/AgentTeams/commit/6f8bf9829e8f9eb4a0642d37a0a68640463c0b09), [a31f660b](https://github.com/agentscope-ai/AgentTeams/commit/a31f660b4ac244ca2a428b9d8c17f4d85130207a), [281f3a30](https://github.com/agentscope-ai/AgentTeams/commit/281f3a30f87f40715e002bd14de1b29635c2a30e))
- **DeepAgents Worker runtime**: Add the vendored DeepAgents 0.7.3 runtime, Matrix-thread orchestration and Human approval, Higress model/MCP adapters, encrypted PostgreSQL checkpoints, MinIO workspace synchronization, credential-free Runner Pods, durable per-Worker state, and `ExecutionSandbox` lifecycle/network isolation.
- **QwenPaw 2.0 runtime unification**: Migrate the Manager container from copaw 1.0.2 to QwenPaw 2.0.1 on a single venv, register projectflow/taskflow/message/filesync tools through a QwenPaw plugin instead of monkey-patching CoPawAgent, replace the physical Matrix channel overlay with the QwenPaw plugin system, read Matrix credentials directly from agent.json so the manager tools work without importing copaw at runtime, align CMS observability packages and env vars with the Worker image, inject session-file privacy policy into prompt files, set approval_level=AUTO in the agent template, bridge YOLO mode to Qwenpaw approval_level=OFF, disable the built-in QA Agent, replace start-copaw-manager.sh with start-qwenpaw-manager.sh, publish the QwenPaw Worker image as part of the stable release set, make task assignment state and Matrix notification atomic with room membership validation and m.mentions delivery, preserve m.mentions metadata in streamed/edit events, make the TeamHarness MCP `delegate_task` path atomic too (validate assignee room membership — strictly `join`, not `invite` — prepare → stable-txn notification → commit assigned + event_id; the initial file publish gates the notification and the assigned/eventId commit gates success, both returning a retryable failure so an idempotent retry finishes the sync instead of reporting success with stale shared storage), and migrate a legacy Worker `.copaw` working dir to `.qwenpaw` on the qwenpaw_worker startup path only (idempotent; the migration follows the target runtime — an explicitly configured copaw Worker keeps `.copaw` and never migrates before a switch).
- **Custom model capability overrides**: `AGENTTEAMS_MODEL_VISION` and `AGENTTEAMS_MODEL_REASONING` env vars let deployments override vision and reasoning capabilities for custom models not in the built-in presets table (e.g. local multimodal models like `qwen3.6-27b-fp8`).

**Bug Fixes**

- **DeepAgents Runner bearer-process isolation**: Require Linux process hardening before the Runner listens, remove the bearer from its environment, use non-dumpable/core-limit protections, and run approved commands with a minimal environment and closed inherited descriptors. ([dbc4565d2dfc4f5c600ae8bbf8ddd82018033489](https://github.com/agentscope-ai/AgentTeams/commit/dbc4565d2dfc4f5c600ae8bbf8ddd82018033489))
- **ExecutionSandbox stale-lease revocation**: Revoke sandboxes when Worker ownership, UID, runtime, DeepAgents configuration, or execution mode no longer matches, deleting Service, token Secret, Pod, NetworkPolicy, and the CR in containment order. ([1755ac9724a8491fd3199fef8d2d584468311571](https://github.com/agentscope-ai/AgentTeams/commit/1755ac9724a8491fd3199fef8d2d584468311571))
- **ExecutionSandbox stale cleanup convergence**: Allow stale-sandbox deletion to continue independently of the previous ownership check, including delayed NetworkPolicy removal after the Pod is gone. ([673ad5f053d8ebcc5ab100864035b1a6be100bb7](https://github.com/agentscope-ai/AgentTeams/commit/673ad5f053d8ebcc5ab100864035b1a6be100bb7))
- **ExecutionSandbox generation-safe revocation**: Add finalizer-backed ordered cleanup, uncached Pod-absence confirmation, exact owner/label validation, UID/resourceVersion delete preconditions, and a spec-indexed Worker watch with an observable API-reader fallback. ([46b59ed8b826d5977ee60e35f4ff638fdef4536d](https://github.com/agentscope-ai/AgentTeams/commit/46b59ed8b826d5977ee60e35f4ff638fdef4536d))
- **ExecutionSandbox foreground-GC isolation**: Keep Runner NetworkPolicies outside the Kubernetes owner-reference garbage-collection graph, bind them to an exact Sandbox UID, watch them explicitly, and migrate only exact legacy-owned policies in place so foreground deletion cannot remove isolation before the Runner Pod disappears. ([c6dff29a65f33724fbe0f4138d2d726ce29f153f](https://github.com/agentscope-ai/AgentTeams/commit/c6dff29a65f33724fbe0f4138d2d726ce29f153f))
- **DeepAgents Runner readiness under default-deny**: Probe the Runner health endpoint from container loopback so NetworkPolicy-enforcing CNIs cannot block kubelet-originated readiness traffic, without widening allowed ingress. ([c70cd800bc941d501eb3310d89986916004c43f7](https://github.com/agentscope-ai/AgentTeams/commit/c70cd800bc941d501eb3310d89986916004c43f7))
- **DeepAgents filesystem audit isolation**: Replace synchronous and asynchronous filesystem-tool shell fallbacks with bounded `/v1/files/*` APIs while preserving verified persistence manifests, so each Human-approved explicit command maps to exactly one Runner `/v1/execute` record. ([a6987cf14abe05a12a13339a3ff6c81f4ac11b14](https://github.com/agentscope-ai/AgentTeams/commit/a6987cf14abe05a12a13339a3ff6c81f4ac11b14))
- **Matrix readiness lifecycle**: Report a running but not-yet-ready DeepAgents Worker as `Starting`; reserve terminal failure for explicit failing container states. ([b75cbe7bc8f7a44d419894150728e1dcad1f6aee](https://github.com/agentscope-ai/AgentTeams/commit/b75cbe7bc8f7a44d419894150728e1dcad1f6aee))
- **DeepAgents Runner endpoint readiness**: Probe the credential-free Runner health endpoint before the single side-effecting execution request, and gate Kubernetes Pod readiness on the same application-level check so Service/EndpointSlice convergence cannot create an ambiguous first command. ([a60c209e](https://github.com/agentscope-ai/AgentTeams/commit/a60c209e3c76290121d427d69edd75d5ab49d287))
- **DeepAgents Runner capture contract**: Stop advertising capture offload because credential-free Runner Pods intentionally mount only `/workspace` and `/tmp`, preventing successful commands from emitting false `/large_tool_results` errors on the read-only root filesystem. ([b1a34e5f](https://github.com/agentscope-ai/AgentTeams/commit/b1a34e5f47e0d774b7f19f52e8ab30a06664fa86))
- **DeepAgents Matrix E2EE store initialization**: Create and permission the private matrix-nio store directory before encrypted client construction, preserve existing store contents across reconnects, and reject unsafe leaf symlinks. ([c5ab6f7f](https://github.com/agentscope-ai/AgentTeams/commit/c5ab6f7fd20aba4f3d4078a36a8f4bd21510d226))
- **DeepAgents state PVC ownership**: Prepare the persistent state mount root for UID/GID 65532 with a credential-free, non-recursive ownership init while preserving existing Matrix/SQLite state contents and paths. ([eb192500](https://github.com/agentscope-ai/AgentTeams/commit/eb192500b535a2aee46c1657db0c77d867dc160d))
- **Bundled DeepAgents PostgreSQL local-path startup**: Prepare the persistent-volume mount root with a minimal non-recursive ownership init while preserving PostgreSQL's official data path and existing root-level databases.
- **DeepAgents Matrix task diagnostics**: Retrieve and log asynchronous Matrix message-handler failures instead of silently discarding failed background tasks.
- **DeepAgents workspace manifest integrity**: Validate Runner file sizes and SHA-256 digests within bounded persistence limits before applying any MinIO uploads or deletions.
- **DeepAgents Matrix room boundary**: Ignore invitations outside Controller-projected Personal/Team rooms and fail explicitly when Matrix rejects room joins or reply sends.
- **DeepAgents sandbox lifecycle convergence**: Schedule idle/max-lifetime reconciliation after a Runner becomes ready, and report terminal Runner Pods as failed without automatically replaying possibly side-effecting commands.
- **DeepAgents merge hardening**: Enforce canonical gateway/storage contracts, global Agent approval identities, structured inline prompts, Manager-safe runtime configuration, refreshed sandbox leases/policy, and convergent compute-resource rejection. ([56171271](https://github.com/agentscope-ai/AgentTeams/commit/56171271), [72ac7249](https://github.com/agentscope-ai/AgentTeams/commit/72ac7249), [119904e0](https://github.com/agentscope-ai/AgentTeams/commit/119904e0), [6e92d421](https://github.com/agentscope-ai/AgentTeams/commit/6e92d421), [02723e70](https://github.com/agentscope-ai/AgentTeams/commit/02723e70), [734036e8](https://github.com/agentscope-ai/AgentTeams/commit/734036e8), [99911dc8](https://github.com/agentscope-ai/AgentTeams/commit/99911dc8))
- **DeepAgents crash-safe delivery**: Persist accepted Matrix events through pending/processing/completed states, retain exact completed event IDs, and make an approved command issue exactly one Runner request with no automatic replay after an ambiguous outcome. ([17401047](https://github.com/agentscope-ai/AgentTeams/commit/1740104734d7626a43486f0e6334aed63e1e857b), [f72bb5a2](https://github.com/agentscope-ai/AgentTeams/commit/f72bb5a257e4883e01beb69c130dcaca4501d4a7))
- **DeepAgents live approval identity checks**: Authorize each approval against a fresh, self-scoped Controller lookup using the Worker ServiceAccount identity, fail closed on lookup errors, and bind the endpoint to the caller namespace. ([be6799da](https://github.com/agentscope-ai/AgentTeams/commit/be6799da7ca96d4d6cc549475fade1b428e362d8), [02e57b50](https://github.com/agentscope-ai/AgentTeams/commit/02e57b509e434c2eacd0d9bcf06743c2312b40b4))
- **ExecutionSandbox invalid-policy containment**: Converge invalid sandbox policy to a failed state, asynchronously delete every workload and network surface, continue all containment attempts after partial failures, and preserve the fixed retry cadence. ([004b90c0](https://github.com/agentscope-ai/AgentTeams/commit/004b90c0fe9a37990d2f8996fa040ca43d87fcbf), [262fb548](https://github.com/agentscope-ai/AgentTeams/commit/262fb5480283fdb33c655ac637c2f61464f44b90), [595856ff](https://github.com/agentscope-ai/AgentTeams/commit/595856ff9ae02eecf2c28de8bfa80477eb85cc1a), [e3375633](https://github.com/agentscope-ai/AgentTeams/commit/e3375633989e24e3c2f68902a91dc4d910e9cdfe))
- **DeepAgents sandbox duration and ownership contracts**: Align Controller duration validation with the runtime's whole-second `h`/`m`/`s` grammar, reject invalid Worker policy before Pod creation, and isolate ExecutionSandbox API/cache/watch/reconcile operations by exact controller ownership. ([59845125](https://github.com/agentscope-ai/AgentTeams/commit/598451254cffdc64cdaab19e4cb96987f56db101))
- **DeepAgents exact decimal duration parsing**: Accumulate decimal `h`/`m`/`s` parts as exact fractions so mathematically integral totals such as ten `0.1s` parts resolve to one second instead of failing because of binary floating-point error. ([9c552a66](https://github.com/agentscope-ai/AgentTeams/commit/9c552a665241c4e78722de7afe4a69161eb5acd6))
- **DeepAgents Human approval identities**: Project the Manager as a known Matrix Agent, classify Team coordinators as Humans by roster role, and make Agent identity override conflicting Human approval configuration.
- **QwenPaw Worker runtime management**: Recognize `qwenpaw` as a valid Worker runtime in Manager guidance and runtime-switch validation, invoke the installed `agt` CLI for runtime changes, and align Worker CLI help with the Controller's supported runtimes.
- **CoPaw Team Worker resolution**: Resolve task assignment Matrix IDs from the Controller-owned `runtime.yaml` Team roster before falling back to legacy `AGENTS.md`, so a running Team Leader can delegate after late Team context injection without relying on a stale prompt copy.
- **CoPaw to QwenPaw Worker state migration**: Restore Worker storage before creating any QwenPaw directories, migrate and verify both `.copaw` runtime state and `.copaw.secret` credentials with legacy state authoritative on conflicts, honor QwenPaw's configured secret directory (including relative paths) while rejecting targets outside persistent Worker storage, rebase migrated workspace metadata to `.qwenpaw`, persist migrated files before the idempotency marker, and cover a real CoPaw persistence → QwenPaw runtime switch in E2E tests. ([#1131](https://github.com/agentscope-ai/AgentTeams/pull/1131))
- **Team deletion idempotency**: Verify the target user's current Matrix room membership when Tuwunel returns the ambiguous `joined or banned` invite error, allowing already-joined members to complete Team cleanup without suppressing real bans.
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
- **QwenPaw 2.0 runtime compatibility**: Adapt the QwenPaw Worker image, custom Matrix Channel, and native plugins to QwenPaw 2.0.1 startup and schema contracts.
- **Multimodal model image support**: Add `supports_multimodal` and `supports_image` to `agents.defaults` in generated openclaw.json when the selected model supports image input, so QwenPaw does not strip images at the framework layer. Fixes #931.
- **Install script model env passthrough**: Pass custom model override env vars to the Controller container so that `AGENTTEAMS_MODEL_VISION` and related settings actually reach the config generator.
- **Bridge model capability propagation**: Propagate model `input` modalities from openclaw.json through `_write_providers_json()` so QwenPaw's `ModelInfo` receives `supports_image`/`supports_video`/`supports_multimodal` flags instead of relying on fail-open defaults.
- **Manager diagnostic loops**: Manager prompts and Worker lifecycle guidance stop repeated no-op troubleshooting commands and treat a missing Worker in `agt get workers` as the deletion boundary instead of looping on Matrix room probes. ([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**新增功能**

- **DeepAgents Worker Runtime**：新增内置 DeepAgents 0.7.3、Matrix thread 编排与 Human 审批、Higress 模型/MCP 适配、PostgreSQL 加密 checkpoint、MinIO 工作区同步、无平台凭据 Runner Pod、每 Worker 持久化状态，以及 `ExecutionSandbox` 生命周期和网络隔离。
- **QwenPaw 2.0 运行时统一**：Manager 迁移到 QwenPaw 2.0.1 和原生插件体系，正式发布 QwenPaw Worker 多架构镜像，并完善任务分配原子性、TeamHarness 委派、Matrix mention、运行时策略与 CoPaw 状态迁移。
- **自定义模型能力覆盖**：可通过 `AGENTTEAMS_MODEL_VISION` 和 `AGENTTEAMS_MODEL_REASONING` 为内置预设表之外的模型显式声明视觉与推理能力。

**Bug 修复**

- **ExecutionSandbox 跨代安全撤销**：新增 finalizer 有序清理、未缓存 Pod 消失确认、精确 owner/label 校验、UID/resourceVersion 删除前置条件，以及带可观察 API-reader 回退的 spec 字段索引 Worker watch。([46b59ed8b826d5977ee60e35f4ff638fdef4536d](https://github.com/agentscope-ai/AgentTeams/commit/46b59ed8b826d5977ee60e35f4ff638fdef4536d))
- **ExecutionSandbox foreground GC 隔离**：将 Runner NetworkPolicy 移出 Kubernetes owner-reference 垃圾回收图，以精确 Sandbox UID 绑定并显式 watch，只原地迁移身份完全匹配的旧策略，确保 foreground 删除不能在 Runner Pod 消失前移除隔离边界。([c6dff29a65f33724fbe0f4138d2d726ce29f153f](https://github.com/agentscope-ai/AgentTeams/commit/c6dff29a65f33724fbe0f4138d2d726ce29f153f))
- **default-deny 下的 DeepAgents Runner readiness**：从容器 loopback 探测 Runner 健康端点，避免执行 NetworkPolicy 的 CNI 拦截 kubelet 发起的 readiness 流量，同时不扩大允许的 ingress。([c70cd800bc941d501eb3310d89986916004c43f7](https://github.com/agentscope-ai/AgentTeams/commit/c70cd800bc941d501eb3310d89986916004c43f7))
- **DeepAgents 文件工具审计隔离**：以受限 `/v1/files/*` API 覆盖同步与异步文件工具的 shell 回退，保持写回清单校验，使 Human 批准的显式命令与 Runner `/v1/execute` 记录严格一一对应。([a6987cf14abe05a12a13339a3ff6c81f4ac11b14](https://github.com/agentscope-ai/AgentTeams/commit/a6987cf14abe05a12a13339a3ff6c81f4ac11b14))
- **DeepAgents Runner 端点就绪性**：在唯一一次有副作用的执行请求前探测无凭据 Runner 健康端点，并让 Kubernetes Pod readiness 使用同一个应用级检查，避免 Service/EndpointSlice 收敛竞态导致首次命令结果不明。([a60c209e](https://github.com/agentscope-ai/AgentTeams/commit/a60c209e3c76290121d427d69edd75d5ab49d287))
- **DeepAgents Runner capture 契约**：无凭据 Runner Pod 按设计仅挂载 `/workspace` 与 `/tmp`，因此不再宣称支持 capture offload，避免成功命令在只读根文件系统上产生虚假的 `/large_tool_results` 错误。([b1a34e5f](https://github.com/agentscope-ai/AgentTeams/commit/b1a34e5f47e0d774b7f19f52e8ab30a06664fa86))
- **DeepAgents Matrix E2EE 存储初始化**：在构造加密客户端前创建 matrix-nio 私有存储目录并收紧权限，重连时保留已有存储内容，并拒绝不安全的叶子符号链接。([c5ab6f7f](https://github.com/agentscope-ai/AgentTeams/commit/c5ab6f7fd20aba4f3d4078a36a8f4bd21510d226))
- **DeepAgents 状态 PVC 所有权**：通过无凭据、非递归的所有权 initContainer 将持久状态挂载根目录准备为 UID/GID 65532，同时保留已有 Matrix/SQLite 状态内容和路径。([eb192500](https://github.com/agentscope-ai/AgentTeams/commit/eb192500b535a2aee46c1657db0c77d867dc160d))
- **内置 DeepAgents PostgreSQL local-path 启动**：通过最小权限 initContainer 非递归修正持久卷挂载根目录所有权，同时保留 PostgreSQL 官方数据路径和已有的根目录数据库。
- **DeepAgents Matrix 任务诊断**：主动获取并记录异步 Matrix 消息处理异常，避免失败的后台任务被静默丢弃。
- **DeepAgents 工作区清单完整性**：在执行任何 MinIO 上传或删除前，按持久化边界校验 Runner 文件大小与 SHA-256 摘要。
- **DeepAgents Matrix Room 边界**：忽略 Controller 未投影的 Personal/Team Room 邀请，并在 Matrix 拒绝加入 Room 或发送回复时显式失败。
- **DeepAgents Sandbox 生命周期收敛**：Runner Ready 后按 idle/max-lifetime 继续调度回收检查，并将终止的 Runner Pod 标记为失败，不自动重放可能产生副作用的命令。
- **DeepAgents 崩溃安全投递**：将已接受的 Matrix 事件持久化为 pending/processing/completed 状态，保留精确的已完成 event ID；每次获批命令只发出一次 Runner 请求，结果不确定时不自动重放。([17401047](https://github.com/agentscope-ai/AgentTeams/commit/1740104734d7626a43486f0e6334aed63e1e857b), [f72bb5a2](https://github.com/agentscope-ai/AgentTeams/commit/f72bb5a257e4883e01beb69c130dcaca4501d4a7))
- **DeepAgents 实时审批身份校验**：使用 Worker ServiceAccount 身份对每次审批执行实时、自限定的 Controller 查询，查询失败时 fail-closed，并将接口绑定到调用者命名空间。([be6799da](https://github.com/agentscope-ai/AgentTeams/commit/be6799da7ca96d4d6cc549475fade1b428e362d8), [02e57b50](https://github.com/agentscope-ai/AgentTeams/commit/02e57b509e434c2eacd0d9bcf06743c2312b40b4))
- **ExecutionSandbox 无效策略隔离**：将无效 sandbox 策略收敛为 Failed，异步删除全部工作负载和网络暴露面，在部分失败后继续其它隔离动作，并保持固定重试节奏。([004b90c0](https://github.com/agentscope-ai/AgentTeams/commit/004b90c0fe9a37990d2f8996fa040ca43d87fcbf), [262fb548](https://github.com/agentscope-ai/AgentTeams/commit/262fb5480283fdb33c655ac637c2f61464f44b90), [595856ff](https://github.com/agentscope-ai/AgentTeams/commit/595856ff9ae02eecf2c28de8bfa80477eb85cc1a), [e3375633](https://github.com/agentscope-ai/AgentTeams/commit/e3375633989e24e3c2f68902a91dc4d910e9cdfe))
- **DeepAgents Sandbox 时长与归属契约**：将 Controller 时长校验对齐运行时的整数秒 `h`/`m`/`s` 语法，在创建 Pod 前拒绝无效 Worker 策略，并按精确 Controller 归属隔离 ExecutionSandbox API、cache、watch 和 reconcile。([59845125](https://github.com/agentscope-ai/AgentTeams/commit/598451254cffdc64cdaab19e4cb96987f56db101))
- **DeepAgents 小数时长精确解析**：以精确分数累计十进制 `h`/`m`/`s` 片段，使十个 `0.1s` 等数学上为整数秒的总和正确解析为一秒，不再受二进制浮点误差影响。([9c552a66](https://github.com/agentscope-ai/AgentTeams/commit/9c552a665241c4e78722de7afe4a69161eb5acd6))
- **DeepAgents Human 审批身份**：将 Manager 显式投影为已知 Matrix Agent，按 Team roster 角色识别人类协调员，并在 Human 审批配置冲突时始终以 Agent 身份优先拒绝。
- **QwenPaw Worker 运行时管理**：在 Manager 指引和运行时切换校验中将 `qwenpaw` 识别为合法 Worker runtime，切换时调用镜像内实际安装的 `agt` CLI，并使 Worker CLI 帮助与 Controller 实际支持的运行时保持一致。
- **CoPaw Team Worker 解析**：任务分配优先从 Controller 管理的 `runtime.yaml` Team roster 获取 Matrix ID，仅在旧部署缺少该 roster 时回退 `AGENTS.md`，避免运行中的 Team Leader 因 prompt 副本过期而无法委派任务。
- **CoPaw 到 QwenPaw Worker 状态迁移**：在创建任何 QwenPaw 目录前先恢复 Worker 存储，迁移并校验 `.copaw` 运行时状态和 `.copaw.secret` 凭据，冲突时以旧 CoPaw 状态为准，遵循 QwenPaw 配置的 secret 目录（包括相对路径）并拒绝持久化 Worker 目录之外的目标，将工作区元数据改写到 `.qwenpaw`，先持久化迁移数据再写入幂等标记，并增加真实 CoPaw 持久化后切换 QwenPaw 的 E2E 覆盖。([#1131](https://github.com/agentscope-ai/AgentTeams/pull/1131))
- **Team 删除幂等性**：当 Tuwunel 返回含义不明确的 `joined or banned` 邀请错误时，核验目标用户当前的 Matrix Room 成员状态，使已加入成员能够继续完成 Team 清理，同时不吞掉真实的封禁错误。
- **CoPaw Team 任务分配交接**：由 `taskflow(delegate_task)` 返回必须执行的 Team Room `message` 动作，根据 Team roster 规范化 Worker 别名，每分钟刷新 Controller 管理的运行时上下文，并将非 Team Room 中的任务分配回复重定向到 Team Room。([#1120](https://github.com/agentscope-ai/AgentTeams/pull/1120))
- **Worker 端口暴露 CLI**：将 `--expose` 参数编码为数值端口，并在创建、更新或应用请求到达 Controller 前拒绝无效或越界输入。
- **Manager 诊断循环**：Manager 提示和 Worker 生命周期指引会停止重复执行无效果的排障命令，并以 `agt get workers` 不再列出目标 Worker 作为删除完成边界，避免继续循环探测 Matrix Room。([#975](https://github.com/agentscope-ai/AgentTeams/pull/975))

---

**Change list / 变更列表**

- [`b1a34e5f`](https://github.com/agentscope-ai/AgentTeams/commit/b1a34e5f47e0d774b7f19f52e8ab30a06664fa86) fix(deepagents): align runner capture contract
- [`a60c209e`](https://github.com/agentscope-ai/AgentTeams/commit/a60c209e3c76290121d427d69edd75d5ab49d287) fix(deepagents): wait for runner endpoint readiness
- [`c5ab6f7f`](https://github.com/agentscope-ai/AgentTeams/commit/c5ab6f7fd20aba4f3d4078a36a8f4bd21510d226) fix(deepagents): create Matrix E2EE store directory
- [`eb192500`](https://github.com/agentscope-ai/AgentTeams/commit/eb192500b535a2aee46c1657db0c77d867dc160d) fix(controller): prepare DeepAgents state PVC ownership
- [`9c552a66`](https://github.com/agentscope-ai/AgentTeams/commit/9c552a665241c4e78722de7afe4a69161eb5acd6) fix(deepagents): parse durations exactly
- [`59845125`](https://github.com/agentscope-ai/AgentTeams/commit/598451254cffdc64cdaab19e4cb96987f56db101) fix(deepagents): align sandbox ownership contracts
- [`c6dff29a`](https://github.com/agentscope-ai/AgentTeams/commit/c6dff29a65f33724fbe0f4138d2d726ce29f153f) fix(controller): keep runner policy outside foreground GC
- [`c70cd800`](https://github.com/agentscope-ai/AgentTeams/commit/c70cd800bc941d501eb3310d89986916004c43f7) fix(controller): probe runner health inside network boundary
- [`a6987cf1`](https://github.com/agentscope-ai/AgentTeams/commit/a6987cf14abe05a12a13339a3ff6c81f4ac11b14) fix(deepagents): separate file tools from command execution
- [`e3375633`](https://github.com/agentscope-ai/AgentTeams/commit/e3375633989e24e3c2f68902a91dc4d910e9cdfe) fix(controller): preserve sandbox containment cadence
- [`595856ff`](https://github.com/agentscope-ai/AgentTeams/commit/595856ff9ae02eecf2c28de8bfa80477eb85cc1a) fix(controller): attempt all sandbox containment
- [`262fb548`](https://github.com/agentscope-ai/AgentTeams/commit/262fb5480283fdb33c655ac637c2f61464f44b90) fix(controller): keep invalid sandboxes isolated
- [`004b90c0`](https://github.com/agentscope-ai/AgentTeams/commit/004b90c0fe9a37990d2f8996fa040ca43d87fcbf) fix(controller): converge invalid sandbox policies
- [`02e57b50`](https://github.com/agentscope-ai/AgentTeams/commit/02e57b509e434c2eacd0d9bcf06743c2312b40b4) fix(controller): bind identity lookup namespace
- [`be6799da`](https://github.com/agentscope-ai/AgentTeams/commit/be6799da7ca96d4d6cc549475fade1b428e362d8) fix(deepagents): authorize approvals against live identities
- [`f72bb5a2`](https://github.com/agentscope-ai/AgentTeams/commit/f72bb5a257e4883e01beb69c130dcaca4501d4a7) fix(deepagents): retain exact Matrix event dedupe
- [`17401047`](https://github.com/agentscope-ai/AgentTeams/commit/1740104734d7626a43486f0e6334aed63e1e857b) fix(deepagents): make delivery crash-safe
- [`99911dc8`](https://github.com/agentscope-ai/AgentTeams/commit/99911dc8) fix(controller): converge invalid sandbox compute resources
- [`734036e8`](https://github.com/agentscope-ai/AgentTeams/commit/734036e8) fix(deepagents): refresh execution sandbox leases and policy
- [`02723e70`](https://github.com/agentscope-ai/AgentTeams/commit/02723e70) fix(agt): reject ambiguous runtime config YAML
- [`6e92d421`](https://github.com/agentscope-ai/AgentTeams/commit/6e92d421) fix(manager): configure DeepAgents sandbox workers
- [`119904e0`](https://github.com/agentscope-ai/AgentTeams/commit/119904e0) fix(deepagents): refresh Edge approval identities
- [`72ac7249`](https://github.com/agentscope-ai/AgentTeams/commit/72ac7249) fix(deepagents): harden approval identity and prompt policy
- [`56171271`](https://github.com/agentscope-ai/AgentTeams/commit/56171271) fix(deepagents): enforce gateway and storage contracts
- [`bffedf1f`](https://github.com/agentscope-ai/AgentTeams/commit/bffedf1f) fix(deepagents): surface Matrix task failures
- [`8fd67797`](https://github.com/agentscope-ai/AgentTeams/commit/8fd67797) fix(deepagents): validate durable workspace manifests
- [`56dcc083`](https://github.com/agentscope-ai/AgentTeams/commit/56dcc083) fix(deepagents): enforce Matrix room response boundaries
- [`d68ad9e2`](https://github.com/agentscope-ai/AgentTeams/commit/d68ad9e2) fix(deepagents): converge sandbox lifecycle status
- [`9fd79fbb`](https://github.com/agentscope-ai/AgentTeams/commit/9fd79fbb) fix(deepagents): enforce Matrix human approval identities
- [`3c3b4feb`](https://github.com/agentscope-ai/AgentTeams/commit/3c3b4feb) feat(deepagents): package Kubernetes worker runtime
- [`30216225`](https://github.com/agentscope-ai/AgentTeams/commit/30216225) feat(deepagents): add Matrix runtime and workspace sync
- [`44eb0bb1`](https://github.com/agentscope-ai/AgentTeams/commit/44eb0bb1) feat(deepagents): implement secure worker runtime core
- [`74661a63`](https://github.com/agentscope-ai/AgentTeams/commit/74661a63) feat(deepagents): add controller runtime and sandbox contracts
- [`65c7b611`](https://github.com/agentscope-ai/AgentTeams/commit/65c7b611) feat(deepagents): add gateway HITL and runner boundaries
- [`57620ef1`](https://github.com/agentscope-ai/AgentTeams/commit/57620ef1) feat(deepagents): add AgentTeams adapter contracts
- [`bd60a9df`](https://github.com/agentscope-ai/AgentTeams/commit/bd60a9df) chore(deepagents): import upstream 0.7.3 subtree
- `a6c98182` fix(manager): stop repeated diagnostic loops (#975)
- `fb3a40be` feat(qwenpaw): adapt worker runtime to QwenPaw 2.0 (#1077)
- `ce3a4770` fix(install): skip QwenPaw pull and update dashboard default (#1115)
- `99131d6e` chore: update default dashboard version to v1.2.0 (#1118)
- `47c8f284` chore: archive changelog for v1.2.0 (#1112)
- `1145f796` docs: add v1.2.0 release news (#1121)
- `00a5d20c` fix(cli): encode exposed Worker ports as integers (#1123)
- `ba161a85` fix(controller): rotate Docker Worker ServiceAccount token files (#1120)
- `2ea02740` fix(controller): enable multimodal for custom models with env override + openclaw.json injection (#1103)
- `124f06d1` feat(manager): migrate Manager runtime from copaw to qwenpaw 2.0 (#1095)
- `2fd9ddde` fix(qwenpaw): preserve CoPaw state during runtime migration (#1131)
- `062f1c8d` fix(controller): make Team deletion invite idempotent (#1140)

**Also in this window / 同期其他变更**

- Update the bundled Dashboard default to v1.2.0 and publish the v1.2.0 release news. ([#1118](https://github.com/agentscope-ai/AgentTeams/pull/1118), [#1121](https://github.com/agentscope-ai/AgentTeams/pull/1121))
- Archive the v1.2.0 changelog before collecting the v1.2.1 release window. ([#1112](https://github.com/agentscope-ai/AgentTeams/pull/1112))
