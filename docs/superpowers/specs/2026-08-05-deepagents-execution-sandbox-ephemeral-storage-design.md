# DeepAgents ExecutionSandbox 临时存储设计

## 目标

为 DeepAgents `ExecutionSandbox` 增加可审计、可覆盖且受集群上限约束的临时存储配额，避免 Runner 的 `/workspace`、`/tmp`、容器可写层和日志无限占用节点磁盘。该能力只属于 DeepAgents Runner，不改变 OpenClaw、CoPaw、Hermes、OpenHuman、QwenPaw 或普通 Worker Pod 的资源语义。

## 方案选择

采用“Helm 集群默认值与上限 + Worker 逐字段覆盖 + Controller 统一解析”的方案。相比只给 Pod 写死配额，它允许不同 DeepAgents Worker 按任务规模调整；相比允许 Worker 任意指定配额，它保留平台级上限；相比扩展通用 `AgentResourceValues`，ExecutionSandbox 专属资源类型不会向其他 runtime 暴露尚未实现的字段。

## 配置与 API

Helm 增加以下值：

```yaml
deepagents:
  sandbox:
    ephemeralStorage:
      defaultRequest: 256Mi
      defaultLimit: 2Gi
      maxLimit: 8Gi
```

Controller 通过以下环境变量接收同一组值：

```text
AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_REQUEST
AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_LIMIT
AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_MAX_LIMIT
```

Worker 可在现有路径逐字段覆盖：

```yaml
spec:
  runtimeConfig:
    deepagents:
      execution:
        resources:
          requests:
            cpu: 250m
            memory: 256Mi
            ephemeralStorage: 512Mi
          limits:
            cpu: "2"
            memory: 2Gi
            ephemeralStorage: 4Gi
```

`DeepAgentsExecutionConfig.Resources` 和 `ExecutionSandboxSpec.Resources` 改用 JSON 形状兼容的 ExecutionSandbox 专属资源类型。已有 CPU、memory 配置和未声明 `ephemeralStorage` 的 Worker CR 保持兼容；缺失字段逐项使用 Helm 默认值。

## 解析与校验

新增一个无状态的 `internal/sandboxpolicy` 边界，负责：

1. 解析 Controller 的默认 request、默认 limit 和最大 limit。
2. 复制 Worker 或直接创建的 ExecutionSandbox 资源配置，不修改调用方对象。
3. 逐字段应用默认值，并返回有效的 Kubernetes `ResourceRequirements` 和 `emptyDir` size limit。
4. 拒绝无法解析、为零或负数的 ephemeral-storage quantity。
5. 拒绝有效 ephemeral-storage request 大于有效 limit。
6. 拒绝有效 ephemeral-storage limit 大于集群 max limit。
7. 拒绝平台自身的默认 request 大于默认 limit，或默认 limit 大于 max limit。

HTTP `ensure` 在创建 `ExecutionSandbox` 之前调用该边界。非法 Worker 覆盖返回 `400 Bad Request`，不创建 CR。合法配置以已补齐默认值的形式写入 `ExecutionSandbox.spec.resources`，便于审计。

Reconciler 对每个 ExecutionSandbox 再次解析，覆盖绕过 HTTP 直接创建 CR 的场景。非法 CR 的状态设为 `Failed`，`Ready=False`，reason 为 `InvalidResources`；Controller 不创建 token Secret、Pod、Service 或 NetworkPolicy，并清理同名的既有 Runner 资源，避免从合法配置改成非法配置后旧 Pod 继续运行。配置错误是稳定状态，不以 reconcile error 持续重试。

## Pod 落地

对合法配置，Runner 容器获得：

```yaml
resources:
  requests:
    ephemeral-storage: <effective request>
  limits:
    ephemeral-storage: <effective limit>
```

`workspace` 与 `tmp` 两个 `emptyDir` 的 `sizeLimit` 都设置为有效 limit。`emptyDir.sizeLimit` 分别约束卷，容器的 `ephemeral-storage` limit 则覆盖容器可写层、日志和本地临时卷的聚合用量；两者共同工作，不把两个 `emptyDir` 的上限相加作为容器额度。

资源字段已经包含在现有 Runner Pod 的 managed-spec hash 中，因此配额变化会触发旧 Pod 的受控重建。

## CRD 与向后兼容

Controller 自带 CRD 和 Helm CRD 同步增加 `ephemeralStorage` 字符串字段，路径包括：

- `Worker.spec.runtimeConfig.deepagents.execution.resources.{requests,limits}.ephemeralStorage`
- `ExecutionSandbox.spec.resources.{requests,limits}.ephemeralStorage`

不设置新字段的已有清单仍可通过 schema 校验，并自动获得 `256Mi/2Gi`。不新增 PVC，不改变 MinIO 的最终 workspace 持久化、PostgreSQL checkpoint 或 Matrix 状态 PVC 设计。

## 测试与验收

自动测试覆盖：

- 默认值、逐字段覆盖、quantity 规范化和输入不被修改。
- 非正数、request 大于 limit、limit 大于 max、平台默认值非法。
- HTTP ensure 的合法 CR 内容与非法请求不创建 CR。
- 直接非法 CR 进入 `Failed` 且没有 Runner 子资源。
- Runner Pod 的容器 request/limit、两个 `emptyDir.sizeLimit` 和 spec hash 重建语义。
- Config 环境变量默认值/覆盖值，Controller Helm env 渲染和两份 CRD 同步。

集群验收创建一个带 `512Mi/4Gi` 覆盖的 DeepAgents Worker，并检查实际 Runner Pod；随后创建一个超过 `8Gi` 的直接 ExecutionSandbox，确认其为 `Failed` 且没有 Pod。最终端到端流程仍为 Matrix 任务、Human 审批、Runner 执行、MinIO 同步、PostgreSQL checkpoint 和超时回收。

## 运维边界

这些配额保护节点临时磁盘，但不替代 MinIO/PVC 容量规划，也不保证 local-path 卷高可用。节点磁盘压力、容器日志策略和 kubelet eviction 仍需独立监控。默认 `8Gi` 上限是本次局域网集群的安全边界；如未来需要更大 Runner 工作集，应由平台管理员提升 Helm max limit，而不是由单个 Worker 绕过。
