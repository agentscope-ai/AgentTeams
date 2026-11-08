# AgentTeams: 基于 Kubernetes 原生的多 Agent 协作编排

## 1. 定位

AgentTeams 是一个开源的**协作式多 Agent 操作系统**：面向多个 AI Agent 协同工作的声明式编排平面。

与单 Agent 运行时不同，AgentTeams 回答一个命题：**当自主 Agent 必须在复杂工作上表现得像一个真正的团队时，如何编排组织结构、通信策略、委派与共享状态？**

AgentTeams 借鉴 Kubernetes 的思想——声明式 API、控制器 reconcile 循环、CRD 风格扩展——为 Agent*团队*构建控制平面。你用 YAML 声明期望结构；控制器接线基础设施与通信拓扑。

## 2. 为什么需要多 Agent 协作编排

### 2.1 从单 Agent 到 Agent 团队

生态正在从"单兵作战"走向"团队作战"：

| 阶段 | 特征 | 例子 |
|------|-----------------|----------|
| 单 Agent | 一个 Agent 独自完成任务 | OpenClaw、Cursor、Claude Code |
| 多 Agent 编排 | 多个 Agent 独立运行；统一生命周期 | NVIDIA NemoClaw |
| 多 Agent**协作** | Agent 组成有结构、有协议、有共享状态的团队 | **AgentTeams** |

单 Agent 的上限来自上下文与工具。越过那条边界就需要分工——但"多个 Agent 在运行" ≠ "多个 Agent 在协作"：

- **编排（Orchestration）**：生命周期、资源、隔离——*如何运行*多个 Agent。
- **协作（Collaboration）**：组织结构、谁可以给谁发消息、委派、共享状态——*它们如何一起工作*。

AgentTeams 聚焦协作。

### 2.2 类比 Kubernetes 的演进路径

| 容器世界 | Agent 世界 | 回答的问题 |
|----------------|------------|-------------------|
| Docker | OpenClaw / Claude Code | 如何运行一个隔离单元 |
| Docker Compose | NemoClaw（单 Agent 沙箱运维） | 如何管理生命周期与配置 |
| **Kubernetes** | **AgentTeams** | 如何使多个单元组成连贯系统 |

正如 Kubernetes 构建在 Docker 之上而不替代它，AgentTeams 构建在 Agent 运行时之上，并加上协作编排。

## 3. 核心架构

### 3.1 三层组织架构

AgentTeams 映射企业式组织结构：

```
Admin (human administrator)
  │
  ├── Manager (AI coordinator; optional deployment pattern)
  │     ├── Team Leader A (special Worker; in-team scheduling)
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C (standalone Worker, not in a Team)
  │
  └── Human users (real people, permission tiers)
        ├── Level 1: Admin-equivalent, can talk to all roles
        ├── Level 2: Talk to configured Teams’ Leaders + Workers (+ standalone Workers)
        └── Level 3: Talk only to configured standalone Workers
```

设计原则：

- **Team Leader 仍是 Worker**：同一容器/运行时类别；不同的 SOUL 与技能——就像控制面节点与工作节点都运行 kubelet。
- **Manager 不穿透 Team**：它只与 Team Leader 对话，不与团队内 Worker 对话——委派边界；避免瓶颈。
- **声明式通信策略**：`groupAllowFrom` 门控 @mention；用 CRD**`channelPolicy`**（`groupAllowExtra` / `groupDenyExtra` / `dmAllowExtra` / `dmDenyExtra`）在默认值之上追加允许或拒绝。

### 3.2 声明式资源（CRD 风格）

四个核心 kind 共享 `apiVersion: agentteams.io/v1beta1`：

```
apiVersion: agentteams.io/v1beta1
```

#### Worker — 执行单元

**命名：** Python Worker 运行时是 **QwenPaw**（镜像 `agentteams-copaw-worker`）。旧材料有时把同一运行时称为 **CoPaw**。

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6           # required: LLM model
  runtime: copaw                     # openclaw | copaw | hermes (defaults with chart image mapping)
  skills: [github-operations]        # platform built-in skills
  mcpServers:                        # MCP servers callable via mcporter
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http                # "http" (default) or "sse"
  package: file://./alice-pkg.zip    # optional: file/http(s)/nacos/packages/…
  soul: |                            # persona
    You are a frontend-focused engineer...
  expose:                            # ports published via Gateway
    - port: 3000
      protocol: http
  # state: Running                   # desired lifecycle: Running | Sleeping | Stopped
  # channelPolicy:                   # optional: allow/deny extras on group + DM defaults
  #   groupAllowExtra: ["@human:domain"]
