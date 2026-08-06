# DeepAgents Worker Runtime

AgentTeams 可以把 LangChain `deepagents` 作为独立的 Worker Runtime 使用。该集成保留 AgentTeams 的 Matrix 协作、Higress 模型/MCP 网关、MinIO 工作区和 Kubernetes CRD 控制面，同时将长任务 checkpoint、Human-in-the-loop 审批和命令执行分别放入明确的安全边界。

> DeepAgents 目前仅作为 **Worker Runtime**；Manager 仍使用 OpenClaw 或 CoPaw。生产部署以 Kubernetes 为目标，`execution.mode: sandbox` 依赖 `ExecutionSandbox` CRD 和 AgentTeams Controller。
>
> 当前 `execution.mode: sandbox` 仅支持 Worker 与 Runner 位于 Controller 所在的同一集群、同一命名空间；`deployMode: Edge`/`Remote` 会在创建 Matrix、MinIO、Pod 等资源前被 Controller 明确拒绝。跨集群 Runner、跨命名空间 Service/NetworkPolicy 和 checkpoint Secret 分发不在本阶段支持范围内。
>
> 本版本的 DeepAgents 工作区后端**仅支持 `storage.provider=minio`**。Helm values 或 Controller 投影若组合 `runtime=deepagents` 与 `storage.provider=oss`，会在创建 Worker 前明确拒绝；其它 Worker Runtime 的 OSS 支持不受影响。

## 运行架构

一次消息处理链路如下：

1. Human 或 Agent 在允许的 Matrix Room 中发送消息；DeepAgents Worker 按 Matrix thread 建立稳定的 LangGraph `thread_id`。
2. Worker 通过 Higress 调用模型和 MCP Server，使用 Worker 自己的 Gateway Consumer key。
3. LangGraph checkpoint 使用 AES-EAX 加密后写入 PostgreSQL；DSN 和 AES key 只注入 DeepAgents Worker，不会注入其它 Runtime。
4. `execute`、配置为 `required` 的文件写入或 MCP tool 会产生 Matrix 审批请求。只有 CRD 投影出的 Human 身份可以批准、拒绝或编辑，Agent 身份不能代替 Human 决策。
5. `execute` 获批后，Worker 通过 ServiceAccount 身份请求 `ExecutionSandbox`。Runner Pod 不持有 Matrix、Higress、MinIO、PostgreSQL 或 Kubernetes 凭据，只接收一个短期 bearer token。
6. Worker 在凭据边界内把安全工作区文件从 MinIO 注入 Runner，并根据 Runner 返回的变更清单写回 MinIO。

Worker 使用专属 PVC 保存 Matrix E2EE 设备数据、sync token 和待审批元数据。model、MCP 或 DeepAgents 策略变更会重建 Worker Pod，但该 PVC 和 PostgreSQL checkpoint 会保留。

同一状态 PVC 还保存持久化 Matrix 事件 journal。Worker 在接受新的 `next_batch` 前先把
本批事件按精确 event ID 记录为 `pending`，处理前再持久化为 `processing`，成功后标记为
`completed` 并删除消息正文。重启后会先排空已接受的 journal，再发起下一次 sync：
`pending` 事件可以继续处理；`completed` 事件按精确 ID 去重；恢复出的 `processing` 表示
上一次处理结果无法确认，Worker 会明确回复 unknown 并标记完成，而不会再次执行可能产生
副作用的处理。`completed` ID 在该 PVC 生命周期内保留，因此即使 Matrix 再次投递旧事件，
也不会因压缩哈希或时间窗口碰撞而重放。

