# Harness 接入 Task 协作协议

适用于接入新的 Manager、Leader 或 Worker 运行时，以 AgentTeams 1.2.9 候选分支的 `task/v1` 为准。Controller 管理任务、权限、依赖、产物与通知。Harness 将可信入站事件、原生模型上下文和工具执行接到公共 Task 实现。先声明支持的角色，按角色完成接入和验收。[协议细节](task-cli-protocol.md) · [认证场景](task-cli-certification.md)

## 一、启动配置与打包

| 配置或组件 | 要求 | 启动后核对 |
| --- | --- | --- |
| `AGENTTEAMS_CONTROLLER_URL` | 所有托管角色必备。使用容器内可访问的 Controller 基础 URL，公共桥接层追加 `/api/v1` | 用实际运行进程的网络和角色凭据调用公共接口 |
| `AGENTTEAMS_AUTH_TOKEN_FILE` | 执行恢复使用可读的角色凭据文件。共享 Python 恢复默认 `/var/run/secrets/agentteams/token`。公共原生桥接支持 `AGENTTEAMS_AUTH_TOKEN`，各启动入口须验证自己的优先级和挂载路径 | 核对实际凭据身份与 scope，不打印 token。Manager 凭据和 Worker 凭据分别配置 |
| `AGENTTEAMS_AGENT_ROLE`、`AGENTTEAMS_TEAM_NAME` | Team 成员由 Controller 投影实际角色与 owning Team。常见部署值为 `manager`、`team_leader`、`worker`。公共 schema 使用 `leader` 表示 Leader，沿用公共角色归一化。团队身份来自注册配置 | 实际 Leader 拿到 `plan/accept/reject/complete`，Worker 拿到 `begin/submit`。核对 CR、进程环境和模型 schema 三层 |
| `AGENTTEAMS_PROCESS_ID` | 参与原生执行恢复或协调阶段回执的进程必备。实际进程身份用于启动登记、所有权核对与 `X-AgentTeams-Process-ID` | 在真正的启动入口初始化，并在同一进程生命周期保持一致。重启后使用新身份重新登记 |
| `agt`、TeamHarness 公共资产、角色提示词 | 候选镜像必须包含对应版本的 `agt`、公共桥接、原生插件和角色 skill。读取 `AGENTTEAMS_PLUGIN_DIR` 的适配器须从真实 launcher 显式导出路径。Hermes 适配器读取插件目录本身，例如 `/opt/agentteams/plugins/teamharness` | 核对实际进程环境、加载路径和打包文件摘要。Controller 用到的插件父目录配置按其入口定义核对 |
| Matrix 登录与房间绑定 | Runtime 需要有效的 Matrix 连接、原生账号和已登记的会话路由。角色身份和路由由部署与收件链路提供 | 收到实际消息并核验收件人、发送人和房间。分清 Manager 与 Leader 的交接私聊、执行房间、原始 Human 会话 |
| 工作区与存储 | Runtime 启动时准备共享目录、Task 工作区和对象存储访问。共享 CLI 解析文件源时使用 `TEAMHARNESS_SHARED_DIR` 或 `AGENTTEAMS_SHARED_DIR`。共享恢复上传检查点时使用 `AGENTTEAMS_STORAGE_PREFIX` 和可用的 `mc` | 用该角色实际文件权限验证可读输入、可写输出及检查点读写。存储配置留在运行环境，模型只收到 Task 标识、文件引用和准备后的本地路径 |
| Trace | 联调和认证启用 `AGENTTEAMS_OTEL_ENABLED=true` 与可达的 OTLP Collector，设置 `AGENTTEAMS_OTEL_CAPTURE_CONTENT=false`。按运行时实际传输配置 endpoint、协议和必要的认证 header | 在 Collector 或 Langfuse 回查真实 span，核对 Harness 标签、角色、事件 ID 和内容采集开关 |

