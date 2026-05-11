# Manager 使用指南

HiClaw Manager 的详细配置和使用指南。

## 安装

基本安装步骤参见 [quickstart.md](quickstart.md) 第一步。

## 配置

Manager 通过安装时设置的环境变量进行配置。安装脚本会生成包含所有配置的 `.env` 文件。

### 环境变量

| 变量 | 是否必填 | 默认值 | 说明 |
|------|----------|--------|------|
| `HICLAW_LLM_API_KEY` | 是 | - | LLM API Key |
| `HICLAW_LLM_PROVIDER` | 否 | `qwen` | LLM 提供商（`qwen` 为阿里云百炼，`openai-compat` 为 OpenAI 兼容 API） |
| `HICLAW_DEFAULT_MODEL` | 否 | `qwen3.5-plus` | 默认模型 ID |
| `HICLAW_ADMIN_USER` | 否 | `admin` | 人工管理员的 Matrix 用户名 |
| `HICLAW_ADMIN_PASSWORD` | 否 | （自动生成） | 管理员密码（最少 8 位，MinIO 要求） |
| `HICLAW_MATRIX_DOMAIN` | 否 | `matrix-local.hiclaw.io:18080` | Matrix 服务器域名（容器内使用） |
| `HICLAW_MATRIX_CLIENT_DOMAIN` | 否 | `matrix-client-local.hiclaw.io` | Element Web 域名 |
| `HICLAW_AI_GATEWAY_DOMAIN` | 否 | `aigw-local.hiclaw.io` | AI 网关域名（用于 LLM 和 MCP） |
| `HICLAW_FS_DOMAIN` | 否 | `fs-local.hiclaw.io` | 文件系统域名 |
| `HICLAW_PORT_GATEWAY` | 否 | `18080` | Higress 网关的宿主机端口 |
| `HICLAW_PORT_CONSOLE` | 否 | `18001` | Higress 控制台的宿主机端口 |
| `HICLAW_PORT_ELEMENT_WEB` | 否 | `18088` | Element Web 直接访问的宿主机端口 |
| `HICLAW_GITHUB_TOKEN` | 否 | - | GitHub PAT，用于 MCP Server |
| `HICLAW_WORKER_IMAGE` | 否 | `hiclaw/worker-agent:latest` | 直接创建 Worker 时使用的 Docker 镜像 |
| `HICLAW_WORKSPACE_DIR` | 否 | `~/hiclaw-manager` | Manager 工作空间的宿主机目录（bind mount 到 `/root/manager-workspace`） |
| `HICLAW_DATA_DIR` | 否 | `hiclaw-data` | 持久化数据的 Docker 卷名称 |
| `HICLAW_MOUNT_SOCKET` | 否 | `1` | 挂载容器运行时 socket 以支持直接创建 Worker |
| `HICLAW_YOLO` | 否 | - | 设为 `1` 启用 YOLO 模式（自主决策，无交互提示） |
| `HICLAW_MANAGER_RUNTIME` | 否 | `openclaw` | Manager 引擎：**`openclaw`**（默认，`hiclaw-manager` 镜像）或 **`copaw`**（`hiclaw-manager-copaw` 镜像）。**Hermes** 仅支持 **Worker**，不能作为 Manager 运行时。 |

### QwenPaw Manager（原 CoPaw，`HICLAW_MANAGER_RUNTIME=copaw`）

安装时若选择 QwenPaw Manager，controller 会拉起 **`hiclaw-manager-copaw`** 镜像而非基于 OpenClaw 的 **`hiclaw-manager`**。职责相同（经 Matrix 协调 Worker/Team、驱动 Higress/MCP），差异在于 Agent 引擎与配置形态（Python QwenPaw vs Node OpenClaw）。多通道与技能遵循 QwenPaw 工作区约定（容器内 **`/root/manager-workspace`**）。

### 自定义 Manager Agent

以下三个文件存放在 MinIO **`hiclaw-storage`** 桶中（对象前缀 `agents/manager/`）。安装脚本将宿主机工作区 bind mount 到 Manager 容器的 **`/root/manager-workspace`**，并与该桶保持同步——既可在 MinIO 控制台/API 中编辑，也可直接编辑宿主机 `HICLAW_WORKSPACE_DIR`（默认 `~/hiclaw-manager`）下的对应文件。

1. **SOUL.md** - Agent 身份、安全规则、通信模型
2. **HEARTBEAT.md** - 定期检查例程（随运行时为 OpenClaw 心跳或 QwenPaw 等价机制）
3. **AGENTS.md** - 可用技能和任务工作流

若本地仍暴露 MinIO 端口，可使用 MinIO 控制台；否则在 **`hiclaw-controller`** 内使用 `mc`，或编辑宿主机工作区中的镜像文件。

### 添加技能

