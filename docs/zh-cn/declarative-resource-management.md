# 声明式资源管理

HiClaw 采用 Kubernetes CRD 风格的声明式 YAML 配置来管理平台中的所有资源——Worker、Team 和 Human。你只需要描述期望状态，HiClaw Controller 会自动完成创建、更新和删除。

## 核心概念

### 组织架构

HiClaw 采用三层组织架构，映射企业真实团队结构：

```
Admin (人类管理员)
  │
  ├── Manager (AI Agent, 管理入口)
  │     ├── Team Leader A (特殊 Worker, 团队内任务调度)
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C (独立 Worker, 不属于任何 Team)
  │
  └── Human Users (真人用户, 按权限级别接入)
        ├── Level 1: 等同 Admin，可与所有角色对话
        ├── Level 2: 可与指定 Team 的 Leader + Workers 对话
        └── Level 3: 只能与指定 Workers 对话
```

### 三种资源类型

| 资源 | 说明 | 对应实体 |
|------|------|---------|
| Worker | AI Agent 工作节点 | Docker 容器 + Matrix 账号 + MinIO 空间 |
| Team | 由 Leader + N 个 Worker 组成的协作组 | 一组 Worker 容器 + Team Room |
| Human | 真人用户 | Matrix 账号 + Room 权限 |

所有资源共享统一的 API 版本：`apiVersion: hiclaw.io/v1beta1`。

## Worker

Worker 是 HiClaw 中最基本的执行单元——一个运行在 Docker 容器中的 AI Agent，拥有独立的 Matrix 通信账号和 MinIO 存储空间。

### 基础配置

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6        # LLM 模型
  identity: |                      # Worker 公开身份信息（生成 IDENTITY.md）
    - Name: Alice
    - Specialization: DevOps, CI/CD pipeline management
  soul: |                          # Worker 人格与价值观设定（生成 SOUL.md）
    # Alice - DevOps Worker
    ## 人格
    - 严谨细致，部署前必定反复确认
    - 对潜在风险保持敏感，及早提出顾虑
    - 偏好自动化，尽量避免手动操作
    ## 价值观
    - 稳定优先：绝不为速度牺牲可靠性
    - 透明沟通：始终说明在做什么以及为什么
  agents: |                        # Agent 行为规则（生成 AGENTS.md）
    ## Behavior
    - Monitor CI/CD pipelines proactively
    - Alert on failures immediately
  skills:                          # HiClaw 内置 skills
    - github-operations
    - git-delegation
  mcpServers:                      # HiClaw 内置 MCP Servers（通过 Higress 网关授权）
    - github
```

### 完整字段说明

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `metadata.name` | string | 是 | — | Worker 名称，全局唯一 |
| `spec.model` | string | 是 | — | LLM 模型 ID，如 `claude-sonnet-4-6`、`qwen3.5-plus` |
| `spec.runtime` | string | 否 | `openclaw` | Agent 运行时，`openclaw` 或 `copaw` |
| `spec.image` | string | 否 | `hiclaw/worker-agent:latest` | 自定义 Docker 镜像 |
| `spec.identity` | string | 否 | — | Worker 公开身份信息，用于生成 IDENTITY.md |
| `spec.soul` | string | 否 | — | Worker 人格与价值观设定，用于生成 SOUL.md |
| `spec.agents` | string | 否 | — | Agent 行为规则，用于生成 AGENTS.md |
| `spec.skills` | []string | 否 | — | 内置 skills 列表，由 Manager 统一分发 |
| `spec.mcpServers` | []string | 否 | — | 内置 MCP Servers 列表，通过 Higress 网关授权 |
| `spec.package` | string | 否 | — | 自定义包 URI，支持 `file://`、`http(s)://`、`nacos://` |

### identity / soul / agents 与 package 的关系

配置 Worker 身份和行为有两种方式：

