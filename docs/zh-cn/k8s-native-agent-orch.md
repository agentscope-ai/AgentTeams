# HiClaw: 基于 Kubernetes 原生的多 Agent 协作编排系统

## 1. 项目定位

HiClaw 是一个开源的协作式多 Agent 操作系统（Collaborative Multi-Agent OS），为多个 AI Agent 之间的协作提供声明式编排底座。

与传统的单 Agent 运行时不同，HiClaw 解决的核心问题是：**当多个自主 Agent 需要像一个真实团队一样协作完成复杂任务时，如何编排它们之间的组织关系、通信权限、任务委派和共享状态？**

HiClaw 借鉴了 Kubernetes 的核心设计哲学——声明式 API、Controller Reconcile Loop、CRD 扩展机制——构建了一套面向 AI Agent 团队的编排控制平面。用户通过 YAML 声明期望的团队结构，Controller 自动完成从基础设施配置到 Agent 间通信拓扑的全部编排工作。

## 2. 为什么需要多 Agent 协作编排

### 2.1 从单 Agent 到 Agent 团队

当前 AI Agent 生态正在经历从"单兵作战"到"团队协作"的演进：

| 阶段 | 特征 | 代表方案 |
|------|------|---------|
| 单 Agent | 一个 Agent 独立完成任务 | OpenClaw, Cursor, Claude Code |
| 多 Agent 编排 | 多个 Agent 各自独立运行，统一管理生命周期 | NVIDIA NemoClaw |
| 多 Agent 协作 | 多个 Agent 组成团队，有组织结构、通信协议、共享状态 | **HiClaw** |

单 Agent 的能力上限受限于单次对话的上下文窗口和工具集。当任务复杂度超过单 Agent 能力边界时，需要多个 Agent 分工协作。但"多个 Agent 同时运行"和"多个 Agent 协作"是两个本质不同的问题：

- **编排（Orchestration）**：管理 Agent 的生命周期、资源分配、安全隔离——解决的是"如何运行多个 Agent"
- **协作（Collaboration）**：定义 Agent 间的组织关系、通信权限、任务委派、状态共享——解决的是"多个 Agent 如何一起工作"

HiClaw 聚焦于后者，提供了一套完整的多 Agent 协作编排方案。

### 2.2 类比 Kubernetes 的演进路径

这个演进过程与容器编排的历史高度相似：

| 容器生态 | Agent 生态 | 解决的问题 |
|---------|-----------|-----------|
| Docker（容器运行时） | OpenClaw / Claude Code（Agent 运行时） | 如何运行一个隔离的工作单元 |
| Docker Compose（单机编排） | NemoClaw（单 Agent 沙箱管理） | 如何管理运行时的生命周期和配置 |
| **Kubernetes（集群编排）** | **HiClaw（多 Agent 协作编排）** | 如何让多个工作单元组成一个协调的系统 |

正如 Kubernetes 不替代 Docker，而是在其之上提供编排能力，HiClaw 也不替代底层 Agent 运行时，而是在其之上提供协作编排能力。

## 3. 核心架构

### 3.1 三层组织架构

HiClaw 采用映射真实企业团队结构的三层组织架构：

```
Admin（人类管理员）
  │
  ├── Manager（AI 协调者，可选部署）
  │     ├── Team Leader A（特殊 Worker，管理团队内任务调度）
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C（独立 Worker，不属于任何 Team）
  │
  └── Human Users（真人用户，按权限级别接入）
        ├── Level 1: 等同 Admin，可与所有角色对话
        ├── Level 2: 可与指定 Team 的 Leader + Workers 对话
        └── Level 3: 只能与指定 Workers 对话
```

关键设计原则：

- **Team Leader 本质是 Worker**：同样的容器、同样的运行时，只是 SOUL（人格定义）和 Skills 不同——类似 K8s 中 control plane node 和 worker node 运行相同的 kubelet
- **Manager 不穿透 Team**：Manager 只与 Team Leader 通信，不直接联系团队内 Worker——实现了委派边界，防止 Manager 成为瓶颈
- **通信权限声明式控制**：通过 `groupAllowFrom` 配置矩阵精确控制每个 Agent 接受谁的消息；必要时用 CRD 字段 **`channelPolicy`**（`groupAllowExtra` / `groupDenyExtra` / `dmAllowExtra` / `dmDenyExtra`）在默认值之上做增减