DeepAgents Worker Pod 只有在 Matrix 返回合法 sync 响应、客户端接受该响应，并把
`next_batch` 原子持久化到状态 PVC 后才进入 Ready。Worker 随后只触发一次 post-sync
回调，在 Pod 的 `/tmp` `emptyDir` 中创建
`/tmp/agentteams-deepagents-ready`；Kubernetes exec readiness probe 只检查该文件。
无论是首次 full-state catch-up，还是从已有 sync token 增量恢复，都遵守同一顺序。
认证、join、sync 或 token 持久化失败时不会提前 Ready，因此 Controller 不会把尚未完成
Matrix 同步的 Worker 宣告为 Running，也不会遗漏启动窗口中的首条增量消息。

审批身份采用拒绝优先的角色投影：当前 Worker、Manager、Team Leader 和其它 Worker 都属于 Agent；Team Admin、`spec.humanMembers` 中 `role: coordinator` 的成员，以及额外配置的 `approvals.coordinators` 才可能属于 Human。若同一个 Matrix ID 同时出现在 Agent 与 Human 配置中，始终按 Agent 处理并拒绝其审批。`approvals.coordinators` 因此只能填写真实人类账号，不能用来把 Manager 或 Worker 提升为审批者。

这个 Agent 拒绝列表不是当前 Team 的局部 roster：Controller 会收集其可见范围内所有当前
Manager 和 Worker 的 `status.matrixUserID`（包括 Team Leader），排序、去重后投影为
`matrix.agentUserIds`。即使某个其它 Team、独立 Worker 或旧配置中的 Agent ID 又被填写到
`approvals.coordinators`，DeepAgents 仍按 Agent 拒绝其审批。

静态投影只是第一层防御。每次处理审批命令前，Worker 还会使用当前投影文件中新读取的短期
ServiceAccount token，向 Controller 发起有界、带认证的实时身份查询，只提交待判断的一个
精确 Matrix ID。接口按调用 Worker 的命名空间和身份自限定，只返回该 ID 当前是否属于受
Controller 管理的 Worker 或 Manager，不暴露完整 Agent 名单。实时查询超时、传输失败、
响应格式无效或身份无法验证时一律 fail-closed，拒绝审批；因此新创建或刚变更的 Agent
身份不会利用旧的 `matrix.agentUserIds` 快照获得 Human 权限。

Matrix 客户端只会加入 Controller 投影的 Personal Room 与 Team Room；其它账号发来的邀请会被忽略。加入 Room 或发送回复被 Homeserver 拒绝时，Worker 会显式记录失败，不会把错误响应当成发送成功。

Runner 返回的工作区变更清单不是直接写入 MinIO 的依据。Worker 会先下载全部待写文件，在单文件、文件数量和总字节上限内逐项核对路径、大小与 SHA-256；只有完整清单全部通过后才先上传新内容、再执行删除。校验或下载失败不会提前删除已有持久文件，也不应通过重跑命令来掩盖结果不确定性。

### Runner 的进程边界与 lease 撤销

Runner 只支持 Linux。它必须在打开 HTTP listener **之前**取走 bearer token、把
`RLIMIT_CORE` 设为零并调用 `prctl(PR_SET_DUMPABLE, 0)`；token 缺失、非 Linux 平台、
core-limit 设置失败或 non-dumpable 设置失败，都会使启动立即失败，既不会构造应用也不会
开始监听。

获批命令仅得到最小的非敏感环境（工作目录、locale、`PATH` 和临时目录）、关闭的继承文件
描述符以及独立 process session。Runner 在启动时从自己的环境移除 token；Linux 的
non-dumpable 设置使获批命令不能通过 `/proc` 读取 Runner 进程环境来取得该 token。这些是
缩小能力面的措施：`/bin/sh` **不是**通用的无副作用 sandbox；故意杀死容器 PID 1 仍是本版
未解决的拒绝服务风险。