记录 Controller SHA、Harness SHA、SDK/CLI 版本和实际镜像 ID。构建阶段成功与启动阶段生效各自验收。镜像更新后，核对持久工作区覆盖规则、内置插件标记以及模型真正收到的系统提示词和 schema。直接修改工作区只适用于隔离诊断。

## 二、入站事件与框架字段

### 1. Human 输入

从 Matrix 事件取 `event_id`、`room_id`、`sender_id`，保留用户正文及消息顺序。合批时传入 `matrix_input_sources`，每条都有自己的来源和正文。调用公共 `sources` 生成 `inputId/eventId/roomId/userId` 绑定，模型看到对应用户消息及 `inputId`。相同文本的不同事件保留独立身份。

调用 `create` 时，框架按所选 `inputId` 解析可信来源和输入前沿。桥接层在本次 CLI 子进程内传入 `AGENTTEAMS_ORIGIN_EVENT_ID`、`AGENTTEAMS_REQUESTER_ROOM_ID`、`AGENTTEAMS_REQUESTER_USER_ID`，按需传入 `AGENTTEAMS_INPUT_FRONTIER`、`AGENTTEAMS_INPUT_ID` 和 `AGENTTEAMS_RELATED_INPUTS`。这组字段由框架提供。模型调用只提交 schema 中的参数。

收到 `outcome=input_changed` 和 `noTaskRegistered=true` 时，把服务端校验过的新消息作为原生 **user** 输入持久化，再继续推理和建单。确认每条请求时使用绑定该来源事件的创建回执。撤回、补充与独立新任务由 Agent 结合用户原文判断。

创建回执校验通过后，把桥接返回的 `contextUpdate.registeredInputs` 合并到当前原生回合状态，并交给后续 Task 调用。该字段保存已登记的来源事件 ID，包括这次创建明确绑定的更正事件。公共桥接在回执中列出尚未覆盖的 `inputId`，供 Agent 分别判断后续动作。回复问题、处理撤回和执行 AgentTeams 运维仍由 Manager 按自己的权限完成。单条请求以及已覆盖全部来源时无需额外的 Task 调用。

验证合批时，至少覆盖正文相同的两个独立请求、原请求加更正、原请求加撤回三种输入。第一种场景核对每个 Matrix 来源恰有一个主任务。发现新输入后，核对该来源的 `input_changed` 与最终登记来源，回执中的其他来源状态不能替代这项核对。

### 2. 协调阶段与执行通知

按阶段核对公共通知回执。入站 Matrix 文本用于业务上下文，阶段授权来自 `GET /api/v1/coordination/events/{id}` 的已登记记录。检查协议 `task/v1`、协调事件 ID、实际投递的 Matrix 事件、房间、发送人、接收人及阶段要求的任务身份。

| 事件 `kind` | 核对关键字段 | 原生框架准备 |
| --- | --- | --- |
| `handoff` | 协调事件 ID、当前 Matrix 事件 ID、接收 Leader、公共回执确认的主任务 | 调用阶段 `begin`。只有 `active=true` 才使用返回的 `pending_task_ids` 开启规划，模型使用同一主任务并读取交接正文里的原始需求与注册执行成员 |
| `task_assigned` | `task_id`、`main_task_id`、`delivery_id`、接收 Worker，已有 Project 时核对 `project_id` | 经公共 `begin` 领取并准备输入。用权威执行身份及目录开启工作。已收到有效准备回执时直接使用 |
| `task_submitted` | `task_id`、`main_task_id`、`submission_id`、接收 Leader | 阶段 `begin` 后经 `get` 取得该候选。限制工具为公共 Task 操作，允许查证并验收或退回 |
| `main_task_ready` | `main_task_id`、接收 Leader、全部节点已验收 | 阶段 `begin` 后经 `get` 取得已验收结果及文件版本，完成报告后调用 `complete` |

