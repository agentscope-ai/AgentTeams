# DeepAgents Worker Runtime

AgentTeams 可以把 LangChain `deepagents` 作为独立的 Worker Runtime 使用。该集成保留 AgentTeams 的 Matrix 协作、Higress 模型/MCP 网关、MinIO 工作区和 Kubernetes CRD 控制面，同时将长任务 checkpoint、Human-in-the-loop 审批和命令执行分别放入明确的安全边界。

> DeepAgents 目前仅作为 **Worker Runtime**；Manager 仍使用 OpenClaw 或 CoPaw。生产部署以 Kubernetes 为目标，`execution.mode: sandbox` 依赖 `ExecutionSandbox` CRD 和 AgentTeams Controller。
>
> 当前 `execution.mode: sandbox` 仅支持 Worker 与 Runner 位于 Controller 所在的同一集群、同一命名空间；`deployMode: Edge`/`Remote` 会在创建 Matrix、MinIO、Pod 等资源前被 Controller 明确拒绝。跨集群 Runner、跨命名空间 Service/NetworkPolicy 和 checkpoint Secret 分发不在本阶段支持范围内。

## 运行架构

一次消息处理链路如下：

1. Human 或 Agent 在允许的 Matrix Room 中发送消息；DeepAgents Worker 按 Matrix thread 建立稳定的 LangGraph `thread_id`。
2. Worker 通过 Higress 调用模型和 MCP Server，使用 Worker 自己的 Gateway Consumer key。
3. LangGraph checkpoint 使用 AES-EAX 加密后写入 PostgreSQL；DSN 和 AES key 只注入 DeepAgents Worker，不会注入其它 Runtime。
4. `execute`、配置为 `required` 的文件写入或 MCP tool 会产生 Matrix 审批请求。只有 CRD 投影出的 Human 身份可以批准、拒绝或编辑，Agent 身份不能代替 Human 决策。
5. `execute` 获批后，Worker 通过 ServiceAccount 身份请求 `ExecutionSandbox`。Runner Pod 不持有 Matrix、Higress、MinIO、PostgreSQL 或 Kubernetes 凭据，只接收一个短期 bearer token。
6. Worker 在凭据边界内把安全工作区文件从 MinIO 注入 Runner，并根据 Runner 返回的变更清单写回 MinIO。

Worker 使用专属 PVC 保存 Matrix E2EE 设备数据、sync token 和待审批元数据。model、MCP 或 DeepAgents 策略变更会重建 Worker Pod，但该 PVC 和 PostgreSQL checkpoint 会保留。

审批身份采用拒绝优先的角色投影：当前 Worker、Manager、Team Leader 和其它 Worker 都属于 Agent；Team Admin、`spec.humanMembers` 中 `role: coordinator` 的成员，以及额外配置的 `approvals.coordinators` 才可能属于 Human。若同一个 Matrix ID 同时出现在 Agent 与 Human 配置中，始终按 Agent 处理并拒绝其审批。`approvals.coordinators` 因此只能填写真实人类账号，不能用来把 Manager 或 Worker 提升为审批者。

## kubeadm 集群前提

在三节点或更多节点的 kubeadm 集群中启用前，逐项确认：

- 已安装支持 `NetworkPolicy` 的 CNI，例如 Calico 或 Cilium。仅安装不执行 NetworkPolicy 的 CNI 会破坏 Runner 和 PostgreSQL 的隔离假设。
- 集群存在可动态供给 `ReadWriteOnce` PVC 的默认 `StorageClass`，或者在 values 中显式填写 StorageClass。原生 kubeadm 不会自动提供动态存储类。
- CoreDNS 位于 `kube-system`，Pod 标签包含常见的 `k8s-app=kube-dns`。Runner 仅在请求了外网 egress 时被允许访问该 DNS 服务。
- 所有节点都能拉取 Controller、DeepAgents Worker、DeepAgents Runner 和 PostgreSQL 镜像。局域网环境建议先推送到集群可访问的私有镜像仓库。
- PostgreSQL PVC、每个 DeepAgents Worker 的状态 PVC，以及现有 Tuwunel/MinIO PVC 有足够容量。