```

每个 Worker 映射到：一个 Docker 容器（或 K8s Pod）+ 一个 Matrix 账号 + 一个 MinIO 命名空间 + 一个 Gateway Consumer 令牌。省略 `spec.image` 时，默认值来自 `AGENTTEAMS_WORKER_IMAGE` / `AGENTTEAMS_COPAW_WORKER_IMAGE` / `AGENTTEAMS_HERMES_WORKER_IMAGE`（或 chart 默认值）。

#### Team — 协作单元

```yaml
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: frontend-team
spec:
  description: "Frontend development team"
  peerMentions: true                  # default true: Workers may @mention each other in team rooms
  # channelPolicy: …                  # optional team-wide overrides (same shape as Worker)
  # admin:                             # optional Human resource used as Team Admin
  #   name: pm-zhang
  #   matrixUserId: "@pm:domain"
  heartbeatEvery: 10m
  workerMembers:
    - name: frontend-lead
      role: team_leader
    - name: alice
      role: worker
    - name: bob
      role: worker
```

`frontend-lead`、`alice` 与 `bob` 是既有 Worker CR。它们的模型、运行时、技能、MCP、镜像、资源、通道策略与生命周期设置都保留在 `Worker.spec`；Team 只持有成员关系与协作上下文。

创建 Team 时，控制器接线此拓扑（若设置了 `spec.admin`，"Admin" 指 **Team Admin**；否则指**全局 Admin**）：

```
Leader Room:  Manager + Global Admin + Leader    ← Manager talks only to Leader
Team Room:    Leader + Admin + W1 + W2 + …       ← Manager is NOT here (delegation boundary)
Worker Room:  Leader + Admin + Worker             ← private Leader↔member channel
Leader DM:    Admin ↔ Leader                     ← team alignment / management
```

**Team Room 排除 Manager**；Leader 在团队内部分解工作。哪些人类加入哪些房间，遵循 Human 权限与 `spec.admin`。

#### Human — 真人用户

```yaml
apiVersion: agentteams.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: "John Doe"
  email: john@example.com
  permissionLevel: 2                  # 1=Admin-equiv, 2=Team-scoped, 3=Worker-only
  accessibleTeams: [frontend-team]
  accessibleWorkers: [devops-alice]
```

#### Manager — 协调器（CR）

```yaml
apiVersion: agentteams.io/v1beta1
kind: Manager
metadata:
  name: default                       # common name for the primary instance in embedded installs
spec:
  model: claude-sonnet-4-6            # required
  runtime: openclaw                   # openclaw | qwenpaw
  # soul: | …                         # optional SOUL.md override
  # agents: | …                       # optional AGENTS.md override
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
  # package: https://…/mgr.zip       # optional; same URI semantics as Worker
  config:
    heartbeatInterval: 15m
    workerIdleTimeout: 720m
    notifyChannel: admin-dm
  # state: Running                    # Running | Sleeping | Stopped
```

`Manager` 与 `Worker` / `Team` / `Human` 同 API 组/版本，由同一控制器 reconcile。**你"是否"需要与 Manager Agent 对话是用法选择**：CLI / REST / 纯 YAML 工作流绕开对话入口；默认安装仍运行一个 Manager 容器，其期望配置可经此 CR 声明并 reconcile。

**kubectl 短名**（CRD 安装后）：`wk`、`tm`、`hm`、`mgr`。

### 3.3 Controller 架构

AgentTeams 遵循标准 Kubernetes 控制器模式。

**声明式 apply**：在宿主机上，`install/agentteams-apply.sh` 把 YAML 拷入 Manager 容器并执行 `agt apply -f`。CLI **按 YAML 文档顺序**发起 REST 调用（`POST`/`PUT` `/api/v1/workers`、`/teams`、`/humans`、`/managers`），**不**对依赖做拓扑排序——被依赖的资源放前面（例如引用 `accessibleTeams` 的 `Human` 之前先放 `Team`）。当前 CLI **未实现 `--prune` 与 `--dry-run`**（可能与某些安装脚本的注释不一致；以 CLI 为准）。

```
Declarative YAML
    ↓ agt apply
kine (etcd-compatible, SQLite backend) / native K8s etcd
    ↓ Informer watch
controller-runtime
    ↓ Reconcile loop
