# 本机 Worker 完整会话 Tracing 演示

当前配置与查询入口已迁移到 Controller，见 [QwenPaw 任务诊断方案与验收](../../docs/design/task-tracing-analysis.md)。下文的 `at-tracing-demo`、独立 Dashboard 配置卷及旧 `configuration_acceptance.py` 是此前演示记录，不能作为当前 Controller 配置闭环的验收入口。当前使用 `capture_qwenpaw_live.py`、`verify_qwenpaw_mcp.py` 和 Controller 的 `TestQwenPawLangfuseAcceptance`。

已增加可选 [AgentLoop 上报、查询与隔离验收](agentloop.md)。可单独使用 AgentLoop，也可由 Collector 同时上报两个平台；云上入库和评估仍需目标端点验收。

AT 与 Dashboard 均使用本地分支 codex/worker-otel-langfuse-demo，工作树分别为 output/otel-langfuse-demo/core 和 output/otel-langfuse-demo/dashboard。[技术方案](../../docs/design/worker-otel-langfuse.md)说明了原生采集点、扩展契约和覆盖边界。

## 演收入口

- Dashboard：<http://localhost:13003/tracing-demo/>
- Langfuse：<http://localhost:13100>，项目 AT Worker Tracing / at-workers
- 登录：demo@agentteams.local / AgentTeams-demo-2026
- 独立 Matrix：<http://localhost:16167>；账号和 Room ID 保存在本机 output/observability-demo/matrix-private.json，不提交 Git。

在 Dashboard 选择 trace-qwenpaw 或 trace-hermes，打开执行记录，点“查看整个会话”，逐轮查看模型、工具、错误和 Matrix 回复。点击“查看上游 Worker Trace”回到交接源；“导出会话 JSON”读取所有分页和每轮 observations。Task 为 matrix-qwenpaw-acceptance / matrix-hermes-acceptance，Project 为 tracing-demo，Team 为 tracing-team。

Dashboard 使用正式“控制台 → 治理 → Tracing”同一组件和资源权限 BFF。正式登录后，Team/Worker 下拉和列表、详情、导出使用当前账号经 Controller 确认的访问范围，选择 Team 后 Worker 联动过滤。选择 Worker 后按其配置自动带出并锁定运行时；取消 Worker 选择后恢复手动运行时筛选。

Tracing 顶部的“可观测配置”仅管理员可见。可先启动 AT，再填写 Langfuse 地址/项目查询密钥和已部署的 Collector 地址/采集凭证，测试、保存并应用到指定 Worker。保存无需重启 Dashboard；运行中的托管 Worker 由 Controller 重建，休眠中的下次启动生效。默认关闭，历史数据查询不受采集开关影响。当前配置和查询密钥保存在 Controller 的 `agentteams-observability` Secret，新建容器自动读取同一配置。Dashboard 通过 Controller 接口读写，配置读取接口不返回密钥；Worker 只接收 Collector 配置。

独立入口由 AGENTTEAMS_TRACING_DEMO=true 启用；演示仅绑定 localhost，使用本地开发身份，仅持有这个隔离 Langfuse 项目密钥。本机没有真实组织账号/Controller，因此启动脚本显式配置 AGENTTEAMS_TRACING_DEMO_SCOPE，只允许 tracing-team 的 trace-qwenpaw / trace-hermes，不以 Langfuse 历史记录推导账号权限。该演示清单仅在演示开关和 AUTH_DISABLED 同时开启、身份为本地管理员时使用；正式账号必须查询 Controller。

## 本次覆盖

| 层级 | 可查看的数据 |
| --- | --- |
| session / turn | 真实 runtime session、用户隔离、每次原生执行独立 trace；后台常驻消费者不串线 |
| 模型 | 实际输入上下文、输出、模型名、usage、finish reason、HTTP 请求状态/耗时 |
| 工具 | 原生调用 ID、参数、结果、失败、阻断；返回错误也记为 ERROR |
| 消息 | 接收/发送事件 ID、W3C carrier、流结束后的回复、编辑消息、上游链接 |
| 发送门控 | 新消息导致重触发时保留 delivery decision、前后执行链接，区别于模型失败 |
| 任务 | 复用 TeamHarness 唯一匹配规则，歧义或缺失原因保留，不猜任务 |
| 运行诊断 | 版本、采集策略、警告/错误事件、取消、父子关系、耗时与用量 |

此前演示真实模型为用户授权复用的 qwen3.6-plus。Worker、Matrix、存储和 Langfuse 均独立；Worker 只连接 at-tracing-demo_default。原模型密钥仅放在受限代理中，代理只开放指定模型接口，不能访问 AT 其他 API。当前 Langfuse 写入密钥保留在 Collector，查询密钥保留在 Controller。

完整采集有明确边界：单字段 256 KiB 超限标记长度/哈希/预览；脱敏凭证；不能恢复运行时已经截断的输出或模型未返回的内部推理。Collector 持久队列保护已接收数据，SDK 未导出/未结束 span 和强杀不能承诺不丢。OpenClaw、DSH、OpenHuman 尚未完成本分支适配。不同架构/历史上下文的示例不能直接当作 runtime 性能排名。

## 重复验收

服务已运行时，无需重新创建账号或镜像。以下命令均在 AT 演示工作树执行。每步等 Worker 实际回复后继续，可在 Dashboard 刷新查看：

~~~bash
# 新会话保留此前的排障与 trace 历史
python3 examples/observability/prepare_matrix_workers.py --new-room

