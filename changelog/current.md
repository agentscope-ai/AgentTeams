# Changelog (Unreleased)

Target release: `v1.2.0-beta1`

Comparison baseline: `v1.1.2`

Record release-facing changes here before the next release.

---

**Breaking Changes / Migration Notes**

- **AgentTeams naming becomes the public contract**: Runtime environment variables, container images, Matrix domains and aliases, Helm defaults, storage paths, Kubernetes resources, and documentation now use AgentTeams naming. Kubernetes APIs move to `agentteams.io/v1beta1`; deployments upgrading from HiClaw manifests must install the new CRDs and update resource manifests.

- **Team membership is decoupled from inline Workers**: Team resources now reference standalone Worker CRs through `spec.workerMembers`. Existing Team definitions that rely on the previous inline membership shape must be migrated.

- **Fresh-install resource defaults change**: New Helm installations use the `agentteams-` resource prefix, `agentteams-storage` bucket, and `agentteams/` storage root. Selected runtime environment variables and legacy Matrix alias registration retain HiClaw compatibility for migration, but operators should review existing storage and resource names before upgrading.

- **QwenPaw remains a beta runtime**: The QwenPaw package, image, Matrix integration, and WorkerFlow / TeamHarness adapters are included for evaluation. `v1.2.0-beta1` keeps the established runtime defaults rather than switching deployments to QwenPaw automatically.

**What's New**

- **AgentTeams product and runtime contracts**: The project completes the public rename from HiClaw to AgentTeams across images, install flows, controller contracts, Helm, runtime environment variables, Matrix configuration, shared storage, scripts, and user-facing documentation.

- **Expanded Worker runtime portfolio**: OpenHuman is added as a native-Matrix Worker runtime, while the new QwenPaw package adds runtime configuration updates, MinIO synchronization, heartbeat reporting, Matrix channel integration, container packaging, and focused integration coverage.

- **Plugin platform, TeamHarness, and WorkerFlow**: A plugin packaging CLI and schemas are introduced together with TeamHarness collaboration tools, QwenPaw task-trace correlation, WorkerFlow integration, MCP services, prompts, skills, and runtime adapters.

- **Remote and sandbox Worker backends**: The controller supports remote Worker deployment and OpenKruise Sandbox / SandboxClaim backends, including applied-target tracking, remote pod templates, dependency materialization, lifecycle restrictions, and backend-specific status handling.

- **Richer Kubernetes resource contracts**: Manager, Worker, Team Leader, and Team Worker resources support per-agent resource requests and limits. Team membership moves to standalone Worker references, with conflict detection and coordination-context injection.

- **Matrix AppService and Human SSO**: The controller can register as a Matrix Application Service, provision passwordless Matrix identities, process AppService transactions, and resolve SSO-backed Human identities for Team administration and membership.

- **Model routing and LLM preflight**: Resources can select a Higress model provider through `spec.modelProvider`. Helm adds an LLM preflight hook and the `hiclaw llm-preflight` command to validate the API key, base URL, and model before controller startup.

- **Controller observability and diagnostics**: AgentTeams controller metrics, optional Helm ServiceMonitor support, pod container failure reporting, richer status APIs, and expanded integration diagnostics improve production and CI troubleshooting.

**Bug Fixes**

- **Worker lifecycle correctness**: New Workers are no longer misclassified as spec changes; default runtimes are passed to Manager; Team Worker name conflicts are rejected; leader coordination context survives inline `AGENTS.md`; disabled Manager reconciliation is skipped.

- **Gateway and provider authorization**: AI route authorization is serialized, provider-specific authorization remains controller-owned, unsupported gateway port exposure is skipped, and APIG endpoints can be overridden.

- **Remote and sandbox deployment safety**: Sandbox dependencies are materialized before claim creation, stale claims are recycled on runtime changes, OSS Workers remain local or edge scoped, and obsolete OSS target-cluster routing is removed.

- **CoPaw reliability**: Runtime path defaults, AgentTeams environment variables, Matrix routing, task assignment name matching, heartbeat defaults, and missing-MinIO-object handling are corrected.

- **Install and local runtime robustness**: Admin usernames are normalized, mounted sockets follow the selected runtime, Podman systemd autostart is supported, Docker Workers receive the host gateway mapping, and manual Worker installation joins the correct network.

- **Helm and CRD compatibility**: Unsupported CRD `propertyNames` fields are removed, AppService environment wiring uses AgentTeams names, and fresh-install storage/resource defaults are aligned with AgentTeams.

- **AgentTeams rename follow-ups**: README legacy-name text is corrected to “formerly HiClaw”; Matrix AppService accepts new `#agentteams-*` aliases while retaining legacy aliases; OpenHuman consumes canonical `AGENTTEAMS_*` variables with HiClaw fallbacks.