┌─────────────────────────────────────────────┐
│ Provisioner                                 │
│ - Matrix registration & rooms               │
│ - MinIO user & bucket                       │
│ - Higress Consumer & routes                 │
│ - K8s ServiceAccount (incluster)            │
├─────────────────────────────────────────────┤
│ Deployer                                    │
│ - Package fetch (file/http(s)/nacos/packages/…) │
│ - openclaw.json (incl. comms matrix)        │
│ - Push SOUL.md / AGENTS.md / skills         │
│ - Start container / create Pod              │
├─────────────────────────────────────────────┤
│ Worker backend abstraction                  │
│ - Docker (embedded)                         │
│ - Kubernetes (incluster)                    │
│ - Cloud-hosted                              │
└─────────────────────────────────────────────┘
```

部署模式：

| 模式 | 状态存储 | Worker 运行形态 | 典型用途 |
|------|-------------|----------------|-------------|
| Embedded | kine + SQLite | Docker 容器 | 开发 / 小团队 |
| Incluster | K8s etcd | Pod | 企业 / 云上 |

**Embedded 与 Helm（打包）：**

- **Embedded**——`install/agentteams-install.sh` 启动 **`agentteams-controller`**（镜像打包了 Higress、Tuwunel、MinIO、Element Web 与控制器二进制）。随后控制器把 **`agentteams-manager`** 与每个 **Worker** 作为独立容器创建在相同 Docker/Podman 宿主上。
- **Helm / incluster**——Chart [`helm/agentteams`](../../../helm/agentteams) 把相同逻辑组件以 Kubernetes 工作负载部署（网关、homeserver、存储、controller Deployment，以及由 CR 生成的 Manager/Worker Pod）。CRD 语义与 embedded 一致；只有后端驱动不同。

两种模式共享 reconciler；后端镜像了 Kubernetes 抽象 CRI/CSI/CNI 的方式。

### 3.4 通信层：Matrix 协议

AgentTeams 用 Matrix，而不是自研 RPC 总线：

| 关注点 | 为什么选 Matrix |
|---------|------------|
| 透明性 | Agent 流量在房间里可见；人类可实时旁观 |
| 人类介入 | 同一 IM 客户端；随时 @mention 任意 Agent |
| 开放协议 | 联邦设计；更少锁定 |
| 审计 | 持久历史 |
| 客户端 | Element、FluffyChat、移动端 |

Tuwunel 作为高性能 homeserver 打包，供单容器安装使用。

### 3.5 基于 Higress 的 LLM/MCP 安全访问模型

安全层是 **[Higress](https://github.com/alibaba/higress)**——一个**CNCF Sandbox** 的基于 Envoy 的 AI 网关，提供 LLM 代理、MCP 托管与按 Consumer 鉴权。与 AgentTeams 结合后，对每个 Agent 的 LLM 与 MCP 访问都可以策略驱动。

#### 核心安全原则：真实密钥绝不下发到 Agent

```
Worker (holds only Consumer Token / GatewayKey)
    → Higress AI Gateway
        ├── key-auth WASM validates token
        ├── Consumer must be on Route allowedConsumers
        ├── inject real credential (API key / PAT / OAuth)
        └── proxy upstream
            ├── LLM APIs
            ├── MCP servers (GitHub, Jira, …)
            └── other services