当被引用的 Worker 被删除、所属 Controller/Worker UID 改变、runtime 改为非 DeepAgents、
DeepAgents 配置消失，或 `execution.mode` 不再是 `sandbox` 时，Controller 撤销已有 lease。
idle/max lifetime 到期和 HTTP Delete 也进入同一撤销状态机。Controller 会在创建任何子资源前
先持久化 cleanup finalizer；删除 lease 时使用 UID 与 resourceVersion 前置条件，再由 finalizer
按 **Service → token Secret → Pod → 等待 Pod 删除 → NetworkPolicy → 移除 finalizer** 的顺序
收敛。Pod、Service 和 Secret 必须同时匹配当前 controller/worker/sandbox label 与
ExecutionSandbox controller owner UID。NetworkPolicy 是有意的例外：它不设置 ownerReference，
而使用同一组 label、`runtime=deepagents-runner` 和
`agentteams.io/execution-sandbox-uid=<ExecutionSandbox UID>` 绑定当前 generation，并由显式
NetworkPolicy watch 触发 reconcile。这样 foreground 删除 Sandbox 或 Worker 时，Kubernetes GC
不能在 Pod 消失前独立删除隔离策略。Controller 只有通过未缓存 API-server 读取确认这一代 Pod
已不存在后，才以 NetworkPolicy UID/resourceVersion 前置条件删除它，并用当前 resourceVersion
移除 finalizer；同名替代对象、归属漂移、冲突或暂时仍在 Terminating 的 Pod 都会保留隔离边界
并重试。旧版本创建、且 owner 精确指向当前 Sandbox UID 的单一 ownerReference NetworkPolicy 会
原地移除 ownerReference 并补写 UID 注解；任何 foreign/malformed 同名资源都不会被接管或删除。
Runner 的应用级 readiness 使用容器内 `curl 127.0.0.1:8080/healthz`，避免支持 NetworkPolicy
的 CNI 将 kubelet 从节点发起的 HTTP 探针当作未授权 ingress；这不会为节点网段增加例外，也
不会放宽 Worker 到 Runner 之外的访问边界。

Worker watch 通过 `ExecutionSandbox.spec.workerRef.name` 字段索引反查 lease，并继续按命名空间和
当前 Controller label 限定；即使 `agentteams.io/worker` label 缺失或错误，正确的 `workerRef`
仍会立即入队。cache List 失败会留下错误日志并回退到未缓存 API reader，常规生命周期 requeue
继续提供有界安全复查。

DeepAgents Worker 首次 Matrix sync 尚未完成时，`Ready=False` 是预期的 `Starting` 状态，
不是终态失败。只有显式的容器等待/终止失败状态才会标记为失败；成功的合法 sync、客户端
接受响应及 `next_batch` 持久化后，readiness 文件才会创建。

本地验证禁止读取或输出部署密钥。可运行包含非敏感 sentinel 的进程加固测试；测试只断言
token inspection 被阻止，绝不打印 token：

```bash
cd deepagents-agentteams
uv run --locked --extra dev pytest -q tests/test_runner_process_hardening.py
```

在一次性本地集群中，把三个身份变量设置为当前 Controller、Worker 和 `ExecutionSandbox`
名称。下面的检查用 controller/worker/execution-sandbox/runtime 四个 label 精确解析唯一 Runner，
先确认 exec 通道可用，再静默尝试从 `/proc/1/environ` 读取一个字节；它不输出该文件或环境块：

```bash
mapfile -t RUNNER_PODS < <(
  kubectl -n "${AGENTTEAMS_NAMESPACE}" get pod \
    -l "agentteams.io/controller=${AGENTTEAMS_CONTROLLER_NAME},agentteams.io/worker=${DEEPAGENTS_WORKER_NAME},agentteams.io/execution-sandbox=${EXECUTION_SANDBOX_NAME},agentteams.io/runtime=deepagents-runner" \
    -o name
)
if [ "${#RUNNER_PODS[@]}" -ne 1 ]; then
  printf 'expected exactly one Runner Pod, got %d\n' "${#RUNNER_PODS[@]}" >&2
  exit 1
fi
RUNNER_POD="${RUNNER_PODS[0]#pod/}"
kubectl -n "${AGENTTEAMS_NAMESPACE}" exec "${RUNNER_POD}" -- sh -c 'exit 0' >/dev/null
if kubectl -n "${AGENTTEAMS_NAMESPACE}" exec "${RUNNER_POD}" -- \
  sh -c 'dd if=/proc/1/environ of=/dev/null bs=1 count=1' >/dev/null 2>&1; then
  printf 'READABLE\n'
  exit 1
fi
printf 'BLOCKED\n'
```

