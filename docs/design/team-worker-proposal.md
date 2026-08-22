# HiClaw Team 架构设计文档

## 1. 背景与目标

### 1.1 现状分析

当前 HiClaw 采用 Manager-Workers 两层扁平架构：

```
Human (Admin)
  └─ Manager Agent (唯一入口)
       ├─ Worker A (OpenClaw/CoPaw)
       ├─ Worker B
       └─ Worker C
```

所有任务必须经由 Manager 分配，所有人类用户必须与 Manager 对话。这在企业场景中存在瓶颈：

- Manager 成为单点瓶颈，所有指令都需经过它
- 无法支持多人协作，只有 Admin 能有效指挥
- Worker 之间无法自组织协调
- 无法映射企业真实团队结构

### 1.2 目标

引入 Team 概念和 Human 接入机制，实现三层组织架构：

1. Manager 只管人力和顶层任务下发，不介入 Team 内部协调
2. Team Leader 作为特殊 Worker，自主管理团队内任务分解和分配
3. 真人用户按权限级别接入，与对应角色直接对话
4. 统一声明式 YAML 配置，支持 `hiclaw apply` 一键管理所有资源
5. 基于 kine + client-go 实现 K8s 风格的 controller reconcile，未来可无缝切换到原生 K8s

## 2. 核心概念

### 2.1 组织架构

```
Admin (人类)
  │
  ├── Manager (AI, 只管人力 + 顶层任务下发)
  │     ├── Team Leader A (特殊 Worker, 管团队内任务调度)
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C (独立 Worker, 不属于任何 Team, 保持现有模式)
  │
  └── Human Users (真人, 按权限级别接入)
        ├── Level 1: 等同 Admin，可与所有角色对话
        ├── Level 2: 可与指定 Team 的 Leader + Workers 对话
        └── Level 3: 只能与指定 Workers 对话
```

### 2.2 角色定义

| 角色 | 本质 | 职责 | 通信范围 |
|------|------|------|---------|
| Manager | AI Agent（管理容器） | 人力管理、顶层任务下发、Team/Human 生命周期 | Admin、Team Leader、独立 Worker |
| Team Leader | 特殊 Worker 容器 | 接收 Manager 任务、分解为子任务、分配给团队 Worker、汇总结果 | Manager、团队内 Worker |
| Team Worker | 普通 Worker 容器 | 执行子任务、向 Leader 汇报 | Team Leader |
| 独立 Worker | 普通 Worker 容器 | 执行 Manager 直接分配的任务 | Manager |
| Human | 真人（Matrix 账号） | 按权限级别与对应角色协作 | 取决于 permissionLevel |

### 2.3 关键设计原则

- **Team Leader 是 Worker**：同样的容器、同样的运行时，只是 SOUL 和 skills 不同
- **Team 内 Worker 只认 Leader**：groupAllowFrom 配置为 `[Leader, Admin]`，不含 Manager
- **Manager 不穿透 Team**：只与 Leader 通信，不直接联系团队 Worker
- **权限包含关系**：Human Level 1 > Level 2 > Level 3，高级别包含低级别所有权限
- **向后兼容**：独立 Worker（不属于 Team）保持现有模式不变

## 3. 与 ClawTeam 的对比

| 维度 | ClawTeam | HiClaw Team 方案 |
|------|----------|-----------------|
| 定位 | 开发者工具，本地 CLI 驱动 | 企业级数字员工平台，多人多角色协作 |
| 团队定义 | TOML 模板，一次性定义 | K8s CRD 风格 YAML，声明式 apply + reconcile |
| Agent 运行时 | tmux/subprocess 本地进程 | Docker 容器 / 云端部署（local/remote/cloud） |
| Leader 区分 | `is_leader` 布尔标志，纯 prompt 约定 | 独立 `team-leader-agent` 模板 + 专用 skills + 独立 team-state.json |
| 任务管理 | 文件系统 TaskStore + 依赖 DAG | MinIO 共享存储 + state.json/team-state.json + manage-state.sh 脚本 |
| 通信 | 文件 inbox + 可选 ZeroMQ P2P | Matrix 协议（人类可实时旁观）+ @mention 唤醒 |
| 真人接入 | 不支持 | Human 资源 + 3 级权限 + 邮件通知 |
| 状态管理 | JSON 文件 | kine (SQLite) + client-go controller reconcile |
| 云上部署 | 不支持 | hiclaw-controller HTTP API + 阿里云管控对接 |

