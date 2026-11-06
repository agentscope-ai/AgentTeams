# Worker 会话追踪：OpenTelemetry → Langfuse → Dashboard

## 目标与选型

从一个 Worker 会话回看每轮请求、模型上下文、工具执行、异常和消息交接，定位链路问题，并为后续 Team 协作质量分析保留数据。采用 **运行时原生钩子 → 共享 OTel 适配层 → OTLP/HTTP → Langfuse v3 → Dashboard BFF**。可观测默认关闭，AT 可先独立启动；Worker 不依赖 Langfuse SDK。

默认通过标准 OTLP 直连 Langfuse，管理员在 Dashboard 保存配置并应用到所选 Worker，即可下发采集开关、地址和认证头。项目密钥保存在 Dashboard 服务端私有文件，直连时进入所选 Worker 的环境变量，读取配置 API 不返回密钥。已有 Collector 的环境可选择继续经 Collector 上报，Worker 只接收 Collector 的地址和认证头，Langfuse 出口仍由 Collector 管理。

网关 LLM 日志无法覆盖本地工具和会话；各运行时直接使用 Langfuse SDK 会分散字段规则和凭证。共享层负责身份、内容处理、协议和查询，运行时只适配执行边界。本次实现 QwenPaw 和 Hermes；其他运行时未实现，不以环境变量配置代替原生验收。

## 执行路径与采集点

| 接入点 | 实现 |
| --- | --- |
| Python 启动 | 通用开关启用 sitecustomize 和标准 OTel SDK；通用模式优先，原 CMS 模式保留 |
| QwenPaw | 真正的 Runtime.run 入口；AgentScope 公开 middleware 记录 Agent、模型、工具、权限检查、上下文压缩 |
| 直接 Agent 调用 | Agent.reply 在没有外层 turn 时补入口，避免重复 turn |
| Hermes | TurnFacadeMixin.run_conversation 记录原生 session 与结果；顺序/并行工具共用执行边界，包含阻断和错误结果 |
| 模型 | QwenPaw 复用原生模型 span，禁用重复 OpenAI 埋点；Hermes 使用 OpenAI instrumentation 并补实际 SDK 请求 |
| HTTP | HTTPX 每次请求记录状态码、耗时、request ID 和 W3C context；本机消息 API 恢复同 Worker 的业务 baggage，模型服务不接收业务 baggage。不记录 Authorization 或 URL 查询串 |
| Matrix | 文本、命令、媒体接收元数据带 event ID / W3C carrier；nio、mautrix 发送时注入 carrier 并记录发送 event ID |
| 子进程 | 复制 child env 传播 traceparent/tracestate/baggage，不逐轮修改全局环境变量 |
| Task | 两种镜像复用现有 TeamHarness find_task_trace_context，按 Room、Project、Worker、活动状态唯一匹配，歧义时留空并说明原因 |

缓存 QwenPaw 镜像的旧 LoongSuite 插件依赖已移除的 agentscope._config。本方案使用实际 AgentScope 版本的原生 middleware。代码位于 shared/lib/telemetry/；Hermes 工具执行边界有签名兼容检查，升级时需重跑原生验收。

## 会话与跨 Worker 因果关系

~~~mermaid
flowchart LR
  A[Worker A session] --> A1[Turn 1: model / tools / errors]
  A --> A2[Turn 2: model / tools]
  A2 --> M[Matrix event + W3C carrier]
  M --> B[Worker B turn + SpanLink]
  A1 --> C[Collector: persistent queue]
  A2 --> C
  B --> C
  C --> L[Langfuse v3]
  D[Dashboard: admin BFF] --> L
~~~

**一个 session 包含多个 turn trace。** session ID 由 Worker、runtime、真实 runtime session ID 和用户隔离键生成稳定标识。不同 Worker 不会混合；QwenPaw 用户隔离键被保留，Hermes 沿用原生 session 的用户/Room 共享策略。原始 session ID、turn ID 和事件 ID 同时保留。

跨 Worker 创建各自 trace/session，通过 OTel SpanLink、agentteams.upstream.trace_id/span_id/task_id 和 Matrix event ID 关联。上游 Task ID 不覆盖接收 Worker 自身任务归属，避免 Langfuse 的 Worker/session 属性相互覆盖。Dashboard 可回到上游 trace，也可按 Team/Project/Room 组合分析。

