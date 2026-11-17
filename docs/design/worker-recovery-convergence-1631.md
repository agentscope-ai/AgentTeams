# Worker 恢复标准与收敛计划（#1631）

状态：用户已确认的实施基线，按 A → B → C 阶段恢复推进。
新增路径和字段是目标标准，不表示代码已经实现或验收通过。
核查基线：Core `18a3b17c214bee2dc1d172bd8d42da817439adc5`。

## 一、结论与现有基础

采用一套 Worker 数据契约、一个公共恢复入口、六个原生适配器。
运行时切换走 **源 Harness → Worker 公共记录 → 目标 Harness**。
30 个有向组合用于回归，不产生 30 套转换逻辑。

| 已有内容 | 复用方式与证据边界 |
| --- | --- |
| Controller 发布 `runtime/runtime.yaml` | 已有模型、MCP、Skills、身份、通道策略、凭证绑定和资产来源字段；仍须逐项验证原生生效值，投影测试不等于启动验收。 |
| Task 自发现、执行权、条件提交、步骤检查点、产物与回复记录 | 继续使用现有 Task API 和对象存储，不新建 Task 数据库或第二套检查点。Core #1651、Codex #10、Hermes #9、DeepSeek #9 已合并。 |
| 正常重启并复用有效 session | Codex08、Hermes11、QwenPaw29 有各自 SHA 的真实证据，共 3/6；并非六种完整故障集。 |
| 有向切换的任务执行／交付 | Codex↔Hermes、Hermes↔DeepSeek、Hermes↔QwenPaw、Hermes→OpenClaw33，共 7/30；不是当前统一版本重跑。 |
| 后续适配 | Core #1655、Hermes #11、QwenPaw #11、OpenClaw #10、OpenHuman #3 仍为 Draft。OpenHuman 原生测试及镜像 CI 通过，真实任务续跑未测。 |

当前实质缺口：公共 receipt 只有收到／写入会话证据；QwenPaw 的 token、
accepted-event 状态仍在 `WORKING_DIR`，其他 Harness 也有各自消费存储。
公共消费记录、统一启动判断、持久交接操作、Dashboard／CLI 配套仍需补齐。
OpenClaw34 完成了剩余步骤，但有效 session 复用失败，不能增加重启通过数。

新公共契约的 Hermes 正常重启已在 case37 通过：Core262ecb13、Hermes59b756d、
共享客户端19eb30c6、SDK2bd378ed。首次重启保持有效配置版本3并复用实际原生会话，
无新 Matrix 输入完成剩余步骤，产物／最终回复无重复；见
[case37 证据](../development/evidence/runtime-recovery-1631-case37-verified.json)。
前一 case36 的配置版本误增／会话未复用保留为 partial。历史覆盖数仍为3/6、7/30，
不把本次同 Harness 重跑算成新 Harness 或跨 Harness 方向；完整故障集和 UI 仍未验收。

方案选择：只搬私有目录不能解决消费语义；另建执行数据库会重复现有 Task
权威。推荐补足 Worker 公共消息记录，并复用现有 Task 执行协议。

## 二、标准路径、格式与写入责任

以下路径均相对于已配置的租户／安装存储根。`W` 是 Controller 解析的稳定
`member.runtimeName`，不是 Harness、容器 ID 或裸显示名；`S` 是团队 storage ID。
团队归属和读写权限由认证身份解析，客户端不能任意填写其他 Worker／团队路径。
Project 仅为可选查找信息，不进入持久执行身份，也不扩宽 Project list 权限。

```text
agents/W/
  runtime/runtime.yaml                    # 已有：Controller 发布期望配置
  config/、skills/、AGENTS.md、SOUL.md      # 已有：公共配置与资产，保留当前布局
  runtime/state.json                      # 新增：当前启动身份、持久交接操作
  runtime/matrix/state.json               # 新增：房间覆盖位置、消费者游标、待处理索引
  runtime/matrix/events/<event-key>.json   # 新增：持久输入、处理引用／非 Task 执行状态
  runtime/matrix/results/<sha256>.txt      # 新增：非 Task 消息的已保存回复
  runtime/sessions/<harness>/<work-key>.json # 新增：统一 session 缓存描述，不存跨引擎原生格式

teams/S/shared/tasks/T/
  meta.json                              # 已有：Task 状态及 execution.checkpoint 权威
  spec.md、output/、result.md、checkpoints/ # 已有：任务描述、产物和保存的回复
```