借鉴了 ClawTeam 的核心思想：
- 团队模板一键拉起多 Agent
- Leader 负责任务分解和分配
- Worker 执行后向 Leader 汇报

在此基础上做了企业级增强：声明式配置、真人接入、权限控制、云上管控。

## 4. Matrix Room 拓扑

### 4.1 现有拓扑（每个独立 Worker）

```
Worker Room: Manager + Admin + Worker    (3-party, power: Manager=100, Admin=100, Worker=0)
```

### 4.2 新增拓扑（每个 Team）

```
Leader Room:  Manager + Admin + Leader       ← Manager 与 Leader 沟通的通道
Team Room:    Leader + Admin + W1 + W2 + ... ← Leader 与团队 Worker 沟通的通道（Manager 不在）
Worker Room:  Leader + Admin + Worker        ← Leader 与单个 Worker 的私聊（Leader 替代 Manager）
```

关键设计：Team Room 不包含 Manager，实现了委派边界——Manager 只通过 Leader Room 与 Leader 沟通。

### 4.3 @mention 权限矩阵

| 发送方 → 接收方 | Manager            | Team Leader                    | Team Worker                    | 独立 Worker        |
|------------------|--------------------|--------------------------------|--------------------------------|--------------------|
| Manager          | -                  | ✅ via Leader Room             | ❌                             | ✅ via Worker Room |
| Team Leader      | ✅ via Leader Room | -                              | ✅ via Team Room / Worker Room | ❌                 |
| Team Worker      | ❌                 | ✅ via Team Room / Worker Room | ❌ (默认)                      | ❌                 |
| Admin            | ✅                 | ✅                             | ✅                             | ✅                 |
| Human L1         | ✅                 | ✅ (全部)                      | ✅ (全部)                      | ✅ (全部)          |
| Human L2         | ❌                 | ✅ (指定 team)                 | ✅ (指定 team 下的 + 指定独立) | ✅ (指定独立)      |
| Human L3         | ❌                 | ❌                             | ✅ (指定 worker)               | ✅ (指定 worker)   |

### 4.4 groupAllowFrom 配置

通过 openclaw.json 的 `groupAllowFrom` 字段控制每个 Agent 接受谁的 @mention：

| 角色 | groupAllowFrom |
|------|---------------|
| Manager | `[Admin, 所有 Team Leader, 所有独立 Worker, Human L1]` |
| Team Leader | `[Manager, Admin, 所有团队 Worker, Human L1, 该 Team 的 Human L2]` |
| Team Worker | `[Leader, Admin, Human L1, 该 Team 的 Human L2, 指定的 Human L3]` |
| 独立 Worker | `[Manager, Admin, Human L1, 指定的 Human L2/L3]` |

## 5. 声明式 YAML 配置（CRD 风格）

所有资源管理统一使用 YAML 声明式配置，类似 K8s CRD。

### 5.1 Worker

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  runtime: openclaw                                    # openclaw | copaw
  image: hiclaw/worker-agent:latest
  skills:                                              # HiClaw 内置 skills（由 Manager 统一管理和分发）
    - github-operations
    - git-delegation
  mcpServers:                                          # HiClaw 内置 MCP Servers（通过 Higress 网关授权）
    - github
  package: file://./alice-worker.zip                   # 可选，含自定义 SOUL.md、自定义 skills、Dockerfile 等
```

`skills` 和 `mcpServers` 指的是 HiClaw 平台内置的能力，由 Manager 通过 `push-worker-skills.sh` 分发、通过 Higress 网关授权。`package` 中可以包含自定义的 SOUL.md、自定义 skills 目录、Dockerfile 等，会在创建时合并到 Worker 的 MinIO 空间。

### 5.2 Team

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
    package: file://./leader.zip
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
      skills: [github-operations]                      # 内置 skills
      mcpServers: [github]                             # 内置 MCP Servers
      package: file://./alpha-dev.zip                  # 自定义 SOUL.md + 自定义 skills
    - name: alpha-qa
      model: gpt-5-mini
      package: nacos://xxxx/workers/alpha-qa.zip       # 从 Nacos 配置中心拉取
```

