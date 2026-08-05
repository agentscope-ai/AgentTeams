# AgentTeams 三节点 kubeadm 集群部署指南

> 本指南面向 `agent0`、`agent2`、`agent3` 组成的局域网 kubeadm 集群，并使用 `agent1` 作为源码构建和管理机。集群的完整配置、能力边界和运维要求见[《Kubernetes 集群部署与运维指引》](../kubernetes-cluster-guide.md)，AgentTeams 架构背景见[《仓库架构深度解析》](repository-architecture-analysis.md)。

## 1. 适用环境与部署结果

本文按以下已验证环境编写：

| 主机 | 地址 | 角色 | 本文用途 |
| --- | --- | --- | --- |
| `agent0` | `10.13.36.140` | Kubernetes control-plane | API Server；保留 `NoSchedule` 污点 |
| `agent2` | `10.13.36.138` | Kubernetes worker | 运行 AgentTeams 工作负载 |
| `agent3` | `10.13.36.173` | Kubernetes worker | 运行 AgentTeams 工作负载 |
| `agent1` | `10.13.36.129` | 集群外管理机 | 保存源码、构建镜像、运行 Helm/kubectl、提供临时局域网入口并承载 Kuboard |

集群组件为 Kubernetes `v1.36.3`、containerd `2.2.2` 和 Cilium `1.20.0`。安装完成后会得到：

- Higress Gateway 与 Console；
- Tuwunel Matrix Homeserver；
- MinIO 与持久卷；
- Element Web；
- AgentTeams Controller；
- Controller 自动创建的 `Manager/default` CR 与 Manager Pod；
- 后续按需创建的 Worker、Team、Human CR 与 Pod。

管理机上另有 Kuboard `v4.2.0.0`，已通过 Secret Token 导入 `local-k8s` 集群，局域网入口为 <http://10.13.36.129:8000/login>。Kuboard 是现有集群管理工具，不是 AgentTeams 的依赖或安装产物。