```

**真实凭据驻留在网关**；Agent 只持有可吊销的 Consumer 令牌。

#### LLM 访问路径

对每个 Worker，控制器通常：

1. 生成 Consumer 令牌（GatewayKey）。
2. 以 key-auth 在 Higress 注册 `worker-{name}`。
3. 把该 Consumer 加入 AI Route 的 `allowedConsumers`。

```
POST http://aigw-local.agentteams.io:8080/v1/chat/completions
Authorization: Bearer {GatewayKey}
```

Worker 的 `openclaw.json` 指向网关，而非裸 provider URL。

#### MCP 访问路径

```
POST http://aigw-local.agentteams.io:8080/mcp-servers/mcp-github/mcp
Authorization: Bearer {GatewayKey}
```

集中 MCP 注册 + 按 Consumer 的 `allowedConsumers` + 指向网关端点的 mcporter 配置。对外可调用的端点与端口完整列表见 [Higress Gateway API 参考](../../usage/higress-gateway-api.md)。

#### 细粒度控制

| 维度 | 机制 | 例子 |
|-----------|-----------|---------|
| 按 Worker 的 LLM | AI Route allowedConsumers | Worker A：GPT-4；Worker B：仅 GPT-3.5 |
| 按 Worker 的 MCP | MCP allowedConsumers | Worker A：GitHub MCP；Worker B：无 |
| 运行时变更 | 编辑 allowedConsumers | 吊销而不轮换上游密钥 |
| 快速吊销 | 从列表移除 | WASM 热重载（约秒级） |

类比 ServiceAccount + RBAC：Consumer 令牌 ≈ SA 令牌；`allowedConsumers` ≈ 策略。

#### 与 NemoClaw 对比（安全角度）

| 能力 | NemoClaw | AgentTeams + Higress |
|------------|----------|------------------|
| 凭据隔离 | OpenShell 拦截推理 | 网关代理；Worker 永不看到 API key |
| MCP 集中化 | 未内置 | Higress 托管 MCP + 统一鉴权 |
| 按 Agent 差异化 | 按沙箱配置 | 共享网关、按 Consumer 路由 |
| 动态策略 | 常需重建沙箱 | 编辑 allowedConsumers；快速生效 |
| OS 沙箱 | Landlock + seccomp + netns | 当前为 Docker（可与 NemoClaw 组合） |
| 出口策略 | 精细白名单 | 网关路由层 |

互补：NemoClaw 擅长 OS 级单 Agent 隔离；Higress 擅长多 Agent API/MCP 策略。

#### 为什么选 Higress

- AI 原生的网关（多 provider LLM 路由、限流、回退；MCP 托管）。
- WASM 插件（key-auth 热重载）。
- Envoy 内核（性能、Prometheus/OTel）。
- 发现模式（Nacos、K8s、DNS），embedded 与 incluster 皆可。

### 3.6 共享状态与 MinIO

```
MinIO (S3-compatible)
├── agents/                    # Per-Agent config space
│   ├── alice/
│   │   ├── SOUL.md
│   │   ├── openclaw.json
│   │   └── skills/
│   └── bob/
├── shared/
│   ├── tasks/
│   │   └── task-{id}/
│   │       ├── meta.json
│   │       ├── spec.md       # Manager / Leader
│   │       └── result.md     # Workers
│   └── knowledge/
└── workers/                   # Artifacts
```

Worker 在容器边缘无状态：配置从对象存储拉取；容器可像背后带共享持久化的无状态 Pod 一样重建。

## 4. 多 Agent 协作流程

### 4.1 Team 内部

```
Admin: "Ship login feature front + back"
  ↓
Manager: routes to frontend team, @mentions Team Leader
  ↓
Team Leader: splits work
  ├── Subtask 1: login API → @ Worker A
  ├── Subtask 2: login UI → @ Worker B
  └── Subtask 3: integration tests → after 1+2
  ↓
Workers report in Team Room; Leader aggregates
  ↓
Leader @mentions Manager with summary
  ↓
Manager notifies Admin
```

一切留在 Matrix 房间——Admin 可随时介入。

### 4.2 Human-in-the-Loop 介入

```
[Team Room]
Leader: @alice implement password rules (min 8 chars)
Alice: On it...

Admin observes and intervenes:
Admin: @alice hold on—min 12 chars, mixed case + symbols
Alice: Updated.
Leader: I'll refresh the task spec.
```

无隐藏的 Agent 间旁路——按设计可审计。

## 5. 与 NVIDIA NemoClaw 的对比

### 5.1 定位差异

| 维度 | NemoClaw | AgentTeams |
|-----------|----------|--------|
| 焦点 | 单 Agent 沙箱安全 | 多 Agent**协作**编排 |
| 问题 | 安全地运行一个 Agent | 多个 Agent 组成有结构的团队 |
| 形态 | 每沙箱一个 Agent | Manager → Leader → Workers |
| Agent 之间 | 隔离 | 声明式通信矩阵 + 房间 |
| 共享状态 | 每沙箱工作区 | MinIO + 任务流 |
| 人类 | 单操作者 | 多角色、三级 Human CRD |
| 配置 | 蓝图 YAML + 向导 | CRD 风格 YAML + reconcile |

### 5.2 架构速写

**NemoClaw**

```
NemoClaw CLI → onboard → OpenShell
    ├── Sandbox A (OpenClaw)
    ├── Sandbox B (Hermes)
    └── Sandbox C (OpenClaw)
No cross-sandbox chat, no shared coordinator.
```

**AgentTeams**

```
AgentTeams Controller
    ↓
