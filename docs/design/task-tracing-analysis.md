# QwenPaw 任务 Trace 查询与分析

## 一、目标与现状

Manager 和 Leader 需要从团队任务找到全部执行轮次，读取模型、工具、HTTP 和消息交接的实际记录，再引用 Trace/span 判断故障位置。本次只验收 QwenPaw；Hermes、OpenClaw、DeepSeek Harness、OpenHuman 后续单独适配。

现有 Controller 保存 Project/Task 状态与依赖；Dashboard 持有 Langfuse 查询配置；QwenPaw 通过共享 OTel 适配器上报。缺口包括 Agent 查询入口、完整分页、任务上下游聚合，以及 TeamHarness 长驻 MCP 子进程和委派通知的上下文传播。

## 二、架构选择

查询放在 Controller，复用其现有身份认证与项目权限。Manager/Leader 使用自己已有的 Controller token 调用 `agt`，不获取 Langfuse 密钥，不依赖浏览器登录状态。Dashboard 的浏览器会话 API 不作为 Agent 的认证入口。

配置统一保存在 Controller 所属命名空间的 `agentteams-observability` Secret（`data["config.json"]`）。嵌入式部署随 Controller 现有 Kubernetes/Kine 数据持久化；集群部署使用 Kubernetes Secret。管理 API 和新容器创建直接读取同一数据源，API 使用非缓存 Reader，避免保存后立即读取旧配置。Dashboard 不再通过运行时 API 读写本地 `observability.json`，也不获取 Langfuse 查询密钥。

新建或重建 QwenPaw 容器时，Controller 自动注入采集开关、内容开关、Collector 地址、采集认证头和配置 revision。已有容器通过显式“应用”更新 Worker 环境变量并触发原有重建流程；仅保存配置不立即重启运行中的任务。关闭采集时清空旧 Collector 覆盖项。查询密钥仅由 Controller 的查询处理器读取。

## 三、接口与采集改造

- `GET /api/v1/tasks?team=...`：返回有权访问的项目任务及状态、负责人、依赖和项目/团队标识。
- `GET/PUT /api/v1/observability/config`：读取脱敏配置或由管理员保存；空密码字段保留旧值，revision 防止覆盖并发修改。
- `POST /api/v1/observability/test`、`POST /api/v1/observability/apply`：由 Controller 测试连接或向选定已有 QwenPaw Worker 应用配置。
- `GET /api/v1/traces`、`GET /api/v1/traces/{traceId}`：Dashboard 的 Langfuse 列表、完整详情和会话导出统一使用 Controller 查询权限。
- `GET /api/v1/projects/{id}/tasks/{taskId}/traces?team=...&includeRelated=true`：返回任务及可选依赖上下游的全部轮次和 observations。无默认最近 24 小时限制；读取所有后端分页，失败不伪装完整。
- `agt get tasks --team ... -o json`、`agt tracing task <project> <task> --team ... [--related]`：复用已有 token。Manager 和 Leader 共用任务诊断技能。
- QwenPaw MCP 客户端逐次注入 W3C carrier，TeamHarness 服务端逐次恢复并清理。Matrix 通知携带 carrier；上下游保留独立 Worker session，通过 Trace/span 标识关联。
- QwenPaw 先使用原生请求标准化结果建立会话身份，任务文件同步后再更新入口 span 的任务归属。MCP 子进程显式继承采集环境变量；HTTP 委派通知记录发送 span 并携带 W3C carrier。

任务依赖边与实际消息因果边分别返回。输入输出受采集开关、脱敏和字段长度限制，缺失内容、未结束 span、未归属任务、无 Trace 的任务和不可访问的关联必须显式报告。后端分页读取完成仅代表当前查询快照，不代表采集无丢失。

## 四、权限与分析

先验证项目和任务权限，再访问 Langfuse。Leader 只能读本团队项目，Manager 使用已有管理权限；返回数据逐条核对团队、项目、任务归属，不信任请求方提供的 Trace ID 或后端过滤结果。跨权限边界的内容不返回。

分析由 Manager/Leader 完成，服务端只汇总可验证证据（错误 span、HTTP 状态和耗时）。报告包含具体 Trace/span、观察到的事实、推断及缺失证据。使用原有项目/任务工具提出修订、重新委派或请求人工决策；查询本身不自动改变任务状态，不把 Trace 当作成果验收。

## 五、实施与验收

1. Controller API、权限、Langfuse 完整分页和失败测试；CLI 参数与 token 回归。
2. QwenPaw 原生模型/工具与 MCP/Matrix 传播回归；验证并发隔离、错误、内容关闭和上报字段。
3. 独立本地 QwenPaw、Collector、Langfuse 跑实际工具失败和恢复任务，回读全部轮次；以 Manager/Leader 凭据调用查询，验证越权拒绝。
4. 用 Agent 的实际分析输出验证它能引用失败 span、区分根因与后续恢复，并明确数据缺口。代码、镜像、真实模型运行与 Langfuse 入库分别记录证据。

