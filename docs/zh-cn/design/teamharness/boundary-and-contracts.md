# TeamHarness v0.1 边界与契约

本文档定义当前 TeamHarness 插件包的功能边界。

TeamHarness v0.1 是运行时无关的团队协作基座。它打包了稳定的提示词、团队技能、MCP 工具、生命周期脚本与运行时适配器入口。它不拥有 Worker 生命周期、控制器调和（reconcile）、Worker 期望状态应用循环、运行时钩子行为，或周期性工作区持久化。

## 职责

TeamHarness 拥有：

- 稳定的团队协作提示词与角色提示词。
- 用于组织、通信、共享文件、项目、任务委派与任务执行的团队协作技能。
- 用于团队消息、共享文件、项目流、任务流与插件健康的显式 MCP 工具。
- 单个插件 tarball，默认经 AgentTeams `agt` CLI 安装，并兼容 LoongSuite `plugin-probe` 以用于本地运行时。

TeamHarness 不拥有：

- 控制器生成 `agents/{runtimeName}/runtime/runtime.yaml`。
- Worker 进程生命周期、Pod 重启或运行时进程监督。
- Worker 期望状态的解析、轮询、应用或诊断。
- 运行时无关的顶层钩子。
- 运行时钩子的触发契约、载荷格式与强制执行行为。
- 依赖运行时特定文件或工具守卫支持的凭据访问强制。
- Worker 期望状态应用循环内部的 AgentSpec 包下载、应用、回滚或更新。
- 周期性工作区推/拉循环。
- 基座包内对 QwenPaw 或 Claude Code 运行时的直接变更。
- 密钥值存储。

## 契约关系

控制器到运行时：

- 控制器将非密期望状态与团队事实写入 `agents/{runtimeName}/runtime/runtime.yaml`。
- 密钥保留在环境变量、挂载文件或服务账户令牌中。

运行时 Worker 到 TeamHarness：

- Worker 为所选运行时安装或暴露 TeamHarness 资产。
- Worker 拥有期望状态应用循环，包括运行时配置轮询与 AgentSpec 包应用。
- Worker 可以调用 TeamHarness MCP 工具，但 TeamHarness 自身不轮询 CR 状态或对象存储。

运行时适配器到 TeamHarness：

- 适配器将 TeamHarness 的提示词、技能与 MCP 映射到具体运行时。
- 运行时特定钩子位于适配器实现之下（例如 `adapters/qwenpaw/hooks/`），前提是相应运行时集成阶段定义了它们。
- 运行时配置的消费属于 Worker/运行时适配层，而非 TeamHarness 插件包。
- 适配器应消费控制器写入的运行时配置事实，而不是查询 `agt` CLI 以获取团队或成员身份。

TeamHarness 插件包到 AgentSpec 包：

- TeamHarness 插件包是运行时基础设施。
- `runtime.yaml` 中的 `desired.agentPackage` 是 AgentTeams AgentSpec 包，属于 Worker 期望状态应用路径。
- 更新 AgentSpec 包不得被建模为更新 TeamHarness 插件包。

## 标准资产集

提示词：

- `prompts/team/TEAMS.md`
- `prompts/agent/leader.md`
- `prompts/agent/worker.md`
- `prompts/agent/remote-member.md`
- `prompts/manager/AGENTS.md`
- `prompts/manager/TOOLS.md`
- `prompts/manager/HEARTBEAT.md`

技能：

- Agent 技能：`mcporter`、`find-skills`。
- 团队技能：`organization`、`communication`、`file-sharing`、`team-coordination`、`project-management`、`task-delegation`、`task-execution`。

MCP 工具：

- `health`
- `message`
- `filesync`
- `projectflow`
- `taskflow`
