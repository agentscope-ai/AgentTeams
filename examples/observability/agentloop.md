# AgentLoop 接入

同一套 Worker 原生埋点输出 OTel/OTLP；Langfuse 和 AgentLoop 共用采集、脱敏、Session / Turn、W3C carrier 和 Dashboard 瀑布流。可观测默认关闭，先启动 AT，再配置 Collector 和 Dashboard，应用到 Worker 后由 Controller 重建生效。

本实现按官方协议完成代码和本机隔离验收，尚未连接真实 AgentLoop 测试端点。协议夹具中的模型结果、工具结果和 Token 均为合成数据，带 fixture 标签；不代表云端识别、轨迹清洗、评估或计价已经验收。

## Collector 上报

从目标 AgentSpace 绑定的 CMS Workspace 的 OpenTelemetry 接入页获取 OTLP/HTTP endpoint 和鉴权头。将以下变量注入 Collector，使用 [collector.agentloop.yaml](collector.agentloop.yaml)，并为 `/var/lib/otelcol` 挂载独立持久卷：

```text
AGENTLOOP_OTLP_ENDPOINT=<OTLP/HTTP base endpoint，不含 /v1/traces>
AGENTLOOP_LICENSE_KEY=<x-arms-license-key>
AGENTLOOP_ARMS_PROJECT=<x-arms-project>
AGENTLOOP_WORKSPACE=<x-cms-workspace>
```

只启用该配置时，数据仅发往 AgentLoop。需要同时上报时，将其中 `otlphttp/agentloop` exporter 合入现有 [collector.yaml](collector.yaml)，保留原 Langfuse exporter、processor 和存储配置，并将 `service.pipelines.traces.exporters` 设为 `[otlphttp/langfuse, otlphttp/agentloop]`。同一批 Span 保持相同 Trace / Span ID，不要同时启用两套运行时自动埋点以免重复计量。

Worker 仍只设置通用开关、Collector 地址和可选的 Collector 入口采集凭证。阿里云出口 LicenseKey 留在 Collector；阿里云查询身份留在 Dashboard。不要将查询 AK/SK 或 Langfuse 项目读取密钥作为 Collector 入口 Header。

## Dashboard 查询

1. 从 `GetAgentSpace.cmsWorkspace` 确认 CMS Workspace；按官方查询指南，通过 CMS EntityStore 的 `apm.trace_set.storage` 定位 `project_name`、`logstore_name` 和地域。Trace 原始存储与 AgentSpace 的审计/轨迹 Project 不同，不能填写 `agent-trajectory` 或凭名称猜测。
2. 在 Dashboard → Tracing → 可观测配置，将“查询后端”选为“阿里云 AgentLoop”，填写地域、CMS Workspace、已定位的 Trace SLS Project / Logstore，以及 Worker 可达的 Collector 地址。
3. Dashboard 服务端使用官方 `@alicloud/credentials` 默认凭证链，支持环境变量、临时 STS、RAM/OIDC 角色等。仅授予目标 Project / Logstore 的 `log:GetLogStoreLogs`（SLS GetLogs）读取权限；不需要浏览器接收查询密钥，也不需要 AgentLoop 全管理权限。RAM Action 的最终名称与资源 ARN 以目标账号的 SLS 策略文档为准。
4. 测试连接、保存并应用到已包含适配器的 QwenPaw / Hermes Worker。保存配置不重启 Dashboard，开关和地址下发后的 Worker 需要重建；休眠中的下次启动生效。

服务端也可用以下默认配置，页面保存的值优先：

```text
AGENTTEAMS_TRACING_BACKEND=agentloop
AGENTLOOP_REGION=<Trace store region>
AGENTLOOP_WORKSPACE=<CMS Workspace>
AGENTLOOP_SLS_PROJECT=<resolved Trace project>
AGENTLOOP_SLS_LOGSTORE=<resolved Trace logstore>
```