### 3.2 声明式资源模型（CRD 风格）

HiClaw 定义了四种核心资源类型，全部采用 Kubernetes CRD 风格的声明式 YAML：

```
apiVersion: hiclaw.io/v1beta1
```

#### Worker — 基本执行单元

**命名说明：** Python Worker 运行时当前名称为 **QwenPaw**（镜像 `hiclaw-copaw-worker`）。早期文档曾使用 **CoPaw**，指同一运行时。

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6           # 必填：LLM 模型
  runtime: copaw                     # openclaw | copaw | hermes（默认随安装/CR）
  skills: [github-operations]        # 平台内置技能
  mcpServers:                        # 通过 mcporter 调用的 MCP Server
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http                # "http"（默认）或 "sse"
  package: file://./alice-pkg.zip    # 可选：file/http(s)/nacos/packages/…
  soul: |                            # Agent 人格定义
    你是一个专注于前端开发的工程师...
  expose:                            # 通过 Gateway 暴露的端口
    - port: 3000
      protocol: http
  # state: Running                   # 期望生命周期：Running | Sleeping | Stopped
  # channelPolicy:                   # 可选：在默认 groupAllow/DM 策略上增减允许/拒绝列表
  #   groupAllowExtra: ["@human:domain"]
```

每个 Worker 对应：一个 Docker 容器（或 K8s Pod）+ 一个 Matrix 通信账号 + 一块 MinIO 命名空间 + 一个 Gateway Consumer Token。未指定 `spec.image` 时由环境变量 `HICLAW_WORKER_IMAGE` / `HICLAW_COPAW_WORKER_IMAGE` / `HICLAW_HERMES_WORKER_IMAGE`（或 Chart 默认值）决定默认镜像。

#### Team — 协作单元

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: frontend-team
spec:
  description: "前端开发团队"
  peerMentions: true                  # 默认 true：团队 Worker 可在群 Room 互相 @mention
  # channelPolicy: …                  # 可选：团队级通信策略覆盖（字段同 Worker）
  # admin:                             # 可选：团队专属人类管理员（humans-registry 中的 name）
  #   name: pm-zhang
  #   matrixUserId: "@pm:domain"
  leader:
    name: frontend-lead
    model: claude-sonnet-4-6
    heartbeat:
      enabled: true
      every: 10m
    workerIdleTimeout: 720m
    # state: Running                  # Leader 期望生命周期（可选）
  workers:
    - name: alice
      model: claude-sonnet-4-6
      skills: [github-operations]
      mcpServers:
        - name: github
          url: https://gateway.example.com/mcp-servers/github/mcp
    - name: bob
      model: qwen3.5-plus
      runtime: copaw
      skills: [github-operations]
      # expose / channelPolicy / state 等与独立 Worker 对齐的字段亦可出现在此
```

Team 创建时，Controller 自动编排以下拓扑（若配置了 `spec.admin`，则「Admin」指 **Team Admin**；否则为全局 Admin）：

```
Leader Room:  Manager + Global Admin + Leader    ← Manager 仅与 Leader 对接
Team Room:    Leader + Admin + W1 + W2 + …       ← Manager 不在此 Room（委派边界）
Worker Room:  Leader + Admin + Worker             ← Leader 与单个成员的私聊
Leader DM:    Admin ↔ Leader                     ← 团队管理与对齐
```

关键：**Team Room 不包含 Manager**，任务在团队内由 Leader 分解；全局 Admin / Team Admin 是否进入各 Room 由 Humans 与 `spec.admin` 共同决定。

#### Human — 真人用户

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: zhangsan
spec:
  displayName: "张三"
  email: zhangsan@example.com
  permissionLevel: 2                  # 1=Admin, 2=Team, 3=Worker
  accessibleTeams: [frontend-team]
  accessibleWorkers: [devops-alice]