不要通过容器运行时另行创建的 exec 进程枚举环境来推断获批命令的环境。获批命令的最小环境
只能由上面的真实进程 pytest 验证，或经 Human 批准的正常 Runner 执行路径返回单一布尔/状态；
任何路径都不得输出环境块或 token。

### 模型 URL 与 Worker prompt 合约

Controller 管理的 Higress 根地址在运行时文档中规范为 OpenAI-compatible base，结尾恰好
一个 `/v1`；例如 `https://aigw.example.lan` 与
`https://aigw.example.lan/v1/` 都得到 `https://aigw.example.lan/v1`。适配器也会兼容
旧文档中的无版本 Higress 根地址。显式配置的外部 provider 若已经使用版本化或非根路径，
则保留该路径，不盲目追加第二个 `/v1`。

Worker 的 `spec.identity`、`spec.soul` 和 `spec.agents` 会以明确、互不混淆的 section
加入 DeepAgents system prompt，同时保留固定的 AgentTeams 房间、审批和凭据安全边界。
本版本**不把 `spec.packages` 或 `spec.skills` 安装/转换为 DeepAgents subagent、tool 或
prompt**；这些字段可由其它 Runtime 消费，但不能据此声称 DeepAgents 已加载对应包或技能。
需要工具时应通过 Controller 投影的 MCP Server 明确配置。

## kubeadm 集群前提

在三节点或更多节点的 kubeadm 集群中启用前，逐项确认：

- 已安装支持 `NetworkPolicy` 的 CNI，例如 Calico 或 Cilium。仅安装不执行 NetworkPolicy 的 CNI 会破坏 Runner 和 PostgreSQL 的隔离假设。
- 集群存在可动态供给 `ReadWriteOnce` PVC 的默认 `StorageClass`，或者在 values 中显式填写 StorageClass。原生 kubeadm 不会自动提供动态存储类。
- 若为本地演练选择 `local-path`，要明确它的卷是节点本地卷：Pod 被重调度到其它节点或节点故障时数据不可高可用，不能把它当作生产 HA 存储。
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
export AGENTTEAMS_OPENCLAW_BASE_IMAGE="${AGENTTEAMS_OPENCLAW_BASE_IMAGE:-higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base}"
export AGENTTEAMS_OPENCLAW_BASE_VERSION="${AGENTTEAMS_OPENCLAW_BASE_VERSION:-20260423-8359cbc}"
make build-agentteams-controller build-manager \
  build-deepagents-worker build-deepagents-runner \
  VERSION="${AGENTTEAMS_LOCAL_TAG}" \
  OPENCLAW_BASE_IMAGE="${AGENTTEAMS_OPENCLAW_BASE_IMAGE}" \
  OPENCLAW_BASE_VERSION="${AGENTTEAMS_OPENCLAW_BASE_VERSION}"
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
export AGENTTEAMS_DEEPAGENTS_ARCHIVE="/var/tmp/agentteams-deepagents-${AGENTTEAMS_LOCAL_TAG}.tar"

docker save \
  "agentteams/agentteams-controller:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/manager:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-worker:${AGENTTEAMS_LOCAL_TAG}" \
  "agentteams/deepagents-runner:${AGENTTEAMS_LOCAL_TAG}" \
  -o "${AGENTTEAMS_DEEPAGENTS_ARCHIVE}"

