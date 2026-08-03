# TeamHarness Runtime 集成 TDD 计划

本文用于规划 TeamHarness / QwenPaw / Claude Code 的分阶段改造。每个阶段都先用测试锁定边界，再做最小实现。

## 原则

- 先写测试锁住契约和职责边界，再移动或实现代码。
- 不改变 OpenClaw、CoPaw、Hermes 现有行为。
- 清楚区分 TeamHarness plugin、AgentSpec package、worker lifecycle、runtime adapter 的职责。
- 本 PR 不实现 controller 写 runtime config 的逻辑。controller 同事按契约实现写入。

## 阶段 1：契约和计划文档

产出并评审 runtime config 契约：

- `docs/member-runtime-config-contract.md`
- 本 TDD 计划

契约必须明确：`desired.agentPackage` 是 AgentTeams AgentSpec package，不是 TeamHarness plugin package。

TDD 目标：

- 本阶段以文档评审作为验收。

## 阶段 2：CLI 默认安装和 LoongSuite 标准兼容

先做 TeamHarness 标准包和默认 CLI 安装路径。LoongSuite/Pilot 只作为本地部署兼容入口，
不作为默认安装器，也不参与集群 QwenPaw worker 生命周期。

先写测试：

- `teamharness.tar.gz` 包含 `plugin.yaml`、prompts、skills、MCP、adapters 和
  `scripts/install.sh` / `scripts/uninstall.sh`。
- `agentteams plugin install/list/update/uninstall` 能以同一个 tarball 作为默认安装路径。
- CLI 解包到 `.agentteams/` 管理目录，调用 lifecycle script，并记录本地 manifest。
- 重复安装和 update 使用同一套 lifecycle；uninstall 只清理该 plugin 自己的状态。
- LoongSuite 兼容定义使用 `deployMode: plugin-probe`，指向
  `$PILOT_DIR/plugins/teamharness.tar.gz` 和 `$PILOT_DATA/plugins/teamharness`。
- 模拟 LoongSuite 解包后能执行同一个 `scripts/install.sh`。
- 用 fake `qwenpaw` / fake `claude` 验证 lifecycle script 会分发到对应 adapter 入口。
- 包不存在、tarball 非法、install script 失败、manifest name 不匹配时有明确错误。

实现目标：

- CLI 是默认安装器。
- TeamHarness tarball 同时兼容 CLI 和 LoongSuite `plugin-probe` 标准。
- CLI 和 LoongSuite 不维护两套包结构。
- 本阶段不要求 TeamHarness 业务能力完整，也不做真实 QwenPaw / Claude Code runtime 调用。

## 阶段 3：TeamHarness 基础包集成

在 plugin 管理逻辑可用后，再迁移 TeamHarness 基础包。

本阶段的核心是树立 TeamHarness v0.1 的功能边界和契约关系。TeamHarness plugin 只提供团队协作
基础能力，不承载 worker desired-state apply loop 或 runtime apply 逻辑。

标准资产清单：

- Prompts：
  - `prompts/team/TEAMS.md`
  - `prompts/agent/leader.md`
  - `prompts/agent/worker.md`
  - `prompts/agent/remote-member.md`
  - `prompts/manager/AGENTS.md`
  - `prompts/manager/TOOLS.md`
  - `prompts/manager/HEARTBEAT.md`
- Agent skills：
  - `mcporter`
  - `find-skills`
- Team skills：
  - `organization`
  - `communication`
  - `file-sharing`
  - `team-coordination`
  - `project-management`
  - `task-delegation`
  - `task-execution`
- MCP tools：
  - `health`
  - `message`
  - `filesync`
  - `projectflow`
  - `taskflow`

阶段 3 明确不包含：

- `runtime-config` hook 或 `config-sync` skill。
- 顶层 runtime-neutral hooks。runtime trigger、payload、拦截和改写能力依赖具体 runtime，放到各 adapter 自己实现。
- credential guard。凭据访问控制依赖具体 runtime enforcement，先放到 QwenPaw worker/adapter 阶段处理。
- runtime config 读取、轮询、apply、diagnostics。
- AgentSpec package 拉取、应用、回滚或热更新。
- QwenPaw / Claude Code runtime 配置写入。
- plugin 自己启动 5 秒 loop。

