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

契约必须明确：`desired.agentPackage` 是 HiClaw AgentSpec package，不是 TeamHarness plugin package。

TDD 目标：

- 本阶段以文档评审作为验收。

## 阶段 2：Plugin 管理逻辑与 CLI 集成

先做 plugin 管理逻辑和 CLI 功能，不急着接入具体 TeamHarness 内容。

先写测试：

- plugin manifest 可以被 CLI 发现、解析和校验。
- CLI 能执行 plugin list / inspect / install / uninstall 或当前项目已有的等价命令。
- 安装逻辑能把 plugin asset 复制到约定位置。
- 重复安装是幂等的。
- 缺失 manifest、非法 manifest、asset 缺失时有明确错误。
- uninstall 只移除该 plugin 自己安装的内容，不误删 runtime 或其它 plugin 文件。

实现目标：

- plugin 管理逻辑和 CLI 功能可测试、可复用。
- 本阶段不要求 TeamHarness 业务能力完整，也不要求 runtime adapter 已存在。

## 阶段 3：TeamHarness 基础包集成

在 plugin 管理逻辑可用后，再迁移 TeamHarness 基础包。

先写测试：

- TeamHarness plugin manifest 校验。
- Team prompt 能表达稳定团队协作契约，不包含实时成员、room、model、package version 或 secret。
- Role prompt 能区分 leader、worker、remote member 的职责边界。
- Team skills 覆盖组织关系、沟通、文件共享、任务推进等团队协作能力，并且按角色暴露。
- MCP tools 覆盖 message、filesync、taskflow、projectflow、runtimehealth 等团队操作，并有参数/权限/错误行为测试。
- Hooks 覆盖 team context 注入、credential guard、output sanitizer、message route、runtime config 等团队运行态逻辑，并有降级行为测试。
- TeamHarness 基础包可以通过阶段 2 的 plugin CLI 安装。

实现目标：

- `plugins/teamharness` 作为 runtime-neutral 的团队基础能力包存在。
- 本阶段仍不要求 qwenpaw 或 Claude Code adapter 行为。

## 阶段 4：QwenPaw Worker + Plugin 集成

QwenPaw 集成包含两部分：QwenPaw worker runtime 和 TeamHarness QwenPaw adapter/plugin。基于模拟 controller runtime config 输入完成 worker + plugin 集成。

职责边界：

- Worker 负责启动生命周期、存储恢复、runtime config 轮询、AgentSpec package 拉取与应用、runtime health。
- QwenPaw adapter/plugin 负责把 TeamHarness prompts、skills、MCP、hooks 和 QwenPaw runtime glue 安装/接入到 QwenPaw。
- Worker 不承载 TeamHarness 团队协作语义；adapter/plugin 不承载 worker 存储生命周期和 package 轮询。

先写测试：

- QwenPaw worker 能读取模拟 controller 写入的 runtime.yaml：
  `shared/runtime/members/{memberName}/runtime.yaml`
- runtime.yaml 里的 team/member/storage 事实能被 plugin adapter 使用。
- worker 能安装/加载 TeamHarness QwenPaw plugin，但不直接实现 plugin 内部 prompt/skill/MCP/hook 逻辑。
- adapter 能把 TeamHarness assets 安装进 QwenPaw，且不查询 `hiclaw` CLI。
- worker 失败和 adapter/plugin 失败能分别暴露。
- 集成测试必须基于 qwenpaw worker 镜像运行，使用真实 QwenPaw runtime 和真实 model 调用验证，不用纯 mock 代替 agent 推理。

实现目标：

- QwenPaw worker 和 TeamHarness plugin 能基于契约形状的输入一起工作。

## 阶段 5：Claude Code 集成

实现 Claude Code adapter，并确认 remote worker 与集群 worker 的共同模型和差异点。

共同模型：

- 二者消费同一套 team/member/desired/storage 契约形状。
- 二者遵守同一套 TeamHarness 协作协议。

预期差异：

- Remote worker 不由 controller 管进程生命周期。
- Remote worker 不接收 controller 注入的 pod env 或 Kubernetes ServiceAccount mount。
- Remote worker 不一定像集群托管 worker 一样持续强制应用 CRD 的 model 和 MCP 期望态。

先写测试：

- Claude Code adapter 能基于模拟 controller 写入的 runtime.yaml 安装 TeamHarness assets。
- remote-mode runtime.yaml 不假设 pod lifecycle、controller-managed env 或 Kubernetes mount。
- cluster-mode 和 remote-mode 的测试显式记录共同点和差异点。
- 集成测试必须基于真实 Claude Code 本地 runtime 运行，验证本地 plugin/adapter 安装、runtime.yaml 消费和 TeamHarness 协议行为，不用纯 mock 代替本地 runtime。

实现目标：

- Claude Code 能作为 remote/runtime adapter 接入 TeamHarness，不改变托管 QwenPaw 语义。

## 阶段 6：热更新设计与测试

通过模拟 runtime config 变化测试热更新。

先写测试：

- `metadata.generation` 变化会触发 runtime config apply。
- generation 不变或 digest 不变时不会重复 apply。
- `desired.agentPackage.version` 或 `desired.agentPackage.digest` 变化时，会拉取并应用 HiClaw AgentSpec package。
- 热更新不重启 QwenPaw 进程。
- 拉取、解包或应用失败时保留上一个成功版本，并暴露 unhealthy diagnostics。

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
