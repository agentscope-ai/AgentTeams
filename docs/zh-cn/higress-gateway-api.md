# Higress 网关 API 参考

AgentTeams 内嵌 [Higress](https://github.com/alibaba/higress) 作为 AI 网关和 API 网关。
本文档说明 Higress 对外暴露的接口（**数据面**），以及 AgentTeams 自身用于管理网关的
Console API（**控制面**）。

- **数据面** —— Worker、Manager 和外部客户端调用 LLM Provider、MCP Server、暴露的
  Worker 端口以及内置服务的端点。
- **控制面** —— `agentteams-controller` 和旧版 Manager 脚本用于配置路由、Consumer 和
  MCP Server 的 Higress Console REST API。

## 默认域名与端口

| 资源 | 默认域名 | 容器内端口 | 宿主机端口（安装器） |
|------|---------|-----------|---------------------|
| AI 网关（LLM + MCP） | `aigw-local.agentteams.io` | `8080` | `18080`（`AGENTTEAMS_PORT_GATEWAY`） |
| Higress Console API | （Controller 内部） | `8001` | `18001`（`AGENTTEAMS_PORT_CONSOLE`） |
| Matrix 服务器 | `matrix-local.agentteams.io` | `8080` | `18080` |
| Element Web | `matrix-client-local.agentteams.io` | `8080`（经网关）/ `8088`（直连） | `18080`（经网关）/ `18088`（直连，`AGENTTEAMS_PORT_ELEMENT_WEB`） |
| MinIO 文件系统 | `fs-local.agentteams.io` | `9000`（MinIO S3 API，**不是**网关 `8080`；controller 会把 `:8080` 重写为 `:9000`） | 无独立宿主映射 |
| OpenClaw Console | `console-local.agentteams.io` | `8080`（经网关）/ `18888`（直连） | `18080`（经网关）/ `18888`（直连） |

> **端口说明**：在 `agentteams-net` Docker 网络内（即从 Worker 或 Manager 容器内访问），
> 网关监听在 **`:8080`**。安装器把它发布到宿主机为 **`:18080`**。编写 Worker 配置时
> 请使用容器内形式（`AGENTTEAMS_AI_GATEWAY_URL=http://aigw-local.agentteams.io:8080`）。

## 数据面 —— 对外可调用端点

### 1. LLM OpenAI 兼容 API

AI 路由 `default-ai-route`（路径前缀 `/v1`，上游由 `AGENTTEAMS_LLM_PROVIDER` 决定）通过
Higress 的 `ai-proxy` 插件暴露 OpenAI 兼容的 LLM 端点。请求必须携带调用方的 Consumer key。

```
POST /v1/chat/completions   # 对话补全（支持流式）
POST /v1/embeddings         # 向量化（配置 memorySearch 时使用）
```

`GET /v1/models` 在 Higress 中并不是完整的 OpenAI 模型列表端点——`ai-proxy` 插件只匹配
`/v1/chat/completions` 和 `/v1/embeddings` 路径。从 Worker 执行 `curl /v1/models` 仍有
价值，它是**认证/连通性检查**——`401`/`403` 说明 Consumer key 或 `allowedConsumers`
配置有误，`404` 说明该路径不是 ai-proxy 路由（见 Worker 指南的故障排查章节）。

`/v1/chat/completions` 也是 controller 在 Manager/Worker 上线前验证其 Consumer 是否
已被 AI 路由授权的就绪探测端点（`agentteams-controller/internal/service/provisioner.go`
中的 `IsManagerLLMAuthReady`）。

示例——验证 Consumer 是否已被 AI 路由授权（在 Worker 容器内执行，探测体与 controller 的
`IsManagerLLMAuthReady` 一致）：

```bash
# 200 = 已授权；401 = key 错误；403 = 不在 allowedConsumers；404 = 路径错误
curl -s -o /dev/null -w '%{http_code}\n' http://aigw-local.agentteams.io:8080/v1/chat/completions \
  -H "Authorization: Bearer ${AGENTTEAMS_WORKER_GATEWAY_KEY}" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"<model>\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with only one word: ok\"}]}"
```

认证方式为按身份区分的 **key-auth**（Bearer）。每个 Manager/Worker Consumer 都以自己的
`GatewayKey` 注册到 Higress，并且只能访问在 `authConfig.allowedConsumers` 中列出它的
AI 路由。该授权由 controller 通过 `AuthorizeAIRoutes` / `DeauthorizeAIRoutes` 管理
（见 `agentteams-controller/internal/gateway/higress.go`）。

### 2. MCP Server 端点

每个注册到 Higress 的 MCP Server 都暴露在 AI 网关域名的 `/mcp-servers/{name}/mcp` 下。
`name` 是 MCP Server 名称——对于内置 GitHub MCP Server，它是 `mcp-github`。
`transport: http`（Streamable HTTP）对应此 URL；mcporter 默认使用。

```
POST /mcp-servers/{name}/mcp
```

示例（在 Worker 容器内执行）：

```bash
mcporter --transport http \
  --server-url "http://aigw-local.agentteams.io:8080/mcp-servers/mcp-github/mcp" \
  --header "Authorization=Bearer ${AGENTTEAMS_WORKER_GATEWAY_KEY}" \
  call list_repos '{"owner": "test"}'
```

MCP 访问同样受 per-consumer 授权控制（MCP Server 上的 `consumerAuthInfo`）。注册由
controller（嵌入式栈）或旧版 `setup-higress.sh` / `setup-mcp-server.sh` 脚本
（≤v1.0.9 Manager 镜像）完成；参见 `manager/agent/skills/mcp-server-management/`。

### 3. 暴露的 Worker 端口（服务发布）

`spec.expose` 中列出端口的 Worker 会获得一条带自动生成域名的网关路由，使其 HTTP 服务
可以从容器外部访问。

自动生成域名格式：

```
worker-{name}-{port}-local.agentteams.io
```

示例：Worker `alice` 暴露端口 `8080` 后，可从 `agentteams-net` 网络内通过
`http://worker-alice-8080-local.agentteams.io:8080` 访问（宿主机上为 `:18080`，与网关
发布端口一致）。域名绑定在网关端口上，因此访问端口是网关端口，而不是 Worker 的内部端口。

暴露的路由**不启用认证**（设计如此，公开访问）；controller 在 reconcile 过程中创建
Higress 的 domain、service source 和 route（`agentteams-controller/internal/service/provisioner_expose.go`
中的 `ReconcileExpose`）。用法参见 `manager/agent/skills/service-publishing/SKILL.md`。

### 4. 内置服务路由

安装器还会为嵌入式栈附带的服务注册路由：

| 路由 | 域名 | 路径 | 后端 |
|------|------|------|------|
| Matrix 服务器 | 任意（`domains: []`） | `/_matrix` | Tuwunel（`tuwunel.static:6167`） |
| Element Web | `matrix-client-local.agentteams.io` | `/` | `element-web.static:8088` |
| HTTP 文件系统 | `fs-local.agentteams.io` | `/` | MinIO S3（`minio.static:9000`） |
| OpenClaw Console | `console-local.agentteams.io` | `/` | `openclaw-console.static:18888`（basic-auth） |

这些资源在首次启动时由 `setup-higress.sh`（非幂等，受 marker 保护）或嵌入式栈的
controller initializer 创建。

## 认证方式汇总

| 接口 | 机制 | 凭据 |
|------|------|------|
| LLM AI 路由（`/v1/*`） | key-auth WASM（Bearer） | Consumer `GatewayKey`（`Authorization: Bearer <key>`） |
| MCP 端点（`/mcp-servers/*`） | key-auth（Bearer），经 `consumerAuthInfo` | Consumer `GatewayKey` |
| 暴露的 Worker 端口 | 无（公开） | — |
| OpenClaw Console 路由 | basic-auth | `AGENTTEAMS_ADMIN_USER` / `AGENTTEAMS_ADMIN_PASSWORD` |
| Higress Console API | session cookie | `POST /session/login` |

Consumer key 由 controller 按 Manager/Worker 分别生成，并注入为
`AGENTTEAMS_MANAGER_GATEWAY_KEY` / `AGENTTEAMS_WORKER_GATEWAY_KEY`。AI 路由上的授权
通过 `authConfig.allowedConsumers` 按 Consumer 隔离。

## 控制面 —— Higress Console API

controller 和旧版脚本通过 Higress Console REST API（容器内 `http://127.0.0.1:8001`）
管理网关。使用 session-cookie 认证：`POST /system/init` 初始化 admin 账号，
`POST /session/login` 获取 cookie。

| 端点 | 方法 | 用途 |
|------|------|------|
| `/system/init` | POST | 初始化 admin 账号（首次启动） |
| `/session/login` | POST | 登录，获取 session cookie |
| `/user/changePassword` | POST | 轮换 admin 密码 |
| `/v1/consumers` | GET, POST | 列出 / 创建 key-auth Consumer |
| `/v1/consumers/{name}` | DELETE | 删除 Consumer |
| `/v1/ai/routes` | GET, POST | 列出 / 创建 AI 路由 |
| `/v1/ai/routes/{name}` | GET, PUT | 读取 / 更新 AI 路由（含 `authConfig.allowedConsumers`） |
| `/v1/ai/providers` | GET, POST | 列出 / 创建 LLM Provider |
| `/v1/ai/providers/{name}` | GET, PUT | 读取 / 更新 Provider |
| `/v1/domains` | POST | 创建域名 |
| `/v1/domains/{name}` | DELETE | 删除域名 |
| `/v1/service-sources` | GET, POST | 列出 / 创建服务源 |
| `/v1/service-sources/{name}` | PUT, DELETE | 更新 / 删除服务源 |
| `/v1/routes` | GET, POST | 列出 / 创建经典路由 |
| `/v1/routes/{name}` | PUT, DELETE | 更新 / 删除经典路由 |
| `/v1/routes/{name}/plugin-instances/{plugin}` | PUT | 启用 / 配置路由插件（如 OpenClaw Console 路由上的 `basic-auth`） |
| `/v1/mcpServer` | GET, PUT | 列出 / 覆盖 MCP Server |
| `/v1/mcpServer/consumers` | GET, PUT | 查询 / 授权 MCP Server 上的 Consumer |
| `/system/higress-config` | GET, PUT | 读取 / 修改网关配置（如 stream `idleTimeout`） |

AI 路由上的 Consumer 授权是 reconciler 的职责——initializer 从不写
`authConfig.allowedConsumers`（见 `agentteams-controller/internal/gateway/higress.go`
中的 `EnsureAIRoute`）。

## 相关文档

- [架构总览](architecture.md) —— Higress 在系统中的角色。
- [Worker 使用指南](worker-guide.md) —— 从 Worker 排查 LLM / MCP 连通性。
- [Kubernetes 原生编排](k8s-native-agent-orch.md) —— LLM/MCP 安全模型。
- [开发指南](development.md) —— 贡献者的 Higress 配置指引。