阶段 `begin/retry` 使用 `POST /api/v1/coordination/events/{id}/phase`。请求字段为 `action`、`native_event_id`、`process_id`。HTTP 边界的 `native_event_id` 使用 Matrix `$eventId`，公共记录把投递身份表示为 `matrix:$eventId`。优先复用 `phase_recovery.notify` 或原生桥接 `admit_event`，两个入口都要传裸 Matrix 事件 ID，例如 `$example`。传入 `matrix:$example` 会被 Controller 拒绝。公共 `prepare_event` 校验并产生 `planning`、`execution`、`review` 或 `completion` 作用域。固定 Task 工具须使用当前作用域。

Worker 使用实际启动入口调用 `POST /api/v1/runtime/matrix` 登记 `action=start`、`instance_id`、`process_id`。读取状态 ETag 后带 `If-Match`，首次创建使用 `If-None-Match: *`。请求头 `X-AgentTeams-Process-ID` 必须与请求体一致。

执行恢复使用 Controller 领取返回的 `task_id/team_id/instance_id/process_id/generation` 和条件写入版本。重启、重复投递、旧执行代次、传输失败及工具结果未知都通过公共恢复检查处理。已保存回复走仅投递路径。批量或嵌套工具只有在子调用逐一经过准入并保存回执时启用。

## 三、模型可见的 Task 工具

基于 `task_native.py` 的 `schema` 和 `task` 接入，复用公共角色 schema 及 CLI 语义。注册时调用 `{"operation":"schema","role":"leader"}` 取得角色 schema。调用工具时传 `operation="task"`、模型参数对象 `arguments` 和框架对象 `context`，后者包含真实 `role`、当前阶段作用域及建单所需的来源绑定。固定工具的身份须关联实际 SDK 注册与受信桥接进程。它使用任务系统已有权限。Shell、其他 MCP 和文件工具依原有权限执行。托管执行和协调回合限制旧 TaskFlow、ProjectFlow、FileSync，普通消息能力用于沟通。

| 操作 | 角色 | 必填 / 条件必填 | 可选 |
| --- | --- | --- | --- |
| `create` | Manager | `leader`、`title`。多条来源可选时 `inputId` | `relatedInputIds` 仅关联同一请求的补充或更正，`spec` 仅补充必要先前上下文 |
| `plan` | Leader | 主 `taskId`、`nodes`。每节点 `id/assignedTo/spec/dependsOn`，独立节点显式 `[]` | 节点 `title` |
| `begin` | Worker | 执行 `taskId` |  |
| `submit` | Worker | 执行 `taskId`、`summary`。需要交付文件时传 `files` | `report`、`status` |
| `accept`、`reject` | Leader | 执行 `taskId`、精确 `submissionId`、`note` | `reject` 可用 `replacementSpec` 描述返工 |
| `complete` | Leader | 主 `taskId`、最终 `report` | `artifacts` 指定已验收文件，默认使用已验收输出 |
| `cancel` | Manager 或 Leader | 主 `taskId`、`reason` |  |
| `get`、`files_list` | 按角色权限 | `taskId` |  |
| `files_read`、`files_get` | 按角色权限 | `taskId` 加 `name` 或 `artifactId` | `version`；`files_read` 支持 `offset/limit` |
| `files_put` | Worker | 执行 `taskId`、`source`、`name` | `kind=output/intermediate` |

只有任务 ID 存在歧义且接口明确要求时才传 `team`。计划依赖引用局部步骤 ID，实际执行 Task ID 由 Controller 分配。文件提交使用准备好的输出目录下的相对路径。`files_read` 返回有边界的文本，`files_get` 返回经过校验的本地路径。

## 四、回执、交付与可观测性