协作演示通过 Worker 自己的主动消息 API 发送新 Matrix 消息。QwenPaw 最终回复可能编辑已有消息，而 Hermes 默认跳过编辑事件；因此不能把“界面上出现一条编辑后的回复”当作对方已接收任务。发送与接收政策仍由各运行时负责，此分支不改变这些政策。

异步生成器只在推进与关闭时 attach context，yield 前 detach，支持跨任务关闭。Hermes 原生线程池复制 ContextVars。回复可能发生在 turn generator 返回以后，使用请求级 reply context 恢复所属链路，不使用全局“最近会话”。

| 数据 | 排障及分析用途 |
| --- | --- |
| Worker/runtime/package versions/schema/capture policy | 比较版本和配置，识别采集覆盖 |
| session/turn/Task/Team/Project/Room/event ID | 会话回看、任务归属、跨 Worker 交接 |
| Agent 输入输出、模型上下文、工具定义及参数 | 上下文缺失、工具选择与交接信息分析 |
| model/usage/finish reason/模型与 HTTP 耗时 | 模型慢、调用量、输入膨胀、失败后重试 |
| tool call ID、输入输出、阻断/执行状态 | 参数错误、权限问题、执行失败、重复调用 |
| 接收时间、dispatch delay | 进入运行时前的等待，不代表所有上游排队时间 |
| 活动 span 下的警告、错误日志及取消状态 | 将错误和恢复路径留在同一执行上下文 |

标准 gen_ai.* 映射为 Langfuse GENERATION/TOOL/AGENT、输入输出与 usage，原始属性保留。身份、session、标签和业务元数据传播到每个 span。worker-turn 标签将真实执行与后台健康检查区分。

## Dashboard 与质量分析

支持 Worker、runtime、Task、Team、Room、session、时间筛选；session 查询覆盖所有时间范围并可逐页浏览。详情提供父子瀑布、模型/工具内容、错误、耗时、token、上游跳转与 Langfuse 链接。会话 JSON 导出读取全部分页和每轮 observations，供后续离线标注、评估和比较。

错误数、HTTP 失败、重复工具参数、最长子步骤是排查线索，不是自动质量评分。后续可比较任务的调用量、重复工具、失败恢复、耗时、token 和交接前后输入输出，再结合人工验收与产物质量构建评估集。Trace 本身不能证明答案正确或任务完成。

用量来自实际响应 usage，缺失显示未上报；费用只使用 Langfuse 已知模型计价，空计价明细显示“未计价”，不把零汇总值视为免费。

Team 和 Worker 下拉选项使用当前登录凭据查询 Controller，复用 Controller 的 accessibleTeams / accessibleWorkers 关系。选择 Team 后只展示其中可访问的 Worker；“全部”也仅指当前账号的范围。列表、详情、上游跳转和会话导出逐次校验同一权限范围，手填 Worker、Team、Session 或 Trace ID 不能绕过。

BFF 将授权 Worker 过滤下推给 Langfuse，再校验每条 Trace 的 Worker 和当时的 Team 归属，完成过滤后才计算分页和总数。归属缺失、Worker 已删除或归属与当前 Team 不一致的历史数据不向该资源范围放行。Controller 无法确认权限时查询失败，不回退到整个 Langfuse 项目；普通账号的会话缺失时也不允许代理降级使用管理员 SA。

Langfuse 项目及密钥必须按 Controller 部署/组织隔离，不同组织不可共用项目后仅依靠同名 Worker/Team 标签划分权限。这里复用现有 Controller 的资源关系，不引入新的组织模型。服务端地址固定，禁止浏览器改选 Controller 后套用当前 Langfuse 密钥；禁止重定向、限制超时，不向浏览器暴露项目密钥或上游原文错误。普通账号不返回 Langfuse 项目直达链接。

## 内容与可靠性边界

启用通用开关时默认采集内容，AGENTTEAMS_OTEL_CAPTURE_CONTENT=false 关闭输入输出字段。写入和导出时脱敏结构化凭证键、Bearer token 和进程中已知密钥环境变量值。单字段 256 KiB 上限，超限保留 truncated、原长度、SHA-256 和预览；采集失败记录 capture_error，不能打断业务。

