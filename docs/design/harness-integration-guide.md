# AgentTeams Harness 接入与准入标准

本指南定义 Harness 接入 AgentTeams（AT）的能力要求和准入标准，覆盖运行时启动、Task 协作、会话与回执、重启续跑和 Worker 跨 Harness 切换。Task 协作使用公共协议 `task/v1`。Harness 是承载模型交互、原生工具和会话的运行时。接入时先声明支持的角色（Manager、Leader、Worker）和部署模式，按声明范围验收。外部纳管模式复用同一协作语义，按自身部署方式提供身份、会话和执行证据。托管镜像的启动变量见下一节。

任务房间的消息顺序、业务结论展示及上下文交接，另见[标准数据流与 30 个历史反例（讨论稿）](task-room-data-flow-standard.zh-CN.md)。接入或修改 Task 协作时，先阅读反例和场景矩阵，按 `FLOW-xx` 记录适用案例、实际证据及通过／失败／未验证／跳过状态。发现同类问题时定位并修复对应生产或消费路径，不以隐藏消息、清空状态或放宽判定代替修复。反例被记录不等于已经修复；讨论稿的目标行为仍待确认。

## 接入边界

| 位置 | 维护内容 |
| --- | --- |
| Core | 公共状态机与权限、任务及文件 API、CLI 和原生桥接、角色提示词与技能、测试用例和判定器。这里维护接入指南、运行时注册与构建引用 |
| 独立 Harness 仓库 | 原生 SDK/工具注册、Matrix 或受信通道适配、会话和回执注入、启动入口、镜像构建及原生回归 |

**实现归属是接入的固定原则。** 通用权限、任务与运行身份绑定、状态机、审批语义、消息和文件协议、steer 的来源授权与路由规则统一在 Core 维护，不能在各 Harness 为某个角色或某个故障复制业务判断，也不能在 Dashboard 隐藏错误来补偿不合约行为。各 Harness 的 SDK 调用、配置/消息格式转换、原生会话持久化和输入接收回执适配必须放在对应独立 Harness 仓库；Core 不嵌入某个 SDK 的实现。公共逻辑改变时，先更新同一套契约和回归，再验证各原生适配是否正确消费；已有原生能力直接复用，不修改 SDK 内部来实现 AT 业务权限。

Core 中的 `worker/`、`qwenpaw/`、`hermes/`、`deepseek-harness/`、`openhuman/` 是对应独立仓库的 Git 子模块路径。Codex Harness 由发布工作流独立检出。原生适配的改动提交到所属仓库，再在 Core 更新对应引用。能复用同一镜像的多个角色使用同一构建产物，由启动配置选择角色。[子模块定义](../../.gitmodules) · [Harness 发布工作流](../../.github/workflows/publish-harnesses.yml)

在启动入口读取 Controller 投影的成员配置，加载模型、工具、技能和通道，绑定稳定的成员身份并报告实际可用状态。Native SDK 的配置和会话数据由 Harness 维护，公共配置及持久状态以 Core 的契约为准。

一次任务从 Human 原会话进入。Manager 用 `create` 绑定原始请求和 Leader；Leader 在同一主任务上用 `plan` 自主决定节点、分工和依赖；Worker 经 `begin` 领取并准备输入，完成后用 `submit` 保存候选和产物。Leader 用精确 `submissionId` 验收或退回，Controller 根据已验收结果推进依赖。Leader 用 `complete` 完成主任务，框架将最终报告和确定版本的附件投递回原会话，并把交付事实同步到派单 Agent 的上下文。取消、重试和恢复也沿用同一套任务身份、状态及回执。

接入按顺序完成三件事。

1. 在 Harness 仓库实现正式启动入口和角色配置，接入公共 Task 工具、可信入站、原生回执和恢复。以真实 SDK 注册、实际模型请求和进程环境为证据。
2. 在 Core 对接运行时注册与构建引用。使用同一候选的 Core 和 Harness 版本构建或部署，验证实际加载的公共资产、角色、运行时身份和镜像摘要。
3. 用真实会话跑完下述准入验证。组合间发现协议缺口时，在公共契约或产生不合约事实的一侧修复。

[公共协议与 CLI](teamharness/task-cli-protocol.md) · [成员运行时配置](member-runtime-config-contract.md) · [统一健康接口](harness-health.md)

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
| Trace | 联调和认证启用 `AGENTTEAMS_OTEL_ENABLED=true` 与可达的 OTLP Collector。全局内容采集默认开启，显式 `AGENTTEAMS_OTEL_CAPTURE_CONTENT=false` 关闭正文。按运行时实际传输配置 endpoint、协议和必要的认证 header | 在 Collector 或 Langfuse 回查真实 span，核对 Harness、角色、Task、事件 ID、模型与工具输入输出以及服务实际返回的推理正文；同时验证关闭采集与凭证脱敏 |

所有托管 Harness 镜像提供 `/opt/agentteams/harness-health`，按统一契约报告原生就绪和模型探测结果。Controller 只消费统一结果和下发探测请求；模型协议、凭据解析与原生探测桥接留在 Harness 仓库。进程存活、Matrix 就绪和模型可用分别验收，旧镜像缺少接口时为 unknown。详见[统一健康接口](harness-health.md)。

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

支持异步工具的 Harness 要保留原始调用 ID。工具返回运行句柄时，框架继续使用同一次调用的等待机制，直到取得结束状态并为原操作保存回执。后续工具和任务提交遵循现有准入规则。接入验证需让真实原生工具跨过首次等待窗口，检查后续等待能够取得终态、回执关联原调用、任务继续推进。进程中断后缺少终态证据时，仍走公共核验流程。

框架能确认原生注册工具只读时，在准入日志为该次调用记录 `read_only: true`。收到同一调用的终态错误后，用 `status: error` 和 `effect: none` 保存错误回执并恢复后续工具准入。核对真实 SDK 注册身份及调用 ID，工具参数和输出文字不参与只读能力判断。缺少可信终态或副作用未知时保留公共核验。

MCP 经过原生 Driver 或工具包装器后，也要保留已配置服务的工具定义和只读声明；不能因包装器丢失 annotations，就把有据可查的只读查询失败统一变成未知写操作。声明缺失或为 false 时不推断只读，更不能按工具名猜测。验收应覆盖同一注册工具的只读终态错误、未声明只读的传输失败以及伪造参数/结果声明。保留未知操作时仍应保存并展示原生错误，避免通用隔离提示覆盖实际原因。

SDK 能以结构化错误证明在任何副作用前拒绝时，框架可在准入记录的 `pre_effect_error_codes` 声明该工具的精确错误码。回执须关联原调用 ID，并同时提供 `status: error`、`effect: none` 和匹配的 `error_code`。错误码来自原生工具结果，准入列表来自 SDK 对接代码。

### 有状态 MCP 的会话恢复

MCP 连接会话与任务、聊天及原生 Agent 会话分别管理。HTTP MCP 服务重启、清理闲置会话或负载路由变化后，原会话可能失效。恢复该连接不能清空任务上下文，也不能要求用户重新配置有效凭证。

各 Harness 通过其原生 MCP 客户端重建连接、重新初始化和发现工具；Core 继续使用统一操作回执协议，不增加运行时或 MCP 服务名称判断。只读声明取自真实服务发现。成功重连只证明连接可用，不证明失败调用没有产生副作用。

- 服务返回有效 `CallToolResult`（含 `isError: true`）：保存同一调用的终态错误；不能将业务错误伪装成断线，也不能默认写入 `effect: none`。
- 协议明确的会话过期：重建会话。只有适配器持有受信的原生未派发证明时，才使用准入时声明的结构化错误码结束原操作；不解析模型输出或任意错误文案授权重放。
- 超时、断联、取消或服务端执行后响应丢失：保留未知副作用，不能把“重连成功”视为允许重做写操作。
- 会话失效与鉴权失效分别处理；有效 Token 不应因会话失效被要求重新录入。

接入验收必须用真实 HTTP MCP 服务注入一次会话失效，验证旧调用的实际执行次数、原生会话 ID 更新、下一调用成功及 Task 操作回执。还应覆盖业务失败、只读工具、写工具响应丢失、重新注册后的审批策略，以及通用 MCP 服务而非仅 GitHub。仅验证 `tools/list` 或模型连接短测不能代替这些用例。


## 三、模型可见的 Task 工具

托管回合通过受信注册的原生 Task 工具执行状态变更。通用 shell 会在运行前登记待核验操作，使用 shell 调用 `agt task submit` 会和提交所需的 active 执行状态互斥。独立 CLI 入口需要继承当前 Worker 的有效运行身份。

从当前 SDK 的工具列表读取 Task 工具的完整名称，包含运行时添加的命名空间，并把它放进当前回合上下文。核对模型收到的工具 schema、上下文里的名称与实际执行的名称一致。托管回合缺少该工具时应报告接入异常，保留原阶段状态。

提交的 `files` 使用相对输出目录的 JSON 数组，单文件也使用数组。完成时默认交付已验收产物。显式选择时使用已验收结果中的 `artifactId`，版本哈希用于校验内容。公共工具在登记变更前核对显式选择。

执行通知给出本次任务的 `outputDirectory` 和 `scratchDirectory`。原生写入、编辑工具的相对路径从 `outputDirectory` 解析，写入前核对其解析后的目标位于这两个目录之一。错误路径直接返回可纠正的工具错误，不能先在别的任务目录落盘，再让临时删除走审批。