---

**破坏性变更 / 升级说明**

- **AgentTeams 命名成为新的公开契约**: 运行时环境变量、容器镜像、Matrix 域名与 alias、Helm 默认值、存储路径、Kubernetes 资源和文档统一使用 AgentTeams 命名。Kubernetes API 迁移到 `agentteams.io/v1beta1`，从 HiClaw 清单升级时需要安装新 CRD 并更新资源定义。

- **Team 成员与内联 Worker 解耦**: Team 通过 `spec.workerMembers` 引用独立 Worker CR。仍使用旧内联成员结构的 Team 需要迁移。

- **全新安装的资源默认值变化**: Helm 新安装默认使用 `agentteams-` 资源前缀、`agentteams-storage` bucket 和 `agentteams/` 存储根路径。部分运行时环境变量和旧 Matrix alias 仍保留 HiClaw 兼容，但升级前需要确认现有资源名和存储路径。

- **QwenPaw 仍处于 beta 阶段**: 本版本包含 QwenPaw 包、镜像、Matrix 集成以及 WorkerFlow / TeamHarness 适配器，供预览验证；`v1.2.0-beta1` 不会自动把现有部署默认运行时切换到 QwenPaw。

**新增功能**

- **AgentTeams 产品与运行时契约**: 完成从 HiClaw 到 AgentTeams 的公开改名，覆盖镜像、安装流程、控制器契约、Helm、运行时环境变量、Matrix 配置、共享存储、脚本和用户文档。

- **扩展 Worker 运行时**: 新增原生 Matrix 的 OpenHuman Worker；新增 QwenPaw 包，支持运行时配置更新、MinIO 同步、心跳上报、Matrix Channel、容器镜像及专项集成测试。

- **插件平台、TeamHarness 与 WorkerFlow**: 新增插件打包 CLI 和 Schema，以及 TeamHarness 协作工具、QwenPaw 任务 Trace 关联、WorkerFlow 集成、MCP 服务、提示词、Skills 和运行时适配器。

- **Remote 与 Sandbox Worker 后端**: 控制器支持远程 Worker 部署和 OpenKruise Sandbox / SandboxClaim 后端，包括已应用目标记录、远程 Pod 模板、依赖物化、生命周期限制和后端状态处理。

- **更完整的 Kubernetes 资源契约**: Manager、Worker、Team Leader 和 Team Worker 支持单 Agent 资源规格；Team 成员改为引用独立 Worker CR，并增加冲突检查和协作上下文注入。

- **Matrix AppService 与 Human SSO**: 控制器支持注册 Matrix Application Service、无密码创建 Matrix 身份、处理 AppService Transaction，并使用 SSO Human 身份解析 Team 管理员和成员。

- **模型路由与 LLM 预检**: 资源可通过 `spec.modelProvider` 选择 Higress 模型提供方；Helm 新增 LLM preflight hook 和 `hiclaw llm-preflight` 命令，在控制器启动前验证 API Key、Base URL 和模型。

- **控制器可观测性与诊断**: 新增 AgentTeams 控制器指标、可选 ServiceMonitor、Pod 容器失败状态、增强的状态 API 和集成测试诊断。

**Bug 修复**

- **Worker 生命周期正确性**: 新建 Worker 不再被误判为 spec 变更；默认运行时会传给 Manager；拒绝 Team Worker 重名；内联 `AGENTS.md` 不再覆盖 Leader 协作上下文；禁用 Manager 时跳过 Reconcile。

- **网关与模型提供方鉴权**: AI Route 鉴权改为串行执行；provider 专属鉴权保持由控制器 Reconcile 管理；跳过不支持的网关端口暴露；支持覆盖 APIG Endpoint。

- **Remote 与 Sandbox 部署安全**: 创建 SandboxClaim 前先物化依赖；运行时变更时回收旧 Claim；OSS Worker 保持 local / edge 部署边界；移除无效的 OSS 目标集群路由。

- **CoPaw 稳定性**: 修复运行时路径默认值、AgentTeams 环境变量、Matrix 路由、任务分配名称匹配、心跳默认值和缺失 MinIO 对象处理。

- **安装与本地运行稳健性**: 管理员用户名统一小写；挂载 Socket 跟随所选容器运行时；支持 Podman systemd 自启动；Docker Worker 注入 host gateway；手动安装 Worker 时加入正确网络。

- **Helm 与 CRD 兼容性**: 移除不受支持的 CRD `propertyNames`；AppService 环境变量切换到 AgentTeams；全新安装的存储和资源默认值完成 AgentTeams 对齐。