```

#### Manager — 协调 Agent（CRD）

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Manager
metadata:
  name: default                       # 嵌入式部署常见主实例名
spec:
  model: claude-sonnet-4-6            # 必填
  runtime: openclaw                   # openclaw | copaw
  # soul: | …                         # 可选：覆盖 SOUL.md
  # agents: | …                       # 可选：覆盖 AGENTS.md
  skills: [worker-management]         # 按需启用的 Manager skills
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
  # package: https://…/mgr.zip       # 可选：与 Worker 相同的包 URI 语义
  config:
    heartbeatInterval: 15m
    workerIdleTimeout: 720m
    notifyChannel: admin-dm
  # state: Running                    # Running | Sleeping | Stopped
```

`Manager` 与 `Worker`/`Team`/`Human` 同属 `hiclaw.io/v1beta1`，由同一套 Controller Reconcile。**是否依赖「对话式 Manager Agent」取决于你的使用方式**：只用 `hiclaw` CLI / REST API / YAML 编排时，可以不通过聊天入口；默认一键安装仍会拉起 Manager 容器，其期望配置可通过该 CR 声明并持续调和。

**kubectl 短名（安装 CRD 后）**：`wk`、`tm`、`hm`、`mgr`。

### 3.3 Controller 架构

HiClaw Controller 采用标准的 Kubernetes Controller 模式。

**声明式入口说明**：宿主机上的 `install/hiclaw-apply.sh` 将 YAML 拷入 Manager 容器并执行 `hiclaw apply -f`。CLI **按 YAML 文档顺序**依次调用 REST API（`POST`/`PUT` `/api/v1/workers|teams|humans|managers`），**不会**自动按依赖拓扑排序；多文档文件中应先写被依赖资源（例如先 `Team` 再引用 `accessibleTeams` 的 `Human`）。当前 CLI **未实现** `--prune` / `--dry-run`（与个别安装脚本注释可能不一致，以 CLI 为准）。

```
YAML 资源声明
    ↓ hiclaw apply
kine (etcd 兼容层, SQLite 后端) / 原生 K8s etcd
    ↓ Informer Watch
Controller Runtime
    ↓ Reconcile Loop
┌─────────────────────────────────────────────┐
│  Provisioner（基础设施配置）                   │
│  - Matrix 账号注册 & Room 创建               │
│  - MinIO 用户 & Bucket 配置                  │
│  - Higress Gateway Consumer & Route 配置     │
│  - K8s ServiceAccount 创建（incluster 模式）  │
├─────────────────────────────────────────────┤
│  Deployer（配置部署）                         │
│  - Package 解析（file/http(s)/nacos/packages/…） │
│  - openclaw.json 生成（含通信权限矩阵）        │
│  - SOUL.md / AGENTS.md / Skills 推送         │
│  - 容器启动 / Pod 创建                        │
├─────────────────────────────────────────────┤
│  Worker Backend 抽象层                        │
│  - Docker Backend（embedded 模式）            │
│  - K8s Backend（incluster 模式）              │
│  - Cloud Backend（云上托管模式）               │
└─────────────────────────────────────────────┘
```

支持两种部署模式：

| 模式 | 状态存储 | Worker 运行 | 适用场景 |
|------|---------|------------|---------|
| Embedded | kine + SQLite | Docker 容器 | 开发者本地、小团队 |
| Incluster | K8s 原生 etcd | K8s Pod | 企业级、云上部署 |

**Embedded 与 Helm（交付形态）：**

- **Embedded**：`install/hiclaw-install.sh` 拉起 **`hiclaw-controller`**（镜像内嵌 Higress、Tuwunel、MinIO、Element Web 与 controller 二进制），再由 controller 在同一 Docker/Podman 宿主机上创建 **`hiclaw-manager`** 与各 **Worker** 容器。
- **Helm / in-cluster**：使用仓库内 [`helm/hiclaw`](../helm/hiclaw) Chart，将网关、Homeserver、存储、**hiclaw-controller** 与由 CR 驱动的 Manager/Worker Pod 部署为 Kubernetes 工作负载。CRD 语义与 Embedded 一致，仅后端驱动不同。