透传公共任务上下文的执行说明。Worker 用原始需求理解上下文，按当前节点的委派范围交付。简单读写优先使用原生文件工具，节点需要运行命令或补丁时由对应工具执行并按其风险策略审批。

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

只有任务 ID 存在歧义且接口明确要求时才传 `team`。计划依赖引用局部步骤 ID，实际执行 Task ID 由 Controller 分配。Leader 向本团队独立注册的 Worker 指派执行节点。收到 `invalid_task_plan_assignee` 且 `effect=none` 时，可以更正指派后重提同一任务的计划。文件提交使用准备好的输出目录下的相对路径。`files_read` 返回有边界的文本，`files_get` 返回经过校验的本地路径。

## 四、回执、交付与可观测性

- **工具回执。** 框架把已验证的返回事实放入当前工具上下文，描述实际状态、候选版本与通知状态。排队、发出、接收和验收分别以各自证据为准。stdio 原生桥接的 stdout 输出严格 JSON。诊断日志使用 stderr。
- **终态回告。** Controller 用最初派单身份将报告和附件投递到原始 Human 会话。Manager 框架通过公共 `delivery_context` 或已确认的 `GET /api/v1/task-deliveries?roomId=…` 读取事实，在原会话自然进入模型的轮次同步。可用 `inputText` 重取用户明确引用的 Task。确认进入实际上下文后再持久化 `ack_delivery_context`。会话压缩后，终态事实仍需到达当前模型调用。
- **来源追踪。** 模型和工具保留实际 session/turn ID，导出通过真实模型输入核验的 `agentteams.input.event_ids`、协调 `agentteams.coordination.id/kind`、执行 `agentteams.execution.generation`，以及实际事件、房间、角色和任务关联。W3C 追踪载体随单次调用与延迟回执保存。合批共享的生成和工具 span 在组级去重。
- **异常处置。** 区分明确失败、尚在执行、结果未知、已提交待回告。用公共状态与真实回执核对，避免按文案或相似任务补单。可恢复的传输错误沿用相同已登记身份；遇到权限、过期进程或未知副作用先核验权威状态。

## 五、重启续跑与跨 Harness 切换

Worker 接入必须支持同一 Harness 重启续跑，以及从已有 Harness 切入和向已有 Harness 切出。这里的切换是同一个 Worker 替换执行运行时，有实际停机和接管边界。异构 Leader、Worker 同时协作在第七节单独验证。

阶段性聊天回复已送达不代表执行 Task 已提交。对仍在进行中的 Task，当前有效执行者可以重新准备输入并继续；下一次原生工具操作清除已送达回复的投递字段，保留完成步骤和工具证据。回复尚未送达时只恢复投递；未知副作用、过期执行身份及已提交或已完成的 Task 仍不能借此重新执行业务。

1. **启动时自动发现。** 从公共配置恢复 Worker 身份、团队及通道，读取已接受但未处理的输入，主动发现已分配的可恢复 Task。即使房间没有新消息，启动入口也要完成发现和接管。中断前保存的进度、文件和回执由公共存储提供。
2. **切换时隔离旧执行。** 通过受支持的生命周期入口停止源实例的新执行，核实其进程和工具状态，再启动目标 Harness。用目标实际实例、进程及新执行代次领取同一 Task。源实例的迟到工具、提交或回告不能覆盖新代次。
3. **按检查点决策。** 已确认完成的步骤和产物保持不变，只续做剩余工作。已保存结果但尚未投递时，沿用原投递标识重试回告。工具结果或外部副作用不明确时进入待核验，获得可信结果后再继续。
4. **回切时校验旧会话。** 原生会话和 Matrix 同步游标属于各 Harness 的运行时数据。回切保留旧数据作为验收条件，只有未过期且与当前权威检查点及身份匹配的会话才能复用。其余情况下从公共状态重建上下文，继续使用原 Worker、Task 和房间。

