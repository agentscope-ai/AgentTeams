# HiClaw: 基于 Kubernetes 原生的多 Agent 协作编排系统

当单个 AI Agent 的对话上下文与工具调用已经无法覆盖真实业务复杂度时，工程上很自然的一步是「多 Agent」——让多个自主 Agent 分工协作。但简单地拉起多个运行时并不等于拥有一个**团队**：缺少组织结构，就无法稳定地做任务委派；缺少通信策略，就无法在多角色之间保持可控与可审计；缺少共享状态与统一网关策略，就很难在安全前提下接入 LLM 与 MCP。

HiClaw 要做的，正是把「多 Agent 协作」做成一层**可声明、可调和、可运维**的控制平面：借鉴 Kubernetes 的声明式 API、Controller Reconcile、CRD 扩展思路，把 Worker、Team、Human、Manager 当成一等资源来描述；由 `hiclaw-controller` 持续把实际基础设施与通信拓扑收敛到 YAML 里的期望状态。

下文从技术视角概括 HiClaw 的定位、核心组件与设计取舍，便于把它放进你对「云原生 AI Agent 基础设施」的整体图景里。

---

## 1. 编排 vs 协作：多 Agent 的两类问题

行业里常说的「多 Agent」至少有两种完全不同的诉求：

- **编排（Orchestration）**：谁来启动、销毁、升级 Agent，资源与镜像如何管理——本质是**生命周期与运行时隔离**。
- **协作（Collaboration）**：谁向谁汇报、谁能 @mention 谁、任务如何在角色之间拆分与汇总、共享产物放在哪里——本质是**组织结构 + 通信边界 + 共享状态**。

Kubernetes 解决的是前者（工作负载如何在大规模基础设施上以期望形态运行）；HiClaw 并不替代 OpenClaw / CoPaw 等 Agent 运行时，而是在其上补充**协作语义**：谁属于哪个 Team、谁能进入哪个 Matrix Room、Human 的三级权限如何映射到网关侧的策略生成。

---

## 2. 控制平面长什么样：声明式 CRD 与 Reconcile

HiClaw 对外采用统一的 API 版本：`apiVersion: agentteams.io/v1beta1`。当前四类核心资源为：

| 资源 | 一句话 |
|------|--------|
| **Worker** | 最小执行单元：容器/Pod + Matrix 账号 + MinIO 空间 + Gateway Consumer |
| **Team** | 一名 Team Leader 与多名成员：附带 Team Room、Leader Room、私聊通道等拓扑 |
| **Human** | 真人账号与权限级别（L1/L2/L3），驱动 Room 邀请与 Agent 侧 `@mention` 白名单 |
| **Manager** | 协调 Agent：模型、人格与技能包、心跳与空闲策略等，与其它 CR 同一套调和闭环 |

典型 Worker 片段如下（字段以仓库内 CRD 为准）：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: qwen3.5-plus
  runtime: openclaw          # 或 copaw
  skills: [github-operations]
  mcpServers: [github]
  # state: Running | Sleeping | Stopped —— 期望生命周期，由 Controller 收敛
  # channelPolicy —— 在默认策略上增减 group/DM 允许或拒绝列表