本文主线从当前源码构建 Controller、OpenClaw Manager 和 OpenClaw Worker 镜像，再将镜像直接导入两个 worker 的 containerd。若只希望使用稳定发布版，可跳到[第 16 节](#16-官方发布版替代路径)。

## 2. 当前集群的限制与风险

开始前必须了解以下限制：

1. `agent0` 是唯一 control-plane，也是单节点 etcd。它不可用时，Kubernetes API、调度和控制器会中断。
2. 当前没有默认 StorageClass 或动态存储供应器。未解决存储问题时，Tuwunel 和 MinIO PVC 会一直处于 `Pending`。
3. 当前没有 Ingress Controller、MetalLB 或裸机 LoadBalancer 实现。本文使用 `agent1` 上的 `kubectl port-forward` 提供局域网入口。
4. 当前没有集群内私有镜像仓库。本地镜像必须导入 `agent2` 和 `agent3`；只导入一个节点会让重新调度后的 Pod 拉取失败。
5. Tuwunel、MinIO 和默认 Higress 组件均以单副本为主。即使底层存储可用，这也不是高可用部署。
6. 本地卷通常绑定具体节点，节点故障后不能自动在另一节点接管。不要把“PVC 已绑定”误认为“数据已经高可用”。

生产或长期使用前，应另外完成存储故障恢复、入口 TLS、备份、监控告警和 control-plane 高可用设计。

## 3. 在 agent1 准备管理环境

除明确标注为“在 worker 执行”的命令外，本文命令均在 `agent1` 的仓库根目录 `/home/agent1/CsAgnet` 执行。

### 3.1 安装工具

`agent1` 需要：

- Docker Engine 24+，并启用 BuildKit；
- 与集群相差不超过一个次版本的 `kubectl`；
- Helm 3.7+；
- Git、Make、Bash、SSH/SCP；
- 到 `10.13.36.140:6443`、两个 worker SSH 端口、镜像源、Higress Helm 仓库和 LLM Endpoint 的网络连通性。

检查工具版本：

```bash
docker version
kubectl version --client
helm version
git --version
make --version
```

### 3.2 配置 kubeconfig

如果 `agent1` 已有指向 `https://10.13.36.140:6443` 的正确 kubeconfig，不要覆盖，直接执行下一组验证命令。否则可通过受保护的 SSH 通道复制 `agent0` 上的管理员配置：

```bash
mkdir -p "${HOME}/.kube"
scp agent0@10.13.36.140:/home/agent0/.kube/config "${HOME}/.kube/config"
chmod 600 "${HOME}/.kube/config"
```

该文件具有集群管理员权限。不要把它放入仓库、镜像、ConfigMap、聊天记录或普通备份目录。

确认当前上下文和 API Server：

```bash
kubectl config current-context
kubectl config view --minify
kubectl cluster-info
kubectl get nodes -o wide
```

预期看到 `agent0`、`agent2`、`agent3` 均为 `Ready`，API Server 地址为 `10.13.36.140:6443`。

### 3.3 使用 Kuboard 辅助观察

局域网内可打开 <http://10.13.36.129:8000/login>，使用现有管理员账号进入 `local-k8s`。Kuboard 可辅助查看 Namespace、Pod、PVC、事件、日志和 Metrics，适合定位调度、镜像、存储与资源不足问题。

Kuboard 只作为观察和排障入口，不替代 Helm 或 `kubectl`：

- AgentTeams 的部署、升级、验收和卸载仍以本文的 Helm 与 `kubectl` 命令为准；
- Kuboard 不提供 StorageClass、Ingress Controller 或 MetalLB，不能绕过第 4 节的存储门禁；
- `kuboard/kuboard-admin` ServiceAccount 持久绑定 `cluster-admin`，对整个集群拥有完全管理权限；
- 当前入口是局域网明文 HTTP，只能在受信任管理网使用，并应通过 `agent1` 防火墙限制来源；
- 不要在本文、Shell 历史或仓库中记录 Kuboard 管理员密码、ServiceAccount Token 或导入 kubeconfig。

Kuboard 与 Kubernetes `1.36.3` 的核心连接和资源同步已经实测可用，但不等同于官方完整兼容认证。每次升级 Kubernetes 或 Kuboard 后，都应重新验证资源、日志、终端、Metrics 和增删改操作。集群内的概况与安全边界见[《Kubernetes 集群部署与运维指引》](../kubernetes-cluster-guide.md)；详细 Kuboard 运维说明保存在 `agent1` 的 `/home/agent1/sealos/deploy/kuboard-v4/README.md`。

### 3.4 设置本文变量

```bash
cd /home/agent1/CsAgnet

export AGENTTEAMS_NAMESPACE=agentteams-system
export AGENTTEAMS_RELEASE=agentteams
export AGENTTEAMS_GATEWAY_PUBLIC_URL=http://10.13.36.129:18080
```

后续命令依赖这些变量；打开新终端后需要重新设置。

## 4. 部署前集群与存储门禁

### 4.1 检查节点、Cilium 和资源

```bash
kubectl get nodes -o wide \
  -L kubernetes.io/arch,kubernetes.io/os

kubectl describe node agent0 | sed -n '/Taints:/p'

kubectl get pods -n kube-system \
  -l k8s-app=cilium -o wide

kubectl get pods -A
kubectl top nodes
kubectl get events -A --sort-by=.lastTimestamp | tail -50
```

继续安装前应满足：

- 三个节点均为 `Ready`；
- `agent0` 仍有 control-plane `NoSchedule` 污点；
- Cilium Pod 在三个节点上均为 `Running`；
- 没有持续失败的系统 Pod；
- `agent2`、`agent3` 有足够 CPU、内存和磁盘运行 MinIO、Tuwunel、Higress、Controller、Manager 和至少一个 Worker。

### 4.2 把 StorageClass 作为硬门槛

当前集群记录中没有默认 StorageClass：

```bash
kubectl get storageclass
```

在安装 AgentTeams 前，必须先由集群管理员选择并安装适合该环境的存储方案，例如经过验证的 NFS CSI、Longhorn、Rook-Ceph 或本地卷供应器。选型至少应覆盖容量、读写模式、节点故障、快照、备份和恢复演练；本文不替基础设施管理员作该决定。

安装并验证存储供应器后，显式设置实际名称：

```bash
export AGENTTEAMS_STORAGE_CLASS=storage-class-name

test -n "${AGENTTEAMS_STORAGE_CLASS}"
kubectl get storageclass "${AGENTTEAMS_STORAGE_CLASS}"
```

将 `storage-class-name` 替换为真实名称。命令找不到该 StorageClass 时立即停止，不要继续执行 Helm 安装。

建议先用独立测试 PVC 完成绑定、挂载、读写、Pod 重建和节点故障验证。本次启用 DeepAgents 后，Tuwunel、MinIO 和内置 PostgreSQL 默认各申请 `10Gi`，每个 DeepAgents Worker 另申请 `1Gi` 状态卷；确认所选方案有足够容量，并为备份和增长预留空间。

### 4.3 检查镜像与外部服务连通性

基础设施镜像仍需由 worker 从远程仓库拉取。可在两个 worker 上分别验证 containerd 和代理状态：

```bash
ssh agent2@10.13.36.138 'sudo crictl info >/dev/null && sudo crictl images'
ssh agent3@10.13.36.173 'sudo crictl info >/dev/null && sudo crictl images'
```

如果拉取 Higress、Tuwunel、MinIO 或 Element Web 镜像失败，按集群运维指引检查 containerd 代理、DNS 和仓库限流。LLM Endpoint 也必须能从 Pod 网络访问；不要使用只在 `agent1` 上可达的 `127.0.0.1` 地址。

## 5. 从当前源码构建镜像

### 5.1 构建默认 OpenClaw 与 DeepAgents 镜像

使用包含 Git 提交和 UTC 时间的不可变 Tag，避免两个不同构建复用同一标签：

```bash
cd /home/agent1/CsAgnet

export DOCKER_BUILDKIT=1
export AGENTTEAMS_LOCAL_TAG="dev-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"
export AGENTTEAMS_OPENCLAW_BASE_IMAGE="higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base"
export AGENTTEAMS_OPENCLAW_BASE_VERSION="20260423-8359cbc"

make VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  OPENCLAW_BASE_IMAGE="${AGENTTEAMS_OPENCLAW_BASE_IMAGE}" \
  OPENCLAW_BASE_VERSION="${AGENTTEAMS_OPENCLAW_BASE_VERSION}" \
  build-manager \
  build-worker \
  build-deepagents-worker \
  build-deepagents-runner
```

这里显式使用项目 CI 已验证的固定 OpenClaw 基础镜像路径和 Tag，避免远程仓库路径或浮动 `latest` 造成不可复现构建。`build-manager` 会先构建 Controller，因此应得到：

```text
agentteams/agentteams-controller:<tag>
agentteams/manager:<tag>
agentteams/worker-agent:<tag>
agentteams/deepagents-worker:<tag>
agentteams/deepagents-runner:<tag>
```

检查镜像名、Tag 和架构：

```bash
docker image inspect \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/worker-agent:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}" \
  --format '{{.RepoTags}} {{.Architecture}}'
```

镜像架构必须与 `agent2`、`agent3` 的 `kubernetes.io/arch` 一致。若源码树包含会进入镜像的未提交修改，记录完整 `git status` 和构建时间，确保日后可以追溯该镜像内容。

### 5.2 使用本地 openclaw-base

默认构建会使用远程 `openclaw-base`。若修改了 `openclaw-base/`，必须显式让下游镜像使用本地基础镜像：

```bash
make VERSION="${AGENTTEAMS_LOCAL_TAG}" build-openclaw-base

make VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  OPENCLAW_BASE_IMAGE=agentteams/openclaw-base \
  OPENCLAW_BASE_VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  build-manager build-worker
```

只设置 `OPENCLAW_BASE_VERSION` 而不设置 `OPENCLAW_BASE_IMAGE`，仍可能使用远程仓库中的基础镜像。

### 5.3 可选 Runtime

只在实际需要时构建：

```bash
make VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  build-manager-copaw \
  build-copaw-worker \
  build-hermes-worker \
  build-openhuman-worker \
  build-qwenpaw-worker
```

Manager 当前只选择 OpenClaw 或 CoPaw；DeepAgents、Hermes、OpenHuman 和 QwenPaw 只用作 Worker。本次部署已在 5.1 节构建 DeepAgents；其它可选镜像也必须按下一节导入两个 worker。

## 6. 将镜像导入 agent2 与 agent3

### 6.1 生成镜像归档

在 `agent1` 执行：

```bash
export AGENTTEAMS_IMAGE_ARCHIVE="/var/tmp/agentteams-images-${AGENTTEAMS_LOCAL_TAG}.tar"

docker save \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/worker-agent:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}" \
  -o "${AGENTTEAMS_IMAGE_ARCHIVE}"

ls -lh "${AGENTTEAMS_IMAGE_ARCHIVE}"
sha256sum "${AGENTTEAMS_IMAGE_ARCHIVE}"
export AGENTTEAMS_IMAGE_ARCHIVE_SHA256="$(sha256sum "${AGENTTEAMS_IMAGE_ARCHIVE}" | awk '{print $1}')"
```

归档可能较大，先确认 `agent1` 和两个 worker 的 `/var/tmp` 所在文件系统有足够空间。不要默认使用 `/tmp`：部分 kubeadm 节点会把它挂载为容量较小的 `tmpfs`，多镜像归档可能耗尽内存文件系统。

### 6.2 复制并导入两个 worker

以下循环会逐台复制归档，并通过 `ctr` 导入 containerd 的 `k8s.io` namespace：

```bash
(
  set -e
  for AGENTTEAMS_NODE in agent2@10.13.36.138 agent3@10.13.36.173; do
    scp "${AGENTTEAMS_IMAGE_ARCHIVE}" \
      "${AGENTTEAMS_NODE}:${AGENTTEAMS_IMAGE_ARCHIVE}"

    ssh "${AGENTTEAMS_NODE}" \
      "printf '%s  %s\n' '${AGENTTEAMS_IMAGE_ARCHIVE_SHA256}' '${AGENTTEAMS_IMAGE_ARCHIVE}' | sha256sum -c -"

    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images import '${AGENTTEAMS_IMAGE_ARCHIVE}'"

    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images list | grep '${AGENTTEAMS_LOCAL_TAG}'"

    ssh "${AGENTTEAMS_NODE}" \
      "rm -- '${AGENTTEAMS_IMAGE_ARCHIVE}'"
  done
)
```

必须使用 `k8s.io` namespace；导入到 containerd 的默认 namespace 后，kubelet 看不到镜像。

再分别核对五张镜像：

```bash
for AGENTTEAMS_NODE in agent2@10.13.36.138 agent3@10.13.36.173; do
  ssh -t "${AGENTTEAMS_NODE}" \
    "sudo crictl images | grep '${AGENTTEAMS_LOCAL_TAG}'"
done
```

两个节点都能看到完全一致的 repository、Tag 和 digest 后才能继续。导入成功后删除的只是 worker 上的临时归档；containerd 镜像仍然保留，需要恢复归档时可从 `agent1` 重新复制。`agent0` 保持不可调度，因此默认不导入；如果以后移除它的污点或增加新的可调度节点，也必须把镜像导入相应节点。

## 7. 准备并校验 Helm Chart

### 7.1 下载依赖

```bash
cd /home/agent1/CsAgnet
helm repo add higress.io https://higress.io/helm-charts --force-update
helm repo update
helm dependency build ./helm/agentteams
```

该命令按 `Chart.lock` 下载 Higress 依赖。网络受限环境应提前把依赖包放入 `helm/agentteams/charts/`，并确保两个 worker 能取得所有基础设施镜像。

### 7.2 lint 与静态渲染

先确认本节依赖的变量仍然存在：

```bash
test -n "${AGENTTEAMS_LOCAL_TAG}"
test -n "${AGENTTEAMS_STORAGE_CLASS}"
test -n "${AGENTTEAMS_GATEWAY_PUBLIC_URL}"
```

执行 lint：

```bash
helm lint ./helm/agentteams \
  --set-string credentials.llmApiKey=dummy \
  --set-string credentials.adminPassword=dummy-password \
  --set-string gateway.publicURL="${AGENTTEAMS_GATEWAY_PUBLIC_URL}" \
  --set-string matrix.tuwunel.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string storage.minio.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set deepagents.enabled=true \
  --set-string deepagents.state.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string deepagents.postgresql.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}"
```

静态渲染完整安装参数：

```bash
helm template "${AGENTTEAMS_RELEASE}" ./helm/agentteams \
  --namespace "${AGENTTEAMS_NAMESPACE}" \
  --set-string global.imageTag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string credentials.llmApiKey=dummy \
  --set-string credentials.adminPassword=dummy-password \
  --set-string gateway.publicURL="${AGENTTEAMS_GATEWAY_PUBLIC_URL}" \
  --set-string matrix.tuwunel.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string storage.minio.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set deepagents.enabled=true \
  --set-string deepagents.state.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string deepagents.postgresql.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string deepagents.runnerImage.repository=agentteams/deepagents-runner \
  --set-string deepagents.runnerImage.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string controller.image.repository=agentteams/agentteams-controller \
  --set-string controller.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string controller.image.pullPolicy=Never \
  --set-string manager.runtime=openclaw \
  --set-string manager.image.repository=agentteams/manager \
  --set-string manager.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultRuntime=openclaw \
  --set-string worker.defaultImage.openclaw.repository=agentteams/worker-agent \
  --set-string worker.defaultImage.openclaw.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultImage.deepagents.repository=agentteams/deepagents-worker \
  --set-string worker.defaultImage.deepagents.tag="${AGENTTEAMS_LOCAL_TAG}" \
  > /tmp/agentteams-rendered.yaml
```

检查关键值：

```bash
grep -nE 'storageClassName:|image:|imagePullPolicy:|AGENTTEAMS_MANAGER_SPEC|AGENTTEAMS_(WORKER_IMAGE|DEEPAGENTS_(WORKER|RUNNER)_IMAGE)' \
  /tmp/agentteams-rendered.yaml
```

只有 Controller Deployment 和复用 Controller 镜像的 Helm Hook 支持 `controller.image.pullPolicy=Never`。Chart 没有 `manager.image.pullPolicy`、`worker.defaultImage.*.pullPolicy` 或 Runner 独立 pullPolicy；Controller 创建的 Manager/Worker/Runner Pod 使用 `IfNotPresent`。因此必须依靠第 6 节确保两个 worker 预先存在准确镜像。

## 8. 配置 LLM 并安装

### 8.1 准备敏感值

交互读取密钥，避免把字面量直接写进 shell 历史：

```bash
read -rsp 'LLM API Key: ' AGENTTEAMS_LLM_API_KEY; echo
read -rsp 'Matrix admin password: ' AGENTTEAMS_ADMIN_PASSWORD; echo
export AGENTTEAMS_LLM_API_KEY AGENTTEAMS_ADMIN_PASSWORD

export AGENTTEAMS_LLM_PROVIDER=openai-compat
export AGENTTEAMS_MODEL=gpt-5.4
export AGENTTEAMS_LLM_BASE_URL=
```

对于 OpenAI-compatible 第三方服务，填写供应商实际提供的 Base URL 与模型名：

```bash
export AGENTTEAMS_LLM_PROVIDER=openai-compat
export AGENTTEAMS_LLM_BASE_URL=https://provider.example.com/v1
export AGENTTEAMS_MODEL=provider-model-name
```

对于通义千问，可使用：

```bash
export AGENTTEAMS_LLM_PROVIDER=qwen
export AGENTTEAMS_LLM_BASE_URL=
export AGENTTEAMS_MODEL=qwen3.5-plus
```

环境变量仍可能在 Helm 进程运行期间短暂出现在本机进程参数中。共享管理机应改用权限为 `0600` 的临时 values 文件或组织内 Secret 管理流程。

### 8.2 安装当前源码版本

```bash
helm upgrade --install "${AGENTTEAMS_RELEASE}" ./helm/agentteams \
  --namespace "${AGENTTEAMS_NAMESPACE}" \
  --create-namespace \
  --set-string global.imageTag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string credentials.llmApiKey="${AGENTTEAMS_LLM_API_KEY}" \
  --set-string credentials.adminPassword="${AGENTTEAMS_ADMIN_PASSWORD}" \
  --set-string credentials.llmProvider="${AGENTTEAMS_LLM_PROVIDER}" \
  --set-string credentials.defaultModel="${AGENTTEAMS_MODEL}" \
  --set-string credentials.llmBaseUrl="${AGENTTEAMS_LLM_BASE_URL}" \
  --set-string gateway.publicURL="${AGENTTEAMS_GATEWAY_PUBLIC_URL}" \
  --set-string matrix.tuwunel.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string storage.minio.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set deepagents.enabled=true \
  --set-string deepagents.state.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string deepagents.postgresql.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string deepagents.runnerImage.repository=agentteams/deepagents-runner \
  --set-string deepagents.runnerImage.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string controller.image.repository=agentteams/agentteams-controller \
  --set-string controller.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string controller.image.pullPolicy=Never \
  --set-string manager.runtime=openclaw \
  --set-string manager.image.repository=agentteams/manager \
  --set-string manager.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultRuntime=openclaw \
  --set-string worker.defaultImage.openclaw.repository=agentteams/worker-agent \
  --set-string worker.defaultImage.openclaw.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultImage.deepagents.repository=agentteams/deepagents-worker \
  --set-string worker.defaultImage.deepagents.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --timeout 15m
```

默认启用 LLM preflight。它会复用 Controller 镜像，从集群内发出最小 `/chat/completions` 请求；Key、Base URL、模型、Pod 网络或代理错误都会让安装提前失败。只有明确知道暂时无法探测的原因时，才临时添加：

```bash
--set preflight.llm.enabled=false
```

跳过 preflight 不会修复运行时网络或凭证问题。

Helm 命令完成后清除当前 shell 中的明文敏感变量：

```bash
unset AGENTTEAMS_LLM_API_KEY AGENTTEAMS_ADMIN_PASSWORD
```

## 9. 等待系统收敛与分层验收

### 9.1 先检查调度、镜像和持久卷

```bash
kubectl get pods,pvc -n "${AGENTTEAMS_NAMESPACE}" -o wide
kubectl get events -n "${AGENTTEAMS_NAMESPACE}" \
  --sort-by=.metadata.creationTimestamp | tail -80
```

创建 DeepAgents Worker 前，Tuwunel、MinIO 和 PostgreSQL 三个 PVC 必须为 `Bound`；Worker 创建后还会出现其专属状态 PVC。任何 PVC 长期 `Pending`、Pod 因 `ErrImageNeverPull`、`ImagePullBackOff` 或资源不足而无法调度时，都应先修复根因再继续。

### 9.2 等待基础设施与 Controller

```bash
kubectl rollout status statefulset/agentteams-tuwunel \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl rollout status statefulset/agentteams-minio \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl rollout status statefulset/agentteams-deepagents-postgresql \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl wait --for=condition=Available \
  deployment/agentteams-controller \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl get deployment,statefulset,service \
  -n "${AGENTTEAMS_NAMESPACE}"
```

### 9.3 等待 Manager

```bash
kubectl get managers.agentteams.io \
  -n "${AGENTTEAMS_NAMESPACE}"

kubectl wait --for=condition=Ready pod \
  -l agentteams.io/manager=default \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl get manager default \
  -n "${AGENTTEAMS_NAMESPACE}" -o yaml
```

健康的首次启动大致经历：MinIO/Tuwunel/Higress Ready，Controller Initializer 创建存储目录、Matrix Admin/AppService、路由和 AI Provider，随后创建 `Manager/default`，ManagerReconciler 再创建身份、Consumer、配置和 Pod。

确认 AgentTeams Pod 实际位于两个 worker，而不是 `agent0`：

```bash
kubectl get pods -n "${AGENTTEAMS_NAMESPACE}" -o wide
```

## 10. 从局域网访问 Element 与 Higress Console

### 10.1 Element Web 与 Matrix

在 `agent1` 启动并保持以下命令运行：

```bash
kubectl port-forward --address 10.13.36.129 \
  -n "${AGENTTEAMS_NAMESPACE}" \
  svc/higress-gateway 18080:80
```

`--address 10.13.36.129` 会让端口对局域网开放。确认 `agent1` 防火墙只允许受信任来源访问 `18080`，不要直接暴露到互联网。

在局域网浏览器打开 <http://10.13.36.129:18080>，使用：

- 用户名：`admin`
- 密码：安装时填写的 `credentials.adminPassword`

验证统一入口：

```bash
curl -fsSI http://10.13.36.129:18080/
curl -fsS http://10.13.36.129:18080/_matrix/client/versions
```

`gateway.publicURL` 必须与浏览器实际使用的 Origin 一致。不能一边安装为 `localhost`，一边从其他局域网设备通过 `10.13.36.129` 访问。

### 10.2 Higress Console

只在排障或配置检查期间临时运行：

```bash
kubectl port-forward --address 10.13.36.129 \
  -n "${AGENTTEAMS_NAMESPACE}" \
  svc/higress-console 18081:8080
```

打开 <http://10.13.36.129:18081>。本地 Chart 使用同一套 Admin 用户名和密码初始化 Console。检查：

- `element-web` 与 `matrix-homeserver` 路由；
- 匹配 `/v1` 的 `default-ai-route`；
- Manager/Worker Consumer；
- AI Provider 与上游配置。

使用完立即停止转发。不要把 Console、Controller、Tuwunel 或 MinIO 管理端口直接暴露到公网。

## 11. 创建并验证第一个 Worker

### 11.1 通过 Element 创建

在 Admin 与 Manager 的房间中发送：

```text
创建一个名为 alice 的 Worker，使用 openclaw runtime，负责前端开发，并直接创建。
```

Manager 会调用 `agt`/Controller REST API，该路径会自动给 CR 添加 Controller 归属标签。

### 11.2 直接使用 CR 创建

如果改用 `kubectl apply`，必须显式添加 Controller 标签：

```bash
kubectl apply -n "${AGENTTEAMS_NAMESPACE}" -f - <<EOF
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
  labels:
    agentteams.io/controller: agentteams-controller
spec:
  model: ${AGENTTEAMS_MODEL:-gpt-5.4}
  runtime: openclaw
  identity: |
    - Name: Alice
    - Specialization: frontend development
  state: Running
EOF
```

`agentteams-controller` 是 release 名为 `agentteams` 时的标准 Controller Deployment 名。如果修改了 release/fullname，先查询实际名称并替换标签值：

```bash
kubectl get deployment -n "${AGENTTEAMS_NAMESPACE}" \
  -l app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].metadata.name}{"\n"}'
```

观察调和结果：

```bash
kubectl get worker alice -n "${AGENTTEAMS_NAMESPACE}" -w

kubectl get pod -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=alice -o wide

kubectl logs -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=alice --all-containers --tail=200
```

成功标准包括：

- `Worker.status.phase=Running`；
- `status.matrixUserID` 与 `status.roomID` 已填写；
- Higress 出现 Worker Consumer，并将其加入 AI Route 授权；
- Element 出现对应房间；
- Worker 能接收消息并通过 Higress 调用模型。

### 11.3 创建并验证 DeepAgents Worker

确认内置 PostgreSQL 和 checkpoint migration 已就绪：

```bash
kubectl rollout status statefulset/agentteams-deepagents-postgresql \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout=10m

kubectl logs deployment/agentteams-controller \
  -n "${AGENTTEAMS_NAMESPACE}" \
  -c deepagents-checkpoint-migrate --tail=100
```

创建使用 sandbox execution 的 DeepAgents Worker。不要在 CR 中写入 Matrix、Higress、MinIO 或 PostgreSQL 凭据：

```bash
kubectl apply -n "${AGENTTEAMS_NAMESPACE}" -f - <<EOF
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: deep-researcher
  labels:
    agentteams.io/controller: agentteams-controller
spec:
  model: ${AGENTTEAMS_MODEL:-gpt-5.4}
  runtime: deepagents
  identity: |
    - Name: Deep Researcher
    - Specialization: evidence-based research and controlled local execution
  state: Running
  runtimeConfig:
    deepagents:
      approvals:
        fileWrites: required
        mcpDefault: required
      execution:
        mode: sandbox
        idleTimeout: 5m
        maxLifetime: 30m
        resources:
          requests:
            cpu: 250m
            memory: 256Mi
          limits:
            cpu: "2"
            memory: 2Gi
EOF
```

等待 Worker Pod 与状态 PVC 收敛，并确认 checkpoint Secret 只通过 `SecretKeyRef` 注入：

```bash
kubectl get worker deep-researcher -n "${AGENTTEAMS_NAMESPACE}" -w

kubectl get pod,pvc -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=deep-researcher -o wide

kubectl get pod -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=deep-researcher -o yaml \
  | grep -nE 'AGENTTEAMS_CHECKPOINT_(DSN|AES_KEY)|secretKeyRef:'
```

在 Element 的 Deep Researcher Personal Room 中，由登录的 Human admin 发送一条要求使用 `execute` 在 `/workspace` 创建测试文件的任务。`execute` 无论其它策略如何都必须中断并请求 Human 审批；在同一 Matrix thread 回复 `approve 1` 后观察：

```bash
kubectl get executionsandbox,pod,service,networkpolicy \
  -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=deep-researcher -w
```

验收要求：

- Agent 身份回复审批命令会被拒绝，任务发起者 Human admin 可以批准；
- Runner Pod 使用 `agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}`、UID/GID `65532:65532`，且 `automountServiceAccountToken=false`；
- Runner 不包含 Matrix、Higress、MinIO、PostgreSQL 凭据；
- 命令只在 Runner 的 `/workspace` 执行，变更清单校验通过后才写回 MinIO；
- 相同 request ID 不会重复执行命令；
- 空闲超过 `5m` 或总生命周期超过 `30m` 后，`ExecutionSandbox` 及其 Pod、Service、NetworkPolicy 被 Controller 回收，Worker 与 checkpoint 保留。

更完整的策略、外部 PostgreSQL 和故障排查说明见[《DeepAgents Worker Runtime》](deepagents-runtime.md)。

## 12. 运行时选择

### 12.1 CoPaw Manager

先在 `agent1` 构建 `agentteams/manager-copaw:<tag>`，重新生成包含该镜像的归档并导入 `agent2`、`agent3`，再更新 Manager CR：

```bash
kubectl patch manager default -n "${AGENTTEAMS_NAMESPACE}" \
  --type=merge \
  -p "{\"spec\":{\"runtime\":\"copaw\",\"image\":\"agentteams/manager-copaw:${AGENTTEAMS_LOCAL_TAG}\"}}"
```

Runtime 切换会重建 Manager Pod，但 Matrix 身份、CR 和对象存储数据应保留。

`manager.*` Helm values 只用于首次创建 `Manager/default`。Initializer 发现 CR 已存在时不会覆盖它，因此已安装环境应更新 Manager CR，而不是只执行 `helm upgrade --set manager.*`。

### 12.2 其他 Worker Runtime

DeepAgents 已按 11.3 节启用。CoPaw、Hermes、OpenHuman 或 QwenPaw Worker 也必须先把对应镜像导入两个 worker。随后可设置 Chart 默认 Runtime，或直接在 Worker CR 的 `spec.runtime` 和 `spec.image` 中指定准确镜像。

不要把 Manager Runtime 设置为 DeepAgents、Hermes、OpenHuman 或 QwenPaw；Manager 启动入口当前只支持 OpenClaw 和 CoPaw。

## 13. 日常运维与升级

### 13.1 日常检查

```bash
# AgentTeams CR
kubectl get managers,workers,teams,humans \
  -n "${AGENTTEAMS_NAMESPACE}"

# 核心资源和调度节点
kubectl get pod,svc,pvc \
  -n "${AGENTTEAMS_NAMESPACE}" -o wide

# Controller 日志
kubectl logs -n "${AGENTTEAMS_NAMESPACE}" \
  deployment/agentteams-controller --tail=200 -f

# Manager 日志
kubectl logs -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/manager=default --tail=200 -f
```

上述 Pod、PVC、事件、日志和 Metrics 也可在 Kuboard 的 `local-k8s` 集群中辅助观察。涉及部署、升级、删除或权限变更时，仍应执行并保存命令行流程及错误输出，以便审计和复现。

Controller Service 的 REST 默认端口为 `8090`，metrics 默认端口为 `8080`。需要临时检查指标时，在 `agent1` 本机绑定回环地址：

```bash
kubectl port-forward -n "${AGENTTEAMS_NAMESPACE}" \
  svc/agentteams-controller 18082:8080

curl -fsS http://localhost:18082/metrics | grep agentteams
```

### 13.2 升级当前源码

每次构建都生成新 Tag，不复用 `latest` 或已有 `dev` Tag：

```bash
cd /home/agent1/CsAgnet
git pull --ff-only

export AGENTTEAMS_LOCAL_TAG="dev-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"
export AGENTTEAMS_OPENCLAW_BASE_IMAGE="${AGENTTEAMS_OPENCLAW_BASE_IMAGE:-higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base}"
export AGENTTEAMS_OPENCLAW_BASE_VERSION="${AGENTTEAMS_OPENCLAW_BASE_VERSION:-20260423-8359cbc}"
make VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  OPENCLAW_BASE_IMAGE="${AGENTTEAMS_OPENCLAW_BASE_IMAGE}" \
  OPENCLAW_BASE_VERSION="${AGENTTEAMS_OPENCLAW_BASE_VERSION}" \
  build-manager build-worker \
  build-deepagents-worker build-deepagents-runner

export AGENTTEAMS_IMAGE_ARCHIVE="/var/tmp/agentteams-images-${AGENTTEAMS_LOCAL_TAG}.tar"
docker save \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/worker-agent:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}" \
  -o "${AGENTTEAMS_IMAGE_ARCHIVE}"

export AGENTTEAMS_IMAGE_ARCHIVE_SHA256="$(sha256sum "${AGENTTEAMS_IMAGE_ARCHIVE}" | awk '{print $1}')"

(
  set -e
  for AGENTTEAMS_NODE in agent2@10.13.36.138 agent3@10.13.36.173; do
    scp "${AGENTTEAMS_IMAGE_ARCHIVE}" \
      "${AGENTTEAMS_NODE}:${AGENTTEAMS_IMAGE_ARCHIVE}"
    ssh "${AGENTTEAMS_NODE}" \
      "printf '%s  %s\n' '${AGENTTEAMS_IMAGE_ARCHIVE_SHA256}' '${AGENTTEAMS_IMAGE_ARCHIVE}' | sha256sum -c -"
    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images import '${AGENTTEAMS_IMAGE_ARCHIVE}'"
    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images list | grep '${AGENTTEAMS_LOCAL_TAG}'"
    ssh "${AGENTTEAMS_NODE}" \
      "rm -- '${AGENTTEAMS_IMAGE_ARCHIVE}'"
  done
)
```

先备份 MinIO 与 Tuwunel 数据，再升级 Chart 和 Controller：

```bash
helm dependency build ./helm/agentteams

helm upgrade "${AGENTTEAMS_RELEASE}" ./helm/agentteams \
  -n "${AGENTTEAMS_NAMESPACE}" \
  --reuse-values \
  --set-string global.imageTag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string controller.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string manager.image.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultImage.openclaw.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string worker.defaultImage.deepagents.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --set-string deepagents.runnerImage.tag="${AGENTTEAMS_LOCAL_TAG}" \
  --timeout 15m
```

现有 Manager 不会因 `manager.image.tag` 变化自动更新，需显式 patch：

```bash
kubectl patch manager default -n "${AGENTTEAMS_NAMESPACE}" \
  --type=merge \
  -p "{\"spec\":{\"image\":\"agentteams/manager:${AGENTTEAMS_LOCAL_TAG}\"}}"
```

Helm 中的 Worker 默认镜像主要影响新建 Worker。已有 Worker 需要逐个写入新镜像；例如：

```bash
kubectl patch worker alice -n "${AGENTTEAMS_NAMESPACE}" \
  --type=merge \
  -p "{\"spec\":{\"image\":\"agentteams/worker-agent:${AGENTTEAMS_LOCAL_TAG}\"}}"
```

DeepAgents Worker 同样需要逐个更新，例如：

```bash
kubectl patch worker deep-researcher -n "${AGENTTEAMS_NAMESPACE}" \
  --type=merge \
  -p "{\"spec\":{\"image\":\"agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}\"}}"
```

CRD 位于 Chart 的 `crds/` 目录，Helm 不会像普通模板一样升级已存在 CRD。Schema 变化时应先审阅差异，并按发布说明单独应用。

## 14. 常见故障排查

### 14.1 PVC 一直 Pending

```bash
kubectl get pvc -n "${AGENTTEAMS_NAMESPACE}"
kubectl describe pvc -n "${AGENTTEAMS_NAMESPACE}"
kubectl get storageclass
```

检查 StorageClass 是否存在、Provisioner 是否健康、容量/访问模式是否满足，以及 Helm 中两个 `storageClassName` 是否拼写正确。不要通过删除 PVC 反复重试来掩盖存储问题。

### 14.2 Controller 报 ErrImageNeverPull

Controller 使用 `pullPolicy=Never`。如果 Pod 被调度到未导入镜像的节点，会直接失败：

```bash
kubectl get pod -n "${AGENTTEAMS_NAMESPACE}" -o wide
kubectl describe pod POD_NAME -n "${AGENTTEAMS_NAMESPACE}"
ssh -t agent2@10.13.36.138 'sudo ctr -n k8s.io images list | grep agentteams'
ssh -t agent3@10.13.36.173 'sudo ctr -n k8s.io images list | grep agentteams'
```

确认 repository 和 Tag 与 Pod spec 完全一致，并确认镜像导入的是 containerd `k8s.io` namespace。

### 14.3 Manager/Worker 报 ImagePullBackOff

Manager/Worker 使用 `IfNotPresent`。本地镜像缺失时，kubelet 会尝试按 `agentteams/...` 从远程仓库拉取并失败。把准确镜像导入发生故障的 worker，然后删除失败 Pod，让 Controller 重建；不要将同一 Tag 重新指向不同内容。

### 14.4 安装在 LLM preflight 阶段失败

```bash
kubectl get job,pod -n "${AGENTTEAMS_NAMESPACE}" | grep preflight
kubectl logs -n "${AGENTTEAMS_NAMESPACE}" \
  job/agentteams-llm-preflight
```

重点检查 Key、模型名、Base URL 路径、Pod DNS/代理和供应商配额。Hook 失败后可能立即删除 Pod；必要时从临时 Pod 使用相同 Endpoint 验证网络。

### 14.5 Controller Ready，但没有 Manager

```bash
kubectl logs deployment/agentteams-controller \
  -n "${AGENTTEAMS_NAMESPACE}" --tail=300
kubectl get manager -n "${AGENTTEAMS_NAMESPACE}" -o yaml
kubectl get pod -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/manager=default -o wide
```

Initializer 可能卡在 MinIO、Matrix、AppService 或 Higress 初始化。按日志中最后一个成功步骤定位，不要先重启并丢失现场信息。

### 14.6 Element 无法从局域网访问

在 `agent1` 检查：

```bash
ss -lnt | grep 18080
curl -v http://10.13.36.129:18080/_matrix/client/versions
kubectl get svc higress-gateway -n "${AGENTTEAMS_NAMESPACE}"
```

确认 port-forward 仍在运行、绑定地址为 `10.13.36.129`、主机防火墙允许受信任来源，并且 `gateway.publicURL` 使用相同地址。`localhost` 只代表发起访问的设备。

### 14.7 Agent 调用模型返回 401/403

- `401` 通常表示 Consumer Token 或上游 Key 无效；
- `403` 通常表示 Consumer 尚未加入 AI Route 的 `allowedConsumers`；
- 在 Higress Console 同时检查 Consumer、`default-ai-route` 和 Provider；
- 查看对应 Manager/Worker CR 的 `status.message` 与 Controller 日志。

Gateway Pod Ready 只表示进程健康，不表示 Provider、Route 和 Consumer 已全部配置成功。

### 14.8 Worker Pod Running 但 Agent 不工作

```bash
kubectl get worker alice -n "${AGENTTEAMS_NAMESPACE}" -o yaml
kubectl logs -n "${AGENTTEAMS_NAMESPACE}" \
  -l agentteams.io/worker=alice --all-containers --tail=300
```

检查 MinIO 配置是否拉取成功、Matrix Token/Room 是否生成、Runtime entrypoint 是否 Ready，以及 `status.phase` 是否仍为 `Pending`、`Starting` 或 `Failed`。

## 15. 卸载与数据处理

卸载前先备份 MinIO Bucket 与 Tuwunel 数据，并确认不再需要 Manager/Worker 的工作区与 Matrix 历史。

正常卸载必须让 pre-delete Hook 先删除 CR 并等待 finalizer：

```bash
helm uninstall "${AGENTTEAMS_RELEASE}" \
  -n "${AGENTTEAMS_NAMESPACE}" --timeout 20m
```

不要先删除 Controller，也不要直接使用 `--no-hooks`。如果必须跳过 Hook，应先手工删除并等待所有 CR：

```bash
kubectl delete managers.agentteams.io --all \
  -n "${AGENTTEAMS_NAMESPACE}" --wait=true
kubectl delete workers.agentteams.io --all \
  -n "${AGENTTEAMS_NAMESPACE}" --wait=true
kubectl delete teams.agentteams.io --all \
  -n "${AGENTTEAMS_NAMESPACE}" --wait=true
kubectl delete humans.agentteams.io --all \
  -n "${AGENTTEAMS_NAMESPACE}" --wait=true
```

Helm 卸载不会删除 CRD。确认没有其他 AgentTeams release 使用后，才删除：

```bash
kubectl delete crd \
  managers.agentteams.io \
  workers.agentteams.io \
  teams.agentteams.io \
  humans.agentteams.io
```

卸载后单独检查残留 PVC：

```bash
kubectl get pvc -n "${AGENTTEAMS_NAMESPACE}"
```

删除 PVC 或存储后端卷可能不可恢复地删除数据，必须另行确认。卸载 AgentTeams 不删除也不重置 kubeadm 集群，不应执行任何集群销毁命令。

## 16. 官方发布版替代路径

如果不修改源码，优先使用官方 Chart 与同版本发布镜像，避免本地构建和 containerd 导入：

```bash
helm repo add higress.io https://higress.io/helm-charts
helm repo update
helm search repo higress.io/agentteams --versions
```

选择目标版本后安装，并保留当前集群所需的 StorageClass 与局域网公共 URL：

```bash
helm upgrade --install agentteams higress.io/agentteams \
  --version CHART_VERSION \
  -n agentteams-system \
  --create-namespace \
  --set-string credentials.llmApiKey="${AGENTTEAMS_LLM_API_KEY}" \
  --set-string credentials.adminPassword="${AGENTTEAMS_ADMIN_PASSWORD}" \
  --set-string gateway.publicURL=http://10.13.36.129:18080 \
  --set-string matrix.tuwunel.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}" \
  --set-string storage.minio.persistence.storageClassName="${AGENTTEAMS_STORAGE_CLASS}"
```

将 `CHART_VERSION` 替换为明确版本。不要混用当前 `main` 分支的 CRD/模板与较旧 Controller 镜像；稳定部署应把 Chart、CRD、Controller、Manager 和 Worker 固定在同一发布线，并使用不可变 Tag。
