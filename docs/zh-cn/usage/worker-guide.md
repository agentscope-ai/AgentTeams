# Worker 使用指南

AgentTeams Worker Agent 的部署、管理和故障排查指南。

## 概述

Worker 是轻量级无状态容器，负责：
- 通过 Matrix 连接 Manager 接收任务
- 从集中式 MinIO 存储同步配置
- 通过 AI 网关访问 LLM
- 通过 mcporter CLI 调用 MCP Server 工具（GitHub 等）

### 声明式创建与更新（v1.1.0+）

Worker 由 **CR** 描述。除在 Matrix 里让 Manager 创建外，你还可以：

- 在 **`agentteams-controller`** 或 **`agentteams-manager`** 容器内执行 **`agt create worker` / `agt update worker`**（见 [faq.md](troubleshooting/faq.md)）。
- 使用 **`install/agentteams-apply.sh`** 应用 YAML（转发到 Manager 容器内的 `agt apply -f`）。

字段说明见 [Declarative Resource Management](resource-management.md)。

### 按 `spec.runtime` 区分的目录布局

| 运行时 | 主要工作目录 | 说明 |
|--------|----------------|------|
| **openclaw** | `/root/agentteams-fs/agents/<worker-name>/`（`HOME` 指向此处） | `openclaw.json`、`SOUL.md`、`AGENTS.md`、skills、`.openclaw/` 等。共享数据：`/root/agentteams-fs/shared/`。 |
| **copaw** | `/root/.agentteams-worker/<worker-name>/`（运行时配置在 `.copaw/`） | 旧版兼容路径；符号链接 **`/root/agentteams-fs`** 指向该 Worker 树，便于沿用 OpenClaw 风格路径的脚本。 |
| **qwenpaw** | `/root/agentteams-fs/agents/<worker-name>/`（QwenPaw 配置在 `.qwenpaw/`） | QwenPaw 2.x 路径；从 `copaw` 切换时会在恢复持久化数据后，将旧 `.copaw/` 状态迁移到 `.qwenpaw/`。共享数据：`/root/agentteams-fs/shared/`。 |
| **hermes** | `/root/agentteams-fs/agents/<worker-name>/`（`HOME` 即工作区，与 OpenClaw 相同的镜像根） | Hermes 状态在目录内 **`.hermes/`**（如 `.hermes/config.yaml`、`state.db`）。 |

Controller 中已包含 OpenHuman 后端，但当前发布的 Worker CRD enum 尚不接受显式的 `spec.runtime: openhuman`。在业务代码单独修正该契约前，不应按普通 Worker CR 流程创建 OpenHuman Worker。

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
docker run -d --name agentteams-worker-alice \
  -e AGENTTEAMS_WORKER_NAME=alice \
  -e AGENTTEAMS_FS_ENDPOINT=http://<MANAGER_HOST>:9000 \
  -e AGENTTEAMS_FS_ACCESS_KEY=<ACCESS_KEY> \
  -e AGENTTEAMS_FS_SECRET_KEY=<SECRET_KEY> \
  agentteams/worker-agent:latest