仓库内置 **16** 个 Manager 技能，源码位于 `manager/agent/skills/`，同步到桶内路径 `agents/manager/skills/<name>/SKILL.md`：**channel-management**、**file-sync-management**、**git-delegation-management**、**hiclaw-find-worker**、**human-management**、**matrix-server-management**、**mcp-server-management**、**mcporter**、**model-switch**、**project-management**、**service-publishing**、**task-coordination**、**task-management**、**team-management**、**worker-management**、**worker-model-switch**。

将更多自包含的 `SKILL.md` 放到 `agents/manager/skills/<skill-name>/`。Manager 运行时会自动发现该目录下的技能。

添加新技能的步骤：
1. 创建目录：`agents/manager/skills/<your-skill-name>/`
2. 编写 `SKILL.md`，包含完整的 API 参考和示例
3. Manager Agent 会自动发现它（约 300ms）

### 管理 MCP Server

添加新的 MCP Server（如 GitLab、Jira）：

1. 在 Higress 控制台配置 MCP Server
2. 通过 Higress API 添加 MCP Server 条目：`PUT /v1/mcpServer`
3. 授权 Consumer：`PUT /v1/mcpServer/consumers`
4. 为 Worker 创建记录可用工具的技能文件

## 多渠道通信

Manager 支持 Matrix 私信之外的多种通信渠道。管理员可以通过 Discord、飞书、Telegram 或 OpenClaw 支持的任何其他渠道联系 Manager。

### 添加非 Matrix 渠道

