# Manager 使用指南

AgentTeams Manager 的详细配置和使用指南。

## 安装

基本安装步骤参见 [quickstart.md](../quickstart.md) 第一步。

## 配置

Manager 通过安装时设置的环境变量进行配置。安装脚本会生成包含所有配置的 `.env` 文件。

### 环境变量

| 变量 | 是否必填 | 默认值 | 说明 |
|------|----------|--------|------|
| `AGENTTEAMS_LLM_API_KEY` | 是 | - | LLM API Key |
| `AGENTTEAMS_LLM_PROVIDER` | 否 | `qwen` | LLM 提供商（`qwen` 为阿里云百炼，`openai-compat` 为 OpenAI 兼容 API） |
| `AGENTTEAMS_DEFAULT_MODEL` | 否 | `qwen3.5-plus` | 默认模型 ID |
| `AGENTTEAMS_MODEL_CONTEXT_WINDOW` | 否 | (模型默认) | 覆盖自定义模型的上下文窗口大小 |
| `AGENTTEAMS_MODEL_MAX_TOKENS` | 否 | (模型默认) | 覆盖自定义模型的最大输出 Token 数 |
| `AGENTTEAMS_MODEL_VISION` | 否 | (模型默认) | 覆盖自定义模型的多模态视觉能力（`true`/`false`）。仅模型不在内置预设表中时需要。 |
| `AGENTTEAMS_MODEL_REASONING` | 否 | (模型默认) | 覆盖自定义模型的推理能力（`true`/`false`）。仅模型不在内置预设表中时需要。 |
| `AGENTTEAMS_ADMIN_USER` | 否 | `admin` | 人工管理员的 Matrix 用户名 |
| `AGENTTEAMS_ADMIN_PASSWORD` | 否 | （自动生成） | 管理员密码（最少 8 位，MinIO 要求） |
| `AGENTTEAMS_MATRIX_DOMAIN` | 否 | `matrix-local.agentteams.io:18080` | Matrix 服务器域名（容器内使用） |
| `AGENTTEAMS_MATRIX_CLIENT_DOMAIN` | 否 | `matrix-client-local.agentteams.io` | Element Web 域名 |
| `AGENTTEAMS_AI_GATEWAY_DOMAIN` | 否 | `aigw-local.agentteams.io` | AI 网关域名（用于 LLM 和 MCP） |
| `AGENTTEAMS_FS_DOMAIN` | 否 | `fs-local.agentteams.io` | 文件系统域名 |
| `AGENTTEAMS_PORT_GATEWAY` | 否 | `18080` | Higress 网关的宿主机端口 |
| `AGENTTEAMS_PORT_CONSOLE` | 否 | `18001` | Higress 控制台的宿主机端口 |
| `AGENTTEAMS_PORT_ELEMENT_WEB` | 否 | `18088` | Element Web 直接访问的宿主机端口 |
| `AGENTTEAMS_GITHUB_TOKEN` | 否 | - | GitHub PAT，用于 MCP Server |
| `AGENTTEAMS_WORKER_IMAGE` | 否 | `agentteams/worker-agent:latest` | 直接创建 Worker 时使用的 Docker 镜像 |
| `AGENTTEAMS_WORKSPACE_DIR` | 否 | `~/agentteams-manager` | Manager 工作空间的宿主机目录（bind mount 到 `/root/manager-workspace`） |
| `AGENTTEAMS_DATA_DIR` | 否 | `agentteams-data` | 持久化数据的 Docker 卷名称 |
| `AGENTTEAMS_MOUNT_SOCKET` | 否 | `1` | 挂载容器运行时 socket 以支持直接创建 Worker |
| `AGENTTEAMS_YOLO` | 否 | - | 设为 `1` 启用 YOLO 模式（自主决策，无交互提示） |
| `AGENTTEAMS_MANAGER_RUNTIME` | 否 | `qwenpaw` | Manager 引擎：**`qwenpaw`**（默认，`agentteams-manager-qwenpaw` 镜像）或 **`openclaw`**（`agentteams-manager` 镜像）。**Hermes** 仅支持 **Worker**，不能作为 Manager 运行时。 |

用户看到的 Manager runtime 选项仍是 **CoPaw** 和 **OpenClaw**。启动入口会把 `copaw` 作为 `qwenpaw` 的兼容别名：本地安装器当前写入 `copaw`，Helm 当前使用 `qwenpaw`，两者都启动 CoPaw/QwenPaw Python Manager 镜像。

### QwenPaw Manager（`AGENTTEAMS_MANAGER_RUNTIME=qwenpaw`）

安装时若选择 QwenPaw Manager，controller 会拉起 **`agentteams-manager-qwenpaw`** 镜像而非基于 OpenClaw 的 **`agentteams-manager`**。职责相同（经 Matrix 协调 Worker/Team、驱动 Higress/MCP），差异在于 Agent 引擎与配置形态（Python QwenPaw vs Node OpenClaw）。多通道与技能遵循 QwenPaw 工作区约定（容器内 **`/root/manager-workspace`**）。