# 全部步骤使用同一个全新 run 名，确保首次读取不存在的文件
python3 examples/observability/matrix_acceptance.py qwenpaw 1 --run morning-demo
python3 examples/observability/matrix_acceptance.py qwenpaw 2 --run morning-demo
python3 examples/observability/matrix_acceptance.py hermes 1 --run morning-demo
python3 examples/observability/matrix_acceptance.py hermes 2 --run morning-demo
python3 examples/observability/matrix_acceptance.py qwenpaw handoff --run morning-demo

# 从 Langfuse 和 Dashboard 回读并断言，不能只看 exporter HTTP 成功
python3 examples/observability/verify_full.py --run morning-demo
~~~

第一轮先读取不存在的文件，失败后真实写入并回读；第二轮在同一会话追加另一行并回读；handoff 由 QwenPaw 的真实 Shell 工具调用本机主动消息 API 发新消息给 Hermes，Hermes 用自己的真实工具校验文件。每轮 trace 独立，同一调用者的 session 稳定；跨 Worker 保留各自原生用户隔离策略，通过 SpanLink 与元数据关联。

verify_full.py 检查两个 runtime 的独立轮次、session、Task、模型/工具输入输出、usage、模型 response ID 去重、span 父子关系、发送/重触发、跨 Worker 链接，以及 Dashboard 回读一致性。产物写入 output/observability-demo/full-session-acceptance.json 和 full-session-readback.json。

另有 full_session_demo.py / run_live_acceptance.py 用于不经过 Matrix 的原生循环验证；旧 runtime_demo.py / run_workers.sh / verify.py 仅作协议烟测，其 fixture usage 来自测试响应。429 夹具故障注入设置 DEMO_FAIL_FIRST_REQUEST=429，可验证失败请求与成功重试，不能当作线上限流事件。

首次创建真实模型代理前，显式设置 DEMO_MODEL_API_KEY、DEMO_MODEL 和 DEMO_MODEL_BASE_URL（例如 https://api.example.com/v1）；仅在网关要求时设置 DEMO_MODEL_HOST。脚本不再从其他 AT 容器读取模型凭证。已有隔离代理可直接复用，不打印密钥。

后置配置验收使用已准备好的两个隔离 Worker 和生产构建 Dashboard：

~~~bash
python3 examples/observability/configuration_acceptance.py \
  --worker-tag local-20261106164000-config --keep
~~~

该脚本在 localhost:13004 启动独立 Dashboard，验证无配置启动、测试连接、私有保存、重启持久化、两种运行时关闭/开启/再关闭后的真实回复及 Trace 回读。Controller HTTP API 是只允许重建这两个隔离容器的夹具；它不代表真实 Controller 部署端到端验收。Controller 原生环境变量合并和 spec 变更重建逻辑需另跑 Go 回归。结果写入 output/observability-demo/configuration-acceptance.json，--keep 保留进程供浏览器演示。

## 启停与镜像

~~~bash
bash examples/observability/start.sh
curl -f http://localhost:13100/api/public/health
python3 examples/observability/prepare_matrix_workers.py
# Dashboard 已存在时可重启；首次创建前需构建 ../dashboard
python3 examples/observability/start_dashboard.py ../dashboard

# 仅停止本次演示，保留数据卷
bash examples/observability/stop.sh
~~~

Dashboard 源码构建：在 ../dashboard 执行 npm ci、npm run build。重建后用 docker rm -f at-tracing-demo-dashboard，再运行 start_dashboard.py；不停止其他 AT Dashboard。

本次在已缓存 Worker 上构建 telemetry overlay，不等同于重新构建整个发行镜像。QwenPaw 为 arm64，Hermes 为 amd64：

~~~bash
docker build -f examples/observability/Dockerfile.runtime \
  --build-arg BASE_IMAGE=agentteams/qwenpaw-worker:local-20261106003000-otel \
  --build-arg VENV_PATH=/opt/venv/qwenpaw --build-arg RUNTIME=qwenpaw \
  -t agentteams/qwenpaw-worker:local-20261106023000-full-otel .

docker build --platform linux/amd64 -f examples/observability/Dockerfile.runtime \
  --build-arg BASE_IMAGE=agentteams/hermes-worker:local-20261106003000-otel \
  --build-arg VENV_PATH=/opt/venv/hermes --build-arg RUNTIME=hermes \
  -t agentteams/hermes-worker:local-20261106023000-full-otel .
~~~

SDK 依赖固定在 shared/lib/telemetry/requirements.txt。完整构建走 Makefile，两种运行时复用同一 TeamHarness task resolver。演示固定本地账号/密码仅用于此隔离栈，正式部署使用独立凭证、内部 Collector 和正常管理员会话。

## 证据与验证边界

验收记录、真实回读、镜像 digest 和构建/测试日志保存在 output/observability-demo/。此前排障数据也保留在 Langfuse，不删除失败样本；演收以 full-session-acceptance.json 指定的会话/trace 为准。

已验证的层次包括：Python 与 Matrix 通道回归、Dashboard API/组件交互、会话分页导出、类型检查、lint、标准生产构建、两种标准 Worker 入口与真实模型/工具、Langfuse/Dashboard 回读。故障夹具能保留 HTTP 429 和随后 200 的两个请求。

已完成本机浏览器视觉验收；权限测试覆盖两个账号的 Controller 凭据、资源下拉联动、越权筛选/详情和受限会话分页。未使用真实组织账号进行线上验收，未发布镜像或改动现有 AT 部署。
