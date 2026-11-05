# 本机 Worker Tracing 演示

两个仓库均使用 `codex/worker-otel-langfuse-demo` 分支：

- AT 工作树：`output/otel-langfuse-demo/core`
- Dashboard 工作树：`output/otel-langfuse-demo/dashboard`
- [技术方案](../../docs/design/worker-otel-langfuse.md)

## 明早演示入口

- Dashboard：<http://localhost:13003/tracing-demo/>
- Langfuse：<http://localhost:13100>
- Langfuse 登录：`demo@agentteams.local` / `AgentTeams-demo-2026`
- Langfuse 项目：`AT Worker Tracing`（ID `at-workers`）

Dashboard 使用与正式导航“控制台 → 治理 → Tracing”相同的组件和 BFF。独立演示入口需要显式 `AGENTTEAMS_TRACING_DEMO=true`；演示容器仅绑定 localhost，并启用现有本地开发身份模式。它只持有本次演示 Langfuse 项目密钥，未复制既有 AT 的凭证或加入既有 AT 网络。

选择 QwenPaw / Hermes 记录，查看 `worker.turn → runtime.invoke → openai.chat` 瀑布，点击模型 span 查看输入、输出、模型与 token，点击 Langfuse 打开原始详情。Worker、运行时、Task ID 和时间均可查询。

## 新增两条记录

在 AT 演示工作树执行：

```bash
bash examples/observability/run_workers.sh
python3 examples/observability/verify.py
```

`runtime_demo.py` 执行真实 `QwenPawAgent.reply` 与 `Hermes AIAgent.run_conversation`，OpenAI instrumentation 采集真实 SDK 的 HTTP/流式调用。默认使用确定性本地模型响应，因此 trace 标记 `fixture`，usage=66 来自测试服务响应，不能当成真实模型消耗或计费。

根 span、runtime.invoke 和 task/room/project 的关联由演示 wrapper 提供。实际 Worker 的 SDK 调用可以自动采集；正式 Matrix turn/tool hook、跨 Worker W3C 传播，以及 OpenClaw/DSH/OpenHuman 的完整适配尚未完成，详见方案的运行时矩阵。

## 停止后重新启动

```bash
bash examples/observability/start.sh
# 确认 Langfuse 健康检查就绪后，再运行 Worker。
curl -f http://localhost:13100/api/public/health
bash examples/observability/run_workers.sh
python3 examples/observability/verify.py
```

Dashboard 已运行时不需要再创建容器。重启使用 `docker restart at-tracing-demo-dashboard`。首次创建：

```bash
cd ../dashboard
npm ci
npm run build
cd ../core
python3 examples/observability/start_dashboard.py ../dashboard
```

停止演示并保留 trace 数据：

```bash
docker stop at-tracing-demo-dashboard
docker compose -f examples/observability/compose.yaml down
```

这套环境使用 `at-tracing-demo` 项目名和独立数据卷，不需要停止已有的 AT 环境。

## 镜像构建与来源

本次仅在已有缓存 Worker 镜像上构建 telemetry overlay，包含最终的共享环境脚本、Worker 入口和采集依赖，未重新构建整个 AT 发行镜像。默认 Hermes 缓存镜像为 amd64，QwenPaw 为 arm64。

```bash
docker build -f examples/observability/Dockerfile.runtime \
  --build-arg BASE_IMAGE=agentteams/agentteams-qwenpaw-worker:local-20261104120000 \
  --build-arg VENV_PATH=/opt/venv/qwenpaw --build-arg RUNTIME=qwenpaw \
  -t agentteams/qwenpaw-worker:local-20261106003000-otel .

docker build --platform linux/amd64 -f examples/observability/Dockerfile.runtime \
  --build-arg BASE_IMAGE=agentteams/hermes-worker:latest \
  --build-arg VENV_PATH=/opt/venv/hermes --build-arg RUNTIME=hermes \
  -t agentteams/hermes-worker:local-20261106003000-otel .
```

SDK 依赖固定在 `shared/lib/telemetry/requirements.txt`。Langfuse v3/Collector 使用官方 GHCR 镜像，ClickHouse 使用官方 Docker Hub 镜像，S3 存储使用与本机 AT 同类的 RustFS。Collector 的独立队列卷归 UID 10001 所有，启动脚本负责初始化权限。

演示账号、密码和 API key 是固定的本地测试值，仅适用于此隔离环境。正式部署替换凭证和服务地址，并保留 Dashboard 的正常管理员会话校验。

## 验证状态与边界

- Dashboard：43 项相关测试（含组件交互、管理员权限、API 边界、导航和瀑布坐标）通过；类型检查、lint 和生产构建通过。
- Python：并发房间/任务上下文、父子 span、错误状态、清理，以及已有 QwenPaw 启动契约测试通过。
- 镜像：两种运行时 overlay 已构建；QwenPaw 共享环境入口可自动创建 TracerProvider。Hermes 原始镜像已有 browser-harness/Pillow 版本冲突，本次未修改这项无关依赖。
- 协议：两种真实原生 Agent 已向 OTLP protobuf 探针输出三层 trace 和模型 usage；探针仅用于诊断，不是 Langfuse 替代品。
- 完整 Langfuse / Dashboard 回读：以 `verify.py` 实际输出及 `output/observability-demo/` 验收记录为准，不能用运行时 stdout 或导出 HTTP 200 替代查询验证。
- 浏览器视觉验收：当前 Work 设置禁用了本地浏览器自动化；已完成组件交互测试与页面 HTTP 检查，尚未完成浏览器视觉验收。