### 5.3 Human

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: 张三
  email: john@example.com          # 注册完成后自动发送账号密码到此邮箱
  permissionLevel: 2               # 1=等同Admin, 2=Team级, 3=Worker级
  accessibleTeams: [alpha-team]    # L2: 可与该 team 的 leader + 所有 worker 对话
  accessibleWorkers: [standalone-worker-1]
  note: 前端负责人
```

### 5.4 多资源批量导入

用 `---` 分隔多个资源，一次 apply 完成 Team 创建 + Human 配置：

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: 张三
  email: john@example.com
  permissionLevel: 2
  accessibleTeams: [alpha-team]
```

### 5.5 Package URI 格式

`spec.package` 支持三种 URI 格式：

| 格式 | 示例 | 处理方式 |
|------|------|---------|
| `file://` | `file://./alice.zip` | 容器外薄壳 `docker cp` 到 Manager 容器，controller 从本地读取 |
| `http(s)://` | `http://market.hiclaw.io/workers/alice.zip` | controller 内部下载到 `/tmp/import/` |
| `nacos://` | `nacos://instance-xxx/namespace-xxx/agent-spec/worker-xxx/v1` | controller 通过 Nacos Open API 拉取配置，解析为 package 目录结构 |

Nacos URI 格式：`nacos://{instance-id}/{namespace}/{group}/{data-id}/{version}`

- `instance-id`：Nacos 实例标识（对应 `HICLAW_NACOS_*` 环境变量中的连接信息）
- `namespace`：Nacos 命名空间
- `group`：配置分组（如 `agent-spec`）
- `data-id`：配置 ID（如 `worker-xxx`）
- `version`：版本标签（如 `v1`，可选，默认最新）

Package 目录结构约定（无论哪种 URI，解压/拉取后都遵循同一结构）：

```
{package}/
├── config/
│   ├── SOUL.md             # Worker 身份和角色（必须）
│   ├── AGENTS.md           # Agent 行为规则（可选）
│   └── ...
├── skills/                 # 自定义 skills（可选，与内置 skills 合并）
│   └── <skill-name>/
│       └── SKILL.md
├── crons/                  # 定时任务（可选）
│   └── jobs.json
└── Dockerfile              # 自定义镜像构建（可选）
```

内置 skills（`spec.skills` 字段）和自定义 skills（`package` 中的 `skills/` 目录）会合并推送到 Worker 的 MinIO 空间，互不冲突。

## 6. 数据结构

### 6.1 `teams-registry.json`（Manager 本地）

```json
{
  "version": 1,
  "updated_at": "ISO",
  "teams": {
    "alpha-team": {
      "leader": "alpha-lead",
      "workers": ["alpha-dev", "alpha-qa"],
      "team_room_id": "!xxx:domain",
      "created_at": "ISO"
    }
  }
}
```

### 6.2 `humans-registry.json`（Manager 本地）

```json
{
  "version": 1,
  "updated_at": "ISO",
  "humans": {
    "john": {
      "matrix_user_id": "@john:domain",
      "display_name": "John Doe",
      "permission_level": 2,
      "accessible_teams": ["alpha-team"],
      "accessible_workers": ["standalone-worker-1"],
      "rooms": ["!room1:..."],
      "created_at": "ISO",
      "note": "前端负责人"
    }
  }
}
```

## 7. 任务流转

### 7.1 Manager → Team Leader → Team Worker

```
Admin 下发任务 → Manager
  ↓
Manager 判断：匹配某个 Team 的领域？
  ├── 是 → 创建任务，assign_to=Leader，在 Leader Room @mention Leader
  │        manage-state.sh --action add-finite --delegated-to-team alpha-team
  └── 否 → 走现有流程，assign_to=独立Worker
  ↓
Leader 收到任务
  ├── 分解为子任务 (sub-task-01, sub-task-02...)
  ├── find-team-worker.sh 查看团队 Worker 可用性
  ├── 在 Team Room @mention Worker 分配子任务
  └── manage-team-state.sh --action add-finite 记录到 team-state.json
  ↓
Team Worker 执行，完成后 @mention Leader（不是 Manager）
  ↓
Leader 汇总结果，写 result.md，@mention Manager 报告完成
  ↓
Manager 更新 state.json，通知 Admin
```

### 7.2 Heartbeat 对团队任务的处理

Manager 心跳（每 15 分钟）新增 Step 2b：