可先执行：

```bash
kubectl get nodes -o wide
kubectl get storageclass
kubectl -n kube-system get pods -l k8s-app=kube-dns
kubectl api-resources | grep -E 'networkpolicies|persistentvolumeclaims'
```

如果 `kubectl get storageclass` 没有显示 `(default)`，先安装适合三节点集群的数据方案，或在下述 values 中同时填写 PostgreSQL 和 Worker 状态的 `storageClassName`。

下文沿用主部署指南中的命名空间变量；如果尚未设置，请改成实际 Helm release 使用的命名空间：

```bash
export AGENTTEAMS_NAMESPACE=agentteams-system
```

## 构建与发布镜像

DeepAgents 不只需要新增的 Worker/Runner 镜像，还需要包含对应 CRD、Controller 和 Manager skill 的本次 AgentTeams 版本。从仓库根目录构建：

```bash
export AGENTTEAMS_LOCAL_TAG="${AGENTTEAMS_LOCAL_TAG:-dev-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)}"
make build-agentteams-controller build-manager \
  build-deepagents-worker build-deepagents-runner \
  VERSION="${AGENTTEAMS_LOCAL_TAG}"
```

推荐将四张同版本镜像推送到三台节点都能访问的 Registry。以下命令适用于节点架构与构建机一致的局域网开发集群：

```bash
docker tag "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "registry.lan:5000/agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}"
docker push \
  "registry.lan:5000/agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}"

make push-native-manager push-native-deepagents-worker \
  push-native-deepagents-runner \
  VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  REGISTRY=registry.lan:5000 \
  REPO=agentteams
```

生产或混合架构节点使用 `make push-agentteams-controller push-manager push-deepagents-worker push-deepagents-runner` 构建并推送 `linux/amd64,linux/arm64` manifest。

如果三节点集群尚无私有 Registry，沿用主部署指南的 containerd 导入方式。把同一不可变 Tag 的 Controller、Manager、DeepAgents Worker 和 Runner 一起归档，并导入每个可调度节点；下面的节点名称只是示例：

```bash
export AGENTTEAMS_DEEPAGENTS_ARCHIVE="/tmp/agentteams-deepagents-${AGENTTEAMS_LOCAL_TAG}.tar"

docker save \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}" \
  -o "${AGENTTEAMS_DEEPAGENTS_ARCHIVE}"

for AGENTTEAMS_NODE in agent2 agent3; do
  scp "${AGENTTEAMS_DEEPAGENTS_ARCHIVE}" \
    "${AGENTTEAMS_NODE}:${AGENTTEAMS_DEEPAGENTS_ARCHIVE}"
  ssh -t "${AGENTTEAMS_NODE}" \
    "sudo ctr -n k8s.io images import '${AGENTTEAMS_DEEPAGENTS_ARCHIVE}'"
done
```

无 Registry 路径下，把 Helm Controller 的 `image.pullPolicy` 设为 `IfNotPresent`（或严格离线时设为 `Never`）；Controller 创建的 Manager/Worker/Runner 使用 `IfNotPresent`。因此必须把四张镜像导入所有可能承载这些 Pod 的节点，并确保 repository 与 Tag 和 Helm 渲染结果完全一致。

此时把后续 values 中的两个 DeepAgents repository 改为本地导入名称：

```yaml
deepagents:
  runnerImage:
    repository: agentteams/deepagents-runner
    tag: "dev-..." # 替换为 AGENTTEAMS_LOCAL_TAG
worker:
  defaultImage:
    deepagents:
      repository: agentteams/deepagents-worker
      tag: "dev-..." # 替换为 AGENTTEAMS_LOCAL_TAG
controller:
  image:
    repository: agentteams/agentteams-controller
    tag: "dev-..." # 替换为 AGENTTEAMS_LOCAL_TAG
    pullPolicy: IfNotPresent
manager:
  image:
    repository: agentteams/manager
    tag: "dev-..." # 替换为 AGENTTEAMS_LOCAL_TAG
```

