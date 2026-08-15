# AgentTeams 概览

[English](../overview.md) | 中文

AgentTeams 是一个支持人类全程参与的多 Agent 协作系统。你可以通过熟悉的即时通信界面与 Manager 对话，由 Manager 创建和组织不同能力的 Worker、拆分任务、跟踪进度，并让多个 Agent 在共享上下文中协作。

AgentTeams 使用 Matrix 承载人类与 Agent 之间的通信，使用 Higress 统一代理模型与 MCP 流量，并将 Agent 配置、任务上下文和产物保存在共享对象存储中。Agent 的创建、更新和销毁由 controller 统一管理。

## 适用场景

AgentTeams 适合以下场景：

- 将前端、后端、测试、研究等不同职责交给专业 Worker 协作完成。
- 让 Manager 持续拆解和跟踪较长周期的项目任务。
- 在 Agent 工作过程中随时补充要求、检查进度或人工接管。
- 通过统一网关控制模型、MCP 工具和外部凭证的访问权限。
- 在本地单机快速体验，或在 Kubernetes 中为团队部署共享实例。

如果你只需要一次性的单 Agent 对话，且不需要角色分工、共享任务空间或人工监督，直接使用单 Agent 工具通常更简单。

## 核心角色

| 角色 | 职责 |
|---|---|
| **Human** | 通过 Matrix 客户端提出目标、查看完整协作过程，并随时介入。 |
| **Manager** | 理解目标，创建或选择 Worker，拆分和分配任务，跟踪进度并汇总结果。 |
| **Worker** | 执行具体任务。每个 Worker 可以拥有独立的角色、模型、运行时、Skills 和 MCP 配置。 |
| **Team** | 将多个 Worker 和一个 Team Leader 组织为可重复使用的协作单元。 |
| **Team Leader** | 在 Team 内协调成员、维护团队上下文并推进协作。 |

## 系统组成

| 组件 | 作用 |
|---|---|
| **agentteams-controller** | 管理 Worker、Manager、Team 和 Human 资源，以及 Agent 生命周期和相关基础设施配置。 |
| **Matrix / Tuwunel** | 承载 Human、Manager、Worker 与 Team Leader 之间的可见通信。 |
| **Element Web** | 默认提供的 Matrix Web 客户端。也可以使用其他兼容 Matrix 的客户端。 |
| **Higress** | 作为 AI/API 网关统一代理 LLM 和 MCP 流量，并实施身份认证与权限控制。 |
| **MinIO 或兼容对象存储** | 保存 Agent 工作空间、配置、共享任务上下文和任务产物。 |
| **Manager 与 Worker 容器/Pod** | 运行实际的 Agent runtime；它们与基础设施解耦，可以按需创建或替换。 |

更完整的组件关系和数据流参见[架构说明](design/architecture.md)。

## 一次协作如何进行

1. Human 在 Matrix 中向 Manager 描述目标。
2. Manager 判断使用已有 Worker、创建新 Worker，还是组织一个 Team。
3. Controller 准备 Agent 的 Matrix 身份、网关权限、共享存储配置和运行环境。
4. Manager 将任务交给 Worker 或 Team，并在 Matrix 房间中跟踪进度。
5. Worker 通过模型、Skills 和获授权的 MCP 工具执行任务，将上下文与产物写入共享存储。
6. Human 可以在执行期间补充要求、纠偏或审批。
7. Manager 汇总执行结果并向 Human 报告。

所有关键沟通都发生在 Matrix 房间中，因此人类可以看到任务如何被拆解、分配和完成。

## 两种部署方式