### 自定义 Manager Agent

以下三个文件存放在 MinIO **`agentteams-storage`** 桶中（对象前缀 `agents/manager/`）。安装脚本将宿主机工作区 bind mount 到 Manager 容器的 **`/root/manager-workspace`**，并与该桶保持同步——既可在 MinIO 控制台/API 中编辑，也可直接编辑宿主机 `AGENTTEAMS_WORKSPACE_DIR`（默认 `~/agentteams-manager`）下的对应文件。

1. **SOUL.md** - Agent 身份、安全规则、通信模型
2. **HEARTBEAT.md** - 定期检查例程（随运行时为 OpenClaw 心跳或 QwenPaw 等价机制）
3. **AGENTS.md** - 可用技能和任务工作流

若本地仍暴露 MinIO 端口，可使用 MinIO 控制台；否则在 **`agentteams-controller`** 内使用 `mc`，或编辑宿主机工作区中的镜像文件。

### 通过 Manager 为 Worker 安装 Skill

有两种方式可以把 Skill 文件交给 Manager。

#### 使用 Manager 工作空间

将完整的 Skill 目录放入 Manager 工作空间的 Worker Skill 库。使用默认工作空间时，宿主机目录结构如下：

```text
~/agentteams-manager/worker-skills/alert-fusion/
├── SKILL.md
├── scripts/       # 可选
└── references/    # 可选
```

如果通过 `AGENTTEAMS_WORKSPACE_DIR` 配置了其他工作空间，请将 `~/agentteams-manager` 替换为实际目录。在 Manager 容器中，同一个 Skill 的路径为 `~/worker-skills/alert-fusion/`。目录名应与 `SKILL.md` 中的 `name` 保持一致。

然后直接向 Manager 下达指令，例如：

> 请为 Worker `amy-ai` 安装 `alert-fusion` Skill。Skill 文件位于 `~/worker-skills/alert-fusion/`。安装完成后请验证，并确认该 Skill 已加入 Worker 的分配列表。

#### 直接向 Manager 发送 ZIP 附件

将一个完整的 Skill 根目录打包为 ZIP。压缩包必须包含 `SKILL.md`，也可以包含 `scripts/` 和 `references/`。在 Manager 对话中把 ZIP 作为文件附件发送，然后继续发送指令，例如：

> 请将我刚发送的 ZIP 附件中的 Skill 安装给 Worker `amy-ai`。请安全解压并校验 `SKILL.md`，将完整 Skill 放入 `~/worker-skills/`，完成分发后检查最终分配结果。

内置 Matrix 对话支持文件附件。Manager 会下载附件，拒绝存在不安全路径或包含多个 Skill 根目录的压缩包，在临时目录中解压并校验 Skill 元数据，然后才将完整目录放入 Worker Skill 库并开始分发。如果 Skill 还包含脚本或参考资料，应发送完整 ZIP，而不是分别发送多个文件。

对于这种明确指定 Worker 的分发，`SKILL.md` frontmatter 中的 `name` 和 `description` 必填，`assign_when` 可选。缺少 `assign_when` 不影响将 Skill 分配给请求中指定的 Worker，但 Manager 在新建 Worker 时不会将该 Skill 纳入基于角色的自动匹配。

Manager 会依次校验 Skill 源文件、将完整目录上传到 Worker 的隔离存储、确认远端 `SKILL.md` 存在，然后才更新 Worker 的 Skill 分配。对于 QwenPaw Worker，运行时会拉取新的分配，将 Skill 复制到原生工作空间，刷新 Skill 列表并启用该 Skill，无需手动重启 QwenPaw。

可以直接通过自然语言对话让 Manager 检查分配结果：

> 请检查 Worker `amy-ai` 当前分配的 Skill，并确认其中是否包含 `alert-fusion`。

如果需要验证的不只是分配记录，而是 Worker 运行时是否已经实际可用，可以继续询问：

> 请让 Worker `amy-ai` 确认它能够发现并使用 `alert-fusion` Skill。

如果需要从运维侧检查或排障，也可以在 Manager 或 controller 容器中执行等价查询：

```bash
agt get workers amy-ai -o json | jq '.skills'
```

如果 Manager 提示找不到源文件，请确认 `SKILL.md` 位于 `worker-skills/<skill-name>/` 下，而不是直接放在 `worker-skills/` 根目录。

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

**存储位置**：`~/agentteams-manager/primary-channel.json`（跨重启持久化）

**备用方案**：如果主渠道不可用或未配置，Manager 自动回退到 Matrix 私信。

### 受信联系人

默认情况下，只有管理员可以与 Manager 交互。如果你想允许其他人（如团队成员）提问而不赋予他们管理员权限，可以将其添加为**受信联系人**：

1. 让他们向 Manager 发送消息（通过任何已配置的渠道）。
2. 告诉 Manager："你可以和刚才给我发消息的人交流"（或类似表述）。
3. Manager 将其添加到 `~/agentteams-manager/trusted-contacts.json`。

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
~/agentteams-fs/shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

