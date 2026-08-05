# 三节点 kubeadm 集群上的 AgentTeams 部署文档实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 `docs/zh-cn/local-kubernetes-deployment.md` 改写为可在当前 `agent0`、`agent2`、`agent3` kubeadm 集群上从 `agent1` 部署 AgentTeams 的专用指南。

**Architecture:** 目标文档采用“管理机准备 → 集群与存储门禁 → 源码构建 → 双 worker 镜像导入 → Helm 校验与安装 → 局域网访问 → 分层验收 → 升级、排障与卸载”的单一主线。集群事实引用 `docs/kubernetes-cluster-guide.md`，AgentTeams 参数以本地 Helm Chart 和 Makefile 为准。

**Tech Stack:** Markdown、Bash、kubectl、Helm 3、Docker、containerd/ctr、Kubernetes、Cilium、AgentTeams Helm Chart

## Global Constraints

- 文档只面向 `agent0`（`10.13.36.140`）、`agent2`（`10.13.36.138`）、`agent3`（`10.13.36.173`）和管理机 `agent1`（`10.13.36.129`）。
- Kubernetes 为 `v1.36.3`，容器运行时为 containerd `2.2.2`，CNI 为 Cilium `1.20.0`。
- 当前没有默认 StorageClass、Ingress Controller、MetalLB 或集群内镜像仓库。
- `agent0` 保持 control-plane `NoSchedule` 污点，AgentTeams 工作负载默认调度到 `agent2`、`agent3`。
- 没有可用 StorageClass 时必须停止安装；本文不代替基础设施管理员选择存储实现。
- 本地 Controller、Manager、Worker 镜像必须使用不可变 Git Tag，并导入两个 worker 的 containerd `k8s.io` namespace。
- Gateway 局域网地址固定为 `http://10.13.36.129:18080`；Higress Console 仅按需临时暴露。
- Kuboard `v4.2.0.0` 已运行于 `agent1`，局域网入口为 `http://10.13.36.129:8000/login`，并已导入 `local-k8s` 集群。
- Kuboard 只作为资源、事件、日志和 Metrics 的辅助观察入口；部署、验收和卸载仍以 Helm 与 `kubectl` 为准。
- Kuboard 的 `kuboard/kuboard-admin` ServiceAccount 持久绑定 `cluster-admin`；明文 HTTP 入口只允许受信任管理网访问。
- 不保存 kubeconfig、API Key、管理员密码、镜像仓库密码或 kubeadm Token。
- 不修改 Helm Chart、Controller、Manager、Worker 或其他运行时代码。
- 不改动或提交工作树中已有的其他未提交文件。

---

### Task 1: 重写 kubeadm 集群部署主线

**Files:**
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`
- Reference: `docs/kubernetes-cluster-guide.md`
- Reference: `helm/agentteams/values.yaml`
- Reference: `Makefile`
- Reference: `docs/superpowers/specs/2026-08-04-kubeadm-agentteams-deployment-design.md`

**Interfaces:**
- Consumes: 当前集群节点、网络、存储、入口和 containerd 事实；Chart 的 `credentials.*`、`gateway.publicURL`、`matrix.tuwunel.persistence.storageClassName`、`storage.minio.persistence.storageClassName`、`controller.image.*`、`manager.*` 和 `worker.defaultImage.*` values。
- Produces: 一份可以在 `agent1` 逐步执行、在每个不可恢复或阻塞点停止的完整中文部署指南。

- [ ] **Step 1: 建立目标文档章节骨架**

将文档重组为以下顺序，删除 kind/minikube、Docker Desktop 和集群创建内容：

```text
1. 适用环境与部署结果
2. 当前集群的限制与风险
3. 在 agent1 准备管理环境
4. 部署前集群与存储门禁
5. 从当前源码构建镜像
6. 将镜像导入 agent2 与 agent3
7. 准备并校验 Helm Chart
8. 配置 LLM 并安装
9. 等待系统收敛与分层验收
10. 从局域网访问 Element 与 Higress Console
11. 创建并验证第一个 Worker
12. 运行时选择
13. 日常运维与升级
14. 常见故障排查
15. 卸载与数据处理
16. 官方发布版替代路径
```

- [ ] **Step 2: 写入管理机和 kubeconfig 准备步骤**

说明所有仓库、Docker、Helm 和 kubectl 命令默认在 `agent1` 执行；给出以下安全准备与检查命令：

```bash
mkdir -p "${HOME}/.kube"
scp agent0@10.13.36.140:/home/agent0/.kube/config "${HOME}/.kube/config"
chmod 600 "${HOME}/.kube/config"