- **工具回执。** 框架把已验证的返回事实放入当前工具上下文，描述实际状态、候选版本与通知状态。排队、发出、接收和验收分别以各自证据为准。stdio 原生桥接的 stdout 输出严格 JSON。诊断日志使用 stderr。
- **终态回告。** Controller 用最初派单身份将报告和附件投递到原始 Human 会话。Manager 框架通过公共 `delivery_context` 或已确认的 `GET /api/v1/task-deliveries?roomId=…` 读取事实，在原会话自然进入模型的轮次同步。可用 `inputText` 重取用户明确引用的 Task。确认进入实际上下文后再持久化 `ack_delivery_context`。会话压缩后，终态事实仍需到达当前模型调用。
- **来源追踪。** 模型和工具保留实际 session/turn ID，导出通过真实模型输入核验的 `agentteams.input.event_ids`、协调 `agentteams.coordination.id/kind`、执行 `agentteams.execution.generation`，以及实际事件、房间、角色和任务关联。W3C 追踪载体随单次调用与延迟回执保存。合批共享的生成和工具 span 在组级去重。
- **异常处置。** 区分明确失败、尚在执行、结果未知、已提交待回告。用公共状态与真实回执核对，避免按文案或相似任务补单。可恢复的传输错误沿用相同已登记身份；遇到权限、过期进程或未知副作用先核验权威状态。

## 五、接入验收与已遇问题

| 实跑症状 | 首查 | 验收证据 |
| --- | --- | --- |
| Worker 能调用旧文件工具，公共执行绑定却没启动 | 实际 launcher 是否将 `AGENTTEAMS_PLUGIN_DIR` 导出到子进程。旧角色是否仍装入持久配置 | 同一镜像从正式入口启动，实际 Worker 使用 `begin/submit`，受限回合 schema 与 SDK 注册匹配 |
| Leader 收到任务却按 Worker 工具运行 | Controller 投影的角色与 Team，SDK 恢复的角色配置 | Leader 实际提交原主任务计划，Worker 用相应执行 ID 接单 |
| JSON 解析报错但进程仍在工作 | stdout 是否混入日志。CLI 路径及任务桥接的原生连接对象是否正确 | 实际子进程返回可解析 JSON，stderr 可见诊断，固定注册可证明 |
| Manager 转发消息或说已建单，实际没有 Main Task | 当前角色提示词、实际模型 schema、原会话旧上下文、该请求回执的来源事件 | 对比实际模型入口与 Controller 来源登记；同正文两单仍需逐条核验。保留旧会话失败 |
| 开始或提交后卡在待核验 | 执行代次、候选对应关系、工具是否绕过原生准入、真实运行状态 | 正常路径提交可验收；超时、重复投递和已知副作用可用公共状态判定。未知结果按协议保留 |
| 回告已发，追问仍回答排队 | 原始会话是否与路由一致，已确认的终态事实是否进了压缩后的模型调用 | 在原会话追问真实交付文件或结果，核对回复和公开 Trace |
| Trace 全无或角色标签错误 | 运行时 OTLP 出口、网络 DNS、适配器字段、关闭正文采集 | Collector 回查实际模型、工具和对应 Task/Matrix 事件 |

1. 用正式启动入口验证环境和原生 SDK 绑定，确认当前提示词、角色 schema、严格桥接响应和真实 Trace 可见。测试夹具应使用入口派生配置，以覆盖镜像内漏传变量的问题。
2. 从 Human 与 Manager 的原始会话派任务。用固定例子验证简单、复杂和并发，逐条记录原始事件、Main Task、执行节点、精确候选验收、真实回告和下载文件摘要。额外验证同正文独立请求、建单前撤回、更新输入、拒绝返工和终态回执。
3. 当前 1.2.9 验证对每种 Harness 使用三组简单、三组复杂任务，随后执行混合 Harness 双向验证。分开记录协议结果、业务 oracle、完整交付耗时、分阶段耗时、工具与 token，保留失败和人工恢复往返过程。[认证口径](task-cli-certification.md)

实现参考

- [公共原生桥接及角色 schema](../../../plugins/teamharness/mcp/common/task_native.py) · [公共 Task CLI 参数](../../../plugins/teamharness/mcp/common/task_cli.py) · [阶段准备](../../../plugins/teamharness/mcp/common/task_context.py)
- [原生协调阶段](../../../agentteams-controller/internal/server/coordination_phase.go) · [执行恢复](../../../shared/lib/task_recovery.py) · [原会话的终态交付记录](../../../agentteams-controller/internal/server/task_delivery_context.go)
