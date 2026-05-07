# Worker 使用指南

HiClaw Worker Agent 的部署、管理和故障排查指南。

## 概述

Worker 是轻量级无状态容器，负责：
- 通过 Matrix 连接 Manager 接收任务
- 从集中式 MinIO 存储同步配置
- 通过 AI 网关访问 LLM
- 通过 mcporter CLI 调用 MCP Server 工具（GitHub 等）

### 声明式创建与更新（v1.1.0+）

Worker 由 **CR** 描述。除在 Matrix 里让 Manager 创建外，你还可以：

- 在 **`hiclaw-controller`** 或 **`hiclaw-manager`** 容器内执行 **`hiclaw create worker` / `hiclaw update worker`**（见 [faq.md](../faq.md)）。
- 使用 **`install/hiclaw-apply.sh`** 应用 YAML（转发到 Manager 容器内的 `hiclaw apply -f`）。

字段说明见 [Declarative Resource Management](../declarative-resource-management.md)。

### 按 `spec.runtime` 区分的目录布局

| 运行时 | 主要工作目录 | 说明 |
|--------|----------------|------|
| **openclaw** | `/root/hiclaw-fs/agents/<worker-name>/`（`HOME` 指向此处） | `openclaw.json`、`SOUL.md`、`AGENTS.md`、skills、`.openclaw/` 等。共享数据：`/root/hiclaw-fs/shared/`。 |
| **copaw** | `/root/.hiclaw-worker/<worker-name>/`（QwenPaw 配置在 `.copaw/`） | 兼容性符号链接 **`/root/hiclaw-fs`** 指向该 Worker 树，便于沿用 OpenClaw 风格路径的脚本。 |
| **hermes** | `/root/hiclaw-fs/agents/<worker-name>/`（`HOME` 即工作区，与 OpenClaw 相同的镜像根） | Hermes 状态在目录内 **`.hermes/`**（如 `.hermes/config.yaml`、`state.db`）。 |

## 安装

Worker 由 **Manager Agent** 或 **controller 声明式 API** 创建。Manager 负责（或通过 controller 等价完成）Matrix 账号、Higress Consumer、配置文件等；可直接创建 Worker 容器，也可给出手动执行的 `docker run` 命令。

### 方式一：直接创建（推荐用于本地开发）

如果 Manager 能访问宿主机的容器运行时 socket（使用 `make install` 安装时默认开启），它可以直接创建 Worker 容器：

1. 告诉 Manager："帮我创建一个名为 alice 的 Worker，用于前端开发。直接创建。"
2. Manager 完成所有基础设施配置并自动启动容器
3. 无需任何手动操作

### 方式二：Docker Run 命令（用于远程部署）

如果 Manager 没有 socket 访问权限，它会回复一条 `docker run` 命令：

1. 告诉 Manager："帮我创建一个名为 alice 的 Worker，用于前端开发"
2. Manager 完成基础设施配置并提供 `docker run` 命令
3. 将命令复制到目标宿主机上执行：

```bash
docker run -d --name hiclaw-worker-alice \
  -e HICLAW_WORKER_NAME=alice \
  -e HICLAW_FS_ENDPOINT=http://<MANAGER_HOST>:9000 \
  -e HICLAW_FS_ACCESS_KEY=<ACCESS_KEY> \
  -e HICLAW_FS_SECRET_KEY=<SECRET_KEY> \
  hiclaw/worker-agent:latest
```

Manager 会在回复中提供所有具体参数值。

## 故障排查

### Worker 无法启动

```bash
# 查看容器日志
docker logs hiclaw-worker-alice

# 常见问题：
# - "openclaw.json not found"：Manager 尚未创建配置文件
# - "mc: command not found"：镜像构建问题
# - Connection refused：Manager 容器未运行或端口未暴露
```

### Worker 无法连接 Matrix

```bash
# 验证 Matrix 服务器是否可从 Worker 访问（通过网关端口）
docker exec hiclaw-worker-alice curl -sf http://matrix-local.hiclaw.io:18080/_matrix/client/versions

# 检查 Worker 的 openclaw.json 中的 Matrix 配置
docker exec hiclaw-worker-alice cat /root/hiclaw-fs/agents/alice/openclaw.json | jq '.channels.matrix'
```

### Worker 无法访问 LLM