- **内联方式**：通过 `spec.identity`、`spec.soul` 和 `spec.agents` 字段直接在 YAML 中定义，Controller 会据此生成对应的 IDENTITY.md、SOUL.md 和 AGENTS.md。适合轻量配置场景。
- **包方式**：通过 `spec.package` 引入一个包含完整配置的 ZIP 包（IDENTITY.md、SOUL.md、AGENTS.md、自定义 skills、Dockerfile 等）。适合需要自定义 skills 或系统依赖的复杂场景。

两者可以同时使用——当同时配置时，内联字段会覆盖包中的对应文件。这允许你使用 package 作为基础模板，同时通过 YAML 定制特定部分。例如，导入一个共享的 Worker 包，但通过 `soul` 字段覆盖角色定义，赋予 Worker 独特的身份。

### 内置 Skills 与自定义 Skills

`spec.skills` 指的是 HiClaw 平台内置的能力，由 Manager 通过 `push-worker-skills.sh` 分发到 Worker 的 MinIO 空间。

如果需要自定义 Skills，通过 `spec.package` 引入一个包含 `skills/` 目录的 ZIP 包。内置 skills 和自定义 skills 会合并推送，互不冲突。

### 带自定义包的 Worker

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: devops-alice
spec:
  model: claude-sonnet-4-6
  runtime: openclaw
  skills: [github-operations]
  mcpServers: [github]
  package: file://./devops-alice.zip    # 包含自定义 SOUL.md、skills、Dockerfile 等
```

### Worker 创建流程

当 Controller 收到一个 Worker 资源后，会依次执行：

1. 解析 `spec.package`（如有），下载并解压到临时目录
2. 注册 Matrix 账号，创建通信 Room（Manager + Admin + Worker 三方）
3. 创建 MinIO 用户和 Bucket，配置 Higress 网关授权
4. 生成 `openclaw.json` 配置（含 `groupAllowFrom` 权限矩阵）
5. 推送所有配置文件（SOUL.md、skills、crons 等）到 MinIO
6. 更新 `workers-registry.json`
7. 启动 Worker 容器

### Worker 状态

| Phase | 含义 |
|-------|------|
| Pending | 资源已创建，等待 Controller 处理 |
| Running | 容器运行中，Agent 在线 |
| Stopped | 容器已停止 |
| Failed | 创建或运行失败，查看 `status.message` |

## Team

Team 是 HiClaw 的协作单元，由一个 Team Leader 和若干 Team Worker 组成。Manager 将任务委派给 Team Leader，Leader 负责分解、分配和汇总，实现团队内部自治。

### 基础配置

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  description: 全栈开发团队
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
    soul: |
      # Alpha Lead - Team Leader
      ## 人格
      - 沉稳有条理，带领团队聚焦优先事项
      - 对团队成员有耐心，鼓励开放沟通
      ## 价值观
      - 清晰：每个任务分配前必须有明确的验收标准
      - 信任：充分授权，不微观管理
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
      mcpServers: [github]
      soul: |
        # Alpha Dev - 后端开发
        ## 人格
        - 务实的问题解决者，偏好简单方案而非巧妙方案
        - 严格的代码审查者，善于发现边界情况
        ## 价值观
        - 代码质量：上线前先写测试
        - 保持简单：避免过早抽象
    - name: alpha-qa
      model: claude-sonnet-4-6
      soul: |
        # Alpha QA - 测试工程师
        ## 人格
        - 天生怀疑论者，总是问"哪里可能出问题？"
        - 对复现和记录问题一丝不苟
        ## 价值观
        - 用户体验优先：从用户视角进行测试
        - 不允许静默失败：每个 bug 都要有清晰的报告
```

### 完整字段说明

**Team 级别：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `metadata.name` | string | 是 | Team 名称，全局唯一 |
| `spec.description` | string | 否 | 团队描述 |
| `spec.admin` | object | 否 | 团队管理员（默认为全局 Admin） |
| `spec.leader` | object | 是 | Team Leader 配置 |
| `spec.workers` | []object | 是 | Team Worker 列表 |