原生 session 数据可保留在 SDK 要求的私有目录；统一描述文件仅引用它。
`runtime/compat-config.json` 等原生配置是可重建的本地派生文件，不是持久权威，
不通过 workspace mirror 回写。模型／MCP 密钥继续走声明的绑定与秘密挂载；
Matrix device／加密材料属于传输身份状态，按绑定的账户、设备和格式恢复，
不作为可跨 Harness 导入的 LLM session。
资产保留现有写入责任：Worker 维护的 SOUL／记忆写回 Worker 公共位置；
Controller 发布 YAML 时不能将未声明覆盖的持久个性内容清空。Skills 的文件内容
继续由公共资产／包来源恢复，不把安装后的原生私有副本当成唯一来源。

| 记录 | 格式与最小字段 | 唯一提交责任 |
| --- | --- | --- |
| `runtime.yaml` | 保持现有 `MemberRuntimeConfig`，含 `apiVersion`、`metadata.generation`、`member`、`desired`、`matrix`、`storage`、`credentials`。Skills／MCP 从该记录及公共资产生成原生配置。 | Controller 从现有配置 API／CR 发布；原生 UI 修改公共设置也必须写回该入口。 |
| `runtime/state.json` | `schema_version:1`、`kind:WorkerRuntimeState`、`member_prefix`、`active_start:{instance_id,process_id,runtime}`；`handoff:{id,phase,source,target,checkpoint_refs,error}`。没有切换时 `handoff` 可为空。 | Controller；supervisor 注册实际启动身份，不能把随机 HTTP header 当作旧进程已停止的证明。 |
| `matrix/state.json` | `schema_version:1`、`kind:WorkerMatrixState`、`member_prefix`、`active_start`；`rooms` 保存 `through` 锚点与 `gaps` 分页缺口；`consumers` 保存带 Harness／账户／设备／格式身份的原生游标；`pending` 只保存未完成工作对应的 event-key。 | 公共消息模块通过 Controller 的自身作用域条件提交接口写入；Harness 不直接 mirror 覆盖。 |
| `events/<event-key>.json` | 标准输入与上下文、原生写入证据，以及 Task 引用或非 Task 执行记录，见下例。 | 公共模块持久化；适配器只提交原生事实。 |
| Task `execution` | 保持已有 `generation`、`instance_id`、`process_id`、`runtime`、`state`、`checkpoint`。检查点保持已有步骤、operation/evidence、artifact、result、delivery、reply 字段。 | 现有执行 API 校验归属、进程和 ETag；Worker 消息索引不复制权威检查点。 |
| session 描述 | `schema_version:1`、`kind:WorkerSessionCache`、Worker／团队／工作身份、`runtime`、`native_ref`、`native_digest`、`checkpoint_version`／`checkpoint_digest`、`config_generation`、`expires_at`。 | 公共缓存模块写统一描述；适配器保存／读取真实原生会话，并给出最终持久内容摘要。 |

示例：Task 消息记录，不复制 Task 的执行状态：

```json
{
  "schema_version": 1,
  "kind": "WorkerMatrixEvent",
  "member_prefix": "agents/worker-a",
  "room_id": "!room:example",
  "event_id": "$event",
  "input": {
    "sender": "@leader:example",
    "type": "m.room.message",
    "origin_server_ts": 1794900000000,
    "content": {"msgtype": "m.text", "body": "..."}
  },
  "input_state": "received",
  "intent": "task_notification",
  "work": {"kind": "task", "team_id": "team-a", "task_id": "T"}
}
```

身份摘要函数 `H` 统一使用 UTF-8、紧凑 JSON 数组、不转义 Unicode，再取小写 SHA256：
`event-key = H([room_id,event_id])`；Task 的 `work-key = H([member_prefix,
"task",team_id,task_id])`；普通消息的 `work-key = H([member_prefix,"message",
room_id,event_id])`。禁止依赖文件名转义碰巧一致。检查点摘要继续调用已有
`Recovery.digest`，其他语言通过公共 facade 获取结果，避免各自实现 JSON 排序
和 Unicode 编码。`config_generation` 使用整数；新记录的时间使用 UTC RFC3339，
`expires_at` 使用 Unix 秒数以兼容现有缓存判断；摘要为 64 位小写十六进制，size 为字节数。

