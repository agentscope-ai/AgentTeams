# DeepAgents 最终隔离与生命周期加固设计

## 状态

已批准，并已按最终复审加固。本文补充现有 DeepAgents Worker Runtime、ExecutionSandbox
和 Matrix readiness 设计，关闭初次独立审计确认的三个 Important 问题，以及最终复审确认的
五个 Important 和一个 Minor 问题。本文不改变 DeepAgents 作为 Worker-only Runtime 的产品
定位，也不扩大 Runner、Worker 或 Controller 的凭据权限；cleanup finalizer、field index、
未缓存读取和删除前置条件均为控制面实现，不改变 CRD OpenAPI schema。

## 背景与已确认问题

最终集群验收已证明 Matrix、Human 审批、DeepSeek、Runner、MinIO、PostgreSQL 和
idle reclaim 主链路工作正常，但独立审计确认以下边界仍不完整：

1. Runner HTTP 进程和获批 shell 命令都以 UID 65532 运行。即使命令环境经过清洗，
   命令仍能从 `/proc/1/environ` 读取 Runner 初始环境中的 bearer token，并在已批准
   请求结束后自行调用 `/v1/execute`。
2. ExecutionSandbox reconciler 在 Worker 被删除、UID 改变、切换 Runtime 或把
   `execution.mode` 从 `sandbox` 改为 `disabled` 时，会在生命周期回收逻辑之前返回。
   已存在的 Runner 子资源因此可能不再被回收。
3. DeepAgents Worker 在首次有效 Matrix sync 前按设计保持 `Ready=False`，但 Kubernetes
   backend 把带有 `ContainersNotReady` message 的正常等待状态错误映射为 `Failed`。
4. 直接删除 ExecutionSandbox 或 Worker owner-GC 可绕过撤销顺序；缺少 finalizer 时，
   Pod 仍在 Terminating 而 NetworkPolicy 已被垃圾回收。
5. informer cache 的 Pod NotFound 不是 API server 上该 Pod generation 已消失的证明。
6. 名称删除没有 UID/resourceVersion 前置条件，旧 reconcile/HTTP 请求可能删除同名替代
   generation；Worker mapper 依赖可漂移的 `agentteams.io/worker` label，且 List 失败无日志。
7. 原集群探针使用未定义 Pod 名，并枚举容器运行时另建 exec 进程的环境，检查了错误边界。

第一个问题已用无敏感信息的 sentinel token 在最终 Runner 镜像中复现。命令成功读取
`/proc/1/environ`，因此不能只依赖子进程环境白名单。

## 目标

- 让获批命令无法读取或复用 Runner bearer token。
- Worker 不再满足 sandbox 前提时，立即、可重入且 fail-closed 地撤销旧 Runner。
- 用 finalizer、未缓存 Pod absence 证明和 UID/resourceVersion 前置条件保证撤销不会跨代，
  同名替代对象或 label/owner 漂移不会被误删。
- Matrix 首次同步期间的正常 readiness 等待保持 `Starting`，真实容器故障仍为 `Failed`。
- 保留现有 exactly-once request ID、unknown-outcome、MinIO 写回校验、PostgreSQL
  checkpoint、NetworkPolicy 和非 root Pod 约束。
- 通过自动测试和局域网 kubeadm 集群重新证明完整闭环。

## 非目标

- 不在本次引入认证 sidecar、远程执行服务或每命令一个新 Pod 的架构。
- 不允许 Runner 获得 Kubernetes、Matrix、Higress、MinIO 或 PostgreSQL 凭据。
- 不把 shell 命令本身变成无副作用沙箱；Human 批准的命令仍可修改 Runner workspace。
- 不解决获批命令主动终止 Runner 所造成的拒绝服务。该情况继续按 unknown outcome
  fail-closed，且不会自动重放命令。
- 不改变其它 Worker Runtime 的 readiness 或资源语义。

## 方案一：Runner 进程秘密边界

### 启动顺序

Runner 仅支持 Linux Kubernetes 环境。PID 1 在启动 HTTP 服务前按以下顺序完成初始化：

1. 从 `AGENTTEAMS_RUNNER_TOKEN` 读取非空 token。
2. 立即从 Python 的 `os.environ` 删除该键，避免后续子进程继承。
3. 把 core dump soft/hard limit 都设为零。
4. 通过 Linux `prctl(PR_SET_DUMPABLE, 0)` 把 Runner 进程设为不可 dump。
5. 只有以上步骤全部成功后，才构造 FastAPI app 并开始监听。