先写测试：

- TeamHarness plugin manifest 校验。
- Team prompt 能表达稳定团队协作契约，不包含实时成员、room、model、package version 或 secret。
- Role prompt 能区分 leader、worker、remote member 的职责边界。
- Team skills 覆盖组织关系、沟通、文件共享、任务推进等团队协作能力，并且按角色暴露。
- MCP tools 覆盖 message、filesync、taskflow、projectflow 等团队操作，并有参数/权限/错误行为测试。
- `runtimehealth` 不属于 TeamHarness v0.1 plugin。runtime heartbeat diagnostics 留到 worker/runtime 阶段处理。
- TeamHarness 基础包不声明顶层 hooks；后续 runtime adapter 阶段分别定义 adapter-specific behavior 和测试。
- TeamHarness 基础包可以通过阶段 2 的 plugin CLI 安装。

实现目标：

- `plugins/teamharness` 作为 runtime-neutral 的团队基础能力包存在。
- TeamHarness plugin 不出现 `AGENTTEAM_` 旧前缀，不包含 `runtime-config` 或 `config-sync`。
- 本阶段仍不要求 qwenpaw 或 Claude Code adapter 行为。

## 阶段 4：QwenPaw Worker + Plugin 集成

QwenPaw 集成包含两部分：QwenPaw worker runtime 和 TeamHarness QwenPaw adapter/plugin。基于模拟 controller runtime config 输入完成 worker + plugin 集成。

职责边界：

- Worker 负责启动生命周期、存储恢复、5 秒 desired-state apply loop、runtime heartbeat 上报。desired-state apply loop 读取 runtime.yaml，并在同一条 apply 路径里处理 model、MCP、channel、TeamHarness prompt asset 和 AgentSpec package 变化。
- QwenPaw adapter/plugin 负责把 TeamHarness prompts、skills、MCP 和 QwenPaw runtime glue 安装/接入到 QwenPaw；QwenPaw runtime wrappers 由该 adapter 自己定义。
- Worker 不承载 TeamHarness 团队协作语义；adapter/plugin 不承载 worker 存储生命周期和 desired-state apply loop。
- `runtime-config` parser/helper 放在 QwenPaw worker/runtime 层，不放在 TeamHarness plugin 里。

QwenPaw adapter runtime wrappers：

- 先读 QwenPaw 源码确认可用 hook/middleware/agent toolkit API，不猜测 trigger 名称或 payload 形状。
- Runtime-specific 资产和注册逻辑放在 `plugins/teamharness/adapters/qwenpaw/` 下，不放在 TeamHarness 顶层 manifest。
- Team context：adapter 将 `TEAMS.md` 安装到 QwenPaw agent workspace，并加入 prompt file list；内容合入 TeamHarness team prompt、当前 member role prompt、runtime.yaml 的团队、成员、房间、路由和 AgentSpec package 上下文，不写 secret 或动态任务状态。
- Output sanitizer：adapter 私有包装 QwenPaw tool result 路径，在工具结果进入模型上下文、消息输出或日志前按 `desired.outputSanitize` 和 credential env 值进行 `[REDACTED]` 替换。
- Desired-state apply loop、storage sync、runtime heartbeat 上报仍属于 worker，不通过 hook 实现。AgentSpec package 更新是 desired-state apply loop 的一个分支，不是独立 worker loop。

先写测试：

- QwenPaw worker 能读取模拟 controller 写入的 runtime.yaml：
  `agents/{memberName}/runtime/runtime.yaml`
