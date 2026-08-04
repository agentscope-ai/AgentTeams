# 三节点 kubeadm 集群上的 AgentTeams 部署文档改写设计

## 目标

将 `docs/zh-cn/local-kubernetes-deployment.md` 从单机 kind 开发环境指南改写为当前局域网 kubeadm 集群的专用部署指南。文档应让维护者能够从 `agent1` 上的源码工作区构建并部署 AgentTeams，同时明确现有集群的能力边界与数据风险。

## 适用环境

- `agent0`：`10.13.36.140`，单 control-plane，保留 `NoSchedule` 污点；
- `agent2`：`10.13.36.138`，worker；
- `agent3`：`10.13.36.173`，worker；
- `agent1`：`10.13.36.129`，源码构建、Helm 和局域网访问入口所在的管理机；
- Kubernetes `v1.36.3`，containerd `2.2.2`，Cilium `1.20.0`；
- 当前没有默认 StorageClass、Ingress Controller、MetalLB 或集群内镜像仓库。

集群的权威环境记录仍是 `docs/kubernetes-cluster-guide.md`。目标文档只提取与 AgentTeams 部署直接相关的事实，并链接该记录，避免重复完整的集群运维内容。

## 文档主线

1. 在 `agent1` 配置并验证到 `agent0` API Server 的 kubeconfig。
2. 检查三节点 Ready、Cilium、资源、节点污点、StorageClass 和镜像仓库连通性。
3. 把可用 StorageClass 设为安装硬门槛。文档不替基础设施管理员选择 NFS CSI、Longhorn、Rook-Ceph 或本地卷；必须先安装、验证并通过变量显式指定 StorageClass。说明本地卷不具备跨节点故障转移能力。
4. 在 `agent1` 从当前源码构建带不可变 Git Tag 的 Controller、Manager 和 Worker 镜像。
5. 因集群没有私有仓库，将镜像保存为归档并复制到 `agent2`、`agent3`，再导入 containerd 的 `k8s.io` namespace。两个 worker 都必须导入，以支持重新调度；control-plane 保持不调度业务 Pod，因此默认不导入。
6. 构建 Helm 依赖并执行 lint/template 检查。
7. 使用本地 Chart 安装，显式覆盖镜像、Tag、拉取策略、StorageClass、LLM 凭证和公共 Gateway URL。公共 URL 使用 `http://10.13.36.129:18080`。
8. 在 `agent1` 使用绑定到管理地址的 `kubectl port-forward` 暴露 Higress Gateway；Console 仅在需要时临时暴露，并提醒限制局域网来源。
9. 按基础设施、Controller、Manager、Worker 分层验证部署。
10. 保留运行时选择、升级、故障排查和卸载内容，但把镜像升级改成重新导入两个 worker，并删除 kind 集群生命周期命令。

## 安全与失败处理

- 不在文档中保存 kubeconfig、API Key、管理员密码、镜像仓库密码或 kubeadm Token。
- 复制 kubeconfig 后要求设置 `0600` 权限；说明该 kubeconfig 具有集群管理员权限。
- LLM 密钥和管理员密码通过交互式环境变量读取；保留 Helm 参数可能短暂暴露在进程列表中的警告。
- 无 StorageClass、节点未 Ready、镜像只存在一个 worker、LLM preflight 失败时，不继续安装。
- `port-forward --address 10.13.36.129` 会把端口开放给局域网，不将 Console、Controller、Tuwunel 或 MinIO 管理端口暴露到公网。
- 卸载 AgentTeams 不删除 kubeadm 集群；删除 PVC、CRD 或底层卷前先备份并确认影响。
- 明确当前单 control-plane 和单副本 Tuwunel/MinIO 不是高可用部署。

## 验证标准

- 文档中不再出现 kind/minikube 集群创建、`kind load`、Docker Desktop 或 `kind delete` 操作。
- 所有本地业务镜像均能在 `agent2`、`agent3` 的 containerd `k8s.io` namespace 中查询到。
- Helm 渲染显式设置两个持久卷的 StorageClass，并使用当前源码对应的不可变 Tag。
- `gateway.publicURL` 与局域网实际访问地址一致。
- 验证步骤覆盖 PVC Bound、核心 Pod Ready、Manager CR/Pod Ready、Element 登录、Matrix 路由和首个 Worker 调用模型。
- 文档中的相对链接、标题层级、shell/YAML 代码块和变量名通过静态检查。

## 非目标

- 不在本文安装或选型生产级存储、Ingress、MetalLB、私有镜像仓库、TLS 或监控系统。
- 不改变 Helm Chart、Controller 或任何运行时实现。
- 不把该单控制平面集群描述为生产高可用环境。