附件、长输入和历史可用带 `key/sha256/size` 的持久引用代替内联内容；只有
room/event ID 而没有可读取的内容／历史恢复路径，不算可靠输入记录。
`input_state=native_committed` 只证明原生输入已保存，必须附对应会话／摘要证据，
不能用于判断任务完成。`intent` 按既有发送者／房间策略分类为 context、message、
task_notification 或 control；Task 通知继续验证 #1646 的 receipt，不能信正文。

普通聊天没有 Task ID 时，`work.kind=message`，在同一事件对象中保存与 Task
同语义的 `execution`、产物和回复引用，复用公共恢复判断，不暗中创建用户 Task。
已进入上下文的普通输入也可能尚未执行，不能因为 read 就从 pending 删除。
纯上下文／控制事件只有经既有策略明确处理后，才能以原因记录结束接管。

### 持久化顺序与版本

1. 保存输入对象，确认远端可读，再条件提交 pending 索引与覆盖它的游标。
   `/sync` 出现 limited 时间线时保存缺口和分页位置，补齐之前不宣布完整消费。
   新 Harness 不直接套用旧 SDK 的 token；从公共 pending／房间覆盖记录恢复，
   使用自己的原生消费者，并按稳定事件 ID 去重。
2. 原生会话完成真实落盘后才发布 native-commit／session 描述。会话若仍会
   在 callback 返回后再保存，适配器须在最终保存完成后给摘要；不能复用提前计算的值。
3. 先上传并校验产物／回复字节，再条件提交引用它的检查点；最后发送并记录
   Matrix event。Task 回复沿用 Task 已有 transaction，不另建平行 outbox。
4. Controller 对公共状态执行 If-Match／create-if-absent、当前启动身份和权限
   校验。对象存储权限同步禁止 Worker 直接改写这些公共控制对象，结果／产物仍保留
   允许的上传路径。旧本地文件、旧进程和 workspace sync 都不能无条件覆盖这些对象。
   同容器内替换 native 子进程，也必须先确认旧进程树停止再登记新启动身份；
   目前 Task claim 对同 instance 的不同 process 会拒绝接管，这部分仍要补实现。
5. 新 JSON 的 `schema_version` 是格式版本；Task `checkpoint.version` 是进度版本；
   YAML generation 是配置版本；Task generation 是执行代次；ETag 是并发提交条件。
   这些值和通知 outbox lease 各司其职，不相互替代。未知格式禁止盲写／降级重放。

Controller 是提交边界，MinIO／OSS 是持久载体；本地 JSON／SQLite 是缓存。
不新增独立数据库，不将“本地 rename 成功”或“后台同步稍后会上传”视为远端提交成功。

### 阶段 A：首批实现边界

已实现公共 Matrix 存储、Worker 自身作用域 API 与 Python／JSON-stdin facade：

- `GET /api/v1/runtime/matrix`：返回自身状态、ETag、实际 instance／Harness。
- `POST /api/v1/runtime/matrix`：`action=start` 登记实际启动；`action=ingest`
  先持久化事件，再条件提交 pending／房间覆盖／原生游标。
- `GET /api/v1/runtime/matrix/events/{key}`：读取自身持久输入；不能指定其他 Worker 前缀。

首次登记用 `If-None-Match: *`，后续写入用已读 ETag；进程请求头与请求体必须一致。
消费索引的 `active_start` 与游标在同一 CAS 对象内，启动换代直接使旧提交失效，
避免跨对象“先检查进程、再无条件写游标”的竞态。生命周期 `runtime/state.json`
仍待实现，其启动描述不能替代消费索引的提交 fence。

原生逐事件 callback 可不带 consumer，先保存输入而不虚构 batch token。
有 consumer 时，提交格式为 `{runtime,account,device,format,token,from_token}`；
runtime／账户／设备／格式共同确定消费者，`from_token`
必须匹配共享记录的上次 token。它是请求条件，不作为另一个游标持久化。
普通增量 sync 会保留已有 gaps；显式补页完成及缺口关闭入口仍待实现。

同事件再次接收时只比较不可变输入和工作身份，保留较新的执行／native-commit
证据。输入持久化、未知格式拒绝、严格 CAS、同实例竞争进程、替换后的晚到提交、
并发批次与旧 Harness 游标回归已有确定性测试；不代表真实 Harness 续跑验收。
普通消息已有 `GET/POST events/{key}/execution` 的领取、检查点、补发及取消入口，
复用 Task 检查点验证规则；Task 引用消息不在这里创建平行执行记录。公共 facade
返回 resume／deliver／verify／done、执行身份、检查点摘要和恢复上下文，只有
resume 能进入正常模型／工具执行。投递确认不允许夹带新步骤；取消后晚到回调被拒绝。
这些是公共接口实现与确定性测试，不是原生会话／真实续跑验收。