export AGENTTEAMS_DEEPAGENTS_ARCHIVE_SHA256="$(sha256sum "${AGENTTEAMS_DEEPAGENTS_ARCHIVE}" | awk '{print $1}')"

(
  set -e
  for AGENTTEAMS_NODE in agent2@10.13.36.138 agent3@10.13.36.173; do
    scp "${AGENTTEAMS_DEEPAGENTS_ARCHIVE}" \
      "${AGENTTEAMS_NODE}:${AGENTTEAMS_DEEPAGENTS_ARCHIVE}"
    ssh "${AGENTTEAMS_NODE}" \
      "printf '%s  %s\n' '${AGENTTEAMS_DEEPAGENTS_ARCHIVE_SHA256}' '${AGENTTEAMS_DEEPAGENTS_ARCHIVE}' | sha256sum -c -"
    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images import '${AGENTTEAMS_DEEPAGENTS_ARCHIVE}'"
    ssh -t "${AGENTTEAMS_NODE}" \
      "sudo ctr -n k8s.io images list | grep '${AGENTTEAMS_LOCAL_TAG}'"
    ssh "${AGENTTEAMS_NODE}" \
      "rm -- '${AGENTTEAMS_DEEPAGENTS_ARCHIVE}'"
  done
)
```

归档应放在 `/var/tmp` 等磁盘文件系统，不要假设节点 `/tmp` 有足够空间；部分 kubeadm 节点会把 `/tmp` 挂载为容量较小的 `tmpfs`。上述循环会先校验 SHA-256，并且只有导入和标签检查都成功后才删除 worker 上的临时归档。

启用内置 PostgreSQL 时还必须保证所有可调度节点能取得 `postgres:17-alpine`。Docker Hub 不可达的局域网环境应按主部署指南先从批准的 Docker Official Images 镜像源拉取、验证并重打该标签，再把它加入同一离线归档。

无 Registry 路径下，把 Helm Controller 的 `image.pullPolicy` 设为 `IfNotPresent`（或严格离线时设为 `Never`）；Controller 创建的 Manager/Worker/Runner 使用 `IfNotPresent`。因此必须把四张镜像导入所有可能承载这些 Pod 的节点，并确保 repository、Tag 和 digest 与 Helm 渲染结果完全一致。

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

如果是升级已存在的 AgentTeams release，必须先审阅并显式更新 CRD。Helm 只在首次安装时
创建 Chart `crds/` 中的资源，不会升级集群里已经存在的 CRD；因此不能把 `helm upgrade`
当作 CRD 升级步骤。先确认生成的 CRD 与 Go 类型同步，再执行服务端差异预览：

```bash
make check-crd-sync

AGENTTEAMS_CRD_DIFF_STATUS=0
kubectl diff --server-side \
  --field-manager=agentteams-crd-upgrade \
  -f helm/agentteams/crds/ || AGENTTEAMS_CRD_DIFF_STATUS=$?
if [ "${AGENTTEAMS_CRD_DIFF_STATUS}" -gt 1 ]; then
  exit "${AGENTTEAMS_CRD_DIFF_STATUS}"
fi
```

`kubectl diff` 返回 `1` 只表示存在预期差异；返回值大于 `1` 才是错误。人工确认没有删除或
重命名仍在使用的字段、无默认值的新 required 字段、收紧的 enum/pattern/limit，以及
served/storage version 或 conversion 破坏后，才执行：

```bash
kubectl apply --server-side \
  --field-manager=agentteams-crd-upgrade \
  -f helm/agentteams/crds/