## Helm 启用

如果是升级已存在的 AgentTeams release，先显式更新 CRD。Helm 不会自动升级 `crds/` 中已经存在的定义：

```bash
kubectl apply -f helm/agentteams/crds/workers.agentteams.io.yaml
kubectl apply -f helm/agentteams/crds/executionsandboxes.agentteams.io.yaml
kubectl api-resources | grep -E 'workers|executionsandboxes'
```

创建独立 values 文件，不要把 checkpoint 凭据写入 Worker CR：

```yaml
deepagents:
  enabled: true
  runnerImage:
    repository: registry.lan:5000/agentteams/agentteams-deepagents-runner
    tag: dev
  state:
    size: 2Gi
    storageClassName: "storage-class-name" # 替换为集群实际 StorageClass
  checkpoint:
    existingSecret: ""
    aesKey: ""              # 空值时 Helm 生成并在升级时保留 32-byte key
  postgresql:
    enabled: true
    persistence:
      size: 20Gi
      storageClassName: "storage-class-name" # 替换为集群实际 StorageClass

worker:
  defaultImage:
    deepagents:
      repository: registry.lan:5000/agentteams/agentteams-deepagents-worker
      tag: dev

  # 保持 openclaw，或在所有依赖验证通过后改为 deepagents
  defaultRuntime: openclaw
```

部署或升级：

```bash
helm dependency build helm/agentteams
helm upgrade --install agentteams helm/agentteams \
  --namespace "${AGENTTEAMS_NAMESPACE}" --create-namespace \
  -f values-local.yaml \
  -f values-deepagents.yaml
```

Helm 会部署 PostgreSQL、checkpoint Secret 和一次幂等 migration init container。Controller 只有在 `deepagents.enabled=true` 且 checkpoint DSN/AES key 可用时才允许实际启动 DeepAgents Worker。
Kubernetes Worker Pod 通过 `SecretKeyRef` 引用这两个值，PodSpec 和 MinIO worker-deps 环境文件都不保存其明文；Docker 后端仍在进程环境中注入它们。

### 使用外部 PostgreSQL

生产环境可以关闭内置 PostgreSQL，并引用预先创建的 Secret：

```yaml
deepagents:
  enabled: true
  checkpoint:
    existingSecret: deepagents-checkpoint
    dsnKey: dsn
    aesKeyKey: aes-key
  postgresql:
    enabled: false
```

Secret 必须包含 `dsn` 和长度恰好为 16、24 或 32 UTF-8 bytes 的 `aes-key`。若同时启用内置 PostgreSQL，Secret 还必须包含 `postgres-password`（可通过 `checkpoint.postgresPasswordKey` 改名）。

## Egress 上限

Runner 默认为 ingress/egress 双向拒绝。Worker CR 只能在 Helm 设置的集群上限内申请 IP/CIDR 和端口，两者取交集：

```yaml
deepagents:
  sandbox:
    egressCeilings:
      - cidr: 10.20.0.0/16
        protocol: TCP
        ports: [443]
      - cidr: 192.168.10.25/32
        protocol: TCP
        ports: [22]
```

不要使用 `0.0.0.0/0` 作为生产上限。域名解析只在存在至少一条有效 egress 时开放给 `kube-system` 中带 `k8s-app=kube-dns` 标签的 DNS Pod；最终连接仍必须命中允许的 CIDR/端口。

## 创建 DeepAgents Worker

