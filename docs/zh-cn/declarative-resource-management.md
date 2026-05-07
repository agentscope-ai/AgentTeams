# 声明式资源管理

HiClaw 采用 Kubernetes CRD 风格的声明式 YAML 配置来管理平台资源——**Worker**、**Team**、**Human** 与 **Manager**。你只需要描述期望状态，HiClaw Controller 会自动完成创建、更新和删除。

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

### 四种资源类型

| 资源 | 说明 | 对应实体 |
|------|------|---------|
| Worker | AI Agent 工作节点 | Docker 容器 + Matrix 账号 + MinIO 空间 |
| Team | 由 Leader + N 个 Worker 组成的协作组 | 一组 Worker 容器 + Team Room |
| Human | 真人用户 | Matrix 账号 + Room 权限 |
| Manager | 协调 Agent（任务分发、Worker/Team 编排） | Manager Agent 运行时（与其它 CR 一样由 Controller 调和） |

所有资源共享统一的 API 版本：`apiVersion: hiclaw.io/v1beta1`。

**kubectl 短名**（安装 CRD 后）：`wk`（Worker）、`tm`（Team）、`hm`（Human）、`mgr`（Manager）。

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
| `spec.runtime` | string | 否 | `openclaw` | Agent 运行时：`openclaw`、`copaw` 或 `hermes` |
| `spec.image` | string | 否 | — | 自定义镜像；留空则使用 `HICLAW_WORKER_IMAGE` / `HICLAW_COPAW_WORKER_IMAGE` / `HICLAW_HERMES_WORKER_IMAGE`（默认 `hiclaw/worker-agent:latest` / `hiclaw/copaw-worker:latest` / `hiclaw-hermes-worker:latest`） |
| `spec.identity` | string | 否 | — | Worker 公开身份（OpenClaw：生成 IDENTITY.md；QwenPaw：按实现合并入 SOUL.md） |
| `spec.soul` | string | 否 | — | Worker 人格与价值观设定，用于生成 SOUL.md |
| `spec.agents` | string | 否 | — | Agent 行为规则，用于生成 AGENTS.md |
| `spec.skills` | []string | 否 | — | 内置 skills 列表，由 Manager 统一分发 |
| `spec.mcpServers` | []string | 否 | — | 内置 MCP Servers 列表，通过 Higress 网关授权 |
| `spec.package` | string | 否 | — | 自定义包 URI：`file://`、`http(s)://`、`nacos://`，或上传后由 Controller 解析的 `packages/{name}.zip` |
| `spec.expose` | []object | 否 | — | 通过 Higress 网关暴露的端口列表（见 [服务发布](#服务发布)） |
| `spec.channelPolicy` | object | 否 | — | 在默认策略之上增减群聊 @mention 与 DM 的允许/拒绝列表（详见下文「通信策略（Worker 与 Team）」） |
| `spec.state` | string | 否 | `Running` | 期望生命周期：`Running`、`Sleeping`、`Stopped`，Controller 将实际容器状态调和到此目标 |

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
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
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
| Running | 容器运行中，Agent 在线（健康时与期望 `spec.state` 一致） |
| Sleeping | 休眠（期望或实际），可唤醒 |
| Updating | 规格或底层变更处理中 |
| Stopped | 已调和到停止期望 |
| Failed | 创建或运行失败，查看 `status.message` |

**状态字段（节选）：** `observedGeneration`、`matrixUserID`、`roomID`、`containerState`、`lastHeartbeat`、`message`、`exposedPorts`（暴露端口及域名）。

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
    heartbeat:
      enabled: true
      every: 30m
    workerIdleTimeout: 12h
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
      mcpServers:
        - name: github
          url: https://gateway.example.com/mcp-servers/github/mcp
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
| `spec.peerMentions` | bool | 否 | 为 `true`（默认）时，团队 Worker 可在群房间中互相 @mention |
| `spec.channelPolicy` | object | 否 | 团队级群聊/DM 允许与拒绝列表覆盖（字段形状与 Worker 的 `channelPolicy` 相同） |
| `spec.admin` | object | 否 | 团队专属人类管理员（须含 `name`；可选 `matrixUserId`）。省略则使用全局 Admin |
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
| `leader.heartbeat.enabled` | bool | 否 | 是否让 Team Leader 利用 heartbeat 轮询做周期检查 |
| `leader.heartbeat.every` | string | 否 | 注入到 Team Leader 工作空间中的 heartbeat 周期间隔提示 |
| `leader.workerIdleTimeout` | string | 否 | Team Leader 判断 team 内 worker 是否可以休眠时使用的空闲超时 |
| `leader.state` | string | 否 | `Running`（默认）、`Sleeping`、`Stopped` — Leader 容器的期望生命周期 |
| `leader.channelPolicy` | object | 否 | Leader 专属的通信策略覆盖 |

**Worker 字段（与独立 Worker 的 spec 一致）：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `workers[].name` | string | 是 | Worker 名称 |
| `workers[].model` | string | 否 | LLM 模型 |
| `workers[].runtime` | string | 否 | Agent 运行时（`openclaw`、`copaw` 或 `hermes`） |
| `workers[].image` | string | 否 | 自定义 Docker 镜像 |
| `workers[].identity` | string | 否 | Worker 公开身份信息（生成 IDENTITY.md） |
| `workers[].soul` | string | 否 | Worker 人格与价值观设定（生成 SOUL.md） |
| `workers[].agents` | string | 否 | 自定义行为规则（追加在内置 AGENTS.md 之后） |
| `workers[].skills` | []string | 否 | 内置 skills |
| `workers[].mcpServers` | []string | 否 | 内置 MCP Servers |
| `workers[].package` | string | 否 | 自定义包 URI |
| `workers[].expose` | []object | 否 | 通过 Higress 网关暴露的端口列表（见 [服务发布](#服务发布)） |
| `workers[].channelPolicy` | object | 否 | 该团队 Worker 的通信策略覆盖 |
| `workers[].state` | string | 否 | `Running`（默认）、`Sleeping`、`Stopped` — 该成员容器的期望生命周期 |

### Team Leader 的特殊性

Team Leader 本质上是一个 Worker 容器，但有以下区别：

- 使用 `team-leader-agent` 模板（SOUL.md.tmpl + AGENTS.md + HEARTBEAT.md）
- 拥有 `team-task-management` skill（管理 team-state.json、查找可用 Worker）
- 拥有 `worker-lifecycle` skill，用于执行 `hiclaw worker status|wake|sleep|ensure-ready`
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
- 团队上下文段自动注入团队名称、成员列表、协调者信息，以及 heartbeat 周期和 worker idle timeout
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
| Active | Leader 与 Worker 已成功调和 |
| Degraded | 部分 Worker 未就绪或不可用；Leader 可能仍在运行 |
| Failed | 调和失败，查看 `status.message` |

**状态字段：** `teamRoomID`、`leaderDMRoomID`、`leaderReady`、`readyWorkers`、`totalWorkers`、`workerExposedPorts`（按 Worker 名索引的暴露端口信息）。

### Team Admin

可以为 Team 指定一个独立的管理员（Team Admin），替代全局 Admin 参与团队管理：

```yaml
spec:
  admin:
    name: pm-zhang
    matrixUserId: "@pm-zhang:domain"
```

如果不指定，默认使用全局 Admin。Team Admin 会被邀请到 Team Room 和 Leader DM，可以直接与 Leader 沟通团队事务。

## Manager

**Manager** 资源描述 HiClaw 的 Manager Agent：接收 Admin 指令并编排 Worker 与 Team。与其它资源同属 `hiclaw.io/v1beta1`，由 `hiclaw-controller` 调和（镜像、SOUL/AGENTS、skills、MCP 授权、可选 package、期望 `state` 等）。

### 基础配置

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Manager
metadata:
  name: default
spec:
  model: qwen3.5-plus
  runtime: openclaw
  soul: |
    # Manager — 以协调为主
  agents: |
    # 可选 AGENTS.md 覆盖
  skills:
    - worker-management
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
  config:
    heartbeatInterval: 15m
    workerIdleTimeout: 720m
    notifyChannel: admin-dm
  # state: Running   # 可选：Running | Sleeping | Stopped
```

### 字段说明

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `metadata.name` | string | 是 | — | Manager 资源名（主实例常为 `default`） |
| `spec.model` | string | 是 | — | LLM 模型 ID |
| `spec.runtime` | string | 否 | `openclaw` | `openclaw` 或 `copaw`（**不支持**将 Hermes 作为 Manager 运行时） |
| `spec.image` | string | 否 | — | 自定义 Manager 镜像；留空则用部署默认值 |
| `spec.soul` | string | 否 | — | 自定义 SOUL.md |
| `spec.agents` | string | 否 | — | 自定义 AGENTS.md |
| `spec.skills` | []string | 否 | — | 启用的按需 skills |
| `spec.mcpServers` | []string | 否 | — | 经网关授权的 MCP |
| `spec.package` | string | 否 | — | 包 URI（`file://`、`http(s)://`、`nacos://`） |
| `spec.state` | string | 否 | `Running` | 期望生命周期：`Running`、`Sleeping`、`Stopped` |
| `spec.config.heartbeatInterval` | string | 否 | — | 心跳检查间隔（如 `15m`） |
| `spec.config.workerIdleTimeout` | string | 否 | — | 空闲自动休眠前等待时间（如 `720m`） |
| `spec.config.notifyChannel` | string | 否 | — | 通知渠道（如 `admin-dm`） |

### Manager 状态

| Phase | 含义 |
|-------|------|
| Pending | 等待首次成功调和 |
| Running | Manager Agent 正常 |
| Sleeping / Stopped | 对应期望生命周期 |
| Updating | 规格或发布中 |
| Failed | 错误，见 `status.message` |

**其它状态字段：** `observedGeneration`、`matrixUserID`、`roomID`、`containerState`、`version`。

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

Worker 和 Team Worker 都支持通过 `spec.package` 引入自定义配置包。支持的 URI 形式包括：

| 格式 | 示例 | 说明 |
|------|------|------|
| `file://` | `file://./alice.zip` | 本地文件，通过 `docker cp` 传入容器 |
| `http(s)://` | `https://example.com/worker.zip` | 远程下载 |
| `nacos://` | `nacos://host:8848/ns/worker-xxx/v1` | 从 Nacos 拉取 |
| （上传） | `packages/<name>.zip` | `POST /api/v1/packages` 上传 ZIP 后，Controller 返回可在 `spec.package` 中引用的 `packages/` 路径 |

Nacos URI 格式：`nacos://[user:pass@]host:port/{namespace}/{agentspec-name}[/{version}|/label:{label}]`

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
    "model": "qwen3.5-plus",
    "runtime": "openclaw",
    "base_image": "hiclaw/worker-agent:latest",
    "apt_packages": ["ffmpeg"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

`worker.runtime`（`openclaw`、`copaw` 或 `hermes`）会被 `hiclaw apply worker --zip` 读取，
显式 `--runtime` 优先级更高。

## 操作方式

### hiclaw-apply.sh — 声明式 Apply（推荐）

在宿主机上将 YAML 复制进 Manager 容器并执行 `hiclaw apply -f …`：

```bash
# 按文档顺序逐个创建或更新资源
bash install/hiclaw-apply.sh -f worker.yaml

# 多文档 YAML（--- 分隔）
bash install/hiclaw-apply.sh -f company-setup.yaml
```

| 选项 | 说明 |
|------|------|
| `-f <path>` | YAML 文件（必填）；可多次指定 `-f` |

`hiclaw apply -f` **按文件中 YAML 文档顺序**依次调用 REST API（如 `Worker`→`/api/v1/workers`，`Team`→`/api/v1/teams`，`Human`→`/api/v1/humans`，`Manager`→`/api/v1/managers`）。依赖关系需自行排序（例如先定义 Team，再定义引用 `accessibleTeams` 的 Human）。**当前 CLI 未实现 `--prune` 与 `--dry-run`**；删除多余资源请使用 `hiclaw delete …` 或 REST API。

### hiclaw-import.sh — 命令式导入

适用于从 ZIP 包导入 Worker 的场景：

```bash
# 从本地 ZIP 导入
bash install/hiclaw-import.sh worker --name alice --zip ./alice.zip

# 从 URL 导入
bash install/hiclaw-import.sh worker --name alice --zip https://example.com/alice.zip

# 从 Nacos 导入
bash install/hiclaw-import.sh worker --name alice --package nacos://host:8848/ns/alice/v1
bash install/hiclaw-import.sh worker --name alice --package nacos://host:8848/ns/alice/label:latest

# 不带包，直接创建
bash install/hiclaw-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation --mcp-servers github
```

### hiclaw CLI — 容器内管理

在 Manager 容器内（或通过 `docker exec`）直接操作：

```bash
# 查看所有资源
docker exec hiclaw-manager hiclaw get workers
docker exec hiclaw-manager hiclaw get teams
docker exec hiclaw-manager hiclaw get humans
docker exec hiclaw-manager hiclaw get managers

# 查看单个资源
docker exec hiclaw-manager hiclaw get worker alice

# 删除资源
docker exec hiclaw-manager hiclaw delete worker alice
docker exec hiclaw-manager hiclaw delete team alpha-team
docker exec hiclaw-manager hiclaw delete human john
docker exec hiclaw-manager hiclaw delete manager default
```

### HTTP API — 云上管控

`hiclaw-controller` 对外提供 REST API（默认 `:8090`），供 `hiclaw` CLI 与其它自动化使用。示例：

```
GET    /api/v1/workers
POST   /api/v1/workers
PUT    /api/v1/workers/{name}
DELETE /api/v1/workers/{name}

GET    /api/v1/managers
POST   /api/v1/managers
PUT    /api/v1/managers/{name}
DELETE /api/v1/managers/{name}
```

> **注意：** 常见嵌入式部署中，8090 在 Manager 容器内可用（`localhost:8090`）。Kubernetes（`HICLAW_KUBE_MODE=incluster`）下可通过 Service 暴露 Controller。

## 批量部署

用 `---` 分隔符在一个 YAML 文件中定义多个资源。**`hiclaw apply -f` 按文档出现顺序依次 apply**，不会按资源类型自动排序。例如应先写 Team，再写引用 `accessibleTeams` 的 Human；先创建独立 Worker，再写引用 `accessibleWorkers` 的 L3 Human。

删除不会自动排序：按需执行 `hiclaw delete`（注意 Human 与 Team 等依赖关系）。

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
      mcpServers:
        - name: github
          url: https://gateway.example.com/mcp-servers/github/mcp
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
bash install/hiclaw-apply.sh -f company-setup.yaml
```

后续变更修改 YAML 后重新 apply。删除资源请使用 `hiclaw delete <kind> <name>`（或 REST API）。

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
| Manager | 部署/更新 Manager Agent | model/skills/package/state 等变更→调和 | 按后端实现回收 Manager 相关资源 |

所有资源使用 Kubernetes finalizer 模式，确保删除前完成清理。

## 服务发布

Worker 可以将容器内运行的 HTTP 服务通过 Higress 网关暴露到外部。在 Worker 配置中添加 `spec.expose` 即可发布容器端口——Controller 会自动创建所需的 Higress 域名、DNS 服务来源和路由。

### 工作原理

每个暴露的端口会自动生成一个域名：

```
worker-{name}-{port}-local.hiclaw.io
```

例如，worker `alice` 暴露 8080 端口后，可通过 `worker-alice-8080-local.hiclaw.io` 访问。

Controller 为每个暴露端口创建三个 Higress 资源：
1. **域名**：`worker-{name}-{port}-local.hiclaw.io`
2. **DNS 服务来源**：通过网络别名 `{name}.local` 指向 worker 容器
3. **路由**：将该域名的所有请求转发到 worker 的对应端口

当 expose 配置被移除或 Worker 被删除时，所有关联的 Higress 资源会自动清理。

### 配置方式

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: qwen3.5-plus
  expose:
    - port: 8080
    - port: 3000
```

**expose 字段说明：**

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `expose[].port` | int | 是 | — | 要暴露的容器端口 |
| `expose[].protocol` | string | 否 | `http` | 协议：`http` 或 `grpc` |

### Team Worker 支持

Team Worker 同样支持 `expose`：

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: dev-team
spec:
  leader:
    name: lead
    model: qwen3.5-plus
  workers:
    - name: backend
      model: qwen3.5-plus
      expose:
        - port: 8080
    - name: frontend
      model: qwen3.5-plus
      expose:
        - port: 3000
```

### CLI 用法

```bash
# 通过 CLI 参数暴露端口
hiclaw apply worker --name alice --model qwen3.5-plus --expose 8080,3000

# 取消暴露（不带 --expose 重新 apply）
hiclaw apply worker --name alice --model qwen3.5-plus
```

### 使用场景

- **Web 应用预览**：Worker 开发了一个 Web 应用，暴露给 Admin 或其他团队成员预览
- **API 服务**：Worker 运行后端 API，供其他 Worker 或外部系统调用
- **开发服务器**：暴露开发服务器，便于开发过程中实时测试

### 注意事项

- Worker 容器必须处于运行状态，且服务已在指定端口监听，才能被访问
- 域名为自动生成，暂不支持自定义域名
- 暴露的路由未配置认证（网络内公开访问）
- 从 `spec.expose` 中移除端口并重新 apply，会自动清理对应的 Higress 资源

### 两种部署模式

| 维度 | embedded（默认） | incluster（K8s） |
|------|-----------------|-----------------|
| 配置存储 | MinIO `hiclaw-config/` | K8s etcd（CRD 直接落 K8s） |
| Controller 感知 | fsnotify → kine → informer | controller-runtime 直接监听 K8s API |
| 切换方式 | `HICLAW_KUBE_MODE=embedded` | `HICLAW_KUBE_MODE=incluster` |

## 通信策略（Worker 与 Team）

`channelPolicy` 在生成 Agent 配置时，在**默认允许策略之上**做增减（群聊 @mention 与 DM）。不是替换整套默认策略。

| 字段 | 作用 |
|------|------|
| `groupAllowExtra` | 额外允许参与群聊 @mention 的 Matrix 用户 ID（或可由 Controller 解析的短用户名） |
| `groupDenyExtra` | 群聊 @mention 拒绝列表（拒绝优先于允许） |
| `dmAllowExtra` | 额外允许 DM 的 ID |
| `dmDenyExtra` | DM 拒绝列表 |

可在独立 Worker 上设置 `spec.channelPolicy`，或在 Team 上设置 `spec.channelPolicy` / `spec.leader.channelPolicy` / `workers[].channelPolicy` 做成员级覆盖。

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

**Q: 声明式 apply 支持 `--prune` 吗？**

当前 `hiclaw apply` CLI **未实现** `--prune`。请用 `hiclaw get …` 查看后自行 `delete`，或通过 REST API 自动化。