- 对 `delegated_to_team` 不为空的任务，只 @mention Team Leader 询问进度
- 不直接联系团队 Worker
- 信任 Leader 管理内部协调

Team Leader 也有自己的简化版心跳：
- 检查 team-state.json 中的活跃子任务
- 跟进未响应的 Worker
- 向 Manager 上报异常（Worker 无响应、任务阻塞等）

### 7.3 Human 参与流程

真人通过 Matrix (Element Web) 登录后，在被授权的 Room 中直接与 Agent 对话：

```
真人后端开发 @li                 backend-dev Agent (Team Worker)
       │                              │
       │  "登录接口用 JWT 还是 Session？" │
       │  ────────────────────────>    │
       │  (Worker Room, @mention)      │
       │                              │
       │  "建议用 JWT，原因是..."       │
       │  <────────────────────────    │
```

## 8. 统一导入与同步架构（Go + kine + MinIO 驱动）

### 8.1 架构分层

MinIO `hiclaw-config/` 目录是声明式配置的 single source of truth。hiclaw-controller 通过 file-watch 感知变化，同步到 kine SQLite，触发 reconcile。

```
┌─────────────────────────────────────────────────────────────────┐
│  入口层                                                         │
│                                                                 │
│  容器外: hiclaw-apply.sh -f resource.yaml                       │
│    → docker exec hiclaw-manager hiclaw apply -f resource.yaml   │
│                                                                 │
│  容器内: hiclaw apply -f resource.yaml                          │
│    → 解析 YAML → mc cp 到 MinIO hiclaw-config/{kind}/{name}.yaml│
│                                                                 │
│  云上: POST /api/v1/apply (HTTP body → mc cp 到 MinIO)          │
├─────────────────────────────────────────────────────────────────┤
│  存储层 (MinIO — single source of truth, embedded 模式)          │
│                                                                 │
│  hiclaw-storage/hiclaw-config/                                  │
│  ├── workers/{name}.yaml                                        │
│  ├── teams/{name}.yaml                                          │
│  └── humans/{name}.yaml                                         │
├─────────────────────────────────────────────────────────────────┤
│  同步层 (mc mirror, 10 秒间隔)                                   │
│                                                                 │
│  MinIO hiclaw-config/ → /root/hiclaw-fs/hiclaw-config/          │
├─────────────────────────────────────────────────────────────────┤
│  Controller 层 (hiclaw-controller)                               │
│                                                                 │
│  fsnotify 监听本地目录 → 解析 YAML → 写入 kine (SQLite)          │
│  → controller-runtime informer → Reconciler (exec 现有脚本)      │
└─────────────────────────────────────────────────────────────────┘
```

### 8.2 两种部署模式

| 维度 | embedded（非 K8s，默认） | incluster（K8s） |
|------|------------------------|-----------------|
| 配置存储 | MinIO `hiclaw-config/` | K8s etcd（CRD 直接落 K8s，不经过 MinIO） |
| `hiclaw apply` | mc cp 到 MinIO → mirror → fsnotify → kine → reconcile | ⚠️ 待实现（CLI 层尚未对接） |
| `hiclaw get` | mc ls/cat MinIO 文件 | ⚠️ 待实现 |
| `hiclaw delete` | mc rm MinIO 文件 | ⚠️ 待实现 |
| Controller 感知 | fsnotify 监听本地目录 | controller-runtime informer 监听 API（已支持） |
| MinIO 角色 | 配置存储 + 运行时数据 | 仅运行时数据（agents/、shared/） |

切换方式：环境变量 `HICLAW_KUBE_MODE=embedded|incluster`

### 8.3 kine 集成

