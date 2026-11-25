# AgentTeams Harness 接入与准入标准

本指南定义 Harness 接入 AgentTeams（AT）的能力要求和准入标准，覆盖运行时启动、Task 协作、会话与回执、重启续跑和 Worker 跨 Harness 切换。Task 协作使用公共协议 `task/v1`。Harness 是承载模型交互、原生工具和会话的运行时。接入时先声明支持的角色（Manager、Leader、Worker）和部署模式，按声明范围验收。外部纳管模式复用同一协作语义，按自身部署方式提供身份、会话和执行证据。托管镜像的启动变量见下一节。

## 接入边界

| 位置 | 维护内容 |
| --- | --- |
| Core | 公共状态机与权限、任务及文件 API、CLI 和原生桥接、角色提示词与技能、测试用例和判定器。这里维护接入指南、运行时注册与构建引用 |
| 独立 Harness 仓库 | 原生 SDK/工具注册、Matrix 或受信通道适配、会话和回执注入、启动入口、镜像构建及原生回归 |

Core 中的 `worker/`、`qwenpaw/`、`hermes/`、`deepseek-harness/`、`openhuman/` 是对应独立仓库的 Git 子模块路径。Codex Harness 由发布工作流独立检出。原生适配的改动提交到所属仓库，再在 Core 更新对应引用。能复用同一镜像的多个角色使用同一构建产物，由启动配置选择角色。[子模块定义](../../.gitmodules) · [Harness 发布工作流](../../.github/workflows/publish-harnesses.yml)

在启动入口读取 Controller 投影的成员配置，加载模型、工具、技能和通道，绑定稳定的成员身份并报告实际可用状态。Native SDK 的配置和会话数据由 Harness 维护，公共配置及持久状态以 Core 的契约为准。

一次任务从 Human 原会话进入。Manager 用 `create` 绑定原始请求和 Leader；Leader 在同一主任务上用 `plan` 自主决定节点、分工和依赖；Worker 经 `begin` 领取并准备输入，完成后用 `submit` 保存候选和产物。Leader 用精确 `submissionId` 验收或退回，Controller 根据已验收结果推进依赖。Leader 用 `complete` 完成主任务，框架将最终报告和确定版本的附件投递回原会话，并把交付事实同步到派单 Agent 的上下文。取消、重试和恢复也沿用同一套任务身份、状态及回执。

接入按顺序完成三件事。

1. 在 Harness 仓库实现正式启动入口和角色配置，接入公共 Task 工具、可信入站、原生回执和恢复。以真实 SDK 注册、实际模型请求和进程环境为证据。
2. 在 Core 对接运行时注册与构建引用。使用同一候选的 Core 和 Harness 版本构建或部署，验证实际加载的公共资产、角色、运行时身份和镜像摘要。
3. 用真实会话跑完下述准入验证。组合间发现协议缺口时，在公共契约或产生不合约事实的一侧修复。

[公共协议与 CLI](teamharness/task-cli-protocol.md) · [成员运行时配置](member-runtime-config-contract.md)

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

支持异步工具的 Harness 要保留原始调用 ID。工具返回运行句柄时，框架继续使用同一次调用的等待机制，直到取得结束状态并为原操作保存回执。后续工具和任务提交遵循现有准入规则。接入验证需让真实原生工具跨过首次等待窗口，检查后续等待能够取得终态、回执关联原调用、任务继续推进。进程中断后缺少终态证据时，仍走公共核验流程。

框架能确认原生注册工具只读时，在准入日志为该次调用记录 `read_only: true`。收到同一调用的终态错误后，用 `status: error` 和 `effect: none` 保存错误回执并恢复后续工具准入。核对真实 SDK 注册身份及调用 ID，工具参数和输出文字不参与只读能力判断。缺少可信终态或副作用未知时保留公共核验。

SDK 能以结构化错误证明在任何副作用前拒绝时，框架可在准入记录的 `pre_effect_error_codes` 声明该工具的精确错误码。回执须关联原调用 ID，并同时提供 `status: error`、`effect: none` 和匹配的 `error_code`。错误码来自原生工具结果，准入列表来自 SDK 对接代码。

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

### 其他原生 Harness 的同一约束（2026-11-25）

- OpenClaw、QwenPaw、Hermes、OpenHuman 都通过公共 `model_context` 与 `model_result` 投影自动准备的上下文和终态工具返回。显式读取仍返回请求的数据；自动交接不重复携带整个 Task、全部报告和文件预览。正常规划完成也不产生成功控制回执聊天。
- 受信 Matrix 用户身份须同时覆盖实时消息与恢复入口，否则提交回执虽保存，实际发送事实无法回填。身份来自已登录原生客户端，不来自模型参数。
- `completed` 是直接成功交接的合法终态。当前同一代次、进程、实例只能完成已有工具返回及持久化、投递；不能以此重新进入模型或执行新工具。空最终文本只有存在权威持久结果才可收尾。
- QwenPaw 的 Leader 直接接收其 Admin 的需求时，同样绑定原始 Human 来源，在原会话结束受理，进入任务房间规划。SDK 的原生上下文必须保留工具结果和已确认发送事实，不能只保存到旁路缓存。
- OpenHuman 在终态关闭可复用会话登记后，仍须把真实工具历史和发送事实写入自身原生会话文件；停止登记不能成为跳过持久化的理由。恢复扫描不得重启已完成节点。