**Leader 字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `leader.name` | string | 是 | Leader 名称 |
| `leader.model` | string | 否 | LLM 模型 |
| `leader.identity` | string | 否 | Leader 公开身份信息（生成 IDENTITY.md） |
| `leader.soul` | string | 否 | Leader 人格与价值观设定（生成 SOUL.md） |
| `leader.agents` | string | 否 | 自定义行为规则（追加在内置 AGENTS.md 之后） |
| `leader.package` | string | 否 | 自定义包 URI |

**Worker 字段（与独立 Worker 的 spec 一致）：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `workers[].name` | string | 是 | Worker 名称 |
| `workers[].model` | string | 否 | LLM 模型 |
| `workers[].runtime` | string | 否 | Agent 运行时 |
| `workers[].identity` | string | 否 | Worker 公开身份信息（生成 IDENTITY.md） |
| `workers[].soul` | string | 否 | Worker 人格与价值观设定（生成 SOUL.md） |
| `workers[].agents` | string | 否 | 自定义行为规则（追加在内置 AGENTS.md 之后） |
| `workers[].skills` | []string | 否 | 内置 skills |
| `workers[].mcpServers` | []string | 否 | 内置 MCP Servers |
| `workers[].package` | string | 否 | 自定义包 URI |

### Team Leader 的特殊性

Team Leader 本质上是一个 Worker 容器，但有以下区别：

- 使用 `team-leader-agent` 模板（SOUL.md.tmpl + AGENTS.md + HEARTBEAT.md）
- 拥有 `team-task-management` skill（管理 team-state.json、查找可用 Worker）
- 不拥有 `worker-management`、`mcp-server-management` 等 Manager 独占 skill
- 在 `workers-registry.json` 中标记为 `role: "team_leader"`
- 采用委派优先原则——始终将任务分配给团队 Worker，自己不执行领域任务

### Team Leader 的 AGENTS.md 组装

Team Leader 的 AGENTS.md 由三层内容组装而成，各自独立管理：

```
<!-- hiclaw-builtin-start -->
[内置：Team Leader 工作空间规则、任务流程、skills 参考]
<!-- hiclaw-builtin-end -->

<!-- hiclaw-team-context-start -->
## Coordination
- Upstream coordinator: @manager:{domain}
- Team Admin: @admin:{domain}
- Team: alpha-team
- Team members: alpha-dev, alpha-qa
<!-- hiclaw-team-context-end -->

[用户通过 spec.agents 提供的自定义内容（如有）]
```

- 内置段由 HiClaw 自动管理，升级时自动更新
- 团队上下文段自动注入团队名称、成员列表和协调者信息
- 用户通过 `spec.agents` 提供的内容放在两段之后，更新时不会被覆盖

### Room 拓扑

一个 Team 创建后会产生以下 Matrix Room：

```
Leader Room:   Manager + Global Admin + Leader        ← Manager 与 Leader 的通信通道
Team Room:     Leader + Team Admin + W1 + W2 + ...    ← Leader 与团队 Worker 的协作空间
Worker Room:   Leader + Team Admin + Worker           ← Leader 与单个 Worker 的私聊
Leader DM:     Team Admin ↔ Leader                    ← 团队管理通道
```

关键设计：Team Room 不包含 Manager，实现了委派边界。Manager 只通过 Leader Room 与 Leader 沟通，不穿透到团队内部。

### 任务流转

```
Admin 下发任务 → Manager
  ↓
Manager 判断匹配某个 Team 的领域
  ↓
Manager 创建任务 spec，@mention Leader
  ↓
Leader 分解为子任务，分配给团队 Worker
  ↓
Worker 执行完成，@mention Leader
  ↓
Leader 汇总结果，@mention Manager
  ↓
Manager 通知 Admin
```