| | 本地部署 | Kubernetes 部署 |
|---|---|---|
| 适用场景 | 个人体验、开发测试、单机使用 | 团队共享、长期运行、生产化部署 |
| 基础设施 | 一个 embedded controller 容器承载 Higress、Tuwunel、MinIO、Element Web 和 controller | 各组件作为 Kubernetes 工作负载或外部服务运行 |
| Agent 运行环境 | Manager 和 Worker 使用独立容器 | Manager 和 Worker 使用独立 Pod |
| 生命周期管理 | Controller 通过 Docker 或 Podman 管理 Agent | Controller 通过 Kubernetes CRD 管理 Agent |
| 安装入口 | `install/agentteams-install.sh` | `helm/agentteams` Helm Chart |

第一次体验建议从[快速入门](quickstart.md)的本地部署开始。团队共享或生产环境参见 [Kubernetes 部署指南](usage/deployment/kubernetes.md)。

## Agent 运行时

Manager 当前支持：

- **CoPaw**：当前基于 QwenPaw 的 Python Manager 实现；规范配置值为 `qwenpaw`，本地安装器仍使用兼容别名 `copaw`。
- **OpenClaw**：Node.js 运行时。

Worker 资源支持以下运行时，实际可用镜像取决于安装方式和版本：

- **OpenClaw**
- **CoPaw**
- **QwenPaw**
- **Hermes**

Controller 和 Helm values 中已经包含 OpenHuman 后端与镜像配置，但当前发布的 Worker CRD enum 尚不接受显式的 `spec.runtime: openhuman`。因此在业务代码另行统一该契约前，不应把 OpenHuman 当作可直接声明的 Worker runtime。

运行时决定 Agent 的执行框架和镜像，但 Worker 的身份、Matrix 房间和持久化数据由 AgentTeams 管理。切换运行时通常会重建 Worker 运行环境，不应在 Worker 执行任务时操作。

## 资源与管理方式

AgentTeams 使用 `Worker`、`Manager`、`Team` 和 `Human` 四类声明式资源描述系统状态。你可以通过以下方式管理它们：

- 在 Matrix 中向 Manager 提出自然语言请求。
- 在 controller 或 Manager 容器中使用 `agt` CLI。
- 使用 YAML 清单和 `agt apply -f` 进行声明式管理。
- 在 Kubernetes 中直接管理 AgentTeams CRD。

字段和操作方式参见[声明式资源管理](usage/resource-management.md)。

## 安全模型

- Worker 不需要直接持有真实的模型或 MCP 服务密钥。
- Higress 使用独立身份和 Consumer 凭证控制 Agent 的模型与工具访问。
- Matrix 房间保留 Human、Manager 与 Worker 的可见协作记录。
- Agent 配置和持久化数据集中保存在对象存储中，Worker 运行环境可以被替换。
- Kubernetes 部署可以接入既有网关、对象存储和凭证提供服务。

生产环境仍应配合 HTTPS、网络策略、最小权限、Secret 管理、备份和审计机制使用。

## 从哪里开始

| 目标 | 文档 |
|---|---|
| 在本地跑通第一个任务 | [快速入门](quickstart.md) |
| 查看可复用的多 Agent 协作案例 | [AgentTeams 使用案例](usage/use-cases.md) |
| 了解本地安装的全部选项 | [本地部署指南](usage/deployment/local.md) |
| 在 Kubernetes 中部署共享实例 | [Kubernetes 部署指南](usage/deployment/kubernetes.md) |
| 理解组件与通信关系 | [架构说明](design/architecture.md) |
| 配置和使用 Manager | [Manager 指南](usage/manager-guide.md) |
| 创建、部署和维护 Worker | [Worker 指南](usage/worker-guide.md) |
| 为已有 Worker 安装 Skill | [Worker 指南：安装 Skill](usage/worker-guide.md#为-worker-安装-skill) |
| 使用 YAML 和 `agt` 管理资源 | [声明式资源管理](usage/resource-management.md) |
| 在 Windows 上安装 | [Windows 部署](usage/deployment/windows.md) |
| 排查常见问题 | [FAQ](usage/troubleshooting/faq.md) |
| 参与项目开发 | [开发指南](usage/development.md) |
