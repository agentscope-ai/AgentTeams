# Dashboard 使用指南

[English](../../usage/dashboard.md) | 中文

本指南面向 Dashboard v1.4.1 与 Core v1.2.7。Dashboard 独立编号；新部署参见[快速入门](../quickstart.md)，连接已有集群参见 [Dashboard 独立部署指南](https://github.com/agentteams-group/agentteams-dashboard/blob/main/README.zh-CN.md)。

## 首次登录与模型配置

本地安装器默认启用 `http://127.0.0.1:13000/`，自动打开浏览器、预填 `admin`，并将生成的密码复制到剪贴板。无法使用桌面集成时，使用终端打印的地址与凭证。Bash 和 PowerShell 都支持此流程。

安装无需模型密钥。Manager 尚未配置模型时，点击会话中的「配置模型」，在模型管理中设置 Provider、选择 Model，保存并验证后返回会话。Provider 和 Model 分两步选择。可见管理操作取决于账号权限；没有模型管理权限的成员，仍可与有权访问且已配置的 Agent 对话。

## 找到对应功能

v1.4.1 左侧导航如下；实际可见项目由权限、部署模式和功能开关决定。

| 入口 | 用途 |
|---|---|
| **助手** | Manager 与 Agent 会话，查看回复和执行过程 |
| **我的团队** | 当前 Human 可访问的团队及协作上下文 |
| **工作台** | 任务看板、产物、编码执行、循环和 **Tracing** |
| **资产中心** | 模型、工具/MCP、技能、知识和文件 |
| **成员与团队 → 团队与Agent** | 统一列出团队、Manager 和未归属 Worker，在页面内展示拓扑与详情 |
| **设置** | 账号与外观设置；手动选主题或跟随系统 |

选择 Agent 可查看运行时、模型、状态及可用的会话和生命周期操作。选择模型时先选 Provider，再选 Model。资源管理与对话权限分别校验，管理按钮不可见不代表不能与 Agent 对话。

## 对话与人工待办

同一轮思考和工具调用合并展示，执行中显示有界实时预览，结束后自动折叠；需要检查中间过程时再展开。

任务看板和会话内的任务上下文展示进度及待处理的人工事项。打开对应任务，检查请求并提交决策。补发一条聊天消息不一定会解决已有的结构化人工请求。需要时使用可用的暂停/恢复操作；生命周期语义见 [Project Workflow API](project-workflow-api.md)。

语音输入先把识别文本写入草稿，检查后再发送。本地识别与可选服务配置见 [Dashboard 语音输入指南](https://github.com/agentteams-group/agentteams-dashboard/blob/main/docs/voice-input.md)。

## 技能与个人文件

在「资产中心 → 技能」上传完整 Skill ZIP，并分发到 Worker。仅上传到目录不会给 Worker 分配技能。分发会写入完整包、更新 `Worker.spec.skills`，并尝试重启 Worker，请在空闲时操作。包结构和验证方法见 [Worker 指南](worker-guide.md#方式二通过-dashboard-分发)。

Human 个人文件与 Agent 工作空间分别管理。向 Agent 提供个人文件时，使用个人文件共享或在会话中发送文件引用。系统管理员同样拥有个人 Human 文件区。权限与存储说明见[人类工作区](human-work-area.md)和[运营人员文件管理](operator-human-files.md)。

## 可选的 Tracing

打开「工作台 → Tracing」。采集默认关闭，未配置时会显示引导。管理员可打开「可观测配置」，填写查询后端与已有的 OTLP/HTTP Collector，测试、保存，并显式应用到已有的受支持 Worker。

Core v1.2.7 将配置保存在 Controller 命名空间的 `agentteams-observability` Secret（`config.json`）。Langfuse 查询和查询密钥由 Controller 管理。Dashboard 不再使用旧本地 `observability.json`；从这种旧配置升级后，需要通过界面重新保存配置。密钥字段留空表示保留已保存值。

保存不会立即重启运行中的 Worker。新建的受支持容器会读取保存的配置；对已有托管 QwenPaw/Hermes Worker 执行「应用」时，会通过正常环境更新和容器重建流程生效。请在空闲时应用，并逐个检查结果。关闭采集也需对已有容器执行应用。配置或连接测试成功不代表已入库：应执行一次简短对话或任务，再查询对应 Trace。

使用 Langfuse 时，Collector 必须已配置向目标项目导出。AgentLoop 配置也由 Controller 保存，但 Dashboard 的 AgentLoop 查询适配器使用其服务端云身份；详见 [Dashboard 后端契约](https://github.com/agentteams-group/agentteams-dashboard/blob/main/docs/INTERFACES.md)。切换查询后端不会自动配置 Collector 出口。

无需先创建正式任务，也可以查询有权访问的 Langfuse Trace：

```bash
docker exec agentteams-controller agt tracing --agent alice --since 1h
docker exec agentteams-controller agt tracing --team my-team --limit 20 --page 1 -o json
docker exec agentteams-controller agt tracing session '<session-id>' -o json
docker exec agentteams-controller agt tracing show '<trace-id>'
docker exec agentteams-controller agt tracing task '<project-id>' '<task-id>' --team my-team --related
```

Agent 过滤器对应 Worker 资源，也包括 Team Leader。未指定时间的普通列表默认查最近 24 小时；Session 查询不套用此限制，覆盖实际保留的历史。列表需要分页。查询只返回当前身份有权访问且运行时实际上报的证据，没有 Trace 不代表没有执行。Controller 契约及采集边界见[任务 Trace 设计](../../design/task-tracing-analysis.md)。