- **AgentTeams 改名收尾**: README 中旧名称修正为“原 HiClaw”；Matrix AppService 同时注册新的 `#agentteams-*` 和旧 alias；OpenHuman 优先消费 `AGENTTEAMS_*` 并保留 HiClaw fallback。

---

**Change list by feature / 按功能点整理（15 项）**

- **AgentTeams rename and contract alignment / AgentTeams 改名与契约统一**: Align images, environment variables, Helm defaults, Matrix aliases, storage paths, Kubernetes resources, scripts, and documentation, while retaining selected migration fallbacks. ([a7b707e](https://github.com/agentscope-ai/AgentTeams/commit/a7b707efcbb28cf09f68af8d77387e94c34cfc37), [e4f4ce6](https://github.com/agentscope-ai/AgentTeams/commit/e4f4ce6d8c38cbdcf68741dbe010609405fd795c), [ef8ec66](https://github.com/agentscope-ai/AgentTeams/commit/ef8ec66506bd7914944776b7e6eba39cb0539db1))
- **Kubernetes API and Team model / Kubernetes API 与 Team 模型**: Move APIs to `agentteams.io/v1beta1`, decouple Team membership from inline Workers, and reference standalone Worker CRs. ([862d59e](https://github.com/agentscope-ai/AgentTeams/commit/862d59e987dd5430928e75a9c1e017c521435715))
- **OpenHuman runtime / OpenHuman 运行时**: Add the native-Matrix OpenHuman Worker runtime, image, configuration, and agent template. ([d2e30c2](https://github.com/agentscope-ai/AgentTeams/commit/d2e30c250cae35a0022860a63e1c2b3be45e145b))
- **QwenPaw runtime / QwenPaw 运行时**: Add the QwenPaw package baseline, runtime configuration, storage synchronization, heartbeat, Matrix channel, tests, and Worker image. ([8ddb0ef](https://github.com/agentscope-ai/AgentTeams/commit/8ddb0efcbc34254c4b00e9955d84441ed83ddc82), [5c555c6](https://github.com/agentscope-ai/AgentTeams/commit/5c555c6da5e454ddd09b7a6256dafd58a6cc5920))
- **Plugin platform / 插件平台**: Introduce AgentTeams plugin schemas, packaging, validation, and CLI workflows. ([906c0f2](https://github.com/agentscope-ai/AgentTeams/commit/906c0f2c41138e5cd4b341b69be168e8071f7702), [7af0304](https://github.com/agentscope-ai/AgentTeams/commit/7af03046b20a43f84cc74fc759dd835cf66b86bc))
- **TeamHarness and WorkerFlow / TeamHarness 与 WorkerFlow**: Add TeamHarness collaboration tools, QwenPaw task-trace correlation, WorkerFlow adapters, MCP services, prompts, skills, and runtime integration. ([04d1d46](https://github.com/agentscope-ai/AgentTeams/commit/04d1d46f2a97f725ff550399c7331f079bab40de), [de206ab](https://github.com/agentscope-ai/AgentTeams/commit/de206ab4b96c48b80bd5b8f0def63db97c95fdfe), [be7596d](https://github.com/agentscope-ai/AgentTeams/commit/be7596d5dc3e6ed0610d39eb920e3e74bbf26bfe))
- **Matrix AppService and Human SSO / Matrix AppService 与 Human SSO**: Support passwordless Matrix identity provisioning, AppService transactions, AgentTeams aliases, and SSO-backed Human identity resolution. ([0d1e603](https://github.com/agentscope-ai/AgentTeams/commit/0d1e603e9e72f020013dc21706986fe51d6c5f24), [4f62efb](https://github.com/agentscope-ai/AgentTeams/commit/4f62efbd121ec44fdc1e0e3291f47a0be44960c4), [9ab93fb](https://github.com/agentscope-ai/AgentTeams/commit/9ab93fbe8db5e714356c7a57a9db57cf4fe0077c))
- **Remote Worker deployment / 远程 Worker 部署**: Add remote deployment mode, applied-target tracking, remote pod templates, and clear local/edge boundaries for open-source Workers. ([98b3d9e](https://github.com/agentscope-ai/AgentTeams/commit/98b3d9e8bb08896ac1e33984fb49db529920e231), [af3fd2a](https://github.com/agentscope-ai/AgentTeams/commit/af3fd2abce88d7b694fc1fb5bb93b53496676663), [6f0c7da](https://github.com/agentscope-ai/AgentTeams/commit/6f0c7daa186f3ed493174609fe8881954fad13e9))
- **Sandbox backend / Sandbox 后端**: Support OpenKruise Sandbox and SandboxClaim Workers, including dependency materialization, runtime-change recycling, lifecycle restrictions, and backend status handling. ([c39d1b4](https://github.com/agentscope-ai/AgentTeams/commit/c39d1b4e9d372dedfee87e3d281bf311fbd08d0f), [0b562ff](https://github.com/agentscope-ai/AgentTeams/commit/0b562ff732b7ccc92ff25079be6ec38c15077f35))
- **Resource and lifecycle correctness / 资源规格与生命周期正确性**: Add per-agent resource requests and limits; fix new-Worker classification, default-runtime propagation, Worker name conflicts, coordination context, and disabled Manager reconciliation. ([d2e9e7b](https://github.com/agentscope-ai/AgentTeams/commit/d2e9e7b203e59cb4fb3a986a3aa2be092f4a9dd5), [b13f1d9](https://github.com/agentscope-ai/AgentTeams/commit/b13f1d9a417c5833a80f46faab3dd04c82158a5a), [fbad70f](https://github.com/agentscope-ai/AgentTeams/commit/fbad70f2acb734dbd945d0699edf289fcd84535e), [fb294d7](https://github.com/agentscope-ai/AgentTeams/commit/fb294d7cb83926fff5e2018644c4015685149fd4))
- **Model routing and LLM preflight / 模型路由与 LLM 预检**: Add `spec.modelProvider`, preserve controller-owned provider authorization, and validate API key, base URL, and model before startup. ([746de8c](https://github.com/agentscope-ai/AgentTeams/commit/746de8c794af93ede2f2cbabc486fc63e48c972f), [b4f00df](https://github.com/agentscope-ai/AgentTeams/commit/b4f00dfa59c712a61c49399c64d7b50ded8676a0), [34425c8](https://github.com/agentscope-ai/AgentTeams/commit/34425c8767f6671701185c28ca1a407134e5e035))
- **Observability and diagnostics / 可观测性与诊断**: Add controller metrics, optional ServiceMonitor support, pod container failure reporting, richer status data, and integration diagnostics. ([6b03ab0](https://github.com/agentscope-ai/AgentTeams/commit/6b03ab037357485d6f65235b27590c1062b09f3b), [295e157](https://github.com/agentscope-ai/AgentTeams/commit/295e157e611f33d673379d7d575fcbe61830db0f))
- **CoPaw and task-flow reliability / CoPaw 与任务流稳定性**: Correct runtime paths, AgentTeams environment variables, Matrix routing, task assignment matching, heartbeat defaults, and missing-object handling. ([70ee282](https://github.com/agentscope-ai/AgentTeams/commit/70ee2826e2bb7bcc267f8662997ee50a59ac15aa), [2202c8c](https://github.com/agentscope-ai/AgentTeams/commit/2202c8c2edbcd1c6676ca810646aa3bcc26dd008), [03c180f](https://github.com/agentscope-ai/AgentTeams/commit/03c180fd1b533244bbb056e0443a384da0a1de11), [1f5e688](https://github.com/agentscope-ai/AgentTeams/commit/1f5e6887061621f2dbe4d225ccc9294662f72467))
- **Install, container, and gateway robustness / 安装、容器与网关稳健性**: Normalize users, select the correct socket and network, support Podman autostart, configure Docker host gateway access, serialize route authorization, and handle gateway endpoint differences. ([6797b76](https://github.com/agentscope-ai/AgentTeams/commit/6797b761039a68dcfcca7fe3524bb084ade186ac), [28c34f2](https://github.com/agentscope-ai/AgentTeams/commit/28c34f272c12d614c7f42aaa311ce38515762549), [de07995](https://github.com/agentscope-ai/AgentTeams/commit/de079956fad5ab4c6f6f8e1cb04ebff10660f14c), [428d3a1](https://github.com/agentscope-ai/AgentTeams/commit/428d3a152f31690b772ce8ba8fba683f7b6bfa7d))
- **Helm, CRD, documentation, tests, and CI / Helm、CRD、文档、测试与 CI**: Remove unsupported CRD fields, align fresh-install defaults, expand deployment and FAQ guidance, strengthen runtime lifecycle coverage, and isolate rename contract checks in CI. ([9552700](https://github.com/agentscope-ai/AgentTeams/commit/955270013407ebce0c3299efa8f9c1912aa05b6b), [06d75c6](https://github.com/agentscope-ai/AgentTeams/commit/06d75c6391ce56c92d0b77d9f3c7004c3c96df03), [c200631](https://github.com/agentscope-ai/AgentTeams/commit/c200631c450e1277edf87bd8e2e0c65038b5496e), [1007e6f](https://github.com/agentscope-ai/AgentTeams/commit/1007e6f434aeca2d5bd9b037e8e0c3673f5566d5))