kubectl config current-context
kubectl cluster-info
kubectl get nodes -o wide
```

说明复制的是集群管理员 kubeconfig，只能通过受保护路径传输且不能提交到 Git。若 `agent1` 已有正确 kubeconfig，不覆盖，直接验证。

- [ ] **Step 3: 写入强制前置检查**

给出节点 Ready、污点、Cilium、资源、StorageClass 和基础镜像网络检查。要求用户显式设置并验证存储类：

```bash
export AGENTTEAMS_NAMESPACE=agentteams-system
export AGENTTEAMS_RELEASE=agentteams
export AGENTTEAMS_STORAGE_CLASS=storage-class-name

kubectl get storageclass "${AGENTTEAMS_STORAGE_CLASS}"
kubectl get nodes -o wide
kubectl describe node agent0 | sed -n '/Taints:/p'
kubectl get pods -n kube-system -l k8s-app=cilium -o wide
kubectl top nodes
```

明确当前 `kubectl get storageclass` 应为空，因此在安装存储供应器并完成 PVC 绑定/故障恢复验证前不得执行 Helm 安装。说明本地卷在节点故障后不能自动跨节点接管，不把单 control-plane、单副本 Tuwunel/MinIO 描述为高可用。

- [ ] **Step 4: 改写源码构建和双 worker 镜像分发**

保留现有 OpenClaw 及可选 Runtime 构建说明，使用：

```bash
export DOCKER_BUILDKIT=1
export AGENTTEAMS_LOCAL_TAG="dev-$(git rev-parse --short HEAD)"

make VERSION="${AGENTTEAMS_LOCAL_TAG}" build-manager build-worker
```

将 kind 导入步骤替换为可审计的归档、复制和 containerd 导入流程。归档固定写入 `/tmp/agentteams-images-${AGENTTEAMS_LOCAL_TAG}.tar`：

```bash
export AGENTTEAMS_IMAGE_ARCHIVE="/tmp/agentteams-images-${AGENTTEAMS_LOCAL_TAG}.tar"

docker save \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/worker-agent:${AGENTTEAMS_LOCAL_TAG}" \
  -o "${AGENTTEAMS_IMAGE_ARCHIVE}"

for AGENTTEAMS_NODE in agent2@10.13.36.138 agent3@10.13.36.173; do
  scp "${AGENTTEAMS_IMAGE_ARCHIVE}" "${AGENTTEAMS_NODE}:${AGENTTEAMS_IMAGE_ARCHIVE}"
  ssh "${AGENTTEAMS_NODE}" \
    "sudo ctr -n k8s.io images import '${AGENTTEAMS_IMAGE_ARCHIVE}'"
  ssh "${AGENTTEAMS_NODE}" \
    "sudo ctr -n k8s.io images list | grep '${AGENTTEAMS_LOCAL_TAG}'"
done
```

两个 worker 都验证成功后才继续；说明添加第三个可调度节点或构建可选 Runtime 时也必须导入对应镜像。

- [ ] **Step 5: 改写 Helm 校验与安装命令**

先使用 `helm repo add higress.io https://higress.io/helm-charts --force-update` 和 `helm repo update` 登记依赖仓库，再执行 `helm dependency build`。保留 `helm lint`、`helm template` 和 LLM preflight 说明。所有命令显式设置：

```text
gateway.publicURL=http://10.13.36.129:18080
matrix.tuwunel.persistence.storageClassName=${AGENTTEAMS_STORAGE_CLASS}
storage.minio.persistence.storageClassName=${AGENTTEAMS_STORAGE_CLASS}
controller.image.pullPolicy=Never
```