### Team 状态

| Phase | 含义 |
|-------|------|
| Pending | 资源已创建，等待 Controller 处理 |
| Active | Leader 和所有 Worker 运行中 |
| Degraded | 部分 Worker 不可用，Leader 仍在运行 |

### Team Admin

可以为 Team 指定一个独立的管理员（Team Admin），替代全局 Admin 参与团队管理：

```yaml
spec:
  admin:
    name: pm-zhang
    matrixUserId: "@pm-zhang:domain"
```

如果不指定，默认使用全局 Admin。Team Admin 会被邀请到 Team Room 和 Leader DM，可以直接与 Leader 沟通团队事务。

## Human

Human 资源代表真人用户。创建后会自动注册 Matrix 账号，并根据权限级别将用户邀请到对应的 Room，实现人机协作。

### 基础配置

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: 张三
  email: john@example.com
  permissionLevel: 2
  accessibleTeams: [alpha-team]
  accessibleWorkers: []
  note: 前端负责人
```

### 完整字段说明

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `metadata.name` | string | 是 | — | 用户标识，全局唯一 |
| `spec.displayName` | string | 是 | — | 显示名称 |
| `spec.email` | string | 否 | — | 邮箱，用于发送账号密码 |
| `spec.permissionLevel` | int | 是 | — | 权限级别：1、2 或 3 |
| `spec.accessibleTeams` | []string | 否 | — | 可访问的 Team 列表（L2 生效） |
| `spec.accessibleWorkers` | []string | 否 | — | 可访问的独立 Worker 列表（L2/L3 生效） |
| `spec.note` | string | 否 | — | 备注 |

### 三级权限模型

权限级别是包含关系——高级别包含低级别的所有权限。

**Level 1 — Admin 等价**

可与系统中所有角色对话，包括 Manager、所有 Team Leader、所有 Worker。`accessibleTeams` 和 `accessibleWorkers` 字段被忽略。

适用场景：CTO、技术总监。

```yaml
spec:
  permissionLevel: 1
```

**Level 2 — Team 级**

可与指定 Team 的 Leader 和所有 Worker 对话，以及指定的独立 Worker。

适用场景：产品经理、团队成员。

```yaml
spec:
  permissionLevel: 2
  accessibleTeams: [alpha-team, beta-team]
  accessibleWorkers: [standalone-dev]
```

**Level 3 — Worker 级**

只能与指定的 Worker 对话。`accessibleTeams` 字段被忽略。

适用场景：外部协作者、特定职能人员。

```yaml
spec:
  permissionLevel: 3
  accessibleWorkers: [alice, bob]
```

### 权限实现机制

Human 的权限通过两个机制实现：

1. **Room 邀请**：将 Human 邀请到对应的 Matrix Room
2. **groupAllowFrom**：将 Human 的 Matrix ID 添加到对应 Agent 的 `openclaw.json` 配置中，Agent 只响应白名单中的 @mention

| 权限级别 | groupAllowFrom 变更 | Room 邀请 |
|---------|---------------------|----------|
| L1 | 添加到 Manager + 所有 Leader + 所有 Worker | 所有 Room |
| L2 | 添加到指定 Team 的 Leader + Worker + 指定独立 Worker | 指定 Team Room + Worker Room |
| L3 | 添加到指定 Worker | 指定 Worker Room |

### Human 创建流程

1. 注册 Matrix 账号（自动生成随机密码）
2. 按 permissionLevel 计算需要修改的 Agent 列表
3. 更新每个 Agent 的 `openclaw.json` 中的 `groupAllowFrom`
4. 邀请 Human 进入对应 Room
5. 更新 `humans-registry.json`
6. 推送更新后的配置到 MinIO，通知 Agent 执行 `file-sync`
7. 发送欢迎邮件（如配置了 SMTP 和 email）

### 自动发送欢迎邮件

当 `spec.email` 不为空且系统配置了 SMTP 时，Human 创建完成后会自动发送一封欢迎邮件，包含登录所需的全部信息：

```
Subject: Welcome to HiClaw - Your Account Details