这些文件存储在共享 MinIO 存储中，Manager 和其他 Worker 均可读取。它们记录了已完成的步骤、当前状态、遇到的问题和下一步计划——即使会话重置后也能提供完整的审计追踪。

#### 任务历史（LRU 最近 10 条）

每个 Worker 维护一个本地任务历史文件：

```
~/agentteams-fs/agents/{worker-name}/task-history.json
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

**v1.1.0+ 嵌入式安装：** Higress、Tuwunel、MinIO 运行在 **`agentteams-controller`** 内。**`agentteams-manager`** 仅运行协调 Agent；基础设施日志在 controller 上查看。

```bash
# Manager Agent（stdout/stderr + 启动脚本）
docker logs agentteams-manager -f
docker exec agentteams-manager cat /var/log/agentteams/manager-agent.log

# OpenClaw 运行时日志（仅 OpenClaw Manager）
docker exec agentteams-manager bash -c 'cat /tmp/openclaw/openclaw-*.log' | jq .

# 基础设施 + Higress 控制台（嵌入式栈）
docker logs agentteams-controller -f
docker exec agentteams-controller cat /var/log/agentteams/higress-console.log
docker exec agentteams-controller cat /var/log/agentteams/tuwunel.log
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
docker exec agentteams-controller curl -sf http://127.0.0.1:6167/_matrix/client/versions
docker exec agentteams-controller curl -sf http://127.0.0.1:9000/minio/health/live

# Higress 控制台（宿主机端口）
curl -s http://127.0.0.1:18001/
```

### 控制台

- **Higress 控制台**：http://localhost:18001 — 网关路由与 Consumer
- **Element Web**：http://127.0.0.1:18088 — IM（宿主机直连端口），或经网关 `http://matrix-client-local.agentteams.io:18080`（需将 `*-local.agentteams.io` 解析到本机）
- **MinIO**：嵌入式安装下 MinIO 在 **`agentteams-controller`** 内，默认不把控制台端口映射到宿主机；请用容器内 `mc`、内部 API，或自行增加访问方式
- **OpenClaw 控制 UI**（仅 OpenClaw Manager）：http://127.0.0.1:18888

## 备份与恢复

### 数据卷

所有持久化数据存储在 `agentteams-data` Docker 卷中：
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
docker run --rm -v agentteams-data:/data -v $(pwd):/backup ubuntu \
  tar czf /backup/agentteams-backup-$(date +%Y%m%d).tar.gz /data
```

### 恢复

```bash
docker run --rm -v agentteams-data:/data -v $(pwd):/backup ubuntu \
  tar xzf /backup/agentteams-backup-YYYYMMDD.tar.gz -C /
```

### 目录结构

系统使用 Docker 卷保存持久化数据，也可以选择共享宿主机目录：

- `agentteams-data` Docker 卷：保存所有系统持久化数据。
- 宿主机 `$HOME`：可以选择挂载到容器内的 `/host-share`。
- 容器内原始宿主机路径（例如 `/home/zhangty`）：启用共享后，通过符号链接指向 `/host-share`。
- 启用共享后，宿主机与容器可以使用一致的文件路径。

目录共享使 Agent 能以与宿主机相同的路径直接读写文件，便于传递和处理文件。

### 使用示例

```bash
# 示例 1：安装时启用主目录共享（推荐）
AGENTTEAMS_LLM_API_KEY=your-key-here ./install/agentteams-install.sh manager

# 示例 2：把需要 Agent 访问的文件放入主目录
mkdir -p ~/project-inputs/
echo "Sample data" > ~/project-inputs/sample.txt

# 示例 3：Agent 在容器内使用与宿主机相同的路径
# 宿主机路径：/home/zhangty/project-inputs/sample.txt
# 容器路径：/home/zhangty/project-inputs/sample.txt（通过符号链接）

# 示例 4：在 Agent 配置中引用宿主机文件
# 宿主机：/home/zhangty/data/input.txt
# 容器：/home/zhangty/data/input.txt（通过符号链接保持一致）
```

## YOLO 模式

YOLO 模式让 Manager 完全自主运行——跳过所有交互式管理员提示，自行做出合理决策。适用于 CI/测试和自动化工作流。

### 激活方式

两种方式均可激活（任选其一）：

```bash
# 方式 1：容器启动时通过环境变量
docker run -e AGENTTEAMS_YOLO=1 ... agentteams/manager:latest

# 方式 2：在工作空间中创建文件（立即生效，无需重启）
docker exec agentteams-manager touch /root/manager-workspace/yolo-mode
```

`make test` 和 `make replay` 都会自动启用 YOLO 模式。

### 行为对比

| 场景 | 普通模式 | YOLO 模式 |
|------|----------|-----------|
| 需要 GitHub PAT 但未配置 | 询问管理员 | 跳过 GitHub 集成，注明"GitHub 未配置" |
| 其他需要确认的决策 | 提示管理员 | 做出最合理的选择，在消息中说明 |

YOLO 模式**不会**影响安全规则、Worker 凭据隔离或 Agent 通信对人工管理员的可见性。