```

Team 资源要求给出 `leader` 与 `workers`；可选 `peerMentions`、`channelPolicy`、`admin`（团队级人类管理员）。**Team Room 不包含 Manager**，协调边界停在 Leader——这与「Manager 只对接 Leader、不穿透团队」的产品约束一致。

声明式入口方面：主机侧常用 `install/hiclaw-apply.sh` 将 YAML 拷入 Manager 容器并执行 `hiclaw apply -f`。CLI **按 YAML 文档顺序**调用 REST API（`/api/v1/workers`、`/teams`、`/humans`、`/managers`），**不会**自动解析依赖顺序——例如应先定义 `Team`，再定义引用 `accessibleTeams` 的 `Human`。这与 Kubernetes 里「多个清单文件的应用顺序仍由提交者负责」是同一类心智模型。

安装 CRD 后，kubectl 可使用短名：`wk`、`tm`、`hm`、`mgr`。

---

## 3. 运行时拓扑：不是 RPC，而是 Matrix Room

HiClaw 选用 **Matrix** 作为 Agent 与人类之间的传输层，而不是自建一对一双向 RPC：

- 会话发生在 **Room** 里，天然适合「多方在场、人类旁观、事后审计」。
- Human-in-the-loop 不需要额外通道：同一套客户端即可 @mention 任意被授权的角色。

Homeserver 侧可采用内置的 **Tuwunel**，以降低单机/小团队部署的成本。

与此并行的是配置与产物：**MinIO**（S3 兼容）承载每个 Agent 的配置空间、技能文件以及团队共享的任务规格与结果——容器本身仍可视为无状态，重建 Worker 不等于丢失「逻辑状态」，只要对象存储中的期望规格仍在。

---

## 4. 安全模型：凭证留在网关，Agent 只拿 Consumer Token

LLM 与 MCP 的密钥不应散落在每个 Agent 容器里。HiClaw 通过 **Higress**（基于 Envoy 的云原生网关，CNCF Sandbox）把上游真实密钥留在网关侧，Worker 仅携带可在控制面吊销的 **Consumer Token**；路由上通过 `allowedConsumers` 等机制做**按 Agent 区分**的授权。  
这与「每个 Pod 持 ServiceAccount、真正权限由 RBAC 与策略决定」的 Kubernetes 安全故事是同构的：身份与能力在数据面解耦。

MCP 侧同样走集中暴露的端点，由网关完成鉴权与凭据注入，配合 **mcporter** 等调用方式，使「谁可以调哪个 MCP」成为可策略化、可审计的能力，而不是每个沙箱各配一套 PAT。

---

## 5. 与「强沙箱单 Agent」方案如何相处

以 **NVIDIA NemoClaw** 为代表的能力，更强调**单 Agent 的 OS 级隔离与凭证路由**；HiClaw 更强调**多角色协作与组织级策略**。二者不是替代关系：

- NemoClaw：把**一个** Agent 关在强沙箱里——适合安全敏感的单任务执行面。
- HiClaw：把**多个** Agent 放进可声明的组织与通信结构里——适合「像团队一样干活」的控制面。

长期来看，Worker Backend 抽象允许在「协作编排」之下替换或增强底层运行时（包括对更强沙箱的接入），类比 Kubernetes 通过 CRI 对接多种容器运行时。

---

## 6. 部署形态与控制面存放

常见两种路径：

- **Embedded**：本地容器栈，状态可经 kine（SQLite 后端）等与 etcd 兼容的存储衔接 Controller 期望；Worker 常以 Docker 形态运行。
- **Incluster**：CR 写入 Kubernetes etcd，Controller 以 Deployment 等形式运行，Worker 以 Pod 调度；仓库提供 **Helm Chart**（如 `helm/hiclaw`）编排依赖组件。

两种模式共享同一套 Reconcile 语义，差异主要在状态存储与进程编排后端。

---

## 7. 小结

HiClaw 把「多 Agent 协作」从脚本与约定提升为 **CRD 风格的声明式 API + Controller 持续调和**：组织结构、通信权限、网关侧 LLM/MCP 策略与共享存储可以在一条流水线上治理。若你已在 Kubernetes 生态里做过运维，这套心智模型会非常熟悉；若你正在评估下一代 Agent 平台架构，可以把 HiClaw 理解为**协作编排控制平面**，而把具体推理与工具执行留在可选的 Agent 运行时与网关实现之下。

更完整的字段说明、Room 拓扑与运维命令见项目文档（例如 `docs/zh-cn/declarative-resource-management.md` 与 `docs/zh-cn/k8s-native-agent-orch.md`）。

---

**License**：Apache 2.0 · **仓库**：<https://github.com/higress-group/hiclaw>