默认查询接入点为 `https://<project>.<region>.log.aliyuncs.com`。内网部署或协议测试可在服务端设置 `AGENTLOOP_SLS_ENDPOINT` 为完整 Project endpoint；不含 API 路径，不由浏览器传入，重定向不会携带凭证继续请求。临时环境变量凭证为 `ALIBABA_CLOUD_ACCESS_KEY_ID`、`ALIBABA_CLOUD_ACCESS_KEY_SECRET`、`ALIBABA_CLOUD_SECURITY_TOKEN`，生产优先使用角色与自动轮转。

当前实现由管理员显式提供 Trace 数据源，不自动调用 CMS / AgentLoop API发现绑定关系；连接测试只验证目标 Trace 查询权限和 Collector 可达，不能证明该存储已绑定所填 Workspace 或云端数据已入库。必须按第 1 步核实关联关系。不同组织/Controller 部署必须隔离数据源。

## 字段与权限

- 入口：`gen_ai.span.kind=ENTRY`、`gen_ai.operation.name=enter`。
- 模型/工具/Agent：`LLM` / `TOOL` / `AGENT` 与 `chat` / `execute_tool` / `invoke_agent`。
- 会话：`gen_ai.session.id`、`gen_ai.turn.id`，保留原有 `agentteams.*` 身份和 Langfuse 兼容字段。
- 内容：`gen_ai.input.messages` / `gen_ai.output.messages`、`gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`；Token 使用模型实际返回的 `gen_ai.usage.input_tokens` / `output_tokens`。不开启内容采集时，两平台内容字段均移除。
- Trace 数据源需为上述标识建立 SLS 字段索引。读取按官方 raw Trace 结构处理 `traceId`、`spanId`、`parentSpanId`、`startTime`（Unix ns）、`duration`（ns）、`statusCode` 和 JSON/扁平 `attributes`。
- 列表和详情由当前账号 Controller 资源范围限制，查询下推 Worker 条件后，再校验 Worker 与当前 Team 归属。后台项目链接仅管理员可见。列表不返回模型/工具输入输出，点击详情才读取内容。
- SLS 每次 100 条，读取到完整空页才结束；`x-log-progress=Incomplete` 重试同页，仍不完整则明确报错。Session 不套最近 24 小时过滤；历史只限于后端实际保留的数据。100000 行、32 MiB 或约 60 秒的查询预算超出时返回错误，不将部分数据标为完整。

本轮不创建评估器、审计事件、Trajectory Pipeline 或 Dataset，也不承诺原始 Trace 自动生成所有这些数据。后续云上验收应逐项核对 Entry、模型、工具、usage、Session 多轮归并、父子完整性、延迟到达、权限和轨迹清洗结果。

## 本机协议验收

先构建配套 Dashboard，并确保已有隔离 Docker 网络 `at-tracing-demo_default`：

```bash
python3 examples/observability/agentloop_acceptance.py --keep
```

使用已安装 `shared/lib/telemetry/requirements.txt` 的 Python 环境替换示例解释器。脚本仅使用本机合成数据与伪凭证，不查询任何云端；路径为 SDK → 实际 Collector → OTLP 接收夹具 → 已验签的 SLS 查询夹具 → 正式 Dashboard 构建。每条 Trace 包含 104 个 Span，验证跨页读取、300 个合成 Token、工具错误、输入输出、后端配置和查询凭证不返回。页面为 <http://localhost:13005/tracing-demo/>，证据写入 `output/observability-demo/agentloop-acceptance.json`。

## 官方契约

- [GenAI 自定义接入](https://help.aliyun.com/zh/agentloop/integrating-llm-applications-with-opentelemetry-genai-utils/)
- [入口、模型和工具 Span](https://help.aliyun.com/zh/agentloop/go-llm-application-custom-burial-best-practices)
- [AgentLoop 数据源定位与查询](https://help.aliyun.com/zh/agentloop/use-cases/agentloop-data-query-api-usage-guide)
- [SLS raw Trace 字段与时间单位](https://help.aliyun.com/zh/sls/raw-source-data-format-association-and-minimum-requirements)
- [SLS 官方 Node SDK 签名实现](https://github.com/aliyun/aliyun-log-nodejs-sdk/blob/master/lib/client.js)