```bash
# 使用 Worker 的 key 测试 AI 网关访问
# 注意：以下命令在 Worker 容器内执行，域名会解析到 Manager 的内部 IP
docker exec hiclaw-worker-alice curl -sf \
  -H "Authorization: Bearer $(jq -r '.models.providers."hiclaw-gateway".apiKey' /root/hiclaw-fs/agents/alice/openclaw.json)" \
  http://aigw-local.hiclaw.io:8080/v1/models

# 401：检查 openclaw.json 中的 Consumer key 是否与 Higress 中的一致
# 403：Worker 可能未被授权访问 AI 路由，请让 Manager 添加权限
```

### Worker 无法访问 MCP（GitHub）

```bash
# 测试 mcporter 连通性（在 Worker 容器内执行）
docker exec hiclaw-worker-alice mcporter --transport http \
  --server-url "http://aigw-local.hiclaw.io:8080/mcp-servers/mcp-github/mcp" \
  --header "Authorization=Bearer <WORKER_KEY>" \
  call list_repos '{"owner": "test"}'

# 403：Worker 未被授权访问此 MCP Server，请联系 Manager 添加权限
```

### 重置 Worker

```bash
# 停止并删除容器
docker stop hiclaw-worker-alice
docker rm hiclaw-worker-alice

# 然后让 Manager 重新创建 Worker：
# "请重新创建 alice worker 容器"
# Manager 会重新运行 create-worker.sh，重新生成凭据并重启容器
```

> 注意：Worker 的配置和任务数据存储在 MinIO 中，而非容器内。删除容器不会丢失任何工作内容。

## 生命周期管理

Manager 自动管理 Worker 容器的生命周期：

- **自动停止**：空闲 Worker（无活跃有限任务）在可配置的超时后自动停止，以节省资源
- **自动启动**：当任务分配给已停止的 Worker 时，Manager 会在发送任务前将其唤醒
- **重启后自动重建**：Manager 容器重启时，会检查所有已注册的 Worker，并重建任何容器缺失或 Manager IP 已变更的 Worker

你也可以通过与 Manager 对话手动控制 Worker：
- "停止 alice worker"
- "启动 alice worker"
- "查看所有 Worker 的状态"

## 架构详情

### 启动流程

1. 配置 `mc` 的 MinIO 别名
2. 从 MinIO 拉取 Worker 配置（`agents/<name>/`）
3. 复制技能模板
4. 启动双向 mc mirror 同步
5. 配置 mcporter 的 MCP 端点
6. 启动 OpenClaw

### 文件同步

- **本地 → 远端**：通过 `mc mirror --watch` 实时同步
- **远端 → 本地**：每 5 分钟定期拉取

### 配置热重载

当 Manager 更新 MinIO 中的 Worker 配置时：
1. MinIO 接收更新后的文件
2. mc mirror 将其拉取到 Worker 本地文件系统（下一个 5 分钟周期，或 Manager 主动推送时立即生效）
3. OpenClaw 检测到文件变更（约 300ms）并热重载配置

### 环境变量

| 变量 | 说明 | 示例值 |
|------|------|--------|
| `HICLAW_WORKER_NAME` | Worker 标识符 | `alice` |
| `HICLAW_MATRIX_URL` | Matrix Homeserver URL | `http://matrix-local.hiclaw.io:18080` |
| `HICLAW_AI_GATEWAY_URL` | AI 网关 URL | `http://aigw-local.hiclaw.io:18080` |
| `HICLAW_FS_ENDPOINT` | MinIO 端点 URL | `http://<MANAGER_HOST>:9000` |
| `HICLAW_FS_BUCKET` | 非默认存储布局下的 bucket 名称 | `hiclaw-storage` |
| `HICLAW_FS_ACCESS_KEY` | MinIO 访问密钥（由 Manager 生成，Worker 专用） | - |
| `HICLAW_FS_SECRET_KEY` | MinIO 密钥（由 Manager 生成，Worker 专用） | - |

> 所有参数值均由 Manager 生成，并在 `docker run` 命令中提供，或在直接创建时自动设置。通常无需手动配置。
>
> 运行时脚本现在直接使用 `HICLAW_MATRIX_URL` 和 `HICLAW_AI_GATEWAY_URL`；旧别名已经不再属于主契约。

### 手动同步文件

在 Worker 容器内执行 `hiclaw-sync`，可立即从 MinIO 拉取最新的配置和技能文件：

```bash
docker exec hiclaw-worker-alice hiclaw-sync
```

当 Manager 向 MinIO 推送了更新的技能或配置，而你不想等待下一个同步周期时，这个命令很有用。