Hi {displayName},

Your HiClaw account has been created:

  Username: {matrix_user_id}
  Password: {generated_password}
  Login URL: {element_web_url}

Please log in and change your password immediately.

— HiClaw
```

SMTP 通过以下环境变量配置（在 Manager 容器中）：

| 环境变量 | 说明 |
|---------|------|
| `HICLAW_SMTP_HOST` | SMTP 服务器地址 |
| `HICLAW_SMTP_PORT` | SMTP 端口 |
| `HICLAW_SMTP_USER` | SMTP 用户名 |
| `HICLAW_SMTP_PASS` | SMTP 密码 |
| `HICLAW_SMTP_FROM` | 发件人地址 |

如果未配置 SMTP 或 `spec.email` 为空，邮件发送会被跳过，不影响 Human 账号的正常创建。初始密码仍会记录在 `status.initialPassword` 中，可通过 `hiclaw get human <name>` 查看。

### 注意事项

- Human 不需要容器、MinIO 空间或 Higress 授权——只需要 Matrix 账号和 Room 权限
- 创建 L2 Human 前，目标 Team 必须已存在
- 创建 L3 Human 前，目标 Worker 必须已存在
- 修改 permissionLevel 会触发全量重算 groupAllowFrom

## Package URI

Worker 和 Team Worker 都支持通过 `spec.package` 引入自定义配置包。支持三种 URI 格式：

| 格式 | 示例 | 说明 |
|------|------|------|
| `file://` | `file://./alice.zip` | 本地文件，通过 `docker cp` 传入容器 |
| `http(s)://` | `https://example.com/worker.zip` | 远程下载 |
| `nacos://` | `nacos://instance-xxx/ns/agent-spec/worker-xxx/v1` | 从 Nacos 配置中心拉取 |

Nacos URI 格式：`nacos://{instance-id}/{namespace}/{group}/{data-id}/{version}`

### Package 目录结构

无论哪种 URI，解压后都遵循统一结构：

```
{package}/
├── manifest.json           # 包元数据（必须）
├── Dockerfile              # 自定义镜像构建（可选）
├── config/
│   ├── SOUL.md             # Worker 身份和角色定义
│   ├── AGENTS.md           # Agent 行为规则
│   ├── MEMORY.md           # 长期记忆
│   └── memory/             # 记忆文件目录
├── skills/                 # 自定义 skills
│   └── <skill-name>/
│       └── SKILL.md
└── crons/
    └── jobs.json           # 定时任务
```

### manifest.json

```json
{
  "version": "1.0",
  "source": {
    "openclaw_version": "2026.3.x",
    "hostname": "my-server",
    "os": "Ubuntu 22.04",
    "created_at": "2026-03-18T10:00:00Z"
  },
  "worker": {
    "suggested_name": "my-worker",
    "base_image": "hiclaw/worker-agent:latest",
    "apt_packages": ["ffmpeg"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

## 操作方式

### hiclaw-apply.sh — 声明式 Apply（推荐）

在宿主机上运行，将 YAML 转发到 Manager 容器内的 `hiclaw` CLI：

```bash
# 创建/更新单个资源
bash hiclaw-apply.sh -f worker.yaml

# 批量创建（YAML 中用 --- 分隔多个资源）
bash hiclaw-apply.sh -f company-setup.yaml

# 全量同步（删除 YAML 中不存在的资源）
bash hiclaw-apply.sh -f company-setup.yaml --prune

