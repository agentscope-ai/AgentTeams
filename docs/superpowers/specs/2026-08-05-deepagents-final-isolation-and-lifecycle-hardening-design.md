# DeepAgents 最终隔离与生命周期加固设计

## 状态

已批准。本文补充现有 DeepAgents Worker Runtime、ExecutionSandbox 和 Matrix
readiness 设计，关闭最终独立审计确认的三个 Important 问题。本文不改变 DeepAgents
作为 Worker-only Runtime 的产品定位，也不扩大 Runner、Worker 或 Controller 的凭据权限。

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

第一个问题已用无敏感信息的 sentinel token 在最终 Runner 镜像中复现。命令成功读取
`/proc/1/environ`，因此不能只依赖子进程环境白名单。

## 目标

- 让获批命令无法读取或复用 Runner bearer token。
- Worker 不再满足 sandbox 前提时，立即、可重入且 fail-closed 地撤销旧 Runner。
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
- 当前 Worker 的 controller label 与本 Controller 不匹配；
- `workerRef.uid` 为空或与当前 Worker UID 不一致；
- Worker 的有效 Runtime 不再是 `deepagents`；
- DeepAgents runtime config 不存在，或 `execution.mode` 不再是 `sandbox`。

不属于当前 Controller 的 ExecutionSandbox 仍保持忽略，不能跨 Controller 删除资源。

### 撤销顺序

撤销必须可重入，并避免在 Pod 仍存活时先移除 NetworkPolicy：

1. 删除同名 Service，停止新的 Worker 到 Runner 连接。
2. 删除 token Secret，阻止任何替代 Pod取得 token。
3. 请求删除 Runner Pod。
4. 如果 Pod 仍存在，保留 NetworkPolicy 和 ExecutionSandbox，短间隔 requeue；Pod
   删除事件也会再次触发 reconcile。
5. 确认 Pod 不存在后，删除同名 NetworkPolicy，再删除 ExecutionSandbox CR。

所有 NotFound 都视为成功。其它 Kubernetes API 错误返回 reconcile error，保留剩余
隔离资源并重试。撤销路径不创建新 Secret、Pod、Service 或 NetworkPolicy。

### Worker watch

ExecutionSandbox controller 增加 Worker watch。收到 Worker create/update/delete 后，
mapper 只列出同命名空间、同 controller label、同 worker label 的 ExecutionSandbox，
并为每个对象入队。这样 Runtime/mode/UID 变化会立即撤销，而不是等旧的 idle/max-lifetime
定时器。

### HTTP Delete

ensure 和 heartbeat 继续要求 Worker 当前为 DeepAgents sandbox 模式。Delete 只要求：

- URL 中 Worker 存在且属于当前 Controller；
- session ID 合法；
- 找到的 ExecutionSandbox 仍引用该 Worker 名称和 UID，并属于当前 Controller。

因此在 `sandbox -> disabled` 的短暂竞态中，Worker 仍能显式删除旧 lease。跨 Worker、
跨 UID 或跨 Controller 删除继续返回冲突或未找到。

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
  -> Service + Secret removed
  -> Pod removed while NetworkPolicy stays in force
  -> NetworkPolicy + ExecutionSandbox removed after Pod disappears
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

- Ready Sandbox 对应 Worker 切换为 `disabled`、其它 Runtime、不同 UID 或 NotFound 时，
  先移除 Service/Secret/Pod，保留 NetworkPolicy；Pod 消失后再删除 NetworkPolicy 和 CR。
- 同一撤销 reconcile 重复执行不报错、不重建资源。
- Worker watch mapper 只入队当前 Controller 和对应 Worker 的 Sandbox。
- HTTP Delete 在 Worker 已 disabled 时仍可删除正确 lease，但拒绝 UID/controller 冲突。
- `Running + Ready=False + ContainersNotReady message` 返回 `Starting`；明确
  CrashLoop/ImagePull/terminated 仍返回 `Failed`。

每项先添加能在当前实现上按预期失败的测试，再实现最小修复并跑 focused/full suite。

## 集群验收

1. 构建新的不可变 Controller、Manager、默认 Worker、DeepAgents Worker 和 Runner 镜像；
   把精确镜像导入 agent2/agent3 并用 `crictl inspecti` 比对 digest。
2. 先 server-side 审阅/应用 CRD，再原子升级 Helm。
3. 验证 DeepAgents Worker 在 Matrix sync 前为 Starting、sync 后为 Running。
4. 发起需要 Human 审批的命令，命令只输出 `/proc` token 探测是否被阻断，绝不输出
   真实 token；探测结果必须为 blocked。
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