使用 [kine](https://github.com/k3s-io/kine) 提供 etcd 兼容 API，底层默认 SQLite：

```go
etcdCfg, err := endpoint.Listen(ctx, endpoint.Config{
    Listener: "127.0.0.1:2379",
    Endpoint: "sqlite:///data/hiclaw-controller/hiclaw.db",
})
```

### 8.4 Reconciler 执行动作

| Reconciler | CREATE | UPDATE | DELETE |
|-----------|--------|--------|--------|
| Worker | `create-worker.sh --name X --model Y [--role R --team T --team-leader L]` | model→重新生成 config, skills→push-worker-skills.sh | 停止容器 + 清理 |
| Team | `create-team.sh --name T --leader L --workers w1,w2` | workers 列表变化→增删 worker | 先删 Workers→删 Leader→删 Team Room |
| Human | `create-human.sh --matrix-id M --level L --email E` | permissionLevel 变化→重算 groupAllowFrom | 从所有 groupAllowFrom 移除→踢出 Room |

所有资源使用 K8s finalizer 模式确保删除前完成清理。

### 8.5 `apply --prune` 全量同步

```
声明式同步 = 期望状态(YAML) - 当前状态(MinIO/kine) = 执行动作(create/update/delete)
```

embedded 模式：对比 YAML 中的资源列表 vs MinIO `hiclaw-config/` 目录中的文件列表，mc rm 多余文件。
incluster 模式：client-go apply + prune。

执行顺序：创建 Team → Worker → Human，删除 Human → Worker → Team。

### 8.6 云上管控

`hiclaw-controller` 内置 HTTP API Server（`:8090`）：

```
POST   /api/v1/apply                    # 增量 apply
POST   /api/v1/apply?prune=true         # 全量同步
GET    /api/v1/workers|teams|humans     # 查看资源
DELETE /api/v1/workers/alice            # 删除资源
```

### 8.7 Go 项目结构

```
hiclaw-controller/
├── go.mod
├── cmd/
│   ├── controller/main.go              # hiclaw-controller 常驻进程
│   └── hiclaw/main.go                  # hiclaw CLI 工具（apply/get/delete）
├── api/v1beta1/
│   ├── types.go                        # CRD 类型 (Worker, Team, Human)
│   └── register.go                     # GVR 注册
├── internal/
│   ├── apiserver/embedded.go           # 嵌入式 kube-apiserver 初始化
│   ├── controller/                     # WorkerReconciler, TeamReconciler, HumanReconciler
│   ├── executor/
│   │   ├── shell.go                    # exec 调用现有 bash 脚本
│   │   ├── package.go                  # file://, http(s)://, nacos:// 包解析
│   │   └── nacos_agentspec.go          # Nacos Agent Spec 协议解析
│   ├── watcher/
│   │   └── file_watcher.go            # fsnotify 监听 + YAML→kine 同步（embedded 模式）
│   ├── store/kine.go                   # kine 初始化 + SQLite
│   ├── server/http.go                  # HTTP API（云管控，写入 MinIO 或 K8s API）
│   └── mail/smtp.go                    # SMTP 邮件发送
├── Dockerfile
└── Makefile
```

## 9. 实现细节

### 9.1 已修改的现有文件

| 文件 | 修改内容 |
|------|---------|
| `create-worker.sh` | 新增 `--role`/`--team`/`--team-leader`；Room 创建支持 Leader 替代 Manager；groupAllowFrom 按角色配置；registry 写入 role/team_id |
| `generate-worker-config.sh` | 第 5 个参数传入 team-leader 名，覆盖 groupAllowFrom 为 `[Leader, Admin]` |
| `find-worker.sh` | 新增 `--team` 过滤；输出增加 `worker_role`/`team_id` |
| `manage-state.sh` | `add-finite` 支持 `--delegated-to-team` |
| `hiclaw-import.sh` | 从 856 行精简为 90 行薄壳，`--zip` 和 `-f` 都转发到容器内 `hiclaw` CLI |
| `start-mc-mirror.sh` | 新增 hiclaw-config 路径的 10 秒间隔 mirror loop |
| `SOUL.md` (Manager) | 职责增加 Team/Human 管理 |
| `HEARTBEAT.md` | 新增 Step 2b 检查团队委派任务 |
| `identity-and-contacts.md` | 新增 Team Leader 和 Human L1/L2/L3 身份识别 |
| `worker-selection.md` | 新增 Team 匹配优先逻辑 |

### 9.2 新增的 Manager Skills

#### team-management

```
manager/agent/skills/team-management/
├── SKILL.md
├── references/
│   ├── create-team.md
│   ├── team-lifecycle.md
│   └── team-task-delegation.md
└── scripts/
    ├── create-team.sh
    └── manage-teams-registry.sh
```

`create-team.sh` 流程：调用 `create-worker.sh --role team_leader` 创建 Leader → 逐个调用 `create-worker.sh --role worker --team-leader` 创建 Worker → 创建 Team Room → 更新 teams-registry.json → 更新 Leader groupAllowFrom。

#### human-management

```
manager/agent/skills/human-management/
├── SKILL.md
├── references/
│   └── create-human.md
└── scripts/
    ├── create-human.sh
    └── manage-humans-registry.sh
```

`create-human.sh` 流程：注册 Matrix 账号 → 按 permissionLevel 配置 groupAllowFrom（L1=全部, L2=指定 Team+Workers, L3=指定 Workers）→ 邀请进 Room → 更新 humans-registry.json → 发送欢迎邮件。

### 9.3 Team Leader Agent 模板

```
manager/agent/team-leader-agent/
├── SOUL.md.tmpl                        # 变量：${TEAM_LEADER_NAME}, ${TEAM_NAME}, ${TEAM_WORKERS}
├── AGENTS.md                           # 双重角色说明
├── HEARTBEAT.md                        # 简化版心跳
└── skills/team-task-management/
    ├── SKILL.md
    ├── references/                     # finite-tasks.md, worker-selection.md, state-management.md
    └── scripts/
        ├── manage-team-state.sh        # 操作 team-state.json
        └── find-team-worker.sh         # 查团队内 Worker 可用性
```

拥有 skills：team-task-management、file-sync、task-progress、mcporter。
不拥有：worker-management、mcp-server-management、model-switch 等（Manager 独占）。

## 10. 企业部署场景

```
公司 HiClaw 实例
│
├─ Manager Agent (IT 管理员操作)
│
├─ Team: 产品研发组
│   ├─ Leader (alpha-lead) ←→ 产品经理张三 (Human L2)
│   ├─ backend-dev ←→ 后端开发李四 (Human L2)
│   ├─ frontend-dev ←→ 前端开发赵六 (Human L2)
│   └─ qa-engineer ←→ 测试工程师陈七 (Human L2)
│
├─ Team: 运维组
│   ├─ Leader (ops-lead) ←→ 运维主管周八 (Human L2)
│   └─ monitor ←→ 值班工程师 (Human L2)
│
└─ Worker: 行政助手 (独立)
    └─ ←→ 行政人员郑十 (Human L3)
```

一键部署：`./hiclaw-apply.sh -f company-setup.yaml`

## 11. 实施状态

| Phase | 内容 | 状态 |
|-------|------|------|
| 1 | 基础设施扩展（create-worker.sh, find-worker.sh, manage-state.sh 等） | ✅ |
| 2 | Team 管理能力（team-management skill + scripts） | ✅ |
| 3 | Team Leader Agent（模板 + team-task-management skill） | ✅ |
| 4 | Human 管理（human-management skill + scripts） | ✅ |
| 5 | Go Controller + kine 引擎（CRD + Reconciler + file watcher + HTTP API） | ✅ |
| 6 | 容器外入口脚本（hiclaw-apply.sh, hiclaw-import.sh 薄壳改造） | ✅ |
| 7 | 文档与协议更新 | ✅ |

### 后续规划

- `hiclaw` CLI 的 incluster 模式支持（`apply`/`get`/`delete` 对接真实 K8s API Server，目前仅 controller 本身支持 incluster 部署，CLI 操作尚未实现）
- Template 市场（预置常见 Worker/Team 模板包）
- 多 IM 平台支持（飞书、钉钉、企微等 Channel 适配）
- 审计日志和成本追踪

### 6.3 `workers-registry.json` 扩展（向后兼容）

每个 worker entry 新增两个可选字段：

```json
{
  "alpha-lead": {
    "...existing fields...",
    "role": "team_leader",
    "team_id": "alpha-team"
  },
  "alpha-dev": {
    "...existing fields...",
    "role": "worker",
    "team_id": "alpha-team"
  }
}
```

无 `role` 字段的 worker 默认为 `"worker"`，无 `team_id` 的为独立 Worker。

### 6.4 Team Leader 的 `team-state.json`

存放在 Leader 的 MinIO 空间 `agents/{leader-name}/team-state.json`，schema 与 Manager 的 `state.json` 一致：

```json
{
  "team_id": "alpha-team",
  "active_tasks": [],
  "updated_at": ""
}
```

### 6.5 Manager `state.json` 扩展

团队委派任务增加 `delegated_to_team` 字段：

```json
{
  "task_id": "task-20260325-100000",
  "title": "Build feature X",
  "type": "finite",
  "assigned_to": "alpha-lead",
  "delegated_to_team": "alpha-team",
  "room_id": "!leader-room:domain"
}
```