# 预览变更
bash hiclaw-apply.sh -f company-setup.yaml --dry-run
```

| 选项 | 说明 |
|------|------|
| `-f <path>` | YAML 资源文件（必填） |
| `--prune` | 删除 YAML 中不存在的资源 |
| `--dry-run` | 只显示变更，不实际执行 |
| `--yes` | 跳过删除确认 |

### hiclaw-import.sh — 命令式导入

适用于从 ZIP 包导入 Worker 的场景：

```bash
# 从本地 ZIP 导入
bash hiclaw-import.sh worker --name alice --zip ./alice.zip

# 从 URL 导入
bash hiclaw-import.sh worker --name alice --zip https://example.com/alice.zip

# 从 Nacos 导入
bash hiclaw-import.sh worker --name alice --package nacos://instance-xxx/ns/agent-spec/alice/v1

# 不带包，直接创建
bash hiclaw-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation --mcp-servers github
```

### hiclaw CLI — 容器内管理

在 Manager 容器内（或通过 `docker exec`）直接操作：

```bash
# 查看所有资源
docker exec hiclaw-manager hiclaw get workers
docker exec hiclaw-manager hiclaw get teams
docker exec hiclaw-manager hiclaw get humans

# 查看单个资源
docker exec hiclaw-manager hiclaw get worker alice

# 删除资源
docker exec hiclaw-manager hiclaw delete worker alice
docker exec hiclaw-manager hiclaw delete team alpha-team
docker exec hiclaw-manager hiclaw delete human john
```

### HTTP API — 云上管控

`hiclaw-controller` 内置 HTTP API Server（`:8090`），供云上管控平台调用：

```
POST   /api/v1/apply                    # 增量 apply（body 为 YAML）
POST   /api/v1/apply?prune=true         # 全量同步
GET    /api/v1/workers                   # 列出所有 Worker
GET    /api/v1/teams                     # 列出所有 Team
GET    /api/v1/humans                    # 列出所有 Human
DELETE /api/v1/workers/alice             # 删除指定资源
```

> **注意：** 当前单容器部署模式下，8090 端口未对宿主机暴露，仅在 Manager 容器内部可访问。后续支持 K8s 部署模式（`HICLAW_KUBE_MODE=incluster`）时，controller 将作为独立 Pod 部署，通过 Kubernetes Service 对外提供该 API 能力。

## 批量部署

用 `---` 分隔符在一个 YAML 文件中定义所有资源，一次 apply 完成整个组织的部署。

执行顺序由 Controller 自动处理：创建时 Team → Worker → Human，删除时 Human → Worker → Team。

```yaml
# company-setup.yaml

# --- 团队定义 ---
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: product-team
spec:
  description: 产品研发组
  leader:
    name: product-lead
    model: claude-sonnet-4-6
  workers:
    - name: backend-dev
      model: claude-sonnet-4-6
      skills: [github-operations, git-delegation]
      mcpServers: [github]
    - name: frontend-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
    - name: qa-engineer
      model: claude-sonnet-4-6
---
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: ops-team
spec:
  description: 运维组
  leader:
    name: ops-lead
    model: claude-sonnet-4-6
  workers:
    - name: monitor
      model: claude-sonnet-4-6
---
# --- 独立 Worker ---
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: admin-assistant
spec:
  model: claude-sonnet-4-6
---
# --- 人员配置 ---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: zhang-san
spec:
  displayName: 张三
  email: zhangsan@example.com
  permissionLevel: 2
  accessibleTeams: [product-team]
  note: 产品经理
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: li-si
spec:
  displayName: 李四
  email: lisi@example.com
  permissionLevel: 2
  accessibleTeams: [product-team]
  note: 后端开发
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: wang-wu
spec:
  displayName: 王五
  email: wangwu@example.com
  permissionLevel: 3
  accessibleWorkers: [admin-assistant]
  note: 行政人员
```

一键部署：

```bash
bash hiclaw-apply.sh -f company-setup.yaml
```

后续人员变动只需修改 YAML 并重新 apply。使用 `--prune` 可以自动清理已移除的资源：

```bash
bash hiclaw-apply.sh -f company-setup.yaml --prune
```

## Controller 架构

### 处理流程

```
入口（hiclaw-apply.sh / HTTP API / hiclaw CLI）
  ↓