两种模式共享同一套 Reconciler 逻辑，通过 Worker Backend 抽象层适配不同的基础设施。这与 Kubernetes 通过 CRI/CSI/CNI 抽象底层运行时的设计思路一致。

### 3.4 通信层：Matrix 协议

HiClaw 选择 Matrix 作为 Agent 间通信协议，而非自建 RPC 框架：

| 选型考量 | Matrix 的优势 |
|---------|-------------|
| 透明性 | 所有 Agent 间通信在 Matrix Room 中可见，人类可实时旁观 |
| Human-in-the-Loop | 人类用户使用同一个 IM 客户端，随时 @mention 任何 Agent 介入 |
| 去中心化 | Matrix 是去中心化开放协议，无供应商锁定 |
| 持久化 | 消息天然持久化，提供完整的审计轨迹 |
| 生态 | Element、FluffyChat 等成熟客户端，移动端零配置接入 |

内置 Tuwunel 作为高性能 Matrix Homeserver，单容器部署，无需外部依赖。

### 3.5 基于 Higress 的 LLM/MCP 安全访问模型

HiClaw 的安全层由 [Higress](https://github.com/alibaba/higress) 提供——Higress 是一个 **CNCF Sandbox 项目**，基于 Envoy 构建的云原生 AI Gateway，原生支持 LLM 代理、MCP Server 托管和细粒度的消费者鉴权。HiClaw 与 Higress 的深度集成，使得多 Agent 场景下的 LLM 和 MCP 访问安全成为一等公民。

#### 核心安全原则：凭证永不下发到 Agent

```
Worker（仅持有 Consumer Token: GatewayKey）
    → Higress AI Gateway
        ├── key-auth WASM 插件验证 Consumer Token
        ├── 检查该 Consumer 是否在目标 Route 的 allowedConsumers 列表中
        ├── 注入真实凭证（API Key / GitHub PAT / OAuth Token）
        └── 代理请求到上游服务
            ├── LLM API（OpenAI / Anthropic / 通义千问 等）
            ├── MCP Server（GitHub / Jira / 自定义 等）
            └── 其他外部服务
```

这个模型的核心思想是：**真实凭证只存在于 Gateway 内部，Agent 永远只持有一个可随时吊销的 Consumer Token**。即使 Agent 被攻破，攻击者也无法获取任何外部服务的真实凭证。

#### LLM 访问安全

每个 Worker 创建时，Controller 自动完成以下编排：

1. **生成 Consumer Token**：为 Worker 生成 32 字节随机 GatewayKey，作为唯一身份凭证
2. **创建 Gateway Consumer**：在 Higress 注册 Consumer（`worker-{name}`），绑定 key-auth BEARER 凭证
3. **授权 AI Route**：将该 Consumer 添加到所有 AI Route 的 `allowedConsumers` 列表

```
Worker 发起 LLM 请求:
    POST https://aigw-local.hiclaw.io/v1/chat/completions
    Authorization: Bearer {GatewayKey}
        ↓
Higress Gateway:
    1. key-auth WASM 插件验证 GatewayKey → 识别为 Consumer "worker-alice"
    2. 检查 "worker-alice" 是否在该 AI Route 的 allowedConsumers 中
    3. 替换 Authorization header 为真实 LLM API Key
    4. 代理请求到上游 LLM Provider
```

Worker 的 `openclaw.json` 中配置的 API endpoint 指向 Gateway 地址，而非真实的 LLM Provider 地址。Worker 完全不知道真实 API Key 的存在。

#### MCP Server 安全访问

MCP（Model Context Protocol）Server 为 Agent 提供工具调用能力（如 GitHub 操作、数据库查询等）。在多 Agent 场景下，MCP Server 的凭证管理尤为关键——多个 Worker 可能需要访问同一个 GitHub 仓库，但不应该每个 Worker 都持有 GitHub PAT。

HiClaw 通过 Higress 实现了 MCP Server 的集中托管和安全代理：

```
MCP Server 注册流程:
    1. 管理员在 Higress 注册 MCP Server（配置真实凭证，如 GitHub PAT）
    2. Higress 托管 MCP Server，对外暴露标准 MCP 端点
    3. Controller 将需要访问该 MCP Server 的 Worker Consumer 添加到 allowedConsumers
    4. 为 Worker 生成 mcporter 配置，指向 Gateway 的 MCP 端点

Worker 调用 MCP 工具:
    POST https://aigw-local.hiclaw.io/mcp-servers/github/mcp
    Authorization: Bearer {GatewayKey}
        ↓
    Higress Gateway:
        1. 验证 Consumer Token
        2. 检查该 Consumer 是否被授权访问 "github" MCP Server
        3. 注入真实 GitHub PAT
        4. 代理请求到 MCP Server 实现
```

#### 细粒度权限控制与动态吊销

Higress 的 Consumer + allowedConsumers 机制为 HiClaw 提供了细粒度的权限控制：

| 控制维度 | 实现方式 | 示例 |
|---------|---------|------|
| Worker 级 LLM 访问 | AI Route 的 allowedConsumers | Worker A 可用 GPT-4，Worker B 只能用 GPT-3.5 |
| Worker 级 MCP 访问 | MCP Server 的 allowedConsumers | Worker A 可访问 GitHub，Worker B 不可以 |
| 动态权限变更 | 修改 allowedConsumers 列表 | Manager 可实时授予/吊销 Worker 的 MCP 访问权 |
| 即时吊销 | 从 allowedConsumers 移除 | 无需轮换凭证，1-2 秒内生效（WASM 插件热同步） |

这个权限模型类似 K8s 中 ServiceAccount + RBAC 的设计——Pod 通过 ServiceAccount Token 访问 API Server，RBAC 策略控制其可访问的资源范围。在 HiClaw 中，Consumer Token 是 ServiceAccount Token，allowedConsumers 是 RBAC Policy。

#### 与 NemoClaw 安全模型的对比

| 安全能力 | NemoClaw | HiClaw + Higress |
|---------|----------|-----------------|
| 凭证隔离 | ✅ OpenShell 拦截推理请求，Agent 不见凭证 | ✅ Higress Gateway 代理，Worker 不见凭证 |
| MCP Server 安全 | ❌ 无 MCP 集中管理 | ✅ Higress 托管 MCP Server，统一鉴权 |
| 多 Agent 权限差异化 | ❌ 每个 Sandbox 独立配置 | ✅ 同一 Gateway 下细粒度 Consumer 权限 |
| 动态权限变更 | ❌ 需重建 Sandbox | ✅ 修改 allowedConsumers，秒级生效 |
| 沙箱级隔离 | ✅ Landlock + seccomp + netns | ⚠️ Docker 容器隔离（可对接 NemoClaw 增强） |
| 网络策略 | ✅ 细粒度 egress 白名单 | ⚠️ Gateway 路由级控制 |

两者在安全层面同样互补：NemoClaw 擅长单 Agent 的沙箱级隔离（OS 层），Higress 擅长多 Agent 场景下的 API 访问控制和凭证管理（网络层）。

#### 为什么选择 Higress

作为 CNCF Sandbox 项目，Higress 为 HiClaw 带来了以下关键能力：

- **AI-Native Gateway**：原生支持 LLM 代理（多 Provider 路由、Token 限流、Fallback）和 MCP Server 托管，而非通过通用 API Gateway 的插件机制勉强实现
- **WASM 插件体系**：key-auth 等安全插件以 WASM 运行，热更新无需重启，权限变更秒级生效
- **Envoy 内核**：继承 Envoy 的高性能和可观测性，与 CNCF 生态（Prometheus、OpenTelemetry）天然集成
- **多后端支持**：同时支持 Nacos、K8s Service Discovery、DNS 等服务发现方式，适配 embedded 和 incluster 两种部署模式

### 3.6 共享状态与文件系统

```
MinIO (HTTP 对象存储)
├── agents/                    # 每个 Worker 的配置空间
│   ├── alice/
│   │   ├── SOUL.md           # Agent 人格
│   │   ├── openclaw.json     # 运行时配置
│   │   └── skills/           # 技能定义
│   └── bob/
├── shared/                    # 共享空间
│   ├── tasks/                # 任务规格、元数据、结果
│   │   └── task-{id}/
│   │       ├── meta.json     # 任务元数据
│   │       ├── spec.md       # 任务规格（Manager/Leader 编写）
│   │       └── result.md     # 任务结果（Worker 编写）
│   └── knowledge/            # 共享知识库
└── workers/                   # Worker 工作产物
```

Worker 是无状态的——所有配置从 MinIO 拉取，可以随时销毁重建而不丢失状态。这与 K8s 中 Pod 无状态 + PV/PVC 持久化的设计理念一致。

## 4. 多 Agent 协作流程

### 4.1 Team 内任务协作

```
Admin: "完成用户登录功能的前后端开发"
  ↓
Manager: 识别任务涉及前端团队，@mention Team Leader
  ↓
Team Leader: 分解任务为子任务
  ├── 子任务 1: "实现登录 API" → @mention Worker A（后端）
  ├── 子任务 2: "实现登录页面" → @mention Worker B（前端）
  └── 子任务 3: "编写集成测试" → 等待 1、2 完成后分配
  ↓
Worker A: 完成后端 API，在 Team Room 汇报 → @mention Leader
Worker B: 完成前端页面，在 Team Room 汇报 → @mention Leader
  ↓
Team Leader: 确认 1、2 完成，分配子任务 3 → @mention Worker A
  ↓
Worker A: 完成集成测试，汇报
  ↓
Team Leader: 汇总结果，@mention Manager
  ↓
Manager: 通知 Admin 任务完成
```

全程所有对话在 Matrix Room 中可见，Admin 可以随时介入任何环节。

### 4.2 Human-in-the-Loop 介入

```
[Team Room]
Leader: @alice 请实现密码强度校验，规则是至少 8 位
Alice: 收到，开始实现...

[Admin 在 Team Room 中观察到，认为规则需要调整]
Admin: @alice 等一下，密码规则改为至少 12 位，必须包含大小写和特殊字符
Alice: 收到，已更新规则
Leader: 好的，我更新一下任务规格
```

没有隐藏的 Agent-to-Agent 调用，所有决策过程透明可审计。

## 5. 与 NVIDIA NemoClaw 的对比

### 5.1 定位差异

| 维度 | NemoClaw | HiClaw |
|------|----------|--------|
| 核心定位 | Agent 运行时安全沙箱 | 多 Agent 协作编排底座 |
| 解决的问题 | 如何安全地运行单个 Agent | 如何让多个 Agent 组成团队协作 |
| 架构层次 | 单 Agent per Sandbox | Manager → Team Leader → Workers 三层组织 |
| Agent 间关系 | 完全隔离，无通信 | 声明式通信权限矩阵，结构化协作 |
| 状态共享 | 每个 Sandbox 独立工作空间 | MinIO 共享文件系统 + 任务状态流转 |
| 人类参与 | 单人操作单 Agent | 多人多角色，3 级权限体系 |
| 配置模型 | Blueprint YAML + 交互式向导 | K8s CRD 风格声明式 YAML + Controller Reconcile |

### 5.2 架构对比

**NemoClaw 架构：**

```
NemoClaw CLI
    ↓ onboard
OpenShell Runtime
    ├── Sandbox A (Agent: OpenClaw)  ← 完全隔离
    ├── Sandbox B (Agent: Hermes)    ← 完全隔离
    └── Sandbox C (Agent: OpenClaw)  ← 完全隔离
    
    Sandbox 之间：无通信、无共享状态、无协调机
```

NemoClaw 的核心价值在于安全隔离：Landlock 文件系统隔离、seccomp 系统调用过滤、网络命名空间隔离、凭证路由（Agent 不接触真实 API Key）。每个 Sandbox 是一个独立的安全域，运行一个 Agent 实例。

**HiClaw 架构：**

```
HiClaw Controller (Reconcile Loop)
    ↓ 声明式编排
┌─────────────────────────────────────────────────┐
│  通信层 (Matrix Protocol)                        │
│  ┌─────────┐  ┌──────────┐  ┌──────────────┐   │
│  │ Manager │←→│ Leader A │←→│ Worker A1/A2 │   │
│  │         │  └──────────┘  └──────────────┘   │
│  │         │  ┌──────────┐  ┌──────────────┐   │
│  │         │←→│ Leader B │←→│ Worker B1    │   │
│  │         │  └──────────┘  └──────────────┘   │
│  │         │←→ Worker C (独立)                  │
│  └─────────┘                                    │
│  共享状态层 (MinIO)                               │
│  安全层 (Higress AI Gateway, CNCF Sandbox)        │
│  人类接入层 (Matrix Rooms, 3 级权限)              │
└─────────────────────────────────────────────────┘
```

### 5.3 能力矩阵

| 能力 | NemoClaw | HiClaw |
|------|----------|--------|
| Agent 生命周期管理 | ✅ Sandbox create/destroy/recover | ✅ Controller Reconcile + 自动容器管理 |
| 安全沙箱隔离 | ✅ Landlock + seccomp + netns | ⚠️ Docker 容器隔离（可对接 NemoClaw 增强） |
| LLM 访问安全 | ✅ OpenShell 拦截，Agent 不见凭证 | ✅ Higress (CNCF) Gateway 代理，Consumer Token 鉴权，Worker 不见凭证 |
| MCP Server 安全 | ❌ 无集中管理 | ✅ Higress 托管 MCP Server，per-Worker allowedConsumers 细粒度授权 |
| 动态权限管理 | ❌ 需重建 Sandbox | ✅ 修改 allowedConsumers，WASM 插件热同步，秒级生效 |
| 网络策略 | ✅ 细粒度 egress 控制 + 预设策略 | ⚠️ Gateway 路由级控制 |
| Agent 间通信 | ❌ 无 | ✅ Matrix 协议，结构化 Room 拓扑 |
| 任务委派与分解 | ❌ 无 | ✅ Manager → Leader → Worker 三级委派 |
| 共享状态 | ❌ 每个 Sandbox 独立 | ✅ MinIO 共享文件系统 + 任务状态机 |
| 团队组织结构 | ❌ 无 | ✅ Team CRD，声明式定义 |
| 多人协作 | ❌ 单人操作 | ✅ Human CRD，3 级权限 |
| Human-in-the-Loop | ❌ 仅 CLI 交互 | ✅ Matrix Room 实时旁观与介入 |
| 声明式配置 | ⚠️ Blueprint YAML（单 Agent） | ✅ K8s CRD 风格（Worker/Team/Human/Manager） |
| K8s 原生部署 | ❌ | ✅ incluster 模式，Helm 安装 |
| 多 Agent 运行时 | ✅ OpenClaw, Hermes | ✅ OpenClaw, QwenPaw, Hermes, ZeroClaw, NanoClaw |

### 5.4 互补关系与未来集成

NemoClaw 和 HiClaw 不是竞争关系，而是互补关系——它们解决的是 Agent 生态中不同层次的问题：

```
┌─────────────────────────────────────────────┐
│  HiClaw（协作编排层）                         │
│  组织结构 / 通信权限 / 任务委派 / 共享状态      │
├─────────────────────────────────────────────┤
│  NemoClaw（安全运行时层）                      │
│  沙箱隔离 / 推理路由 / 网络策略 / 凭证管理      │
├─────────────────────────────────────────────┤
│  OpenClaw / QwenPaw / Hermes（Agent 运行时）   │
│  LLM 交互 / 工具调用 / 技能执行                │
└─────────────────────────────────────────────┘
```

HiClaw 的 Worker Backend 抽象层设计使其可以对接不同的底层运行基础设施。未来 HiClaw 可以支持 NemoClaw 作为 Worker 的底层运行时，将 NemoClaw 的安全沙箱能力与 HiClaw 的协作编排能力结合：

- HiClaw 负责：团队组织、通信编排、任务委派、共享状态
- NemoClaw 负责：每个 Worker 的沙箱隔离、推理路由、网络策略

这类似于 Kubernetes 通过 CRI 接口对接不同的容器运行时（containerd、CRI-O）——编排层不关心底层运行时的具体实现，只关心工作负载的声明式管理。

## 6. 技术栈与生态

| 组件 | 技术选型 | 说明 |
|------|---------|------|
| Controller | Go + controller-runtime | 标准 K8s Controller 开发模式 |
| 状态存储 | kine (SQLite) / K8s etcd | embedded 模式用 kine，incluster 用原生 etcd |
| 通信协议 | Matrix (Tuwunel) | 去中心化开放协议，自托管 |
| IM 客户端 | Element Web | 零配置浏览器客户端 |
| 文件存储 | MinIO | S3 兼容对象存储 |
| AI Gateway | Higress (CNCF Sandbox) | 云原生 AI Gateway，LLM 代理 + MCP Server 托管 + Consumer 鉴权 |
| Agent 运行时 | OpenClaw, QwenPaw, Hermes 等 | 多种运行时，从 500MB 到 <10MB 内存 |
| 技能生态 | skills.sh | 80,000+ 社区技能 |
| MCP 集成 | mcporter | 通过 Gateway 安全调用 MCP Server |

值得注意的是，HiClaw 的 AI Gateway 组件 Higress 是 **CNCF Sandbox 项目**，MCP Server 托管和 Consumer 鉴权能力由 Higress 原生提供。两个项目的结合体现了云原生生态在 AI Agent 领域的延伸——Higress 解决 Agent 的安全访问问题，HiClaw 解决 Agent 的协作编排问题。

## 7. 与 Kubernetes 的设计对应关系

HiClaw 的设计深受 Kubernetes 影响，以下是核心概念的对应关系：

| Kubernetes 概念 | HiClaw 对应 | 说明 |
|----------------|------------|------|
| Pod | Worker | 最小调度单元，无状态，可销毁重建 |
| Deployment | Team | 管理一组 Worker 的期望状态 |
| Service | Matrix Room | Worker 间的通信抽象 |
| ServiceAccount + RBAC | Consumer Token + allowedConsumers | 身份认证与细粒度权限控制 |
| CRD | Worker/Team/Human/Manager | 声明式资源定义 |
| CR 短名（kubectl） | `wk` / `tm` / `hm` / `mgr` | 安装 CRD 后可用的资源别名 |
| Controller + Reconcile Loop | hiclaw-controller | 持续将实际状态收敛到期望状态 |
| kubectl apply | hiclaw apply | 声明式资源管理 CLI（`-f` 为多文档顺序 apply） |

这种设计使得熟悉 Kubernetes 的工程师可以零学习成本理解 HiClaw 的架构和运维模型。

## 8. 部署模式

协调循环见 **3.3 节**；本节仅说明**如何安装**。

### 8.1 Embedded 模式（开发者 / 小团队）

```bash
# 一键安装，包含所有基础设施
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

最低要求：2 CPU + 4 GB RAM + Docker。你会得到 **`hiclaw-controller`**（基础设施 + controller）以及独立的 **`hiclaw-manager`**；创建 Worker 后会出现更多容器。

### 8.2 In-cluster / Helm 模式（企业级 / 云上部署）

```bash
# 在克隆下来的仓库根目录执行（Chart 位于 helm/hiclaw）
helm install hiclaw ./helm/hiclaw
```

也可在配置好 Helm 仓库后使用发布的 chart 包。Chart 按 `values.yaml` 编排 **hiclaw-controller**、网关、Homeserver、存储等；Manager 与 Worker Pod 的 API 与 Embedded 安装一致。

## 9. 项目状态与路线图

- **2026-03-04**: 项目开源，Apache 2.0 协议
- **已发布**: OpenClaw/QwenPaw 多运行时支持、MCP Server 集成、Team 架构、Human 接入
- **进行中**: ZeroClaw（Rust 超轻量运行时，3.4MB）、NanoClaw（极简运行时，<4000 LOC）
- **规划中**: Team 管理中心（可视化 Dashboard）、incluster 模式 Helm Chart、NemoClaw 运行时集成

## 10. 社区与贡献

- GitHub: https://github.com/higress-group/hiclaw
- Discord: https://discord.gg/NVjNA4BAVw
- License: Apache 2.0