pending 保留原生接收批次顺序，不能按 event ID 摘要或发送时间重新排序。
原生适配须在调度前提交有序输入；补页完成前不能将缺口误报为完整覆盖。
分页补齐应保留原覆盖锚点，明确区分“接回已知锚点”和“当前账户无更多可见历史”，
不能因为分页没有 end 就丢弃仍有已知未恢复锚点的缺口，参见
[Matrix messages API](https://spec.matrix.org/v1.17/client-server-api/#get_matrixclientv3roomsroomidmessages)。
显式补页完成、H/Q 原生接入、native-commit／session 描述与完整交接仍在后续实施范围。

## 三、统一恢复与切换机制

每次正常启动、异常重启、替换容器、切换 Harness 都调用同一个公共入口：

1. **初始化配置**：拉取 Worker YAML 和资产，校验身份／版本，解析当前凭证，
   生成目标原生配置并读取实际生效值。配置不完整时报告具体缺项，不读旧私有配置凑齐。
2. **登记启动并发现工作**：核实当前实例与实际进程启动身份；读取公共 pending、
   补齐房间缺口，同时从 Task 系统发现分配给自己的可恢复任务。Task 发现不依赖消息到达，
   也不依赖消息索引必须完整；按 team/Task ID 去重后重读归属与取消状态。
3. **领取与决策**：先取执行权，再校验最新检查点和产物。已有结果待投递只补发；
   未核验 operation 先核验或停在待核验；其余只执行 remaining steps。
4. **选择会话并执行**：存在、未过期、身份／配置／最新检查点及原生摘要均一致才复用。
   否则用任务描述、最新检查点、产物及必要房间历史建立新会话，保留同一 Task ID，
   自动触发原生推理。模型与每个工具调用前继续校验执行权。
5. **保存与结束**：步骤边界远端提交；结果持久化后以稳定 transaction 回复。
   待处理索引在对应工作确已结束后删除引用，历史输入及旧会话保留用于审计和返回切换验证。

计划切换由同一个 Controller 操作执行：

```text
preflight → draining → checkpointed → stopping → stopped → creating → recovering → completed
```

`draining` 停新执行，当前任务走到最近可确认步骤边界即交接，Task 仍为 in_progress。
停止阶段确认旧实例、原生进程和工具子进程都停止，再创建目标。切换过程中重启
Controller 从持久 phase 继续；每个外部动作重试前查询实际资源，避免重复创建。
创建／恢复失败保留失败阶段及检查点，不擅自把旧实例并行启动作为回滚。
未知副作用与进程无法确认停止时报告等待核验原因，不盲跑下一步。

Dashboard 和 `agt` 只提交／读取这一操作的 ID、阶段和失败原因，不自行删容器、
重放 Matrix 或生成检查点。现有 runtime update 仍是配置提交，不能冒充已完成切换。
Worker 与 Manager 角色分别验收；原 Manager 旧消息回放回归保留在 #1631，
不能把 Worker 任务通过直接算成 Manager 通过。

## 四、适配与兼容迁移

公共层负责配置校验、状态存储、发现／领取、恢复决策、检查点和回复事务。
每个 Harness 只接四组原生接口：配置应用／生效读回、session 保存读取、原生执行／取消、
Matrix 收发及输入持久化事实。沿用现有 Python 模块与 JSON/stdin facade，不另造 RPC 框架。

| Harness | 保留的实现 | 接入重点 |
| --- | --- | --- |
| Codex | 已有 discovery/claim、thread 恢复和输入 journal | 事件记录接公共 ledger；中断后自动决策，不提示等待新消息才继续。 |
| Hermes | 已有真实 SessionStore、恢复 turn、Task 工具 | 公共消费记录替代恢复依据；保留原生 MCP／工具生命周期，不复制 Task 决策。 |
| QwenPaw | 已有 TaskExecution、SafeJSONSession 和原生保存钩子 | token／accepted-event 从私有路径迁入标准存储，结果补发与执行中恢复分开；共享启动入口。 |
| DeepSeek | 已有 bridge、native session fallback、Task 协议 | 接公共 ledger／决策，补真实未知副作用核验；不再重复维护状态分支。 |
| OpenClaw | 已有共享 RPC 和原生 Task 适配候选 | 修正最终持久化与 cache 摘要时序；保留设备状态与旧游标，真实验证 session 复用。 |
| OpenHuman | 已有 native dispatcher/tool loop、共享 RPC 和 MCP 取消 | 接统一记录；先验证真实任务链路，原生配置 smoke 不能代替续跑。 |

迁移时先停止并确认旧消费者，再读取旧状态。只在公共记录尚不存在时，通过
受控的一次性导入写入公共格式，记录来源与摘要；公共记录存在时以其为准。
旧 token／accepted IDs 只能导入为消费／接管证据，不能导入成“执行完成”。
无法判断的输入保留待核验，已执行副作用不能靠重新投递来猜结果。
保留旧文件供核对；完成导入后禁止 workspace mirror 回写公共控制对象。
会话格式不做跨 Harness 转换，旧 Harness 切回时重新与最新共享记录比对。
已拥有公共状态的 Worker 若目标旧镜像不支持该格式，预检拒绝切换，不能静默退回私有写入。

## 五、收敛顺序与效率改进

之前耗时的主要可改进点：公共消费与执行边界没有先定稿；原生适配、测试夹具缺依赖、
session 保存时序、镜像构建、Tracing／UI 问题交错处理；各仓 shared SHA 不统一，
一次局部修正又触发后续构建与验证。磁盘曾造成实际失败，但不能解释全部耗时。

后续按以下交付顺序推进，每一段有可审查产物与验收出口：

| 阶段 | 具体产物与出口 | 恢复执行后的初步工时估算 |
| --- | --- | --- |
| A：公共记录与恢复入口 | 按本稿固定 schema／路径，实现自身作用域条件提交、输入先持久化、Task 引用、普通消息执行与恢复决策；用已有 Hermes／QwenPaw 验证公共记录读写、重启和双向切换。 | 1 个工程日；4 小时检查点须有可运行的公共存储／决策测试，不能只有方案更新。 |
| B：六适配器与控制入口 | 复用已有原生代码接统一接口，补 supervisor 启动换代／持久交接，Dashboard／CLI 调同一操作；冻结统一 shared SHA 后各镜像构建一次并保存身份。 | 1–2 个工程日；OpenClaw 保存时序、OpenHuman 真链路是较高不确定项。 |
| C：统一回归与修正 | 先六种正常／异常／替换／session 场景，再全部 30 有向组合及返回旧 Harness；结果补发、未知副作用、执行权冲突、取消／重分配分别出证据；完成原 Manager 回放及真实 trace／UI。 | 1–2 个工程日，取决于新缺陷和模型／CI 稳定性。 |

这是单执行者的规划区间，合计约 3–5 个工程日，不是已验证的日历承诺。
完成 A 后按真实回归耗时及实际剩余缺陷重估一次；构建、CI、模型等待与编码工时分开记录。
不再用“只剩验证”概括尚未实现的公共层或 UI。

执行效率规则：公共状态机／权限／顺序先用不依赖 LLM 的确定性测试覆盖；
每个适配器单独测真实 SDK 配置、session 与工具生命周期；真实模型只负责端到端执行证明。
一个矩阵驱动器接受 from/to/scenario 参数，禁止为失败方向加产品专用补丁。
H/Q 参考链路稳定后，先让其余适配器分别读写同一公共记录，再跑完整方向矩阵。
构建保留 SDK 缓存，公共脚本层变更不无故重编译原生 SDK；源码变化后明确哪些证据失效。
Tracing／UI 用同一配置与证据字段验收，另列阻塞，保留验收要求但不阻止公共层先独立验证。

## 六、恢复开发前的审查结论

本稿确定：上述路径与格式；Controller 条件提交为公共持久边界；Task 权威不搬家；
原生会话为缓存；普通房间输入和 Task 均从同一恢复入口自动推进；六适配器不持有独立恢复策略。
新增标准先有共同测试与迁移，再接原生引擎；完整目标不缩减为配置投影或通知补发。

继续按用户授权开发、验证、推送和跟进 PR；未完成范围保持 Draft，#1631 保持开放。
验证通过后可按授权合并，不发布版本／镜像、不部署现有安装。
详细历史证据与实际 SHA 仍以 [恢复验收记录](../development/runtime-recovery-1631.md)
及相邻 evidence 文件为准；配置现状见 [Member Runtime Config Contract](member-runtime-config-contract.md)。