kubectl api-resources | grep -E 'workers|executionsandboxes'
```

不要用 `--force-conflicts` 跳过字段所有权或兼容性审阅。新 CRD 必须同时兼容当前已存储的
自定义资源，以及滚动升级期间仍在运行的旧 Controller。执行前备份当前 CRD、Manager、
Worker、Team、Human 和 ExecutionSandbox 对象。`helm rollback` 不会回滚 CRD；需要回退时，
先确认现存对象未使用新字段或新版本，再对已备份的已知良好 CRD 重复
`kubectl diff --server-side` 和 `kubectl apply --server-side`。若对象已依赖新 schema，必须先
备份并转换对象，不能直接降级 CRD。

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

Manager 或运维人员应通过 `agt` 写入 `runtimeConfig`，不要手工拼 Controller REST JSON。
安全默认路径会启用 sandbox，并把文件写入和 MCP 默认策略设为 Human 审批：

```bash
agt create worker --name deep-researcher --runtime deepagents \
  --deepagents-sandbox \
  --deepagents-coordinators '@operator:matrix.example.lan' \
  --no-wait
```

如需 egress、idle/max lifetime 或执行资源等高级策略，准备只包含
`WorkerRuntimeConfig` 对象的单份 JSON/YAML 文档，再使用
`agt create worker --runtime deepagents --runtime-config-file <FILE>` 或
`agt update worker --name <NAME> --runtime-config-file <FILE>`。CLI 会拒绝重复 YAML key、
多个文档、未知字段，以及把 `--runtime-config-file` 与 DeepAgents convenience flags
混用；配置文件中不得包含任何凭据。

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
            ephemeralStorage: 512Mi
          limits:
            cpu: "2"
            memory: 2Gi
            ephemeralStorage: 4Gi
        egress:
          - cidr: 10.20.0.0/16
            protocol: TCP
            ports: [443]
```

`idleTimeout` 与 `maxLifetime` 使用 DeepAgents 与 Controller 共同执行的规范语法：只能由
小写 `h`、`m`、`s` 片段组成，允许带前导数字的小数片段，但全部片段相加后必须恰好是
正整数秒。例如 `6h30m`、`0.5m` 和 `0.5s0.5s` 合法；`500ms`、`1000ms`、`1.5s`、
`.5m`、大写单位和前后空白都不合法。Controller 不会把小数秒静默取整，也不会接受
DeepAgents 无法解析的 Go 专用 `ms`/`us`/`ns` 单位。

通过 Controller API 创建或更新 Worker 时，无效时长返回 `400 Bad Request` 且不创建或
修改 Worker；直接提交 Worker CR 时，reconcile 会在创建 Worker Pod 前失败。HTTP ensure
同样会在创建或更新 `ExecutionSandbox` 前拒绝；直接提交的无效 `ExecutionSandbox` CR
则按 `Failed/InvalidPolicy` fail-closed，且不会启动 Runner。

在命名 Controller 的 Kubernetes 部署中，HTTP ensure 创建的 `ExecutionSandbox` 会继承
Worker 的精确 `agentteams.io/controller` 归属。Controller cache、主资源/子资源 watch 和
reconcile 入口都会再次核对该标签及被引用 Worker 的归属；Ensure、heartbeat、delete 也
不会读取或修改其它 Controller 的 sandbox。通过 `kubectl` 直接创建
`ExecutionSandbox` 时必须填写与 Worker 相同的 Controller 标签，未标记或错误标记的对象
不会被该 Controller 接管。空 Controller name 只用于嵌入式兼容，并且只拥有未标记对象。

### Runner 临时存储策略

`runtimeConfig.deepagents.execution.resources` 只作用于每次命令的
`ExecutionSandbox` Runner，不会改变通用 Worker 或 Manager 的 `spec.resources`。
Helm 默认将 Runner 的 `ephemeralStorage` request、limit 和集群最大 limit 分别设为
`256Mi`、`2Gi`、`8Gi`。Worker 可以分别覆盖 request 与 limit；缺失的一侧使用对应
默认值，但 request 不得大于 limit，limit 不得超过 `8Gi`。