- QwenPaw worker 每 5 秒检查 runtime.yaml，generation 或 AgentSpec package identity 未变化时不重复 apply。
- runtime.yaml 里的 team/member/storage/desired 事实能被 worker 标准化，并传给 QwenPaw adapter。
- worker 能安装/加载 TeamHarness QwenPaw plugin，但不直接实现 plugin 内部 prompt/skill/MCP 或 adapter runtime wrapper 逻辑。
- adapter 能把 TeamHarness assets 安装进 QwenPaw，且不查询 `agt` CLI。
- adapter 测试覆盖 workspace `TEAMS.md` prompt asset 和 output sanitizer。二者都是 QwenPaw adapter 私有能力，不进入 TeamHarness 顶层 hook contract。
- worker 在同一个 desired-state apply loop 中负责 AgentSpec package 拉取、校验、应用和失败保留上一成功版本。
- worker 失败和 adapter/plugin 失败能分别暴露。
- 集成测试必须基于 qwenpaw worker 镜像运行，使用真实 QwenPaw runtime 和真实 model 调用验证，不用纯 mock 代替 agent 推理。

实现目标：

- QwenPaw worker 和 TeamHarness plugin 能基于契约形状的输入一起工作。

## 阶段 5：Claude Code 集成

实现 Claude Code adapter，并确认 remote worker 与集群 worker 的共同模型和差异点。

共同模型：

- 二者消费同一套 team/member/desired/storage 契约形状，但消费逻辑属于 runtime/adapter，不属于 TeamHarness plugin。
- 二者遵守同一套 TeamHarness 协作协议。

预期差异：

- Remote worker 不由 controller 管进程生命周期。
- Remote worker 不接收 controller 注入的 pod env 或 Kubernetes ServiceAccount mount。
- Remote worker 不一定像集群托管 worker 一样持续 5 秒强制应用 CRD 的 model、MCP 和 AgentSpec package 期望态。

先写测试：

- Claude Code adapter 能安装 TeamHarness assets。
- Claude Code remote runtime 能在本地消费契约形状的 runtime.yaml 或等价配置输入。
- remote-mode runtime.yaml 不假设 pod lifecycle、controller-managed env 或 Kubernetes mount。
- cluster-mode 和 remote-mode 的测试显式记录共同点和差异点。
- 集成测试必须基于真实 Claude Code 本地 runtime 运行，验证本地 plugin/adapter 安装、runtime.yaml 消费和 TeamHarness 协议行为，不用纯 mock 代替本地 runtime。

实现目标：

- Claude Code 能作为 remote/runtime adapter 接入 TeamHarness，不改变托管 QwenPaw 语义。

## 阶段 6：Desired-State Apply 深化测试

通过模拟 runtime config 变化深化测试 desired-state apply loop。

AgentSpec package 更新不是独立于 runtime config polling 的另一套机制，而是同一个 worker desired-state apply loop 的 AgentSpec package 分支。它属于 worker/runtime 能力，不属于 TeamHarness plugin。阶段 6 可以在阶段 4 的 QwenPaw worker 基础上深化。

先写测试：

- `metadata.generation` 变化会触发 desired-state apply。
- generation 不变且 AgentSpec package identity 不变时不会重复 apply。
- `desired.agentPackage.version` 或 `desired.agentPackage.digest` 变化时，同一个 apply loop 会拉取并应用 AgentTeams AgentSpec package。
- AgentSpec package apply 不重启 QwenPaw 进程。
- 拉取、解包或应用失败时保留上一个成功版本，并暴露 not-ready diagnostics。

实现目标：

- AgentSpec package 更新在 runtime 内部完成，不触发 pod restart。
- TeamHarness plugin package 更新不属于这条热更新路径。

## 阶段 7：E2E 测试

新增 QwenPaw 和 Claude Code 的 runtime 级 e2e，并保持现有 e2e 通过。

先写测试：

- QwenPaw e2e 覆盖 runtime.yaml、TeamHarness context、filesync/taskflow、AgentSpec package 应用。
- Claude Code e2e 覆盖 remote adapter 安装和 TeamHarness 协议行为。
- 现有 OpenClaw、CoPaw、Hermes 和 remote-mode 测试仍然通过。
- QwenPaw e2e 必须使用 qwenpaw worker 镜像和真实 model，验证真实推理链路、插件注入、runtime.yaml 消费和 AgentSpec package 应用。
- Claude Code e2e 必须使用真实 Claude Code 本地 runtime，验证 remote 接入、插件注入、runtime.yaml 消费和 TeamHarness 协议行为。

实现目标：

- 新 runtime 路径有 e2e 覆盖，同时不回归旧 runtime 行为。