Matrix: Manager ↔ Leaders ↔ Workers; standalone Workers ↔ Manager
MinIO shared state
Higress security
Human tiers in the same rooms
```

### 5.3 能力矩阵

| 能力 | NemoClaw | AgentTeams |
|------------|----------|--------|
| 生命周期 | 沙箱 CRUD/恢复 | reconcile + 容器/Pod |
| OS 沙箱 | 强 | Docker（NemoClaw 可选） |
| LLM 密钥 | OpenShell 拦截 | 网关 + Consumer 令牌 |
| MCP | 未集中 | Higress MCP + allowedConsumers |
| 动态策略 | 常重建沙箱 | 编辑 allowedConsumers |
| Agent 间 | 无 | Matrix + 房间拓扑 |
| 委派 | 无 | Manager → Leader → Worker |
| Teams / Humans | 无 | Team + Human CRD |
| 声明式 | 单 Agent 蓝图 | Worker/Team/Human/Manager |
| K8s 原生部署 | 否 | incluster + Helm |
| 运行时 | OpenClaw、Hermes、… | OpenClaw、QwenPaw、Hermes、ZeroClaw*、NanoClaw* |

\* 路线图 / 轻量选项（见项目 README）。

### 5.4 互补的未来

```
┌────────────────────────────────────┐
│ AgentTeams — collaboration layer        │
│ org / comms / delegation / state  │
├────────────────────────────────────┤
│ NemoClaw — sandbox runtime layer    │
│ isolation / routing / policy        │
├────────────────────────────────────┤
│ OpenClaw / QwenPaw / … — Agent engines│
└────────────────────────────────────┘
```

Worker 后端未来可以在每个 Worker 之下接入 NemoClaw——AgentTeams 编排团队；NemoClaw 加固每个单元——就像 Kubernetes 与任意 CRI 运行时。

## 6. 技术栈

| 组件 | 选型 | 注记 |
|-------|--------|------|
| 控制器 | Go + controller-runtime | 标准 kube builder 风格 |
| 状态 | kine（SQLite）/ etcd | embedded 与 incluster |
| 通信 | Matrix（Tuwunel） | 自托管 |
| IM UI | Element Web | 浏览器客户端 |
| 文件 | MinIO | S3 API |
| AI 网关 | Higress（CNCF Sandbox） | LLM + MCP + Consumer 鉴权 |
| 运行时 | OpenClaw、QwenPaw、… | 从重量级到轻量级镜像 |
| 技能 | skills.sh 生态 | 大型社区目录 |
| MCP CLI | mcporter | 经网关调用 |

## 7. 与 Kubernetes 的对应关系

| Kubernetes | AgentTeams | 注记 |
|------------|--------|-------|
| Pod | Worker | 最小可调度单元；可替换 |
| Deployment | Team | 一组期望的协作 Worker |
| Service | Matrix 房间 | 协作"端点"抽象 |
| SA + RBAC | Consumer + allowedConsumers | 身份 + 细粒度路由 |
| CRD | Worker/Team/Human/Manager | 声明式 API |
| CR 短名 | `wk` / `tm` / `hm` / `mgr` | CRD 安装后 |
| 控制器 | agentteams-controller | reconcile 循环 |
| kubectl apply | agt apply | `apply -f` 按顺序遍历多文档 YAML |

## 8. 部署模式

见**第 3.3 节**了解控制器如何 reconcile；本节只讲*怎么安装*。

### 8.1 Embedded 模式（开发 / 小团队）

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

粗略最低配置：2 CPU、4 GB 内存、Docker/Podman。你得到 **`agentteams-controller`**（基础设施 + 控制器）加上独立的 **`agentteams-manager`** 容器；Worker 创建时作为附加容器出现。

### 8.2 Incluster / Helm 模式（企业级 / 云上部署）

```bash
# From repository root (chart lives under helm/agentteams)
helm install agentteams ./helm/agentteams
```

仓库被加入后，也可以从已发布的 Helm chart 安装。chart 按 `values.yaml` 接线 **`agentteams-controller`**、网关、homeserver 与存储；Manager 与 Worker Pod 遵循与 embedded 安装相同的 CRD API。

## 9. 状态与路线图

- **2026-03-04**：开源，Apache 2.0。
- **已交付**：OpenClaw/QwenPaw、MCP 集成、Team + Human 模型。
- **进行中**：ZeroClaw（Rust 超轻量）、NanoClaw（最小 LOC 运行时）——当前状态见 README。
- **规划中**：团队管理 dashboard、更完整的 incluster/Helm 方案、Worker 之下可选的 NemoClaw 式沙箱。

## 10. 社区

- GitHub: https://github.com/agentscope-ai/AgentTeams
- Discord: https://discord.gg/NVjNA4BAVw
- 许可证：Apache 2.0