历史恢复验证记录了六种 Harness 的重启和 30 个有向切换组合。记录中的通过结论适用于各次测试使用的版本和环境。接入新 Harness 或替换候选时，按本指南复验实际声明的角色和部署模式。[公共启动与恢复边界](member-runtime-config-contract.md#common-startup-and-consumption-boundary-1631) · [恢复验收记录](../development/runtime-recovery-1631.md) · [恢复实跑驱动](../../tests/runtime_recovery_e2e.py)

## 六、常见接入问题

| 实跑症状 | 首查 | 验收证据 |
| --- | --- | --- |
| Worker 能调用旧文件工具，公共执行绑定却没启动 | 实际 launcher 是否将 `AGENTTEAMS_PLUGIN_DIR` 导出到子进程。旧角色是否仍装入持久配置 | 同一镜像从正式入口启动，实际 Worker 使用 `begin/submit`，受限回合 schema 与 SDK 注册匹配 |
| Leader 收到任务却按 Worker 工具运行 | Controller 投影的角色与 Team，SDK 恢复的角色配置 | Leader 实际提交原主任务计划，Worker 用相应执行 ID 接单 |
| JSON 解析报错但进程仍在工作 | stdout 是否混入日志。CLI 路径及任务桥接的原生连接对象是否正确 | 实际子进程返回可解析 JSON，stderr 可见诊断，固定注册可证明 |
| Manager 转发消息或说已建单，实际没有 Main Task | 当前角色提示词、实际模型 schema、原会话旧上下文、该请求回执的来源事件 | 对比实际模型入口与 Controller 来源登记；同正文两单仍需逐条核验。保留旧会话失败 |
| 开始或提交后卡在待核验 | 执行代次、候选对应关系、工具是否绕过原生准入、真实运行状态 | 正常路径提交可验收；超时、重复投递和已知副作用可用公共状态判定。未知结果按协议保留 |
| 回告已发，追问仍回答排队 | 原始会话是否与路由一致，已确认的终态事实是否进了压缩后的模型调用 | 在原会话追问真实交付文件或结果，核对回复和公开 Trace |
| Trace 全无或角色标签错误 | 运行时 OTLP 出口、网络 DNS、适配器字段、关闭正文采集 | Collector 回查实际模型、工具和对应 Task/Matrix 事件 |

## 七、新 Harness 准入验收

### 冻结候选与测试组合

- 固定 Core/Harness 提交、实际镜像 ID 或部署产物摘要、模型与参数、角色、资源和工具权限、输入及验收标准。默认一名 Leader、两名 Worker，以相同 Manager、入口和资源配置做比较。认证 Manager 角色时，入口须由候选 Manager 实际处理。依声明的角色验证托管或纳管模式，结果注明实际组合。
- Leader 和 Worker 均支持时，先跑同 Harness 主批次。仅支持一个角色时，其他角色使用已验证的 Harness，报告写明候选所在角色。声明支持两种角色互通时，另外与已验证的 Harness 双向验证，分别交换 Leader 和 Worker，每个方向至少一组简单、一组复杂。交叉场景单列。
- 每次从 Human 与 Manager 的原始会话发送业务需求，使用新的请求事件和 Task ID。用例只给需求、交付物和验收条件，节点数量、分工和依赖由 Leader 决定。记录实际拆解。相同条件下可复测，重复投递同一个事件另做幂等验证。

| 验收 | 必须覆盖 | 通过证据 |
| --- | --- | --- |
| 启动与契约 | 正式启动入口、可信身份与来源、公共工具 schema、角色配置、技能加载、原生 SDK 调用、原生上下文、真实 Trace | 实际模型请求收到正确的公开工具名和参数，框架按角色写入公共 Task 记录，连续轮次不会重复注入；校验真实技能加载内容 |
| 简单任务 | 同一冻结候选独立运行 **10 组**。采用[简单 CSV 汇总用例](../../tests/task-protocol/cases/requirements-only/simple-csv.md)或等价的轻量业务需求 | 10 组干净协议成功；默认全链路耗时目标 **300 秒** |
| 复杂任务 | 同一冻结候选独立运行 **10 组**。采用[复杂对账交接用例](../../tests/task-protocol/cases/requirements-only/complex-settlement.md)或等价的多步骤需求 | 10 组干净协议成功；能证明真实委派、产物交接和依赖推进；默认全链路耗时目标 **900 秒** |
| 交叉组合 | 在已验证的搭档上验证候选角色的交接与验收。按上述角色矩阵验证每个方向 | 通过相同的协议与交付判据，每个方向分别记录；角色能力以实际通过的范围为准 |
| 并发与异常 | 相同正文的独立请求、建单前更正或撤回、同一事件重复投递、上传或通知失败、提交响应未知、旧代次迟到、取消、退回返工。角色涉及异步工具时验证跨等待窗口和终态回执 | 检查来源匹配、权限和幂等，已验收结果保留；未知副作用待核验，可恢复通知继续投递。合理的风险审批按原策略验收，同时排查框架误触发审批 |
| 同 Harness 重启 | 在真实 Worker Task 已写入检查点、仍有步骤待完成时，分别验证正常重启和异常中断。覆盖原生会话缺失或过期 | 原 Worker 和 Task ID 保持，启动后自动恢复。无新派单、无新 Matrix 唤醒、无人工补状态。核对已完成步骤仅执行一次、产物版本及摘要一致、剩余工作继续、终态回告一次 |
| 双向切换与回切 | 候选与至少一个已有验证的 Harness 实跑 `已有 → 候选` 和 `候选 → 已有`。在同一进行中的任务上完成一次往返，每次切换前都有已确认检查点；保留旧会话和游标。声明其他支持组合时分别举证 | 同一 Worker、Task 和原会话延续。核对源实例退出、目标执行代次、检查点推进、旧回调受限、步骤与回告无重复、带版本产物完整。分方向记录 Harness 版本和实际部署模式 |

### 判定、计时和记录

一次**干净协议成功**需要核对四类证据。

1. 每个 Human 来源能追溯到对应主任务，Leader 用公共工具规划，Worker 经真实原生工具完成执行。每次提交都有精确候选和 Leader 决定，依赖由框架推进。
2. 原会话实际收到派单身份发出的最终报告和可下载附件，校验实际文件大小和摘要。原会话随后的自然问答能读到已经交付的事实。通知状态和节点接单各有其真实回执。
3. Trace 能关联来源 Matrix 事件、会话、Task、执行节点、角色和完成回告。持久状态、回执、会话实际展示可相互核对。模型与协作工具调用数、输入和输出 token 缺失时标记未知。
4. 全程没有手工推进状态、补文件、临时改提示词或补发来完成该次任务。漏单补发、环境恢复和迟到交付保留原始尝试，并在报告内单列。模型生成的业务实现结果另用独立验收器检查，业务错误本身不改变协议判定。

全链路时间从原始 Human 请求的 Matrix 服务端时间戳起算，到最终报告和最后一个要求的附件均在原会话可见的时间戳为止。下载验证用于确认交付成立。另记录建单、派发、接单、提交和验收时间，以便识别排队与执行耗时。报告简单、复杂各自的成功数、总尝试数、P50、P95、超时数及失败原因。P50/P95 使用已验证完整交付的耗时，同时保留超时和迟到结果，补跑不覆盖原始失败。

**准入结论**须给出 20 组协议结果、适用的交叉与异常验收、时限达标情况、以及可复核的证据。协议要求简单 **10/10**、复杂 **10/10**，所声明角色的交叉场景和异常门禁通过。声明 Worker 支持时，重启、双向切换和回切均须通过，并独立报告。性能独立验收，以两类完整交付样本各自的 P95 对照 300 秒和 900 秒，并单列超过目标的尝试。性能未达标时记录阶段瓶颈并继续优化，保留协议结果及跨运行时的通用行为。发现协议实现缺陷时保留当批证据，在修复后的冻结候选上重新验收。

复用 Core 的[独立业务验收器](../../tests/task-protocol/run_oracle.py)、[协议判定辅助](../../tests/task-protocol/protocol_verdict.py)和[运行汇总](../../tests/task-protocol/summarize_runs.py)。在 Core 根目录运行以下命令，第一条验证验收器，第二条汇总实际会话实跑记录。

```sh
python3 -m unittest discover -s tests/task-protocol -p 'test_*.py'
python3 tests/task-protocol/summarize_runs.py /path/to/evidence/runs --source FULL_RUNTIME_COMMIT
```

提交接入材料时记录声明角色、部署模式、版本与镜像摘要、场景、运行 ID、原会话/Task/Trace 的关联、已校验附件和结果。证据存放在受控位置，报告使用可核对的摘要和关联 ID，凭据与私人会话原文留在测试环境。[1.2.9 历史认证记录](teamharness/task-cli-certification.md)

## 八、统一 HITL 接入与验收

接入两类 [HITL 协议](runtime-approvals.md)：人工决策、问题确认和选项选择使用 Ask User；工具执行前的授权使用 Tool Approval。Dashboard 和 Mobile Client 按协议读写，不为新 Harness 增加审批命令分支。原生命令、RPC、Future 和 SDK 参数的转换由对应 Adapter 负责。

- **Ask User。** 使用 `org.agentteams.run` 的 `confirmation` 块，提供稳定的 `confirmation_id`、问题及选项、可选截止时间。新请求声明 `reply_format=com.agentteams.answer`。答复使用 `kind=ask_user`，逐题保留 `responses` 的问题 ID、选项 ID 和补充文本。原生能力优先暂停并恢复原 Turn；`request_attention` 等非原生路径先持久化决策，再通过后续 Turn 继续同一工作。
- **Tool Approval。** 使用 `org.agentteams.approval`，新请求提供 `context_id` 并声明相同的 `reply_format`。客户端统一回传 `kind=tool_approval`、请求 ID、上下文、目标 Agent 和 `approved/rejected`。Adapter 保留原有权限策略，在执行前完成原生授权。聊天中的自报 `actor`、自然语言“批准”和普通 Ask User 答案都不能授予工具权限。
- **状态与回执。** 两条链路等待期间统一展示 `waiting_user`，与正常执行、完成和空闲区分。进程仍存活不代表模型仍在推理；发送成功也不代表运行时接受。原生等待器与持久决策分别由其拥有者确认。问题回执使用 `answered/cancelled/expired`，审批回执使用 `approved/rejected/cancelled/expired`。普通问题不应使 Task 进入 blocked；业务审批的可信 Human 门禁保持不变。
- **非原生等待与恢复。** 持久 Task 标记为 `waiting_user` 后，自动恢复扫描跳过该 Task，新的原生工具调用暂停准入；发起问题的工具仍须写完自己的执行回执。收到可信答复并提交决策后，后续 Turn 从公共任务上下文读取 `human_decisions`，继续同一 Task。

| 验收场景 | 必须举证 |
| --- | --- |
| 正常回答与审批 | 真实 SDK 等待器暂停在问题或工具前；答案恢复对应 Turn；只有被接受的批准允许该工具执行；拒绝结果回到原 Turn |
| 多题、多选与自由输入 | 逐题 ID、选项 ID、选择数及文本完整；未知、重复或遗漏的问题与选项被拒绝 |
| 重复与迟到答复 | 同一事件重放幂等；首个有效决定不可被后续事件覆盖；工具及后续工作不重复执行 |
| 越权与错绑 | 校验实际发送人、房间、目标 Agent、请求和上下文；正文或 `actor` 不能伪造授权；错误控制不能落入模型或命令兜底 |
| 超时与取消 | 原请求不再可授权；卡片与会话状态可核对；Task 取消、返工或替换后旧答案不能继续旧执行 |
| 重启恢复 | 原生等待器可恢复时恢复原 Turn；否则使旧请求失效，从公共检查点恢复。非原生决策必须在新 Turn 前提交到持久存储；未知工具副作用不能自动重放 |
| 客户端与升级 | Dashboard、iOS、Android 使用相同两类结构；新 Harness 无需客户端命令分支；请求、答复、回执乱序和历史补载可核对；旧原生命令继续有效 |

记录 Core/Harness/Client 提交、SDK 版本、真实入口、请求/房间/Turn/Task 关联、原生结果和工具执行证据。解析器测试、原生服务测试、镜像构建及部署实跑是不同证据层，不能互相替代。

实现参考

- [公共原生桥接及角色 schema](../../plugins/teamharness/mcp/common/task_native.py) · [公共 Task CLI 参数](../../plugins/teamharness/mcp/common/task_cli.py) · [阶段准备](../../plugins/teamharness/mcp/common/task_context.py)
- [原生协调阶段](../../agentteams-controller/internal/server/coordination_phase.go) · [执行恢复](../../shared/lib/task_recovery.py) · [原会话的终态交付记录](../../agentteams-controller/internal/server/task_delivery_context.go)


## 九、任务交接与上下文最小化（2026-11-24 联调约束）

以下要求来自 Codex 研发小队的实际联调，适用于所有 Harness。接入层保持各自原生会话和工具机制，共用 Core 的身份、状态机与回执，不为某个接收者增加绕过协议的兼容分支。

### 会话分工与消息边界

- 原始 Human 会话只整理必要背景并受理。已登记 Leader 可用原生 `create` 绑定其注册 Admin 的确认；Controller 保留完整原文和原回告路由，先创建主 Task 房间，再以内部控制通知进入该房间规划。`roomId` 指向该 Task 房间。不要在原私聊反复分析、规划或向自己发送 @。保留旧版本明确返回 `inline` 时的兼容行为。
- 正常节点成功后，Core 冻结提交并代当前交付者在任务房间 @ 已就绪的下游；不逐节点回到 Leader 再派发。异常、显式审批和最后整体验收才唤醒 Leader。最后一个执行者通知 Leader 验收，Leader 用 `complete` 完结同一主 Task。
- 接单是内部状态。Controller 和 Harness 均不另发“收到”“已接收，开始处理”“节点已保存”或重复提交回执。被 @ 后直接执行。只保留必要交接、具体阻塞/返工与业务结果。内部规划唤醒使用无自 @ 的控制 notice；客户端不把它组装为未答复的聊天回合。
- `org.agentteams.progress_only` 的思考、工具和执行展示不成为其他 Worker 的业务输入。普通群消息仍按收件人/提及策略处理；补历史也只回填获准输入。原生 SDK 保留自己的实际工具结果，不从房间拉取其他 Worker 的全部过程。

### 交接内容与发送者事实回填

主 Task 保留整体需求。节点 spec 仅补充自己的职责、必要输入、输出和验收点，Core 自动关联原始要求，不把原文复制到每个节点。自动准备的模型上下文采用 `task_context.model_context` 或等价的公共投影：上游只携带简短结论、精确 Task/submission 身份、已验证本地路径、artifact/version/hash 引用。不要自动展开全部 HTML、报告正文或整份子任务历史；按需读取相关文件。整体验收也通过报告/产物引用查证，保留未验证项，不能把省略正文解释为已经验收。

发送者通过可信原生 Task 工具取得提交和交接回执。框架将回执写入其原生会话并确认实际持久化，随后结束该执行阶段；不能为了朗读回执再请求一轮模型，也不能发送自 @ 消息来注入上下文。SDK 的无推理历史注入或固定原生回执回调均可，但必须有实际行/会话提交证据，不能只更新旁路 JSON。

| 已知事实 | 可注入的内容 | 不得声称 |
| --- | --- | --- |
| Task 提交已保存、协调事件已入队 | 原 Task/submission、下游 Task、接收者、协调事件 ID，状态 queued | 已实际发送或下游已接单 |
| 同一发送者的公开协调回执确认 `native_id=matrix:$eventId` | 精确事件/房间/接收者与实际 Matrix 事件，状态 sent | 下游已执行、成功或已完成 |
| 下游 `begin`/执行状态已提交 | 对应 Task/assignment 的实际状态 | 其他节点或整个主 Task 已完成 |

利用 `GET /api/v1/coordination/events/{id}` 的发送者/接收者权限核对实际发送；未确认或查询失败时保留 queued，不伪造成功。实际发送事实只作为原生框架回执追加，不启动新回合，不发进度聊天。Task 工具的终态返回只保留必要身份、状态和回执；完整权威快照仍在 Core。业务展示与内部回执分开，实际 Matrix 回复保存可理解的受理/业务结果；不要把英文控制提示当成最终业务答复。

### 原生执行与展示的一致性

关联 Skill 的正文须在任务/协调阶段实际可用，不能只注册名称。角色决定公开 schema；准备后的工作目录、文件版本、原始来源和执行代次来自框架，不能由模型重填。`phaseComplete` 后保留原生回执并停止该阶段推理；恢复时复用已持久化事实，不重新建 Task、派发或提交。

工具展示统一提供真实工具名、入参、出参、调用 ID、开始/结束和状态。完成后合并为可展开的思考/工具计数，保留失败，不把历史失败隐去。展示层隐藏成功控制回执时，仍须正确结束原回合，避免“本轮未生成最终答复”的假警告。工具数量按真实调用统计，减少重复读取和无效回合，不通过隐藏次数假装变快。

### 本轮新增回归场景

1. 私聊受理恰有一条自然回复，主 Task/房间恰有一个；完整规划在该房间发生，重试丢失建房响应不生成第二房间。
2. 正常成功直接 @ 下游，无 Controller 接单气泡、无模型额外确认、无成功回执聊天；发送者原生历史存在持久化交接事实，其后无新增模型调用。
3. 上下游原生上下文不含别人的思考/工具过程，不自动展开全部输入文件；必要要求和精确文件引用仍可获取。
4. 独立评审失败走同一 Task 的返工 DAG，保留错误样本与修正记录；最后一个执行者唤醒 Leader，验收并完结，最终报告和确定版本文件回原 Human 会话一次。
5. Codex、Qoder、Claude Code 的混合节点按同一规则验证；用正式 runtime/image 更新接口保留 Worker 身份，核对实际实例、状态代次、模型、原生会话和部署镜像。未覆盖组合和完整准入认证仍单列，不由本地一次跑通推断全面支持。

记录源码/部署版本、原始尝试与人工干预、真实模型与工具耗时、Trace、原会话实际附件下载及摘要。不要把单元测试、镜像构建、状态恢复和无干预端到端成功混为一项。

### 原生适配联调记录与回归点

- Codex：将已确认的 Task/发送回执通过 SDK 无推理回调写入真实原生工具历史；保留业务展示，不把协议控制文本再次作为聊天答案。
- Qoder SDK 1.0.17：`PostToolUse` 使用 `continue_=False` 和 `stopReason` 结束提交阶段。SDK 会将 MCP 的多个文本块合并到一条工具结果字符串；只对首个 JSON 用 `raw_decode` 解析身份，并核对真实 `tool_use_id`、receipt 与持久会话，不能对整段拼接文本使用 `json.loads`。房间过滤须同时覆盖进度块、任务群未提及消息和历史回填，避免运行时切换后重放他人过程。
- Claude Agent SDK 0.2.187：实际 MCP `PostToolUse.tool_response` 是文本块列表；核对固定注册的 Task 工具、Core 的阶段完成结果及原生 JSONL 中相同工具调用的结果后停止。已自动完成的成功节点仍须走保存结果的投递路径，不能因 `status=completed` 跳过报告。同步 Bash 必须先登记、后保留实际终态；后台或中断结果仍待核验。
- 关联 Skill 不应只存在于磁盘插件目录。协调阶段禁用 Skill 工具时，角色和关联 Skill 正文仍须直接可用。只给必要投影；模型可按准确文件引用检查详细产物。
- 模型路由须明确。测试账号不支持某种 SDK 的自定义模型入口时，记录实际请求是否到达网关；只有用户明确同意才可另选账号模型，不能把路由降级写成自动兜底，也不能宣称三个运行时用了相同模型。

真实 SDK 的本地协议夹具只证明原生结束和持久化边界；混合 Task 实跑、保留数据切换与完整准入认证分别记录。中途更换镜像造成的未知工具结果保留为失败/人工干预，不能补成干净成功。

原生直出 Trace 仍须带共享可见性字段：真实 Worker 身份、`langfuse.user.id`、原生 session、`agentteams/worker-turn/runtime` 标签以及 Task/Team/Room 关联。仅有 OTLP 到达或能用内部 Langfuse API 找到 Trace，不代表经过 Dashboard 权限过滤后可见。模型/工具 span 按真实原生事件分类，使用实际返回的 token 字段；没有提供的数量保持未知。核对 SDK 自己登记的进行中工具时，`needs_verification` 不是执行权丢失：先比较 generation/instance/process，不能把自己的正常工具打断再重试推理。

原生插件有模块隔离时，适配测试必须使用 SDK 的实际隔离加载方式，不能把另一个插件的包加入全局 `PYTHONPATH` 后视为接入成功。插件之间通过当前请求的 SDK 上下文传递最小可信配置，各自在自己的命名空间完成原生能力识别。QwenPaw 的 TeamHarness 中间件直接导入 Matrix 插件，会使工厂加载失败而缺失 Task 来源注入；直接调用工厂或手动设置 ContextVar 的测试不足以覆盖这一故障。定位工具参数或上下文错误前，先检查同轮中间件工厂和运行时 hook 的首个异常堆栈。

原生导出器只能写字符串属性时，由公共 Collector 的 `transform/native-identity` 将 canonical `agentteams.*` 映射为后端身份、数组标签及关联元数据；已有后端字段保留。只有携带 Worker 身份及 run ID 的记录才补 `worker-turn` 标签，不把无关联的基础设施记录当作 Agent 轮次。该映射不识别具体 Harness，不生成或重建 span，不改变原生 trace/span ID 和时间。用实际 Collector 验证字段转换后，仍须用真实 Agent 调用完成后端和页面读回验收。

### 其他原生 Harness 的同一约束（2026-11-25）

- OpenClaw、QwenPaw、Hermes、OpenHuman 都通过公共 `model_context` 与 `model_result` 投影自动准备的上下文和终态工具返回。显式读取仍返回请求的数据；自动交接不重复携带整个 Task、全部报告和文件预览。正常规划完成也不产生成功控制回执聊天。
- 受信 Matrix 用户身份须同时覆盖实时消息与恢复入口，否则提交回执虽保存，实际发送事实无法回填。身份来自已登录原生客户端，不来自模型参数。
- `completed` 是直接成功交接的合法终态。当前同一代次、进程、实例只能完成已有工具返回及持久化、投递；不能以此重新进入模型或执行新工具。空最终文本只有存在权威持久结果才可收尾。
- QwenPaw 的 Leader 直接接收其 Admin 的需求时，同样绑定原始 Human 来源，在原会话结束受理，进入任务房间规划。SDK 的原生上下文必须保留工具结果和已确认发送事实，不能只保存到旁路缓存。
- OpenHuman 在终态关闭可复用会话登记后，仍须把真实工具历史和发送事实写入自身原生会话文件；停止登记不能成为跳过持久化的理由。恢复扫描不得重启已完成节点。

### Managed Task recovery context

Keep the durable checkpoint in the runtime and Controller. The model's managed Task resume message contains the current Task/team identity, status, completed/remaining business steps when present, and resolved Human decisions. It does not repeat the Task specification, result/report snapshots, operation records or delivery transaction fields. Prepare the current assignment with the common Task tool and read other files on demand. This projection must not erase authoritative recovery evidence or relax unknown-effect fences. Legacy taskflow recovery retains its existing contract.

Apply the same model projection to nonterminal native Task tool results, including begin: retain the current assignment, exact input/artifact references and real local/output paths, but omit execution checkpoints/generations and upstream file previews. Framework-generated tools: operation hashes are control evidence, not business steps. Keep them in durable runtime storage, not in resume prose. Explicit files_read content and primary/extra receipt identities remain available.


### Task 房间续接、运行中补充与故障收尾（2026-11-26）

同一个任务房间具有稳定的原生会话身份，但会话身份不等于执行节点授权。普通 @ 消息、恢复任务与正式派发必须共同遵守下列边界：

- **普通消息续接**：适配器从已验证的 Matrix 事件传入 `sourceRoom`、`sourceEvent` 和已登录 Agent 身份，不能从模型文本取得这些字段。模型指定执行节点后，共享 Task 绑定查询权威快照，确认节点属于当前房间，再通过正式 `begin` 恢复执行上下文；分配者、执行代次与进程权限仍由 Controller 校验。跨房间节点必须拒绝。稳定原生 session 本身不能替代这些校验。
- **运行中的补充**：同房间、当前团队登记的 Leader 发来的补充优先进入当前原生 run 的 steer 接口；不新开并行推理，不重建 Task 或工作区。Controller 的节点 `amend` 保存需求后，由任务系统持久化并重试执行者通知，Harness 发现权威任务变化后续接；不能要求 Leader 额外 @ 才推进，也不能把聊天送达设为执行前置条件。适配器应核对真实 run/turn ID 和原生输入持久化；补充到达时 run 已结束，则走普通消息续接。用户主动停止与授权检查保持有效。
- **可核实的未执行失败**：PreToolUse 在放行工具前失败且明确阻止执行时，适配器可持久保存精确 call ID、工具名与参数摘要及拒绝事实。恢复仅对与当前待核查操作完全匹配的记录，通过正式核查接口登记“未执行”，之后才继续。不匹配记录、已放行命令、超时或缺少结果均不能推断无副作用，仍需核查；不得直接修改状态绕过检查。
- **终态必须闭合**：成功、安静结束、中断、异常各路径都向 Matrix 投递使用原 `run_id` 的幂等结束标记。工具全部结束不代表整轮已经结束；不能依靠 Dashboard 超时猜测，也不能发出 `Codex control accepted.` 等控制聊天代替标记。旧轮中断与恢复后的新轮分别保留真实状态。
- **Leader 权限**：正常的本团队需求修订和节点恢复通过公开 Task 操作完成，不要求平台 Admin 代改元数据。权限仍绑定本团队的真实 Leader；普通成员不能伪装 Leader，也不能操作其他团队。保留提交、核查、验收的事实边界，不能把状态修改等同于实际交付。

Manager 是角色，Harness 是运行时。支持 Manager 的运行时复用通用 Harness 镜像并按 `role=manager` 初始化身份、工作区、工具与消息格式；切换运行时应重新选择对应通用镜像，不能沿用旧运行时镜像。技能分配的公共数据与 Dashboard 交互保持一致，各 Harness 负责让技能正文真正进入其原生执行上下文。

新增准入回归：

| 场景 | 必须核对的证据 |
| --- | --- |
| 正式派发后普通 @ 继续提交 | 同房间续接成功、真实节点归属；跨房间与非执行者请求拒绝 |
| Leader 在执行中补充需求 | 同一 run/turn 只有一次启动，原生历史含补充，产物满足新增要求 |
| 补充与结束同时到达 | 补充被 steer 接收或由后续普通消息处理，不能静默丢失或重复执行 |
| 工具登记前断联后恢复 | 精确未执行证明可核查继续；缺少证明的写操作保持待核查 |
| 取消、异常及无最终正文 | Matrix 原轮终态收敛，刷新后不再永久显示“正在处理” |
| Manager 运行时切换与技能分配 | 实际容器镜像、角色、模型路由、原生回复及技能正文均匹配 |

每个 Harness 分别报告单元/原生 SDK 夹具、镜像、真实任务与 Trace 证据。Codex 实跑的修复不能直接算作其他 Harness 验收通过。

### 任务补充与 MCP 策略的原生接入检查（2026-11-27）

- Task 补充的发送者授权、同团队/房间/接收者边界由 Core 的 `runtime_control.task_supplement` 决定。Harness 只把已通过的输入转换成 SDK 原生操作；不要在各适配器重新写 Leader 例外。Team Admin 的定向补充遵守同一规则，不因此取得其他人的停止或审批权限。
- 运行中输入的原生验收必须同时覆盖空闲时续接和工具运行期间的 steer，并启用 Task 普通输入认领、真实原生持久化回读、消费回执和退休这一完整路径。只验证 SDK 的已消费 UUID 或私聊 steer 不足以证明 Task 补充可恢复。原生记录可能有不同格式，由对应 Harness 识别并核对精确会话、来源输入 ID 和正文；不能在 Core 按运行时分支猜测记录，也不能用放松持久化校验代替适配。
- 消息格式适配必须把 Matrix `m.mentions.user_ids` 保留为公共输入的 `mentions`，不能只用于入口过滤后丢弃。页面正文可以只有显示名，不能依赖正文含完整 Matrix ID 来触发 steer。回归须用真实 Matrix 消息解码后接入授权判断，不能只手造已经补齐 `mentions` 的输入。
- 原生 Task mutation 的参数/目标在开始持久操作前即可被明确拒绝时，返回有依据的 `effect=none`；只有实际副作用不确定才保留核查隔离。主 Task 的 `kind` 不能由 `amend` 改成 execution，因此旧版对主 Task 发起的无效 amend 可据不可变身份恢复准入；执行节点的未知 amend 仍须核查，不能凭状态看似未变或普通 HTTP 错误放行。
- 原生队列接收不等于输入已持久化。先通过真实 SDK writer 保存，再读回原生会话里的精确用户输入，登记 Core 的 native receipt，最后退掉该普通待处理输入。保留原事件身份；未消费的输入继续留在共享队列，结果未知的既有普通工具操作仍需核查。
- MCP 分配和审批是两项能力。必须验证外部服务实际注册到原生工具列表，而非只有 runtime.yaml 里出现服务名或只有 TeamHarness 自身可调用。网关 Consumer 凭证只注入原生传输；不得复制 GitHub 上游 Token 给模型或依赖宿主机 Git 登录。
- Agent 整体工具审批使用 Worker/Manager `spec.approvalMode: ask | allow`，由 Core 投影为 `desired.approvalMode`。未设置时保留原运行时行为。第一期保存后重启 Agent 生效；页面必须明确提示，不宣称五种运行时均已动态生效。已有 MCP 显式策略优先于 Agent 默认。
- 原生审批模式须在实际镜像、进程用户和 SDK 中启动验证，不能只校验参数枚举。原生免审批模式不可用或会跳过细粒度规则时，保留默认权限模式，通过原生审批回调落实 AT 决策；用户问题与显式请求审批仍需人工回答。不要通过关闭原生安全检查来让某个模式强行启动。
- 审批配置验收包含资源定义的实际生效：严格解析 Controller 和 Helm 的 Worker/Manager CRD，读回已安装 schema、保存后的 spec 及运行时投影。HTTP 200 或镜像内存在字段不代表存储成功；旧 schema 可能裁掉新字段。升级测试要核对启动日志中的资源定义注册结果。
- 两个值表达请求原生审批与自动同意原生工具审批；不代替业务确认、用户问题或运行目录边界。各 Harness 在自己的仓库映射 SDK 配置或审批回调。禁止将名称相似的 `never` 直接解释为自动同意：Codex 和 DeepSeek 的该策略可能直接拒绝受限请求。自动模式必须保留原生决策回执，不改变执行目录权限来绕开审批。
- 第一期 Agent 整体审批采用配置投影和真实 SDK 审批回调测试，不增加真实 MCP 审批专项验收门槛。此范围调整不取消研发小队的真实 GitHub MCP 交付，也不撤销已有单服务策略。
- 当前公共审批契约为 `tools: "*"` 配合 `mode: ask | allow`，未设置时遵循运行时默认。每次新调用读取当前策略，不能仅在进程启动时取一次。适配器绑定原生注册的真实服务身份；原生工具名可能被截断、规范化或哈希，不能按显示名称猜服务归属。
- 成功 Task 交接可以没有最终聊天正文，但仍必须结束该 run 的进度展示。不要以“已有正式提交”或“quiet 成功”为理由跳过终态标记。状态标记使用进度协议，不产生额外业务回复。
- 已完成主 Task 的房间仍可接收普通追问。房间发现不能仅依赖当前活跃节点、配置房间或双人私聊；使用公共 `task_coordination.include_task_rooms`，根据当前成员有权读取的 `tasks/room-context` 纳入已加入的 Task 房间。保留发送者、群消息和定向提及策略；追问不自动重开任务。验收包含已完成房间的真实结构化提及、原任务状态与原 PR 回复，以及无权访问房间不能纳入的反例。
- `quiet` 表示本轮结束时不额外生成回执消息，不表示隐藏已经发布的正文。通用消息展示必须覆盖“没有工具过程、只有正式正文”的回合，分别测试终态早于正文、晚于正文和事件倒序到达；纯静默回合仍不显示空卡片。已送达 Matrix 与在页面实际可见分别取证，不能用消息 API 成功代替页面验收。
- 调度耗时分别记录任务节点完成、通知发布、输入接收、原生模型开始和最终答复投递。通知发布不等于模型已经开始；共享执行路径中的 `recovery:` 标识也不证明发生过重启。未取得对应时间证据时，不将等待归因于模型、历史扫描或某个恢复步骤。
- 研发小队验收同时核对实际部署的技能来源。只修改已存在的调试 Worker 不算修好新建链路；官网角色清单、四份角色技能和页面部署意图必须采用同一交付顺序。

集中验收须包含长 MCP 名称、ask/allow 双向热切换、一次拒绝后继续、原生输入持久化失败、结束竞争和无最终正文交接；分别保留成功、失败与人工恢复证据。


### Leader 的执行中节点调整

`task amend` 的 `spec` / `note` 更新当前节点需求；可选 `replacementNodes` 将尚未提交的节点改为一份新的执行 DAG，支持改派与调整依赖。原节点保存为 `revision` 和 `task_revision` 管理变更记录，保留历史与已有产物；不伪造 `task_submission` 或独立测试、验收事实。替换 DAG 沿用同一主 Task，入口继承原上游，下游等待新 DAG 的输出。相同请求重试不重复建节点。

Controller 核对本队真实 Leader、已登记的执行者和有限无环依赖。当前工具操作结果未知时不能改派抹掉它，须先核查；已经激活的下游不能被静默回写。已提交或阻塞的候选仍使用原候选的 `reject` 加明确返工 DAG。普通成员和其他团队无此权限。Harness 只暴露共享 Task schema，不增加自己的一套 Leader 规则。

### 接管后的旧轮展示收尾

本批任务进度使用公共 `task_run_id(team, task, generation)` 标识，原生 session/thread ID 仍由 Harness 管理。Controller 在成功接管执行时，将旧代次及结束类别写入执行记录的 `retired_runs`；这份记录和接管结果一起持久化，不能只保存在新进程内存里。

Harness 通过公共 `retired_task_runs` 获取应投递的结束标记，用自身 Matrix SDK 发送；只有收到真实事件 ID 后才能调用 `retired_run_delivered`。事务 ID 稳定，发送失败、重启和重复扫描仍可重试。未知工具结果继续待核查，不因展示关闭而被视为成功。旧版使用运行时私有 run ID 的历史消息需要保留迁移边界记录，不能假称本批新协议已自动修复全部历史轮次。

### Model protocol and Manager tracing regression

A successful short upstream connection probe does not prove that a native Agent
can combine tools with reasoning. Verify the actual SDK request protocol, tool
schema and reasoning controls before running the development squad. When an
upstream rejects that combination, retain the model and capabilities and select
a supported native protocol explicitly. Preserve a user's native provider choice
across managed-model refreshes; do not infer a transport from a runtime or model
name in Core. Harness-owned configuration maps the chosen protocol to its SDK.
For QwenPaw, an explicit `desired.model.chatModel` takes precedence over the
`QWENPAW_CHAT_MODEL` native environment parameter; an existing native driver is
retained when both are absent. `OpenAIResponseModel` selects native Responses.

Manager and Worker containers both receive the Controller's saved collection
settings when created. Their common `runtime.yaml` records also carry the same
non-secret observability revision and endpoint. Applying settings to an already
running member requires its documented configuration/recreation boundary; never
claim collection from a saved setting alone. Exercise a real native model/tool
call and query its trace back. Backend query credentials stay in Controller;
only Collector ingestion settings may reach a Harness. Include unsuccessful
model calls in the coverage audit, and distinguish an unconfigured member from
an exporter or query failure.


### Native tracing adapter ownership

Python Harness images may provide `at_native_telemetry.py` next to the shared
telemetry modules on `PYTHONPATH`. The module exposes `install()`, `PACKAGES`
(the native package versions to record), and `INSTRUMENT_OPENAI` (whether the
shared OpenAI instrumentation should be enabled). Core loads this fixed contract
without runtime-name branches. Only an absent adapter permits the generic
fallback; a broken native dependency must remain a reported startup error.

Keep SDK entry points, middleware signatures, SDK tool boundaries and gateway
callbacks in the Harness repository, together with their compatibility tests.
Core owns identity correlation, sanitization, OTLP export and transport carriers.
QwenPaw uses its AgentScope middleware instead of adding a second OpenAI model
instrumentation layer; Hermes opts into shared OpenAI instrumentation and owns
its native turn/tool/gateway hooks. Both release Dockerfiles and test packaging
must include the same native adapter.

A successful export alone is insufficient: query a real model/tool turn back
through the Controller and inspect it in the Dashboard. Manager turns use the
current shared `manager` runtime identity and are restricted to namespace-wide
readers; Team or explicit Worker access must not expose Manager private turns.


### 研发小队不得依赖人工催办的回归门槛

初次需求确认、主动变更需求、策略为 ask 时的人工审批及最终合并确认属于正常交互。
系统丢失通知、错误保留执行围栏或没有发布已保存报告后，额外由人补发“继续”或 @ 才能
推进，属于故障恢复介入；即使最后生成 PR，也不能计作自主交付通过。

- **公共提交与通知：** 更新执行要求必须在同一次条件写入保存可重试的通知意图。
  Core 使用已登记的原执行路由派发；Leader 不必另调用消息工具。记录保存、传输接收、
  原生消费和实际续跑是不同证据，queued 不代表 Agent 已消费。
- **会话与结果归属：** 普通定向消息续接了某个 Task 后，正式提交的报告仍经该 Task
  的发布与回执流程完成；不能只更新普通消息 checkpoint。恢复已提交报告只重试发送，
  不重新采样实现、不重复工具副作用或再发一份普通聊天报告。
- **原生错误分类：** 审批拒绝、执行前参数/权限拒绝及正常完成的失败，按原生 SDK 的
  实际终态和调用身份结算。明确的执行前拒绝可声明 effect=none；命令失败不代表它没有
  写入。未知结果、中断及后台仍在执行不能伪装终态。错误分类及原生格式转换属于 Harness，
  Core 不按工具文案或运行时名字新增判断。
- **原生反例：** 使用真实 SDK 覆盖“拒绝且目标未产生”“失败后下一次调用可继续”、
  “部分写入失败不声明回滚”、重启及发送失败。仅手工拼装预期 error/code 的样本不足以
  证明适配正确。新的真实 SDK 用例必须进入对应 Harness 的 CI/smoke 入口。
- **业务验收：** 同一冻结配置下分别运行五种小队。首次启动配置准备完成后，从正常
  Issue 确认起，不额外发催办消息、不改 checkpoint、不代写/代测/代推送，直到 Ready PR
  和原会话回告。任何人工恢复逐条记原因、动作和证据；修复后另用新任务复验。

更完整的停滞扫描、流转事件注入 Leader 上下文及漏消费补偿属于后续任务系统设计，
不能写成已实现，也不在 Harness 私自增加业务状态机或独立定时推进器。


### 保留原生工具可选参数的真实传输语义

工具定义中的optional必须一直保留到模型API的实际HTTP请求，不能只检查本地JSON Schema。
Responses省略strict时可能尝试将schema规范化为严格模式；Chat Completions的默认行为不同。
若原生SDK依赖“可选字段缺省”而不接受null，必须在对应Harness内使用原生provider兼容项明确
发送strict:false，或采用完整且经过原生调用验证的等价映射。不得简单把可选字段改为必填，
也不能用空字符串代替未提供；后者会触发原生参数校验。

接入验收使用真实SDK捕获最终HTTP payload：核对必填列表、strict标志及可选字段类型；再通过
实际网关运行一次仅含必填参数的工具请求。连接探测200、只检查配置对象或手写模拟payload
不足以覆盖这条路径。SDK的兼容开关及原生错误处理仍归Harness，不在Core按运行时修补工具参数。

本次DeepSeek的pi-ai provider在supportsStrictMode缺省时没有发送strict字段；显式启用该provider
的兼容能力后，函数请求实际发送strict:false，保留DSH原生工具的缺省参数语义。
官方依据：[OpenAI Function calling — Strict mode](https://developers.openai.com/api/docs/guides/function-calling#strict-mode)。


### 区分MCP服务器失败回执与传输失败

MCP服务器返回的 `isError=true` 是一次明确的失败回执，不等于连接断开或缺失结果。
Harness必须保留这两种来源的区别，即使原生SDK将它们都转换为异常。可使用原生错误
类型或适配层保留的结构化错误码，并绑定实际工具注册实例；禁止按模型可见工具名、
远端错误文本或模型传入的标志冒充可信来源。

已确认失败按error保存后允许纠错，但不自动声明effect=none：浏览器脚本或远端工具
可能先执行部分动作再报错。只有确认的执行前拒绝才能声明无副作用。传输超时、
连接丢失、取消和缺少结果仍保持未确认，禁止自动重放原操作。嵌套工具的终态也须
使用同一判定，不能让已处理的子调用错误将完成的父调用重新标成未知。

原生验收至少包含：真实MCP服务先执行部分动作再返回isError，接着下一工具可执行；
相同连接在传输失败时保留未确认操作；替换注册实例或复制错误文本不能绕过判定。
这些对照用例应从对应Harness的镜像smoke入口运行，而非只在临时调试命令中执行。


### 规划身份与派发身份必须分开

已返回给Leader的执行节点ID应在planned阶段即可通过公共Task接口读取并更新未完成
需求，不能等依赖完成后才创建可操作记录。planned记录不代表任务已准备执行：
不产生assignment或通知，不允许Worker Begin，依赖满足后才派发。派发必须保留
Leader提前修订的完整spec和历史，不能从原DAG覆盖回旧需求。验收覆盖planned节点
GET/amend、提前Begin拒绝、依赖完成后仅派发一次及修订需求不丢失；公共实现归Core。


### Task context must not depend on Matrix history recovery

Use Matrix to deliver the trigger and its event identity. Reconstruct business context from the current Task specification, execution node, upstream deliveries, accepted independent reports and current state. A verified Task lifecycle notification must remain dispatchable while unrelated room history has gaps. Apply the common Core receipt and recipient checks; do not infer authority from a message body. Ordinary chat keeps its native incremental cursor and ordering rules. Keep genuinely unknown native tool effects fenced; missing chat history is not evidence of an unknown business operation. Include a regression with an outstanding room gap and a valid Task notification, plus a forged/wrong-recipient counterpart. Validate ordinary Task-room followups and formal amendments against the same authoritative Task context before declaring a runtime aligned.

### Optional identifiers and independent Task discovery

An unassociated Project may be omitted, null in retained storage, or an empty string. Producers should write the canonical empty value. Shared notification validation and preparation normalize these equivalent no-Project values before comparing membership; a different nonempty Project must still be rejected. Acceptance must cover both Matrix notification admission and direct Task recovery using newly planned and retained records. A passing old dispatch fixture does not prove the planned-node path.

Adapters periodically discover ready/recoverable Task work through the public Task API using the native runtime loop, even if no Matrix @ arrives. Preserve existing run ownership and native-session deduplication; a scan is not a new assignment or repeated inference. Notification delivery only accelerates this scan. Hermes exercises this in its existing native sync lifecycle; common Task state and authorization remain in Core.


### 大内部上下文的原生进程传递

准备好的内部Task控制上下文不得整体复制进CLI单个参数或环境变量，否则多节点及较大交付记录可能超过操作系统启动限制。原生适配器使用已有私有状态文件、私有临时文件或stdin保留完整数据；能复用已有原生Scope文件时不另建一套存储。模型上下文仍采用必要的Task信息和引用，不借机展开全部内部记录或Matrix历史。此项属于Harness进程传输适配，不修改公共YAML或Task权限协议，也不能靠截断内部数据解决。

准入回归包括大于原生单参限制的UTF-8上下文、实际SDK/CLI启动、真实子工具读取、普通小上下文兼容和原有未知操作拒绝。记录内部传输、模型投影及原生持久化三者的边界，不能把模拟客户端捕获参数当成真实原生启动通过。

### 过程消息与长内容

过程消息只携带预览，不重复文件正文。Python 适配器复用 `shared/lib/run_progress.py`；其他语言在 Harness 内按相同规则投影：工具参数、结果和推理预览按 UTF-8 字节限制，每个字段最多 4096 字节，保留 `preview/truncated` 标记并先脱敏。普通 `body` 使用简短说明，不能再复制完整工具详情。验证 Matrix 编辑消息包含 `m.new_content` 后的完整 JSON 仍小于 64 KiB，覆盖长中文和 emoji。完整内容由文件和启用正文采集的真实 Trace 保留，不能为了让聊天消息发送成功而截断实际工具参数或产物。

### 已确认进度上的传输失败恢复

原生 SDK 结束失败后，适配器须释放本地的“正在运行”标记。若属于可重试的模型传输错误，可通过共享 `Recovery.release_failed_turn(identity)` 释放执行：只有当前所有者、全部登记工具已核验、没有待投递结果的执行才允许释放。任务系统重新认领同进程的已释放执行时生成新代次，保留原 Task、进度、产物及旧 run 的结束记录；无需额外 @ 消息。

本地记录仍为 `running` 不能作为永久跳过恢复的依据。确认原生回合已返回后，将恢复判断交给上述公共操作；下一次正式认领前不得直接重跑模型。回归须覆盖服务重启导致准入失败、同进程保留旧 `running` 记录，以及已核验工具与未确认工具两种结果；不能只验证容器重启后的新进程路径。

主动停止、取消、配置错误和未确认的写操作不能套用这一恢复路径。重试须有界；QwenPaw 对每个已验证进度点最多自动续跑一次。真实验收应在一次工具成功后截断后续模型流，核对任务自动续跑且远端副作用没有重复；同时覆盖主动取消不续跑、未知工具结果仍保持核查状态。单独的“模型健康探测成功”不能覆盖多段系统提示、工具、流中断或长消息兼容性。

### 原生请求超时与诊断级 Trace

不能只因暂时没有 SDK 消息，就提前中断仍由原生 CLI 管理的模型请求。网络超时由原生传输控制；Harness 等待期间继续接收停止和 steer，主动停止仍须有明确完成回执。验收覆盖延迟返回、真实原生超时传播和等待中的停止，不能用额外静默倒计时替代。Claude 原生配置见 [API_TIMEOUT_MS](https://code.claude.com/docs/en/env-vars)。

完整任务 Trace 包含真实模型调用、返回的推理、工具参数/结果、错误及 Task/Worker/Agent/Team 元数据；不代表默认导出 SDK 每个 token 或每次日志落盘的 TRACE 级内部诊断跨度。该级别过滤应在对应 Harness 的原生 exporter 内实现并通过真实 SDK 验证，不能在 Core 按运行时或工具名称写排除分支，也不能删除历史证据来让查询通过。

原生输入既可能保存为字符串，也可能保存为内容块数组。适配器须覆盖长输入、首次请求和续接，按原生请求 ID 与内容哈希核对实际发送的消息；不能只支持一种存储形态，也不能用当前 Task 文本猜补模型输入。采集关闭时不导出正文；请求超时且原生服务没有返回正文时，保留实际错误类型，不能伪造模型输出。

正文采集开启时，模型与工具的输入输出须经过脱敏后完整导出，不套用普通日志或聊天预览的长度限制。回归须覆盖超过 256 KiB 的原生请求，并经过真实 exporter 验证尾部内容仍存在、凭证已脱敏；关闭采集时这些字段仍须消失。普通日志和诊断属性的长度限制不因此取消。

完整查询的单页可能含数 MB 的输入输出。按真实数据验证分页的请求预算，同时遵守调用方总期限；任何页失败均不能标记为完整结果，也不能通过省略输入输出、旧轮次或失败节点使查询通过。

### Manager runtime switches retain terminal input identity

A runtime switch must not execute a previously completed Human request again. Native session caches belong to each Harness; completion of the Manager input belongs to the stable member account and room. Before admitting an ordinary Manager input, consult the shared `Receipts.was_completed(room_id, event_id)` record. Only an exact room/event terminal receipt permits suppression. Received, committed and read receipts do not prove completion, and equal text does not make two different requests duplicates. Publish the terminal receipt only after the actual reply is acknowledged. Keep the common receipt store with the member workspace across image/runtime changes; do not replace it with a new native session cache. Verify switching with an already-completed side-effect request, a still-pending request and a new request with the same text. This rule does not change Task authority or require reconstructing Task context from Matrix history.

### Silent model streams and native output limits

A protocol adapter must preserve connection liveness while awaiting both initial upstream conversion and subsequent model data. When adapting to Anthropic SSE, native `ping` events may keep an idle connection alive; they are transport events and must never become model text or progress claims. Client disconnect must cancel the owned pending upstream request. Preserve authentication, permission and rate-limit error types. Test the actual bundled CLI, delayed initial conversion, silent reasoning, stream cancellation and credential redaction.

Reasoning tokens consume the native output budget. Preserve a native `max-tokens` stop reason as a typed failure, with an ERROR trace; it is not a completed Task delivery. An automatic recovery may release only an execution whose recorded operations have confirmed outcomes. Re-reading inputs does not reset a bounded retry budget. Use the declared model budget (including the existing `AGENTTEAMS_MODEL_MAX_TOKENS` override where supported) rather than silently substituting another model or disabling reasoning.


The same rule applies to native reasoning/acting iteration limits. Preserve the SDK's typed terminal reason through its public event/envelope boundary, rather than matching model text. Wait for native cleanup and persist verified operations before asking Core to release a failed attempt. Bound continuation by the execution node; additional read receipts alone must not reset the budget. Exercise the real native Agent/CLI and its final-event conversion, alongside unknown-effect and Human-stop counterexamples.

Do not impose an undocumented wall-clock Task cutoff while the native run is making valid progress. Retain explicit operator deadlines and bounded interaction expiry separately. Regression tests must cover a progressing turn beyond the old default, an explicitly configured timeout, Human cancellation and approval settlement. A joined Task room must remain discoverable after a runtime configuration refresh; restore it only from verified membership, never from an arbitrary incoming room ID.

For Manager capability acceptance, inspect the current native tool schema and actual terminal tool result. A model may repeat an obsolete limitation from resumed history; its claim alone proves neither the absence of a tool nor successful execution. Preserve history, verify the actual call, and do not count a configured working directory as shell output.


During native control polling, temporary Task-authority unavailability is not proof of reassignment. Keep the owned native run alive and defer that lookup; continue to validate tool authority before new side effects. A confirmed identity change, Human stop or explicit deadline must still terminate and reap the owned process group. Cover both outage and confirmed-stop cases with real child processes.

Progress throttling must account for the awaited publisher's completion time. Slow Matrix acknowledgment must not cause every already-buffered native thinking delta to publish synchronously again. Preserve the latest/final preview, full Trace content and responsive stop/approval handling; use an actual SDK stream with an intentionally slow publisher to verify the adapter, rather than changing model output or reasoning budgets.

### Unified MCP outcome and retry policy

A business result may be a successful application or a verified idempotent match: both finish successfully, including a retry that finds the requested resource already in the intended state. HTTP status alone does not determine business success. Preserve native structured results rather than matching error sentences.

Use one shared business policy across Harnesses: successful completion and a proven idempotent hit are success; do not retry them. Bound repeated attempts of the same tool operation to three, recording each reason and the final result. Exhausting that local budget is not a Task failure: return the confirmed failure to the Worker and keep normal inference and other tools available. The Worker may correct arguments, use another available tool or change its plan. Only when the Worker determines that its available capabilities cannot advance the Task should it submit a BLOCKED report through the existing Task system and ask its Leader to decide. Never infer this Task-level decision from a tool error count alone. A write whose response was lost must be reconciled before another execution. A status code or an "already exists" sentence alone is not evidence that the requested effect was achieved. Adapters translate their native completion envelopes; do not grow independent per-vendor response-code policy lists.

Log the operation and native call IDs, Task/Agent identity, attempt number, original failure reason and final outcome; exclude credentials. Present the concrete final reason to the user. A blocked report may be submitted by the current owner while an operation awaits verification; retain its checkpoint and pending effect, and use the existing Task submission notification to reach the Leader. Reporting a blocker does not mark the tool successful or authorize another write.

For duplicate native return publication, the shared finish helpers already acknowledge an exact recorded return as `idempotent_hit=true`. They verify the current owner, operation identity and immutable stored receipt bytes, return success without another checkpoint mutation, and reject a different result. This acknowledges a previously recorded operation; it never authorizes running its tool body again. Upstream business idempotency requires an explicit successful result or verified target state. Do not claim this receipt path proves every upstream MCP operation is idempotent.

### Conditional checkpoint conflicts and actual native returns

A successful native tool result and its immutable return bytes can exist before the final execution checkpoint commits. Treat a structured metadata CAS conflict (`code=conflict`) separately from ownership loss, changed checkpoints and an unknown network outcome. Shared Core recovery may re-read and retry the same checkpoint once only while instance, process, execution generation and the original checkpoint digest remain unchanged. Never re-run the tool, clear its started operation or accept arbitrary conflicts as success. Keep this logic in shared recovery, not separate Harness branches. Test a concurrent Leader metadata amendment as well as a changed owner/checkpoint and an unavailable response.

When a historical node is already fenced, settle it only from a verified native return with matching call IDs, argument digests and immutable content hash through the normal owned finish API. Record operator settlement as recovery evidence, not autonomous acceptance. A model's statement that an operation succeeded is not a return receipt.

A receipt publication failure must not replace the native tool return with a transport error in the SDK journal. Preserve the actual return before stopping the turn, and fence subsequent model requests and already-queued tools until Core verifies the same operation. Test a completed side-effect body followed by a failed receipt commit: the native result remains intact, the body executes once, and neither a queued tool nor the next model request proceeds. Evidence preservation alone is not automatic recovery; separately prove reconciliation from committed native evidence under the current Core owner, including lost commit responses and changed owners. Never infer `effect=none` from a failed receipt RPC.

Shared `Recovery.finish_tools` and ordinary-message finish helpers stage confirmed returns in the member-private runtime directory before their first remote receipt read, and back them up in the existing private object store. On later Task/message claim, only a matching outstanding operation may consume that return through the normal owned finish API. The current owner is still checked, admitted call IDs and pre-effect rules are still validated, and a missing/unknown return never unlocks inference. Keep original return provenance when ownership transfers. Include local upload failure and loss of the container-local cache in regression coverage; the runtime directory itself is not necessarily a mounted durable volume.

Preserve the distinction between a returned HTTP error response and a request whose response was lost. A native MCP adapter may record a typed HTTP failure as a terminal failed tool receipt, without claiming `effect=none` or rolling back partial effects. An actual disconnect or timeout without a response remains unconfirmed. Verify this at the bundled SDK boundary with a returned HTTP error, a disconnected request and an expired session; a text-only mocked error does not prove the native classification.

For Responses-compatible models, verify actual upstream streaming and reasoning-event conversion, including gateway `reasoning_text` events, rather than checking only local SSE headers or heartbeats. Runtime-native conversion and model capability adaptation belong in the Harness. A missing public pricing-map entry must not silently buffer a requested streaming response. Preserve known capabilities and explicitly selected output/reasoning budgets.

Native uncertainty flags must reach both Task and ordinary-message admission even when the SDK renders, truncates or spills a tool result. Observe the raw result at the installed native executor boundary, bind it to the native registered handler and call ID, and preserve the unknown-effect decision outside the model-visible display text. Do not search a rendered message or user JSON for error phrases. Verify a spilled native unknown return, a normal returned server error, a lookalike from another handler, and cross-agent/call isolation.

A lost HTTP response is not proof that a checkpoint write failed. For Task and ordinary-message checkpoint writes, shared recovery reads the authoritative state back and accepts only the proposed checkpoint business fields under the same instance, process and execution generation. Core writes `saved_at` itself and serializes absent fixed list fields as null; exclude only that server-owned timestamp and normalize those fixed absent/null fields before comparing. Never ignore operation, version, artifact, result or other field changes. It does not repeat the POST or native tool body, infer a release, or accept changed ownership or checkpoint content. Connection resets and incomplete HTTP responses must enter this same transport-error path. Validate a real server committing the checkpoint and closing the connection before sending its response; a mocked timeout alone does not cover this boundary. If read-back cannot establish the exact commit, retain the unknown operation and its evidence.

### Task Trace history survives DAG replacement

Replacing a failed node must not remove its actual model/tool consumption from the main Task query. The current DAG is an execution plan, not the complete execution history. Include a historical node only after its retained execution record in the authorized Task storage scope confirms its Task ID, main Task ID, Team and Project. Preserve its recorded status and Trace identity; do not reinsert it into the active DAG or trust backend tags alone. Test replaced-node inclusion alongside foreign Team, foreign main Task/Project, missing record, wrong kind and unsafe ID exclusions. Total usage covers the returned recorded calls, while capture completeness remains a separate claim.

### 所有生成文件使用统一任务工作区

任务系统提供的 `outputDirectory` 是当前执行节点的生成文件目录，
`scratchDirectory` 是临时目录，`inputs` 为只读输入。代码、克隆后的仓库、
测试脚本、数据库、报告和截图都遵守这个约定；不得依赖业务 Skill 或某个
运行时的默认 cwd 才成立。公共 Task 上下文和任务 Skill 负责表达这个规则，
Harness 负责把这些已验证目录映射到原生工作目录和文件工具的写入范围。
路径被拒绝后应依据这些目录纠正目标，不因普通任务文件写入而扩大权限。

普通任务房间消息续接同样必须在模型和文件工具开始前准备目录。Core 通过
房间绑定选定当前调用者拥有的执行节点，再复用幂等 `begin` 准备上下文；
没有所属执行节点的房间仅提供任务状态，不创建或猜测工作目录。不应等到
模型调用 `submit` 时才补上执行上下文。保留主任务快照与当前执行节点，
让续接继续使用同一节点目录，而不是退回成员主目录。

验收包括正式派单和普通消息续接：均使用真实准备的目录写入文件；上游输入
保持只读；相邻任务和目录穿越不能成为写入目标；明确的本地文件失败正常
记录终态，让 Agent 可以纠正路径，未确认的中断不能伪装为成功回执。


### 单次失败与接收进程生命周期

MCP 初始化握手不应因为本轮业务上下文暂未绑定而直接终止整个 stdio 接收服务。未绑定时不能伪造角色、任务或权限，应对本次工具请求返回明确失败；后续请求重新读取运行时提供的上下文。补记派单或交接历史的原生会话也必须传入其初始化上下文，不能借用已经结束的上一轮环境。

首次控制面注册与后续轮询均须有明确异常边界。暂时断连结束本次尝试，保留接收进程、日志和后续恢复入口，不因全局累计三次失败而永久退出；业务工具是否重试仍受该操作的回执、所有权和真实效果约束。主动停止、真实撤权和安装依赖缺失不能伪装成请求成功。

任何结束回调或清理步骤抛出异常时，也必须在 finally 中清除本地运行标记和上下文变量，避免下一次触发被过期状态拦住。验收至少覆盖：连续通信失败后恢复、MCP 上下文晚到、初始化失败后的再次触发、清理失败后同一会话接纳下一轮；核对旧工具未重放、当前 Task 身份和进度未丢失。原生 CLI 的单轮退出与整个 Worker 接收进程退出应分别记录。


### Admission failure before native dispatch

If checkpoint publication and its exact readback are both unavailable, the common Task and ordinary-message admission helpers must retain a durable error receipt stating that the native body was not dispatched, then return the original failure. Only record this at the admission boundary before returning the operation ID to the adapter. Do not apply it after native invocation, infer an effect from an HTTP code, overwrite an existing native return, or replay a completed call ID. Recovery still validates ownership, call identities and checkpoint evidence through the normal finish API. This is shared Core behavior, not a runtime-specific error-code adapter or a YAML schema extension. Cover both admission paths, unavailable readback, duplicate calls and immutable receipt conflicts.

### Recovery polling and standalone test dependencies

Repeated registration of the same actual Worker instance, process and runtime must not rewrite the shared Matrix state. Polling and tool hooks share its conditional version; only an actual startup change needs a registration write. Subsequent cursor and tool writes still require their normal ownership and conditional checks.

Run Harness tests against the explicitly selected shared Core source, including its telemetry library. Tests that require an installed native SDK must run in the native-image job as well as the lightweight suite; an optional import in a lightweight environment is not native acceptance. Use both unittest-style and pytest-style collection when the repository contains both.

Polling recovery tests must supply a finite sequence of failures followed by success and an explicit test stop. Verify bounded backoff and preservation of the same process/state, rather than expecting the bridge to exit after three failures. A mocked sleep with a permanently failing poll can make a test spin indefinitely and does not demonstrate a runtime crash.
