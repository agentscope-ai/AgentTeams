# HiClaw v1.1.0：从个人工具到企业级多 Agent 协作平台

> 发布日期：2026年4月24日

---

v1.1.0 是 HiClaw 自开源以来变化最大的一个版本。我们重写了整个控制平面，引入了第三种 Agent 运行时，全面升级了底层引擎，并确保从 v1.0.9 的升级路径平滑无感。这篇文章聊聊背后的思考和一些关键变化。

---

## Agent 大升级：三种运行时，全面验证

HiClaw 从第一天起就支持多种 Agent 运行时——不同的任务适合不同的 Agent，这是多 Agent 协作的基本前提。在 v1.1.0 中，我们对三种运行时都做了重要升级。

### 全新 Hermes 运行时：引入自我进化的自主编程 Agent

我们引入了 **Hermes**（[hermes-agent](https://github.com/NousResearch/hermes-agent)，由 Nous Research 开发）作为第三种 Worker 运行时。

Hermes 不是另一个聊天机器人。它是一个**自主编程 Agent**——可以在隔离容器中独立规划、执行和迭代复杂的软件任务。更关键的是，Hermes 具备**自我进化能力**：它会在任务完成后自动创建可复用的 Skill，Skill 在使用过程中自我改进，跨会话的 FTS5 记忆检索让 Agent 随着使用越来越了解你的项目。用 Nous Research 的话说，它是"the agent that grows with you"。

### Leader + Worker：确定性 Agent 指挥自主 Agent

引入 Hermes 后，一个很有意思的架构模式浮现出来：**用确定性更高的 Agent 做 Leader，指挥 Hermes 做重活**。

这不是我们发明的——社区里已经有不少讨论和实践。Dev.to 上有一篇被广泛引用的[文章](https://dev.to/ggondim/how-i-built-a-deterministic-multi-agent-dev-pipeline-inside-openclaw-and-contributed-a-missing-4ool)，作者花了两个月时间探索自主编程 Agent 的编排问题，最终得出的结论是：**"Deterministic orchestration, where LLMs do creative work and YAML workflows handle the plumbing"**——让 LLM 做创造性工作，用确定性流程做管道。AWS 在介绍多 Agent 系统时也专门讨论了 [Agents as Tools 模式](https://dev.to/aws/build-multi-agent-systems-using-the-agents-as-tools-pattern-jce)——将分层委派（hierarchical delegation）引入多 Agent 编排。学术圈也在研究类似方向，[EvoAgent](https://arxiv.org/html/2604.20133) 提出了三层委派路由机制，让 Agent 在开放环境中自主获取和持续优化技能。

在 HiClaw 中，这个模式天然落地：

- **Manager（agent/QwenPaw 运行时）** 作为 Leader，负责任务分解、Worker 调度、进度监控——这些需要**确定性**和**可预测性**
- **Hermes Worker** 作为执行者，负责实际的代码编写、调试、项目级任务——这些需要**自主性**和**创造力**
- Manager 不会替 Hermes 写代码，Hermes 也不需要理解整个团队的调度逻辑——各司其职

Worker 可以随时切换运行时：

```bash
hiclaw update worker --runtime hermes  # 容器重建，Matrix 账号、房间和数据保留
```

多 Agent 协作完全打通——Hermes Worker 可以和 agent、QwenPaw Worker 一起参与团队项目，支持跨运行时 `m.mentions` 消息投递和无人值守的 YOLO 模式自主执行。

### openclaw 与 QwenPaw 同步升级

底层引擎也做了大版本升级：

- **openclaw** 升级到 `2026.4.14`，带来了 Matrix 私有网络安全修复、结构化调试日志（`HICLAW_MATRIX_DEBUG=1`）、网关 Control UI 端口统一等一系列改进
- **QwenPaw** 升级到 `1.0.2`
- **openclaw-base 基础镜像**从 `higress/all-in-one`（~1.79 GB）重置为 `higress/ubuntu:24.04`（~103 MB），**所有下游镜像瘦身约 1.7 GB**

这些升级听起来是"基础设施层面的事"，但实际上直接影响了 Agent 的稳定性——Matrix 连接竞态、房间加入失败、Control UI 不可访问等一系列 v1.0.x 中的偶发问题，在这次升级中一并解决了。

### E2E 集成测试全覆盖

很重要的一点：**以上所有升级都经过了 HiClaw 的多 Agent 端到端集成测试验证**。我们不是简单升级依赖版本然后祈祷没问题——每一种运行时的 Worker 创建、团队协作、消息投递、YOLO 模式、跨运行时通信，都有自动化测试覆盖。升级是放心的。

---

## 从个人到企业：Kubernetes 原生架构

v1.1.0 最核心的架构变化是引入了 **hiclaw-controller**，一个 Kubernetes 原生的控制平面。

### 为什么重构控制平面？

v1.0.x 的架构是一个 "all-in-one" 容器——Manager、Higress 网关、Matrix 服务器、MinIO、Element Web 全部塞在一个镜像里。个人用用没问题，但：

- **重启隔离差**：任何一个组件出问题，整个容器重启，所有 Agent 中断
- **无法水平扩展**：Manager 只能跑一个实例
- **资源浪费**：一个只需要跑 Agent 的容器却要背着 1.7GB 的基础设施
- **多租户不支持**：无法做租户隔离和资源配额

### 两种部署模式，一套代码

v1.1.0 同时支持两种部署模式，共享同一套 Controller 代码：

**Embedded 模式（个人开发者）**

```bash
# 一行命令安装，无需 Kubernetes 集群
bash -c "$(curl -fsSL https://get.hiclaw.ai)"
```

底层是一个轻量级的 embedded kube-apiserver + kine，对外表现就是一个 `hiclaw-controller` 容器 + 一个 `hiclaw-manager` 容器。不依赖任何外部 Kubernetes 集群，部署体验和 v1.0.x 一样简单。

**Helm Chart 模式（企业生产）**

```bash
helm install hiclaw ./helm/hiclaw -n hiclaw
```

同一个 Controller 跑在真正的 Kubernetes 集群中，提供：
- **Leader Election 高可用**：多副本部署，基于 Lease 的自动故障切换
- **Agent Pod Template**：通过 ConfigMap 叠加注入 nodeSelector、tolerations、imagePullSecrets，无需修改 Controller 代码
- **多租户隔离**：可插拔凭证提供者 Sidecar（`hiclaw-credential-provider`），per-worker `accessEntries` 限定对象存储路径
- **CRD 化管理**：`kubectl get workers` 可以直接用，`hiclaw` CLI 和 `kubectl` 完全可互换

### 声明式协调，稳定性的基石

无论哪种模式，核心都是**声明式配置协调**（Controller-Reconciler 模式）：

```
Worker CR (期望状态)  →  Controller 观测差异  →  协调到一致
Team CR              →  Matrix 房间、网关路由  →  协调到一致
Manager CR           →  容器、配置文件         →  协调到一致
```

这意味着：
- **任何组件异常都会自动恢复**——Controller 每 5 分钟协调一次，配置漂移会被自动纠正
- **Token 不再轮转**——之前每次协调都重新生成 Matrix access token 和网关密钥，导致 Agent 频繁重启、消息丢失。v1.1.0 中 Token 持久化复用
- **配置文件不再被覆盖**——`AGENTS.md`、`SOUL.md`、`HEARTBEAT.md` 等文件由各自的权威写入者管理，不会被协调过程中的 mirror 覆盖

这些看起来是细节，但稳定性就是由这些细节构成的。个人用户和企业用户都受益于同一套协调机制。

### 从 v1.0.9 自动迁移

升级非常简单：`hiclaw-controller` 在首次启动时检测到 v1.0.9 的 `workers-registry.json`，自动将 Worker 信息迁移为 CRD 资源。运行时、模型、技能、MCP Server、团队成员关系——全部保留，零配置。

---

## hiclaw CLI：用 Go 替代 Shell 脚本

如果你看一下 HiClaw 仓库的语言占比，会发现一个有意思的趋势：

> **Go（38%）> Shell（35%）> Python（13%）**

在 v1.0.x 时代，Shell 是占比最大的语言。大量的 `setup-*.sh`、`create-*.sh`、`entrypoint-*.sh` 脚本构成了 HiClaw 的操作层。这在早期够用，但问题越来越多：

### 脚本的痛点

**1. Agent 会"读"脚本，浪费 Token**

这是很多人没想到的成本来源。当 Agent 需要创建一个 Worker 时，它可能会先 `cat create-worker.sh` 看看脚本里有什么参数，然后试着跑一下发现参数不对，再读一遍……一个简单的"创建 Worker"操作，Agent 可能消耗 **20+ 轮 LLM 调用**，其中一半在探索脚本接口。

**2. 测试不健全**

Shell 脚本很难写单元测试。脚本改了一行，没人知道会不会影响其他路径。v1.0.x 的一些"默认值不生效"、"参数被忽略"的 bug 就是这么来的。

**3. 输出格式不稳定**

同一个脚本在不同情况下可能输出完全不同的格式。Agent 解析输出失败，重试，又浪费 Token。

### hiclaw CLI 的改进

`hiclaw` CLI 用 Go 重写后解决了这些问题：

```bash
# 创建 Worker
hiclaw create worker --name alice --model qwen-max

# 查看 Worker 列表
hiclaw get workers

# Worker 生命周期管理
hiclaw worker sleep alice   # 优雅停止
hiclaw worker wake alice    # 按需唤醒

# 声明式配置
hiclaw apply worker -f alice.yaml
```

- **结构化输出**：支持 JSON / YAML / 表格格式，Agent 解析零失败
- **参数明确**：`--help` 一目了然，Agent 不需要再读源码猜参数
- **完善的测试体系**：Go 的单元测试 + 集成测试，每个命令都有覆盖
- **Controller 内置**：`hiclaw` CLI 预装在 Controller 容器中，管理员可以直接 `docker exec` 操作

从实际效果看，使用 `hiclaw` CLI 创建一个 Worker 的 LLM 轮次从 **22 轮降到了 10 轮以内**——Token 成本直接减半。

---

## 更多改进

### 镜像瘦身 1.7 GB

Manager 镜像不再打包 Higress、Matrix、MinIO、Element Web，只保留纯 Agent 运行时。基础设施服务跑在独立的 `hiclaw-embedded` 镜像中。从 1.79 GB 到 103 MB。

### 首次启动体验

全新安装后自动发送欢迎/引导消息到管理员私信，安装器会等待欢迎消息发送完成。第一次交互就是顺畅的。

### 可插拔网关与存储

Controller 通过 Provider 接口委托网关和存储操作。阿里云 OSS、AWS S3、MinIO——后端可以随时切换，不改一行 Controller 代码。

---

## 升级指南

从 v1.0.9 升级只需重新运行安装脚本：

```bash
bash -c "$(curl -fsSL https://get.hiclaw.ai)"
```

安装器会自动检测 v1.0.9 状态、迁移数据到 CRD、拉取新镜像。所有 Worker 配置自动保留。

---

*HiClaw 是 [AgentScope](https://github.com/agentscope-ai) 旗下的开源多 Agent 管理协作平台，基于 Higress、Matrix 和 agent 构建。欢迎在 [GitHub](https://github.com/agentscope-ai/HiClaw) 上关注我们，加入 [Discord](https://discord.gg/n6mV8xEYUF) 社区讨论。*