当前接入与现有 Dashboard 一致，使用 Langfuse v3 的 v1 查询接口。官方已提供 v2 observations API；后端升级迁移另行验证，不能默默用不兼容接口降级并声称结果完整。

升级前已存在 Dashboard 本地文件的部署需要将配置重新保存到 Controller；本阶段不自动读取或删除旧私有文件。当前团队任务列表来自项目当前计划；查询旧任务的重规划历史另行扩展，不能把 Loop 的决策 ledger 当作完整历史任务快照。

## 六、本地验证记录与复现

2026-11-11（Asia/Shanghai）在隔离的 `at-qwenpaw-loop` 栈验证通过。原生 QwenPaw 2.2.2.post1 使用真实模型 qwen3.5-plus，完成两轮文件读写：第一次读取不存在的文件报错，随后写入、回读，第二轮追加并回读。原生采集的 OTLP 批次经 Collector 写入 Langfuse 3.227.1，Controller 回读到 2 个 Trace、40 个 observations 和 1 个错误证据。

Manager 与 Leader 分别使用自己的测试身份调用团队任务、项目详情和任务完整 Trace API。两者的真实模型分析均引用错误 span `51207b45f11296a8`（Trace `4522e2f3baa1d5680484b606d9eb0020`），指出文件尚不存在并判断后续恢复；其他团队 Leader 返回 404。验收使用正式 HTTP 路由、权限与查询实现，Kubernetes/对象存储及认证身份提供器是测试夹具，不代表完整部署或线上账号验收。MCP 的长驻子进程和逐次 carrier 隔离另用原生 SDK 验证。

产物保存在本工作树的 `output/native-acceptance/`：`native-summary.json`、真实 OTLP `batch-*.bin`、`controller-bundle.json`、`diagnosis-manager.json`、`diagnosis-leader.json`。JSON 保留查询完整性缺口；两轮数据和模型诊断不能证明采集绝不丢失，也不能代替成果验收。本次未验收其他运行时、跨 Worker 实际交接或 Dashboard 浏览器交互。

复现时先准备包含 QwenPaw 及共享 telemetry 依赖的镜像、Collector/Langfuse，以及权限为 0600 的模型环境文件（`DEMO_LLM_BASE_URL`、`DEMO_LLM_API_KEY`、`DEMO_LLM_MODEL`）。不要在聊天、命令输出或源码中记录模型密钥。在仓库根目录设置 `QWENPAW_IMAGE`、`MODEL_ENV_FILE`、`MODEL_NETWORK` 为自己的隔离测试资源后采集原生数据：

```bash
mkdir -p output/native-acceptance
docker run --rm --network "$MODEL_NETWORK" \
  -v "$PWD:/src:ro" -v "$PWD/output/native-acceptance:/artifacts" \
  --env-file "$MODEL_ENV_FILE" \
  -e PYTHONPATH=/src/shared/lib/telemetry \
  -e AGENTTEAMS_OTEL_ENABLED=true -e AGENTTEAMS_OTEL_RUNTIME=qwenpaw \
  -e AGENTTEAMS_OTEL_CAPTURE_CONTENT=true \
  -e AGENTTEAMS_WORKER_NAME=qwenpaw-full-session \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
  --entrypoint /opt/venv/qwenpaw/bin/python "$QWENPAW_IMAGE" \
  /src/examples/observability/capture_qwenpaw_live.py
```

使用全新容器可确保首次读取的目标文件不存在。保留批次供同一次数据重复回读；重新采集独立验收时使用新的产物目录，避免与旧轮次混合。构建匹配容器架构的 CLI，再运行显式启用的集成验收（本机镜像为 linux/arm64）：

```bash
cd agentteams-controller
GOOS=linux GOARCH=arm64 go build -o bin/agt-linux-arm64 ./cmd/agt
AGENTTEAMS_ACCEPTANCE_LANGFUSE_URL=http://127.0.0.1:13100 \
AGENTTEAMS_ACCEPTANCE_COLLECTOR_URL=http://127.0.0.1:14318 \
AGENTTEAMS_ACCEPTANCE_AGT="$PWD/bin/agt-linux-arm64" \
AGENTTEAMS_ACCEPTANCE_MODEL_ENV="$MODEL_ENV_FILE" \
AGENTTEAMS_ACCEPTANCE_QWENPAW_IMAGE="$QWENPAW_IMAGE" \
AGENTTEAMS_ACCEPTANCE_MODEL_NETWORK="$MODEL_NETWORK" \
go test ./internal/server -run '^TestQwenPawLangfuseAcceptance$' -count=1 -v -timeout=10m
```

默认单元测试跳过这项外部服务验收。常规回归覆盖 Controller 配置持久化、脱敏、并发保存、创建时注入、已有 Worker 应用、分页完整性、越权、CLI 和 QwenPaw MCP 环境继承。Dashboard 变更的测试与 lint 通过；生产构建的编译通过，但最终 Next 路由类型检查受三个未修改路由导出的既有测试辅助符号阻断，不能记为完整构建通过。