Controller 将解析后的临时存储 request/limit 同时应用到 Runner 容器，Kubernetes 按
容器的 aggregate ephemeral-storage 使用量执行该限制。Runner 的 `/workspace` 和
`/tmp` 各自使用 `emptyDir`，两者的 `sizeLimit` 都等于解析后的 ephemeral-storage
limit（例如上例均为 `4Gi`）；因此应同时为两处写入预留容量。

通过 HTTP ensure 接口提交无效值（格式错误、非正数、request 大于 limit 或 limit
超过 `8Gi`）会返回 `400 Bad Request`，不会创建 Runner。直接创建或更新
`ExecutionSandbox` CR 时，Controller 会将其标记为 `Failed`，并设置
`InvalidResources` 状态原因，同样不会创建 Runner Pod、Service 或 NetworkPolicy。

同一个 session 已存在的 `ExecutionSandbox` 会在返回新 generation 的 Ready token 前，
先收敛到 Worker 最新的 CPU/内存/临时存储、egress、idle timeout 和 max lifetime 策略。
idle/max-lifetime 回收后，Worker 只在下一次真正准备发送 Runner 请求之前重新 ensure 并
取得新 lease；健康检查不会主动重建 Runner。若请求已经发出但结果仍不确定，则保持
fail-closed。一次获批命令恰好只向 Runner 发出一次 `POST /v1/execute`；连接中断或 Runner
返回冲突导致结果不确定时，不以相同或新 request ID 重试，也不重新 ensure 后重放命令，
避免重复副作用。request ID 只用于标识这一次请求，不是自动重试许可。

DeepAgents 的同步与异步 `list/read/write/edit/delete/grep/glob` 文件工具均由适配层覆盖，
通过要求 Runner bearer token 的 `/v1/files/*` 受限 API 完成，不继承 `BaseSandbox` 以
shell `execute()` 模拟文件操作的默认实现。Runner 对路径实施 `/workspace` 边界、禁止父目录
穿越和符号链接逃逸，并限制单文件、批量与搜索规模；这些请求不会创建命令执行状态。
写入、编辑和删除仍生成精确 change manifest，由带 MinIO 凭据的 Worker 校验并持久化。
因此 Human 批准的显式 `execute` 与 `/v1/execute` 审计记录保持一一对应，后续 `read_file`
验证不会产生第二条未审批的命令执行记录。

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
| Worker Pod Running 但未 Ready | 检查 Matrix whoami/join/sync、状态 PVC 可写性和 `sync-token` 持久化；就绪文件只会在首次合法同步完成后创建 |
| Runner 一直 Pending | Runner 镜像、节点资源、`ExecutionSandbox.status.conditions` |
| Runner 进入 Failed | Runner Pod 终止原因与事件；Controller 会保留终止 Pod 供诊断，并拒绝自动重放结果不确定的命令 |
| Runner 无法访问目标 | Worker egress 请求、Helm ceiling、实际目标 IP、CNI 是否执行 NetworkPolicy |
| Matrix token 过期 | Worker 会用轮转的 ServiceAccount token 调用 Controller 刷新；检查 Controller API/RBAC 和 Pod token projection |
| Matrix 收到消息但没有回复 | 检查 Worker 日志中的 `Matrix message handling failed`；再沿 traceback 核对 checkpoint、LLM、MCP 或 Runner 失败，不要把消息正文写入诊断脚本 |
| 工作区写回失败 | 检查 Runner 下载响应、变更清单大小/SHA-256、MinIO 可用性；Worker 不会自动重跑已完成的命令 |
| 命令结果 unknown | 唯一一次 Runner `POST /v1/execute` 的传输或执行结果无法确认；Worker 不会以相同或新 request ID 自动重试，也不会更换 Runner 后重放。应由 Human 检查 Runner、工作区和外部副作用后决定下一步 |

不要把 Matrix token、Gateway key、MinIO secret、checkpoint DSN/AES key 或 Runner token 写入 CR、ConfigMap、日志和命令参数。