任何一步失败都必须在监听端口前退出，不能降级为只清洗环境。`PR_SET_DUMPABLE=0`
使同 UID 子进程无法通过 ptrace 权限检查读取父进程的 `/proc/<pid>/environ`、`mem`
和受保护的 fd 入口；`RLIMIT_CORE=0` 防止 token 随 core dump 落盘。token 仍只保存在
Runner 进程内存中并用于固定时长比较。

### 命令子进程

`RunnerService.execute` 继续使用显式最小环境，并显式保持：

- `close_fds=True`，不把 HTTP 连接或内部描述符传给 shell；
- `start_new_session=True`，保留当前超时后杀进程组的行为；
- 无 `AGENTTEAMS_RUNNER_TOKEN`、Matrix、Higress、MinIO、PostgreSQL 或 Kubernetes
  环境变量；
- 相同 request ID 的持久化结果与 ambiguous-result 语义不变。

健康检查保持无认证。其它 Runner API 继续要求 bearer token。命令即使知道 Service
地址，也无法构造已认证的新请求。

### 选择理由与边界

认证 sidecar 需要新增跨容器 IPC；若执行命令能访问同一 IPC，又会形成新的绕过面。
每命令独立 Pod 隔离更强，但会重构 ExecutionSandbox 生命周期、结果恢复和延迟模型。
本方案直接关闭已证实的 `/proc` 和内存检查路径，不增加容器权限，也不改变 CRD。

同 UID 命令仍可能向 PID 1 发送信号，这是拒绝服务而非凭据提升。Worker 对请求结果
不确定的情况继续只报告 unknown，不重试、不换 request ID 重放。

## 方案二：ExecutionSandbox fail-closed 撤销

### 撤销条件

当前 Controller 拥有的 ExecutionSandbox 在以下任一条件成立时必须进入撤销：

- 引用的 Worker 不存在；
- Worker 已设置 `deletionTimestamp`；
- 当前 Worker 的 controller label 与本 Controller 不匹配；
- `workerRef.uid` 为空或与当前 Worker UID 不一致；
- Worker 的有效 Runtime 不再是 `deepagents`；
- DeepAgents runtime config 不存在，或 `execution.mode` 不再是 `sandbox`。
- idle timeout 或 max lifetime 到期，或 HTTP API 请求删除 lease。

不属于当前 Controller 的 ExecutionSandbox 仍保持忽略，不能跨 Controller 删除资源。

### Finalizer 与删除入口

HTTP 创建的 ExecutionSandbox 从创建时就携带稳定 cleanup finalizer。直接创建且尚未删除的
对象由 reconciler 先写入 finalizer，并在该次 reconcile 立即返回；在 finalizer 持久化前
不得创建 Secret、Service、Pod 或 NetworkPolicy。已经进入删除但缺少此 finalizer 的旧对象
仍执行 best-effort 有序清理，但不得在 `deletionTimestamp` 之后补加 finalizer 或创建子资源。

Worker stale/deleting、idle/max lifetime 到期与 HTTP Delete 不直接清理子资源，而是以
ExecutionSandbox 当前 UID 和 resourceVersion 作为删除前置条件，请求 CR 进入 deleting。
Conflict、UID/RV precondition failure 或并发 heartbeat 导致的 resourceVersion 变化必须返回并
重新求值，禁止退化为 name-only delete。真正的子资源清理由 deleting/finalizer 分支唯一负责。

### 撤销顺序与身份边界

撤销必须可重入，并避免在 Pod 仍存活时先移除 NetworkPolicy：

1. 未缓存读取同名 Service，校验精确 controller/worker/execution-sandbox labels 与指向当前
   ExecutionSandbox UID 的 controller owner reference，再以目标 UID+resourceVersion 删除。
2. 对 token Secret 执行相同身份校验和带前置条件删除，阻止替代 Pod 取得 token。
3. 未缓存读取、校验并以 UID+resourceVersion 请求删除 Runner Pod。
4. 通过 `mgr.GetAPIReader()` 再次直接读取 API server；只要精确 owned Pod 仍存在（包括
   Terminating），就保留 NetworkPolicy 与 ExecutionSandbox finalizer 并短间隔 requeue。
   cache NotFound 不能跨过这条隔离边界。