演示 Collector 使用内存保护、批处理、重试和独立持久卷队列。它保护已接收的待发送数据，不能承诺 SDK 未导出的内存数据、未结束 span、进程强杀、磁盘满、后端拒绝绝不丢失。直连模式只具备 Worker SDK 的批处理和重试，没有 Collector 的持久队列。运行时已截断的工具输出和模型未返回的内部推理无法恢复。

本地验证使用独立 Matrix、账号、存储桶、Langfuse。经用户授权仅复用原 AT 模型配置，通过固定接口和指定模型的代理访问；Worker 不接入原 AT 网络，原模型密钥仅保留在代理中，其他 API 路径被拒绝。

## 配置与扩展契约

管理员打开 Dashboard 的 Tracing → 可观测配置，选择是否采集、内容开关、Langfuse 地址和项目密钥，测试连接后保存。保存到 Dashboard 数据卷内的 observability.json，文件权限 0600，使用临时文件原子替换；每次读取使用最新配置，无需重启 Dashboard。Secret Key 和 Collector Headers 为只写字段，留空保留原值。

保存后勾选 Worker 并应用。BFF 使用当前账号凭据重新确认资源权限，读取 Worker 详情后仅合并采集环境变量，保留其他环境变量。配置版本变化时拒绝陈旧应用请求。托管 Worker 的 spec.env 变化由现有 Controller 触发容器重建；接口只报告下发结果，不将其等同于进程就绪或 Trace 已入库。非托管 Worker、未适配运行时及不支持环境变量编辑的旧 Controller 不报成功。

测试连接验证 Dashboard 到 Langfuse 的项目认证，以及到选定 OTLP 接收端的空批次请求；不生成伪造模型 span，不代表 Worker 网络已经可达。完整验收仍需在重建后运行真实任务并从 Langfuse 回读。关闭采集后再次应用到 Worker 才生效，历史记录仍可查询。

已有 Collector 时也可直接使用 Worker CR 的标准环境变量：

~~~yaml
spec:
  env:
    AGENTTEAMS_OTEL_ENABLED: "true"
    AGENTTEAMS_OTEL_CAPTURE_CONTENT: "true"
    OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-collector:4318"
    OTEL_EXPORTER_OTLP_PROTOCOL: "http/protobuf"
~~~

复用 Worker CR 的 spec.env，不新增 tokenUsage 状态字段。接收端地址必须从 Worker 网络可达。关闭通用开关并重建 Worker 即回到原启动路径。认证头及其解码后的 Basic 凭据均参与采集内容脱敏。

| Runtime | 本次范围 | 扩展/升级验证 |
| --- | --- | --- |
| QwenPaw/AgentScope | 真正 Matrix turn、Agent/model/tool middleware | middleware、流式结束、权限、任务归属 |
| Hermes | 原生 turn、共用工具边界、OpenAI chat、Matrix | 执行签名；其他模型协议分别验证输入输出和 usage |
| OpenClaw | 未实现本分支完整适配 | 原生 diagnostics-otel 加 session/tool/Matrix 验收 |
| DeepSeek Harness/OpenHuman | 未实现 | 原生 Node/Rust OTel SDK，遵守同一字段与 W3C 契约 |

新运行时只实现 turn/model/tool 钩子、真实 session 标识、消息 carrier 和退出 flush；协议、后端和 Dashboard 共用。Python 可使用 operation/set_io，异步 generator 使用 trace_stream，不能用 demo wrapper 替代真正入口验收。

标准构建使用 make build-qwenpaw-worker build-hermes-worker。Hermes 增加 teamharness_trace build context，复用同一个任务匹配文件；直接 Docker build 需同时提供它和既有 shared context。演示构建和证据见 [运行说明](../../examples/observability/README.md)。

## 验收与依据

分别验收代码/测试、镜像、标准入口、真实模型工具、Langfuse 入库、Dashboard 回读及浏览器视觉检查。覆盖同会话多轮、返回错误后恢复、W3C 交接、并发隔离、取消、脱敏、费用缺失和导出分页。浏览器受 Work 设置限制时明确保留未做视觉验收的边界。

- [Langfuse OTel 属性映射](https://langfuse.com/integrations/native/opentelemetry)
- [Langfuse Docker Compose](https://langfuse.com/self-hosting/deployment/docker-compose)
- [Collector 配置](https://opentelemetry.io/docs/collector/configuration/)
- [OTLP 配置](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)

本次固定 Langfuse v3 ingestion 与 v1 查询 API，升级后端需一起验证属性、费用和分页语义。