```

Manager 会在回复中提供所有具体参数值。

## 为 Worker 安装 Skill

对于已有 Worker，目前有两种稳定的 Skill 安装方式：

| 方式 | 适用场景 | 持久化结果 |
|---|---|---|
| 通过 Manager 分发 | 希望由 Manager 校验、分发并维护声明式分配记录 | 上传完整 Skill，并更新 `Worker.spec.skills` |
| 通过 Dashboard 分发 | 已经有 ZIP，希望直接在管理页面选择一个或多个目标 Worker | 上传完整 Skill、更新 `Worker.spec.skills` 并触发重新加载 |

两种方式都会先把完整 Skill 写入 Worker 的规范持久化目录，再将 Skill 名称加入 `spec.skills`，Worker runtime 随后从该存储同步并加载文件。它们的区别主要在操作入口和可分发源的维护位置：Manager 会在 `worker-skills/` 下保留源文件，Dashboard 则可以直接从市场或上传的 ZIP 分发。

### 方式一：通过 Manager 分发

可以通过以下任一方式把 Skill 提供给 Manager：

1. 在 Manager 宿主机上，将完整的第三方 Skill 放到 `$AGENTTEAMS_WORKSPACE_DIR/worker-skills/<skill-name>/`。默认路径为 `~/agentteams-manager/worker-skills/<skill-name>/`；或者
2. 直接向 Manager 发送 ZIP 附件，压缩包内包含一个完整的 Skill 根目录、`SKILL.md`，以及可选的 `scripts/`、`references/`。

然后让 Manager 为指定 Worker 安装该 Skill，并验证分配结果。如果使用 ZIP 附件，应明确要求 Manager 在分发前安全解压并校验内容。

例如：

> 请将 `~/worker-skills/alert-fusion/` 中的 `alert-fusion` Skill 安装给 Worker `amy-ai`。请确认文件上传成功，并验证 Worker 的 Skill 分配已经更新。

或者在发送 ZIP 附件后说：

> 请将我刚发送的 ZIP 附件中的 Skill 安装给 Worker `amy-ai`。请安全解压和校验，分发完整 Skill，并验证 Worker 的 Skill 分配。

Manager 会先上传并校验文件，再更新 `Worker.spec.skills`，避免 Worker 收到一个缺少实际内容的 Skill 分配。QwenPaw Worker 随后会把已分配 Skill 同步到原生工作空间，并自动刷新、启用。

可以直接询问 Manager 来检查分配结果：

> 请列出 Worker `amy-ai` 当前分配的 Skill，并确认其中是否包含 `alert-fusion`。

如果需要从运维侧检查或排障，可使用等价的 CLI 查询：

```bash
agt get workers amy-ai -o json | jq '.skills'
```

`agt get workers amy-ai -o json | jq '.skills'` 查询声明式 `spec.skills` 分配记录，Manager 和 Dashboard 分发都会更新该字段。实际文件和 runtime 是否可用仍应按下一节的方法单独验证。

### 方式二：通过 Dashboard 分发

此方式要求 Dashboard 已启用，并且 Dashboard 已配置可访问 AgentTeams 对象存储的凭证。使用 AgentTeams Bash 安装器部署的 embedded 实例会自动完成这些连接配置。

#### 准备 Skill ZIP

上传包应满足以下要求：

- 文件扩展名为 `.zip`，大小不超过 64 MB。
- 包内包含一个完整 Skill 根目录；`SKILL.md` 可以位于 ZIP 根目录或该 Skill 根目录下。
- `SKILL.md` 以 YAML frontmatter 开头，并包含非空的 `name` 和 `description` 字段。
- `name` 只能包含字母、数字、点、下划线和连字符，并且必须以字母或数字开头。
- `scripts/`、`references/` 等附属文件应与 `SKILL.md` 一起放入 ZIP；Dashboard 会保留 Skill 根目录下的完整文件结构。

#### 从资源中心的市场分发

1. 打开 Dashboard，进入左侧导航**资源中心**分组下的**市场**。
2. 如果 Skill 尚未加入市场，点击右上角的**上传技能**，选择 Skill ZIP，点击**解析预览**确认名称和描述，再点击**上传**。
3. 在市场列表中找到目标 Skill，点击该行操作区的发送图标（**分发到 Worker**）。
4. 在**分发技能到 Worker**弹窗中选择一个或多个目标 Worker。
5. 点击**分发到 N 个 Worker**，等待各 Worker 的上传、分配和加载结果。

也可以从 **Workers → 目标 Worker → 详情 → 上传技能包**直接为单个 Worker 上传 Skill ZIP。

创建或编辑 Worker 时，也可以在表单的**技能**字段中选择市场或 Nacos 中已有的 Skill。保存 Worker 后，Dashboard 会将尚未就位的完整 Skill 同步到该 Worker 的规范持久化目录，核对并补齐 `spec.skills`，最后尝试重启 Worker。上传或 `spec.skills` 更新失败会作为部分失败显示，不会被误报为安装成功。

> **注意：** **上传技能**只会把 Skill 加入 Dashboard 的集中式市场，不会自动分发给 Worker。上传完成后，仍需从该 Skill 所在行点击**分发到 Worker**；也可以使用 Worker 详情中的**上传技能包**直接上传，或在创建、编辑 Worker 时选择该 Skill。

#### 加载与验证

Dashboard 会校验 ZIP 和 `SKILL.md`，保留 Skill 根目录下的全部文件，并写入对象存储的 `agents/<worker-name>/skills/<skill-name>/`。从市场分发或在 Worker 详情上传时，只有完整包写入成功后才会更新 `Worker.spec.skills`；在创建、编辑 Worker 时选择 Skill，则会在保存 Worker 后同步缺失的包并核对该字段。文件与声明式分配就位后，Dashboard 会尝试重启 Worker 以立即加载新的分配。因此应在 Worker 空闲时操作，避免中断正在执行的任务。

- 如果重新加载成功，页面会显示该 Worker 的安装或重启完成结果。
- 如果重启未确认，已经上传的文件和分配记录不会丢失；页面会报告部分失败，后续 Controller reconcile 可以继续使该分配生效，无需重新上传。

可以重新打开 **Workers → 目标 Worker → 详情**，在“已分发技能”中确认 Skill 名称。若需要验证 runtime 已经实际加载，而不只是文件已经存在，应让该 Worker 确认它能够发现并使用对应 Skill。

Dashboard 分发会更新 `Worker.spec.skills`，因此该 Skill 应出现在 `agt get workers <name> -o json | jq '.skills'` 中。Controller 后续 reconcile 可能要求 Manager 恢复声明式 Skill，但 Dashboard 分发的 Skill 不强制要求 Manager 保留源文件。规范 Worker 副本仍然存在时会忽略 Manager 恢复失败；该副本缺失且 Manager 也无法恢复时，Worker 会记录非阻塞告警。对于远程 Skill 分配，请求的版本或标签刷新失败时也会记录非阻塞告警，并保留已有的规范副本。告警只标明 Skill 及请求的版本或标签，不包含远程源地址中的凭据。

如果需要从 Skill 打包开始，完整验证分发、runtime 发现和实际使用，可以按照[案例六：添加并使用自定义 Skill](use-cases.md#8-案例六添加并使用自定义-skill)操作。

## 故障排查

### Worker 无法启动

```bash
# 查看容器日志
docker logs agentteams-worker-alice