显式覆盖 Controller、Manager、Worker 的本地 repository 和不可变 Tag。说明 Chart 没有 `manager.image.pullPolicy` 或 `worker.defaultImage.*.pullPolicy` 字段，Controller 创建的 Manager/Worker Pod 在代码中使用 `IfNotPresent`；因此两个 worker 上必须预先存在完全匹配的镜像名和 Tag。保留交互读取 LLM Key/管理员密码与安装后 `unset`，并说明 Helm 参数可能短暂出现在本机进程列表。

- [ ] **Step 6: 改写访问与验收流程**

把 Gateway 的转发改为在 `agent1` 运行：

```bash
kubectl port-forward --address 10.13.36.129 \
  -n "${AGENTTEAMS_NAMESPACE}" svc/higress-gateway 18080:80
```

Element 和 Matrix 验证地址使用 `http://10.13.36.129:18080`。Console 使用 `10.13.36.129:18081` 临时转发，说明两个端口已对局域网开放，应通过主机防火墙限制来源；Console 不对公网开放。

验收顺序必须覆盖：PVC 为 `Bound`、Tuwunel/MinIO/Higress Ready、Controller Available、`Manager/default` Ready、Element 登录、Matrix versions 路由、首个 Worker 的 CR 状态/房间/模型调用。

- [ ] **Step 7: 改写升级、排障和卸载**

升级时构建新 Git Tag、重新生成镜像归档、导入两个 worker、执行 Helm upgrade，并显式 patch 已存在的 Manager/Worker CR 镜像。排障增加：

```text
StorageClass 或 PVC Pending
Controller 镜像缺失导致 ErrImageNeverPull
Manager/Worker 镜像只导入一个 worker 导致 ImagePullBackOff
ctr 导入到了错误 namespace
agent1 port-forward 地址或防火墙错误
gateway.publicURL 与浏览器 Origin 不一致
```

卸载只删除 AgentTeams release，并提醒备份 MinIO/Tuwunel 数据。删除 `kind delete cluster`；CRD 和 PVC/底层卷删除仍需单独确认。

- [ ] **Step 8: 检查正文差异**

Run:

```bash
git diff -- docs/zh-cn/local-kubernetes-deployment.md
```

Expected: 只呈现目标文档改写；没有对其他工作树文件的修改。

---

### Task 2: 补充 Kuboard 辅助管理说明

**Files:**
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`
- Reference: `docs/kubernetes-cluster-guide.md`
- Reference: `/home/agent1/sealos/deploy/kuboard-v4/README.md`

**Interfaces:**
- Consumes: 已部署 Kuboard 的版本、入口、集群导入方式、RBAC 权限和兼容性边界。
- Produces: 不重复 Kuboard 运维手册、但能让 AgentTeams 部署者安全使用现有管理入口的精简说明。

- [ ] **Step 1: 在环境概况中记录 Kuboard**

把 `agent1` 的用途扩展为源码构建、Helm/kubectl、临时 AgentTeams 入口和 Kuboard 宿主机，并在环境说明中写明：

```text
Kuboard v4.2.0.0 已通过 Secret Token 导入 local-k8s，入口为 http://10.13.36.129:8000/login。
```

- [ ] **Step 2: 新增 Kuboard 辅助观察小节**

在“在 agent1 准备管理环境”中新增小节，包含以下准确边界：

```text
- 可用来查看 Namespace、Pod、PVC、事件、日志和 Metrics，辅助定位调度、镜像、存储与资源问题。
- AgentTeams 的部署、升级、验收和卸载命令仍以本文的 Helm/kubectl 命令为准。
- Kuboard 不会提供 StorageClass、Ingress Controller 或 MetalLB，不能绕过存储门禁。
- kuboard/kuboard-admin ServiceAccount 持久绑定 cluster-admin，对整个集群拥有完全管理权限。
- 当前入口是局域网明文 HTTP，只能在受信任管理网使用；禁止在本文记录管理员密码、Token 或导入 kubeconfig。
```

仓库内继续链接 `docs/kubernetes-cluster-guide.md`，另以普通代码路径注明详细运维说明位于 `agent1` 的 `/home/agent1/sealos/deploy/kuboard-v4/README.md`，不创建主机绝对路径 Markdown 链接。

- [ ] **Step 3: 在日常检查中标注 Kuboard 的辅助角色**

在 `kubectl get`、`kubectl logs` 命令后说明同样的信息可以通过 Kuboard 观察，但变更、升级和删除仍按命令行流程执行，以保留可审计命令和错误输出。

- [ ] **Step 4: 检查 Kuboard 内容没有扩展为重复运维手册**

Run:

```bash
rg -n 'Kuboard|10\.13\.36\.129:8000|local-k8s|kuboard-admin|cluster-admin|StorageClass|kubectl|Helm' docs/zh-cn/local-kubernetes-deployment.md
```

Expected: Kuboard 版本、入口、集群名、权限和职责边界均存在；正文不复制 Docker Compose 启停、MariaDB 备份或紧急撤权命令。

---

### Task 3: 静态验证并提交目标文档

**Files:**
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`（仅在检查发现问题时修正）
- Test: `docs/zh-cn/local-kubernetes-deployment.md`