YAML 写入 MinIO hiclaw-config/{kind}/{name}.yaml
  ↓
mc mirror 同步到本地文件系统（10 秒间隔）
  ↓
fsnotify 监听文件变化 → 解析 YAML → 写入 kine (SQLite)
  ↓
controller-runtime informer 感知变化 → 触发 Reconciler
  ↓
Reconciler 执行对应脚本（create-worker.sh / create-team.sh / create-human.sh）
```

### Reconciler 动作

| Reconciler | CREATE | UPDATE | DELETE |
|-----------|--------|--------|--------|
| Worker | 创建容器 + Matrix 账号 + MinIO 空间 | model 变更→重新生成配置；skills 变更→重新推送 | 停止容器 + 清理资源 |
| Team | 创建 Leader + Workers + Team Room | workers 列表变化→增删 Worker | 先删 Workers→删 Leader→删 Team Room |
| Human | 注册 Matrix 账号 + 配置权限 + 发邮件 | permissionLevel 变化→重算 groupAllowFrom | 从所有 groupAllowFrom 移除→踢出 Room |

所有资源使用 Kubernetes finalizer 模式，确保删除前完成清理。

### 两种部署模式

| 维度 | embedded（默认） | incluster（K8s） |
|------|-----------------|-----------------|
| 配置存储 | MinIO `hiclaw-config/` | K8s etcd（CRD 直接落 K8s） |
| Controller 感知 | fsnotify → kine → informer | controller-runtime 直接监听 K8s API |
| 切换方式 | `HICLAW_KUBE_MODE=embedded` | `HICLAW_KUBE_MODE=incluster` |

## 通信权限矩阵

HiClaw 通过 `openclaw.json` 中的 `groupAllowFrom` 字段控制每个 Agent 接受谁的 @mention，实现精细的通信权限控制。

| 角色 | groupAllowFrom 包含 |
|------|---------------------|
| Manager | Admin, 所有 Team Leader, 所有独立 Worker, Human L1 |
| Team Leader | Manager, Admin, 团队内所有 Worker, Human L1, 该 Team 的 Human L2 |
| Team Worker | Leader, Admin, Human L1, 该 Team 的 Human L2, 指定的 Human L3 |
| 独立 Worker | Manager, Admin, Human L1, 指定的 Human L2/L3 |

关键规则：
- Manager 不穿透 Team——只与 Leader 通信，不直接联系团队 Worker
- Team Worker 只认 Leader——groupAllowFrom 中没有 Manager
- 权限包含关系——Human L1 > L2 > L3，高级别包含低级别所有权限
- 独立 Worker 保持现有模式——直接与 Manager 通信

## 常见问题

**Q: Team 和独立 Worker 可以混用吗？**

可以。Team 和独立 Worker 共存于同一个 HiClaw 实例中。Manager 根据任务领域判断是委派给 Team Leader 还是直接分配给独立 Worker。

**Q: 修改 Human 的 permissionLevel 会怎样？**

Controller 会重新计算该 Human 在所有 Agent 上的 groupAllowFrom 配置，移除旧权限、添加新权限，并更新 Room 邀请。

**Q: Team Worker 可以同时属于多个 Team 吗？**

不可以。每个 Worker 只能属于一个 Team（或作为独立 Worker）。

**Q: 创建 Human L2 时，目标 Team 还不存在怎么办？**

Controller 会将 Human 标记为 Pending，等目标 Team 创建完成后自动补全权限配置（backfill）。

**Q: `--prune` 会删除所有不在 YAML 中的资源吗？**

是的。`--prune` 会对比 YAML 中的资源列表与当前状态，删除多余的资源。执行顺序为 Human → Worker → Team，确保依赖关系正确。建议先用 `--dry-run` 预览变更。