# 常见问题：
# - "openclaw.json not found"：Manager 尚未创建配置文件
# - "mc: command not found"：镜像构建问题
# - Connection refused：Manager 容器未运行或端口未暴露
```

### Worker 无法连接 Matrix

```bash
# 验证 Matrix 服务器是否可从 Worker 访问（通过网关端口）
docker exec agentteams-worker-alice curl -sf http://matrix-local.agentteams.io:18080/_matrix/client/versions

# 检查 Worker 的 openclaw.json 中的 Matrix 配置
docker exec agentteams-worker-alice cat /root/agentteams-fs/agents/alice/openclaw.json | jq '.channels.matrix'
```

### Worker 无法访问 LLM

```bash
# 使用 Worker 的 key 测试 AI 网关访问
# 注意：以下命令在 Worker 容器内执行，域名会解析到 Manager 的内部 IP
docker exec agentteams-worker-alice curl -sf \
  -H "Authorization: Bearer $(jq -r '.models.providers."agentteams-gateway".apiKey' /root/agentteams-fs/agents/alice/openclaw.json)" \
  http://aigw-local.agentteams.io:8080/v1/models

# 401：检查 openclaw.json 中的 Consumer key 是否与 Higress 中的一致
# 403：Worker 可能未被授权访问 AI 路由，请让 Manager 添加权限
```

### Worker 无法访问 MCP（GitHub）

```bash
# 测试 mcporter 连通性（在 Worker 容器内执行）
docker exec agentteams-worker-alice mcporter --transport http \
  --server-url "http://aigw-local.agentteams.io:8080/mcp-servers/mcp-github/mcp" \
  --header "Authorization=Bearer <WORKER_KEY>" \
  call list_repos '{"owner": "test"}'

# 403：Worker 未被授权访问此 MCP Server，请联系 Manager 添加权限
```

### 重置 Worker

```bash
# 停止并删除容器
docker stop agentteams-worker-alice
docker rm agentteams-worker-alice

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

不同 runtime 使用不同入口脚本，但都会完成以下工作：

1. 获取对象存储凭据，并恢复 `agents/<name>/` 下的配置与持久化状态。
2. 准备 runtime 对应的工作目录、Agent 提示文件和 skills。
3. 将模型、Matrix channel、MCP Server 与团队上下文转换成 runtime 可读取的配置。
4. 启动文件同步或运行时配置更新循环。
5. 启动所选的 OpenClaw、CoPaw、QwenPaw 或 Hermes runtime。

具体目录见上方[按 `spec.runtime` 区分的目录布局](#按-specruntime-区分的目录布局)。排查问题时应使用对应 runtime 的日志和配置路径，不要把 OpenClaw 的 `openclaw.json` 消费方式直接套用到所有 Worker。

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
| `AGENTTEAMS_WORKER_NAME` | Worker 标识符 | `alice` |
| `AGENTTEAMS_MATRIX_URL` | Matrix Homeserver URL | `http://matrix-local.agentteams.io:18080` |
| `AGENTTEAMS_AI_GATEWAY_URL` | AI 网关 URL | `http://aigw-local.agentteams.io:18080` |
| `AGENTTEAMS_FS_ENDPOINT` | MinIO 端点 URL | `http://<MANAGER_HOST>:9000` |
| `AGENTTEAMS_FS_BUCKET` | 非默认存储布局下的 bucket 名称 | `agentteams-storage` |
| `AGENTTEAMS_FS_ACCESS_KEY` | MinIO 访问密钥（由 Manager 生成，Worker 专用） | - |
| `AGENTTEAMS_FS_SECRET_KEY` | MinIO 密钥（由 Manager 生成，Worker 专用） | - |

> 所有参数值均由 Manager 生成，并在 `docker run` 命令中提供，或在直接创建时自动设置。通常无需手动配置。
>
> 运行时脚本现在直接使用 `AGENTTEAMS_MATRIX_URL` 和 `AGENTTEAMS_AI_GATEWAY_URL`；旧别名已经不再属于主契约。

### 手动同步文件

在 Worker 容器内执行 `agentteams-sync`，可立即从 MinIO 拉取最新的配置和技能文件：

```bash
docker exec agentteams-worker-alice agentteams-sync
```

当 Manager 向 MinIO 推送了更新的技能或配置，而你不想等待下一个同步周期时，这个命令很有用。