5. NetworkPolicy 不设置 ownerReference，避免 Sandbox 或 Worker 的 foreground cascading delete
   让 Kubernetes GC 在 Pod 消失前独立回收隔离策略。它以精确 controller/worker/sandbox/runtime
   labels 和 `agentteams.io/execution-sandbox-uid` 注解绑定当前 Sandbox generation；Controller
   通过显式 label mapper watch 它，而不是依赖 `.Owns()` owner watch。
6. 只有未缓存读取返回 Pod NotFound 后，才校验 NetworkPolicy 的 label、runtime 和 Sandbox UID
   注解，并以目标 UID+resourceVersion 前置条件删除它；随后重新读取当前 ExecutionSandbox，
   校验 UID/controller identity，并用当前 resourceVersion 移除 cleanup finalizer。Kubernetes
   随后完成 CR 删除。

升级时，只允许迁移恰好含一个、且 controller owner 精确指向当前 ExecutionSandbox UID 的旧版
NetworkPolicy：原地更新会删除 ownerReference、补写 UID 注解并保留该对象 UID。缺少新 UID 注解
且又不满足这一旧版身份的对象，以及任何 foreign/malformed owner，都不能被接管、修改或删除。

所有 NotFound 都视为成功。任何同名替代 generation、foreign/malformed owner/UID binding 或 label 转移
都返回错误，保留 NetworkPolicy 和 finalizer 等待重试或运维修复。其它 Kubernetes API 错误
同样返回 reconcile error；撤销路径不创建新 Secret、Pod、Service 或 NetworkPolicy。无效
资源/策略的 fail-closed cleanup 复用相同未缓存 Pod absence 与精确删除边界，但保留 Failed CR
供观察。

### Worker watch

ExecutionSandbox controller 增加 Worker watch。收到 Worker create/update/delete 后，
mapper 使用预先注册的 `ExecutionSandbox.spec.workerRef.name` field index，只列出同命名空间、
同 controller label 且 spec 引用该 Worker 的 ExecutionSandbox，并为每个对象入队。它不依赖
`agentteams.io/worker` label 是否存在或正确。cache List 失败时必须记录错误，再通过未缓存
API reader 按命名空间/controller label 列出并在内存中按 workerRef 过滤；回退失败也记录错误。
正常 Sandbox lifecycle requeue 仍是有界安全复查。Worker update predicate 保留 old-or-new
controller label 语义，使失去归属的更新仍可触发撤销。

### HTTP Delete

ensure 和 heartbeat 继续要求 Worker 当前为 DeepAgents sandbox 模式。Delete 只要求：

- URL 中 Worker 存在且属于当前 Controller；
- session ID 合法；
- 找到的 ExecutionSandbox 仍引用该 Worker 名称和 UID，并属于当前 Controller。

因此在 `sandbox -> disabled` 的短暂竞态中，Worker 仍能显式删除旧 lease。跨 Worker、
跨 UID 或跨 Controller 删除继续返回冲突或未找到。实际 Delete 使用读取到的
ExecutionSandbox UID 与 resourceVersion 作为 API-server 前置条件；并发 heartbeat 或同名替代
generation 使旧请求冲突，而不是误删当前 lease。子资源仍只由 finalizer 分支清理。

## 方案三：Matrix readiness 状态映射

Kubernetes backend 先使用现有 `podContainerFailureStatus` 检查 init/container 的明确
waiting 或 terminated 故障，例如 `ImagePullBackOff`、`CrashLoopBackOff` 和非零退出。
只有没有明确容器故障时才解释 Pod Ready condition：

- `Ready=True`：`Running`；
- `Ready=False` 或 Ready condition 尚未出现：`Starting`；
- Ready condition 的 message 可作为诊断文本保留，但不能单独把状态升级为 `Failed`。

DeepAgents exec readiness probe 在首次合法 Matrix sync token 原子持久化后才创建 ready
文件，因此正常的 `ContainersNotReady` 会保持 `Starting`。其它 Runtime 的真实容器故障
仍由更高优先级的 waiting/terminated 检查标记为 `Failed`。

## 数据流与错误处理