下面的 Worker 强制审批命令和文件写入，并仅允许一个精确 MCP tool 免审批：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: deep-researcher
spec:
  model: qwen-plus
  runtime: deepagents
  soul: |
    You are a research Worker. Keep evidence and report uncertainty.
  mcpServers:
    - name: github
      url: http://github-mcp.agentteams.svc/mcp
  runtimeConfig:
    deepagents:
      approvals:
        fileWrites: required
        mcpDefault: required
        mcpRules:
          - server: github
            tool: get_file_contents
            mode: notRequired
        coordinators:
          - "@operator:matrix.example.lan"
      execution:
        mode: sandbox
        idleTimeout: 30m
        maxLifetime: 8h
        resources:
          requests:
            cpu: 250m
            memory: 256Mi
          limits:
            cpu: "2"
            memory: 2Gi
        egress:
          - cidr: 10.20.0.0/16
            protocol: TCP
            ports: [443]
```

```bash
kubectl -n "${AGENTTEAMS_NAMESPACE}" apply -f deep-researcher.yaml
kubectl -n "${AGENTTEAMS_NAMESPACE}" get worker deep-researcher -o yaml
kubectl -n "${AGENTTEAMS_NAMESPACE}" get pod,pvc -l agentteams.io/worker=deep-researcher
```

`execute` 无论配置如何都需要 Human 审批。审批者在同一 Matrix thread 中回复：

```text
approve 1
reject 1 reason
edit 1 {"command":"safe command"}
approve all
```

## 验证清单

```bash
# PostgreSQL 与 migration
kubectl -n "${AGENTTEAMS_NAMESPACE}" get statefulset,pvc | grep deepagents
kubectl -n "${AGENTTEAMS_NAMESPACE}" logs deployment/agentteams-controller -c deepagents-checkpoint-migrate

# Worker 不应把 checkpoint 凭据传播到其它 Runtime
kubectl -n "${AGENTTEAMS_NAMESPACE}" get pod -l agentteams.io/runtime=deepagents

# Runner 应为按 thread 临时创建的独立资源
kubectl -n "${AGENTTEAMS_NAMESPACE}" get executionsandbox,pod,service,networkpolicy \
  -l agentteams.io/worker=deep-researcher

# Runner 的 PodSpec 应关闭 SA token automount，并以 UID 65532 运行
kubectl -n "${AGENTTEAMS_NAMESPACE}" get pod -l agentteams.io/runtime=deepagents-runner -o yaml
```

验证一个完整闭环：Matrix 发任务 → 出现审批消息 → Human 批准 → 创建 Runner → 文件变更写回 MinIO → `ExecutionSandbox` 到达 idle/max lifetime 后被回收。

## 故障排查

| 现象 | 优先检查 |
|---|---|
| Worker 报 runtime 未启用 | `deepagents.enabled`、Controller 的 `AGENTTEAMS_DEEPAGENTS_ENABLED`、checkpoint Secret keys |
| migration init container 重启 | PostgreSQL Service/PVC、DSN、NetworkPolicy、数据库密码 |
| Worker PVC 一直 Pending | kubeadm 集群是否存在默认 StorageClass，或 `deepagents.state.storageClassName` 是否正确 |
| Runner 一直 Pending | Runner 镜像、节点资源、`ExecutionSandbox.status.conditions` |
| Runner 进入 Failed | Runner Pod 终止原因与事件；Controller 会保留终止 Pod 供诊断，并拒绝自动重放结果不确定的命令 |
| Runner 无法访问目标 | Worker egress 请求、Helm ceiling、实际目标 IP、CNI 是否执行 NetworkPolicy |
| Matrix token 过期 | Worker 会用轮转的 ServiceAccount token 调用 Controller 刷新；检查 Controller API/RBAC 和 Pod token projection |
| 命令结果 unknown | Worker 已用同一 request ID 重试但仍无法确认结果；为避免重复副作用，不要自动重新执行 |

不要把 Matrix token、Gateway key、MinIO secret、checkpoint DSN/AES key 或 Runner token 写入 CR、ConfigMap、日志和命令参数。
