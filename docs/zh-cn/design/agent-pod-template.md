# Agent Pod 模板

agentteams-controller 为每个 Manager 与 Worker 创建一个 Kubernetes Pod（"agent Pod"）。默认情况下，这些 Pod 形态极简：一个名为 `worker` 的容器、一个投射（projected）的 `agentteams-token` 卷、控制器管理的 `ServiceAccount`，以及寥寥无几的其他配置。

为注入集群特定关切——sysctls、nodeSelectors、tolerations、imagePullSecrets、供 CNI/sidecar 注入器消费的 annotations 等——你可通过 ConfigMap 提供一个 `corev1.PodTemplateSpec` 叠加层。

## 工作原理

**每次** `Create()` 时，控制器从自身命名空间读取一个 ConfigMap，其名称等于控制器的 `AGENTTEAMS_CONTROLLER_NAME` 环境变量（对 Helm release `prod`，即 `prod-agentteams-controller`）。若该 ConfigMap 存在且含键 `pod-template.yaml`，其值被解析为 `PodTemplateSpec`，与控制器自持字段合并，产出最终 Pod。

> `AGENTTEAMS_CONTROLLER_NAME` 同时是 leader 选举租约名，也是控制器在它所创建的每个 Worker/Manager/Team/Human CR 上以 `agentteams.io/controller` 标签 stamped 的取值。控制器的 informer 缓存按此标签过滤 CR，因此同一命名空间内的多个 AgentTeams release 绝不会互相调和对方的资源。Helm chart 会从 release 名自动设置它；若手工部署，请显式设置——incluster 模式下不带它启动控制器会快速失败。

无缓存。编辑 ConfigMap → 控制器下一个创建的 Pod 使用新模板。既有 Pod 不受影响（删除它们即可拾取变更）。

若 ConfigMap 缺失、格式错误，或 API 调用因任何原因失败，控制器回退到默认 Pod 形态。Pod 创建永远不会被一个坏模板阻塞。

## ConfigMap 结构

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: <controller-name>    # == AGENTTEAMS_CONTROLLER_NAME env on controller
  namespace: <controller-ns> # same namespace as controller
data:
  pod-template.yaml: |
    metadata:
      annotations: {...}
      labels: {...}
    spec:
      nodeSelector: {...}
      tolerations: [...]
      imagePullSecrets: [...]
      securityContext: {...}
      # ...any corev1.PodSpec field
```

> `pod-template.yaml` 下的值是一个 `PodTemplateSpec`，只有两个顶层字段：`metadata:` 与 `spec:`。**不要**用 `apiVersion: v1` / `kind: PodTemplate` 包裹它。

可直接应用的示例见 [`docs/examples/agent-pod-template-cm.yaml`](../../examples/agent-pod-template-cm.yaml)。

## 合并语义

**模板胜出**的字段：

- `spec.nodeSelector`
- `spec.tolerations`
- `spec.affinity`
- `spec.imagePullSecrets`
- `spec.securityContext`（含 `sysctls`）
- `spec.topologySpreadConstraints`
- `spec.runtimeClassName`、`spec.schedulerName`、`spec.priorityClassName`
- `spec.dnsPolicy`、`spec.dnsConfig`
- `spec.hostAliases`（随后追加控制器 `CreateRequest.ExtraHosts`）
- 名称非 `worker` 的 `spec.containers[]` —— 作为 sidecar 保留
- 其他未在下文列出的 `spec.*` 字段

**控制器胜出**的字段（模板提供的值被丢弃）：

- `metadata.ownerReferences` —— 恒从控制器 Pod 继承
- `spec.serviceAccountName`
- `spec.automountServiceAccountToken` —— 强制为 `false`
- Agent 容器的 `image`、`env`、`workingDir`、`imagePullPolicy`

混合合并：

| 字段 | 规则 |
|---|---|
| `metadata.labels` | 模板优先，键冲突时控制器 labels 覆盖 |
| `metadata.annotations` | 模板优先，键冲突时控制器 annotations 覆盖 |
| Agent 容器的 `resources` | `CreateRequest.Resources`（逐请求）> 模板 resources > 后端默认 |
| Agent 容器的 `volumeMounts` | 模板优先，`agentteams-token` volumeMount 恒追加 |
| `spec.volumes` | 模板优先，`agentteams-token` 投射卷恒追加 |
| `spec.restartPolicy` | 模板已设置则用模板，否则 `Always` |

## 排障

**我的模板生效了吗？** 创建一个新 Worker 并检查其 Pod：

```bash
kubectl get pod <worker-pod> -n <ns> -o yaml | grep -A5 nodeSelector
```

**控制器看到我的 ConfigMap 了吗？** 控制器日志（`V(1)` 或默认级别）会显示其中之一：

- `agent pod template ConfigMap not found; using empty overlay` —— 创建/重命名该 CM。
- `agent pod template YAML parse failed` —— 你的 YAML 非法。
- `agent pod template ConfigMap fetch failed` —— API / RBAC 问题。

**RBAC**：Helm 默认安装的控制器 `ClusterRole` 已授予 `configmaps` 的 `get`。对手工编写的 Deployment，确保该动词存在。