**Interfaces:**
- Consumes: Task 1 生成的完整指南和 Task 2 生成的 Kuboard 辅助管理说明。
- Produces: 通过文本、格式、链接、Chart value 和 Git 范围检查的最终文档。

- [ ] **Step 1: 验证旧平台命令已移除**

Run:

```bash
if rg -n 'kind create|kind load|kind delete|Docker Desktop|minikube' docs/zh-cn/local-kubernetes-deployment.md; then
  exit 1
fi
```

Expected: 命令无匹配并以状态 0 完成外层检查逻辑。

- [ ] **Step 2: 验证集群专用事实和必要参数存在**

Run:

```bash
rg -n 'agent0|agent1|agent2|agent3|10\.13\.36\.129|10\.13\.36\.140|AGENTTEAMS_STORAGE_CLASS|ctr -n k8s\.io|gateway\.publicURL|storageClassName|controller\.image\.pullPolicy=Never|IfNotPresent' docs/zh-cn/local-kubernetes-deployment.md
```

Expected: 每类事实至少出现一次，镜像导入、存储类和公共 URL 参数均在可执行命令中出现。

- [ ] **Step 3: 验证引用的本地文件和 Chart keys**

Run:

```bash
test -f docs/kubernetes-cluster-guide.md
test -f docs/zh-cn/repository-architecture-analysis.md
rg -n '^  storageClassName:|^manager:|^  defaultImage:|^    pullPolicy:' helm/agentteams/values.yaml
```

Expected: 两个引用文件存在，values 搜索能找到持久卷、Manager、Worker 默认镜像和拉取策略配置。

- [ ] **Step 4: 验证 Kuboard 说明与运维记录一致**

Run:

```bash
rg -n 'v4\.2\.0\.0|http://10\.13\.36\.129:8000/login|local-k8s|kuboard/kuboard-admin|cluster-admin|不.*替代.*kubectl|不.*StorageClass' docs/zh-cn/local-kubernetes-deployment.md
```

Expected: 每类事实至少出现一次；Kuboard 被描述为辅助入口，不被描述为 AgentTeams 依赖或基础设施供应器。

- [ ] **Step 5: 验证 Markdown 和 Git 差异**

Run:

```bash
git diff --check -- docs/zh-cn/local-kubernetes-deployment.md
git status --short
```

Expected: `git diff --check` 无输出；状态中原有用户改动保持不变，实施仅新增或修改目标文档。

- [ ] **Step 6: 人工复核命令上下文**

逐个代码块确认运行主机明确：Docker/Helm/kubectl 在 `agent1`，`ctr` 在 `agent2`/`agent3`。确认变量在首次使用前已定义，续行反斜杠正确，所有 URL 与 `gateway.publicURL` 一致，卸载段没有删除 kubeadm 集群。

- [ ] **Step 7: 提交目标文档**

```bash
git add docs/zh-cn/local-kubernetes-deployment.md
git commit -m "docs: adapt local Kubernetes deployment to kubeadm cluster"
```

提交前用 `git diff --cached --name-only` 确认暂存区只有 `docs/zh-cn/local-kubernetes-deployment.md`。