```text
Worker starts
  -> Matrix sync accepted and token durable
  -> readiness file created
  -> Worker becomes Running

Human approves execute
  -> Worker ensures ExecutionSandbox
  -> Runner consumes token and installs non-dumpable boundary
  -> Worker sends one authenticated execute POST
  -> shell receives no token/fds and cannot inspect Runner /proc state
  -> result persisted, workspace changes validated and synced to MinIO

Worker runtime/mode/UID changes
  -> Worker watch enqueues related sandboxes
  -> preconditioned ExecutionSandbox delete enters finalizer cleanup
  -> exact Service + Secret removed with identity/precondition checks
  -> exact Pod removed while NetworkPolicy + finalizer stay in force
  -> uncached API read proves Pod generation absent
  -> exact NetworkPolicy removed, then current-RV finalizer removed
```

Runner hardening失败属于启动失败，Kubernetes 不会把 Pod标记 Ready。撤销中 API 暂时
失败会保持 fail-closed 资源并重试。Readiness 尚未满足不会成为终态失败，调用方继续等待。

## 自动测试

### Python

- 在独立 helper 进程中注入 sentinel token，执行完整 token 消费与 hardening，再通过
  `RunnerService.execute` 尝试读取 `/proc/$PPID/environ`；断言读取失败且输出不含 sentinel。
- 断言 hardening 后普通子进程环境不含 token。
- 模拟 `prctl` 或 core-limit 设置失败，断言 Runner 在创建 app/监听前失败。
- 断言正常 bearer 请求、401、exactly-once request ID 和 unknown outcome 行为不变。

### Go Controller

- Ready Sandbox 对应 Worker deleting/`disabled`、其它 Runtime、不同 UID 或 NotFound 时，
  先用 UID+resourceVersion 请求删除 CR；finalizer 移除 Service/Secret/Pod 并保留 NetworkPolicy，
  未缓存读取确认 Pod generation 消失后再删除 NetworkPolicy 和 finalizer。
- 同一撤销 reconcile 重复执行不报错、不重建资源。
- Worker watch mapper 按 spec.workerRef 入队当前命名空间/Controller 的 Sandbox，覆盖 Worker
  label 缺失/错误，并对 cache/API-reader List 失败留下日志。
- HTTP Delete 在 Worker 已 disabled 时仍可请求删除正确 lease，但拒绝 UID/controller/RV
  冲突和替代 generation。
- fake-client 单测覆盖 finalizer-before-children、cache NotFound/API-reader Pod present、精确
  owner/label、所有 DeleteOptions 前置条件及 fallback；envtest 使用真实 API server 覆盖
  finalizer、Terminating Pod/NetworkPolicy 边界、并发 heartbeat RV 与替代 UID 的 precondition。
- `Running + Ready=False + ContainersNotReady message` 返回 `Starting`；明确
  CrashLoop/ImagePull/terminated 仍返回 `Failed`。

每项先添加能在当前实现上按预期失败的测试，再实现最小修复并跑 focused/full suite。

## 集群验收

1. 构建新的不可变 Controller、Manager、默认 Worker、DeepAgents Worker 和 Runner 镜像；
   把精确镜像导入 agent2/agent3 并用 `crictl inspecti` 比对 digest。
2. 先 server-side 审阅/应用 CRD，再原子升级 Helm。
3. 验证 DeepAgents Worker 在 Matrix sync 前为 Starting、sync 后为 Running。
4. 用 controller/worker/execution-sandbox/runtime 四个 label 精确选出唯一 Runner；集群
   exec 只静默尝试读取 `/proc/1/environ`，不得枚举 exec 进程环境。获批命令最小环境只通过
   真实进程 pytest，或 Human 批准的正常 Runner 路径返回 `BLOCKED`/`READABLE` 单一状态验证，
   绝不输出环境块或真实 token。
5. 再执行正常文件任务，确认恰好一次 `/v1/execute`、MinIO 内容、PostgreSQL checkpoint
   和 idle reclaim。
6. 验证 Worker mode/runtime 切换会撤销旧 Sandbox，且在 Pod 删除前 NetworkPolicy 一直
   存在。
7. 复跑无效资源、duration 和 egress fail-closed 测试；确认所有平台 Pod Ready、0 重启。

## 回滚

如果 Runner hardening 不能启动，回滚 Runner/Worker 镜像到上一不可变版本；Controller
和 CRD 无需回滚。如果 Controller 撤销或 readiness 映射产生回归，回滚 Controller、Manager
和默认 Worker 到上一组已验证 digest。Helm rollback 不回滚 CRD；本设计不新增 CRD 字段。

回滚期间不自动重放结果不确定的命令。任何已存在 Sandbox 继续按旧版本生命周期处理，
运维人员可在确认无正在执行请求后显式删除对应 ExecutionSandbox。