1. 在 Manager 的 `openclaw.json`（或 `manager-openclaw.json.tmpl`）中添加 `channels.<channel>` 块，并在 `dm.allowFrom` 中填入管理员的用户 ID。具体配置参见 [OpenClaw 渠道文档](https://github.com/nicepkg/openclaw)。
2. 重启（或重新加载配置）以激活新渠道。
3. 从该渠道联系 Manager——它会识别你的身份，因为只有白名单中的发送者才能访问它。

### 主渠道

Manager 将主动通知（跨渠道升级等）发送到**主渠道**。默认为 Matrix 私信。

**设置主渠道**：首次从新渠道发送私信时，Manager 会询问是否将其设为主渠道。回复"是"确认。也可以随时切换，例如说"将主渠道切换到 Discord"。

**存储位置**：`~/hiclaw-manager/primary-channel.json`（跨重启持久化）

**备用方案**：如果主渠道不可用或未配置，Manager 自动回退到 Matrix 私信。

### 受信联系人

默认情况下，只有管理员可以与 Manager 交互。如果你想允许其他人（如团队成员）提问而不赋予他们管理员权限，可以将其添加为**受信联系人**：

1. 让他们向 Manager 发送消息（通过任何已配置的渠道）。
2. 告诉 Manager："你可以和刚才给我发消息的人交流"（或类似表述）。
3. Manager 将其添加到 `~/hiclaw-manager/trusted-contacts.json`。

受信联系人可以获得一般性回复，但 Manager **绝不会**向他们透露敏感信息（API Key、凭据、Worker 配置），也不会代表他们执行任何管理操作。

撤销访问权限：说"停止和[某人]交流"——Manager 会将其从列表中移除。

### 跨渠道升级

当 Manager 在 Matrix 项目房间中工作并需要紧急管理员决策时，它可以通过管理员的主渠道（如发送问题到你的 Discord 私信）进行升级，无需你在 Matrix 房间中。你的回复会自动路由回原始房间以继续工作流。

## 会话管理

### OpenClaw 会话保留策略

Manager 和 Worker 的 OpenClaw 实例使用**基于类型的会话策略**：

```json
"session": {
  "resetByType": {
    "dm":    { "mode": "daily", "atHour": 4 },
    "group": { "mode": "daily", "atHour": 4 }
  }
}
```

- **私信会话**（Manager ↔ 人工管理员）：每天 04:00 重置。
- **群组房间**（Worker 房间、项目房间）：每天 04:00 重置，与私信会话一致。

### 会话重置后的恢复机制

当 Worker 的会话被重置（因 2 天无活动导致上下文被清除）时，以下文件可以在不丢失进度的情况下恢复任务：

#### 进度日志

任务执行期间，Worker 在每次有意义的操作后追加到每日进度日志：

```
~/hiclaw-fs/shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

这些文件存储在共享 MinIO 存储中，Manager 和其他 Worker 均可读取。它们记录了已完成的步骤、当前状态、遇到的问题和下一步计划——即使会话重置后也能提供完整的审计追踪。

#### 任务历史（LRU 最近 10 条）

每个 Worker 维护一个本地任务历史文件：

```
~/hiclaw-fs/agents/{worker-name}/task-history.json
```

该文件记录最近 10 个活跃任务（任务 ID、简短描述、状态、任务目录路径、最后操作时间戳）。当新任务使数量超过 10 时，最旧的条目会归档到 `history-tasks/{task-id}.json`。

#### 会话重置后恢复任务

当 Manager 或人工管理员要求 Worker 在会话重置后恢复任务时，Worker 会：

1. 读取 `task-history.json`（或对于较旧的任务读取 `history-tasks/{task-id}.json`）以定位任务目录
2. 读取任务目录中的 `spec.md` 和 `plan.md`
3. 读取最近的 `progress/YYYY-MM-DD.md` 文件（从最新日期开始）以重建上下文
4. 继续工作并追加到今天的进度日志

## 监控

### 日志

**v1.1.0+ 嵌入式安装：** Higress、Tuwunel、MinIO 运行在 **`hiclaw-controller`** 内。**`hiclaw-manager`** 仅运行协调 Agent；基础设施日志在 controller 上查看。

```bash
# Manager Agent（stdout/stderr + 启动脚本）
docker logs hiclaw-manager -f
docker exec hiclaw-manager cat /var/log/hiclaw/manager-agent.log

# OpenClaw 运行时日志（仅 OpenClaw Manager）
docker exec hiclaw-manager bash -c 'cat /tmp/openclaw/openclaw-*.log' | jq .

# 基础设施 + Higress 控制台（嵌入式栈）
docker logs hiclaw-controller -f
docker exec hiclaw-controller cat /var/log/hiclaw/higress-console.log
docker exec hiclaw-controller cat /var/log/hiclaw/tuwunel.log
```

### Replay 对话日志

运行 `make replay` 后，对话日志会自动保存：

```bash
# 查看最新的 replay 日志
make replay-log

# 日志存储在 logs/replay/replay-{timestamp}.log
```

### 健康检查

```bash
# Matrix / MinIO（默认不发布到宿主机 — 在 controller 容器内探测）
docker exec hiclaw-controller curl -sf http://127.0.0.1:6167/_matrix/client/versions
docker exec hiclaw-controller curl -sf http://127.0.0.1:9000/minio/health/live

# Higress 控制台（宿主机端口）
curl -s http://127.0.0.1:18001/
```

### 控制台

- **Higress 控制台**：http://localhost:18001 — 网关路由与 Consumer
- **Element Web**：http://127.0.0.1:18088 — IM（宿主机直连端口），或经网关 `http://matrix-client-local.hiclaw.io:18080`（需将 `*-local.hiclaw.io` 解析到本机）
- **MinIO**：嵌入式安装下 MinIO 在 **`hiclaw-controller`** 内，默认不把控制台端口映射到宿主机；请用容器内 `mc`、内部 API，或自行增加访问方式
- **OpenClaw 控制 UI**（仅 OpenClaw Manager）：http://127.0.0.1:18888

## 备份与恢复

### 数据卷

所有持久化数据存储在 `hiclaw-data` Docker 卷中：
- Tuwunel 数据库（Matrix 历史记录）
- MinIO 存储（Agent 配置、任务数据）
- Higress 配置

此外，用户的主目录可以与 Agent 共享以访问文件：

#### 主目录共享（可选）
你可以选择与 Agent 共享用户主目录：
- 默认情况下，`$HOME` 在容器内以 `/host-share` 形式可访问
- 从原始宿主机主目录路径（如 `/home/zhangty`）创建符号链接指向 `/host-share`
- Agent 可以使用与宿主机相同的路径访问和操作文件
- 这实现了宿主机与 Agent 之间使用一致路径的无缝文件访问
- 安装时，安装脚本会提示选择要共享的目录（默认：$HOME）

### 备份

```bash
docker run --rm -v hiclaw-data:/data -v $(pwd):/backup ubuntu \
  tar czf /backup/hiclaw-backup-$(date +%Y%m%d).tar.gz /data
```

### 恢复

```bash
docker run --rm -v hiclaw-data:/data -v $(pwd):/backup ubuntu \
  tar xzf /backup/hiclaw-backup-YYYYMMDD.tar.gz -C /
```

## YOLO 模式

YOLO 模式让 Manager 完全自主运行——跳过所有交互式管理员提示，自行做出合理决策。适用于 CI/测试和自动化工作流。

### 激活方式

两种方式均可激活（任选其一）：

```bash
# 方式 1：容器启动时通过环境变量
docker run -e HICLAW_YOLO=1 ... hiclaw/manager-agent:latest

# 方式 2：在工作空间中创建文件（立即生效，无需重启）
docker exec hiclaw-manager touch /root/manager-workspace/yolo-mode
```

`make test` 和 `make replay` 都会自动启用 YOLO 模式。

### 行为对比

| 场景 | 普通模式 | YOLO 模式 |
|------|----------|-----------|
| 需要 GitHub PAT 但未配置 | 询问管理员 | 跳过 GitHub 集成，注明"GitHub 未配置" |
| 其他需要确认的决策 | 提示管理员 | 做出最合理的选择，在消息中说明 |

YOLO 模式**不会**影响安全规则、Worker 凭据隔离或 Agent 通信对人工管理员的可见性。
