# Kubernetes 部署指南

[English](../../../usage/deployment/kubernetes.md) | 中文

本指南介绍如何使用官方 Helm Chart 在 Kubernetes 中创建和维护 AgentTeams 实例，包括集群准备、模型服务、Manager 与 Worker runtime、镜像、持久化、访问入口以及升级和卸载选项。

如果只是第一次体验 AgentTeams，建议先使用[快速入门](../../quickstart.md)中的本地部署。Kubernetes 部署更适合团队共享、长期运行和生产环境。

## 1. 部署后会创建什么

默认配置会在 Kubernetes 中部署一套自包含的 AgentTeams 实例：

| 组件 | 默认形态 | 作用 |
|---|---|---|
| Higress | Helm 子 Chart | 模型与 API 网关，同时承载 Web、Matrix 等入口路由 |
| Tuwunel | StatefulSet + PVC | Matrix Homeserver |
| MinIO | StatefulSet + PVC | Agent 配置、工作空间和共享文件存储 |
| Element Web | Deployment | 默认的 Matrix Web 客户端 |
| AgentTeams Controller | Deployment | 管理 Manager、Worker、Team 和 Human CR |
| Manager | 由 `Manager` CR 创建的 Pod | 接收用户目标并编排 Worker |
| Worker | 按需创建的 Pod | 执行具体任务；安装时不会预先创建 |

Kubernetes 模式不会把这些组件塞进同一个容器。基础设施、Manager 和每个 Worker 都是独立工作负载，Controller 通过 CRD 管理它们的生命周期。

## 2. 前置条件

安装前确认：

- Kubernetes 1.24 或更高版本。
- Helm 3.7 或更高版本。
- 当前 `kubectl` context 指向目标集群，并拥有创建 Namespace、CRD、ClusterRole、StatefulSet、Deployment、Job 和 Secret 的权限。
- 集群节点可以拉取所选镜像，并能访问模型服务。
- 使用默认 Tuwunel 和 MinIO 时，集群存在默认 StorageClass，或已经准备好可用的 StorageClass 名称。
- 已准备模型服务 API Key、模型名，以及自定义 OpenAI 兼容服务所需的 Base URL。

先执行以下命令检查环境：

```bash
kubectl version
kubectl cluster-info
kubectl get storageclass
helm version
```

建议至少为首次体验准备 4 核 CPU、8 GiB 内存和 20 GiB 可动态分配存储。实际资源需求取决于 Worker 数量、runtime 和任务类型；并行 Worker、浏览器自动化或构建任务通常需要更多资源。

## 3. 先确定部署方案

### 3.1 基础设施组合

当前 Chart 支持以下组合：

| 能力 | 推荐默认值 | 可选云上组合 |
|---|---|---|
| Matrix | `tuwunel` + `managed` | 当前 Chart 仍只接受托管 Tuwunel |
| Gateway | `higress` + `managed` | `ai-gateway` + `existing` |
| Storage | `minio` + `managed` | `oss` + `existing` |

第一次部署建议全部使用默认的托管组合。外部 AI Gateway 或 OSS 需要额外部署 `credentialProvider` sidecar，用于签发受限的临时凭证；它不是一个只填写外部地址即可完成的通用接入模式。

以下组合会在 Helm 渲染阶段直接报错：

- 非 `tuwunel` 的 `matrix.provider`，或非 `managed` 的 `matrix.mode`。
- `gateway.provider=higress` 与非 `managed` 模式组合。
- `gateway.provider=ai-gateway` 与非 `existing` 模式组合。
- `storage.provider=minio` 与非 `managed` 模式组合。
- `storage.provider=oss` 与非 `existing` 模式组合。
- 使用 AI Gateway 或 OSS，却没有启用并配置 `credentialProvider`。

### 3.2 访问方式

安装前必须确定 `gateway.publicURL`：

| 场景 | 示例值 | 说明 |
|---|---|---|
| 本机临时访问 | `http://localhost:18080` | 配合 `kubectl port-forward`；命令停止后不可访问 |
| 团队内网 | `https://agentteams.example.internal` | 通过内网 Ingress 或 LoadBalancer 暴露 |
| 公网访问 | `https://agentteams.example.com` | 必须配置可信 TLS 证书和访问控制 |

该值会写入 Element Web 和 Matrix 相关配置，必须与用户实际打开的 Origin 完全一致。正式共享实例应先规划好域名和 HTTPS，避免安装后更换地址造成客户端连接异常。

### 3.3 Manager runtime

Manager 对用户呈现的 runtime 选项只有 **OpenClaw** 和 **CoPaw**：

| 选项 | `manager.runtime` | Manager 镜像 | 说明 |
|---|---|---|---|
| OpenClaw | `openclaw` | `agentteams-manager` | 当前 Chart 默认值 |
| CoPaw | `qwenpaw` | `agentteams-manager-qwenpaw` | 当前 Python Manager 实现；`copaw` 是兼容旧配置的别名 |

Hermes、OpenHuman 等 runtime 仅用于 Worker，不能作为 Manager runtime。

当前 Chart 不会仅根据 `manager.runtime` 自动改写 `manager.image.repository`。选择 CoPaw 时必须同时设置：

```yaml
manager:
  runtime: qwenpaw
  image:
    repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-manager-qwenpaw
```

只修改 runtime 而继续使用 OpenClaw Manager 镜像，会导致 Manager Pod 无法启动或运行时行为不匹配。

### 3.4 默认 Worker runtime

`worker.defaultRuntime` 决定后续创建 Worker 时使用的默认 runtime；在单个 `Worker` CR 中显式设置 `spec.runtime` 可以覆盖它。

| runtime | 默认镜像配置项 | 适用说明 |
|---|---|---|
| `openclaw` | `worker.defaultImage.openclaw` | 默认通用 Worker runtime |
| `copaw` | `worker.defaultImage.copaw` | Python / CoPaw Worker |
| `hermes` | `worker.defaultImage.hermes` | Hermes Worker |
| `openhuman` | `worker.defaultImage.openhuman` | Chart 中已有默认镜像配置，但当前 Worker CRD enum 不接受显式的 `spec.runtime: openhuman` |

Controller 也认识 `qwenpaw` Worker runtime，但当前 Chart 没有为它提供独立的默认镜像配置。使用时应在 `Worker.spec.image` 中显式指定 QwenPaw Worker 镜像。OpenHuman 的后端与 Helm values 已存在，但 CRD 契约尚未对齐；业务代码单独修正前，不应在 Worker YAML 中显式使用 `openhuman`。

## 4. 配置模型服务

Helm Chart 使用 `credentials.*` 配置默认模型服务：

| 配置项 | 默认值 | 意义 | 示例 |
|---|---|---|---|
| `credentials.llmApiKey` | 无 | 模型服务 API Key；必填 | `sk-...` |
| `credentials.llmProvider` | `openai-compat` | 网关采用的模型服务类型 | `openai-compat`、`qwen` |
| `credentials.defaultModel` | `gpt-5.4` | Manager 和未显式指定模型的 Worker 使用的模型名 | `gpt-5.4`、`qwen3.5-plus` |
| `credentials.llmBaseUrl` | 空 | OpenAI 兼容 API 的 Base URL；官方 OpenAI 或已有 provider 默认地址可留空 | `https://api.deepseek.com/v1` |

`llmBaseUrl` 应填写 API 根路径，不要填写具体的 `/chat/completions` 接口。模型名必须是该服务端真实接受的标识。

### OpenAI 官方 API

```yaml
credentials:
  llmApiKey: "<your-openai-api-key>"
  llmProvider: openai-compat
  defaultModel: gpt-5.4
  llmBaseUrl: ""
```

### 自定义 OpenAI 兼容服务

```yaml
credentials:
  llmApiKey: "<your-provider-api-key>"
  llmProvider: openai-compat
  defaultModel: your-model-name
  llmBaseUrl: https://your-provider.example.com/v1
```

### 通义千问

```yaml
credentials:
  llmApiKey: "<your-qwen-api-key>"
  llmProvider: qwen
  defaultModel: qwen3.5-plus
```

安装和升级前，Chart 默认创建一个临时 Job 探测模型服务：

```yaml
preflight:
  llm:
    enabled: true
    strict: true
    timeoutSeconds: 30
    retries: 2
    activeDeadlineSeconds: 120
```

- `enabled`：是否执行探测。
- `strict`：探测失败时是否阻止安装或升级。
- `timeoutSeconds`：单次请求超时。
- `retries`：网络错误、限流或服务端错误的重试次数。
- `activeDeadlineSeconds`：整个探测 Job 的最长运行时间。

生产环境建议保留严格探测。只有在集群暂时无法访问模型服务、并且计划安装后再完成网络配置时，才临时设置 `strict: false` 或关闭探测。

## 5. 编写 values 文件

与在命令行中传入大量 `--set` 相比，values 文件更容易审查、复用和升级。下面是一份使用默认托管基础设施、OpenClaw Manager 和 OpenClaw Worker 的示例：

```yaml
# agentteams-values.yaml
credentials:
  llmApiKey: "<replace-with-your-api-key>"
  llmProvider: openai-compat
  defaultModel: gpt-5.4
  llmBaseUrl: ""
  adminUser: admin
  adminPassword: "<replace-with-a-strong-password>"

gateway:
  provider: higress
  mode: managed
  publicURL: http://localhost:18080
  higress:
    enabled: true

matrix:
  provider: tuwunel
  mode: managed
  tuwunel:
    persistence:
      enabled: true
      size: 10Gi
      storageClassName: ""

storage:
  provider: minio
  mode: managed
  bucket: agentteams-storage
  minio:
    persistence:
      enabled: true
      size: 10Gi
      storageClassName: ""
    auth:
      rootUser: minioadmin
      rootPassword: "<replace-with-a-strong-password>"

manager:
  enabled: true
  runtime: openclaw
  image:
    repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-manager
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "2"
      memory: 4Gi

worker:
  defaultRuntime: openclaw
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: "2"
      memory: 2Gi
```

将包含密钥的文件限制为仅当前用户可读：

```bash
chmod 600 agentteams-values.yaml
```

不要把真实密钥提交到 Git。生产环境可以结合 Secret 管理工具生成或注入 values；无论采用哪种方式，最终的 `credentials.llmApiKey` 都必须在 Helm 渲染时可用。

如果 `credentials.adminPassword` 留空，Chart 会生成密码，并在后续升级时复用已有 Secret 中的值。生产环境更建议显式管理一个强密码。

## 6. 安装 AgentTeams

### 6.1 使用官方 Helm 仓库

```bash
helm repo add higress.io https://higress.io/helm-charts
helm repo update
helm show values higress.io/agentteams
```

安装或幂等更新实例：

```bash
helm upgrade --install agentteams higress.io/agentteams \
  --namespace agentteams-system \
  --create-namespace \
  --values agentteams-values.yaml \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

`agentteams` 是 Helm Release 名称，`agentteams-system` 是 Namespace。可以更换名称，但后续所有命令必须保持一致。

### 6.2 从源码安装当前 Chart

需要验证仓库中的未发布 Chart 时，在项目根目录执行：

```bash
helm dependency build helm/agentteams
helm upgrade --install agentteams ./helm/agentteams \
  --namespace agentteams-system \
  --create-namespace \
  --values agentteams-values.yaml \
  --set global.imageTag=latest \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

Chart 默认把 `appVersion` 转换成带 `v` 前缀的镜像 tag；当前仓库中的 `appVersion: 1.1.1` 会解析为 `v1.1.1`。由于该值可能与 AgentTeams 最新发布版本不同，验证当前源码时应像上面的命令一样显式设置 `global.imageTag=latest`；生产环境则应设置为已经确认存在、且与目标部署版本匹配的固定 tag，避免依赖 Chart 的隐式默认值或使用可变标签。

安装前可以先检查渲染后的实际镜像：

```bash
helm template agentteams ./helm/agentteams \
  --namespace agentteams-system \
  --values agentteams-values.yaml \
  --set global.imageTag=latest \
  | grep 'image:' | sort -u
```

## 7. 验证安装

不要只以 `helm install` 返回成功作为验收标准。建议依次确认模型预检、基础组件、Manager、Worker 和 Matrix 消息链路。

### 7.1 确认 Helm、Pod 和存储状态

```bash
helm status agentteams -n agentteams-system
kubectl get pods -n agentteams-system
kubectl get pvc -n agentteams-system
kubectl wait --for=condition=Ready pod --all \
  -n agentteams-system \
  --timeout=15m
```

预期 Helm 状态为 `deployed`，基础组件 Pod 全部为 `Running` 且 Ready，Tuwunel 和 MinIO PVC 为 `Bound`。Worker 按需创建，因此首次安装后没有 Worker Pod 是正常现象。

### 7.2 确认模型预检通过

默认的 LLM preflight 是 Helm 的 pre-install/pre-upgrade hook。严格模式下，只有模型 API Key、Base URL、provider 和模型名均可用，Helm 才会继续安装正式组件。

成功的 hook Job 会被自动删除，因此安装完成后可能无法再通过 `kubectl get job` 找到它。需要保留排查信息时，为安装命令增加 `--debug`，并在执行期间观察：

```bash
kubectl get job,pod -n agentteams-system \
  -l app.kubernetes.io/component=llm-preflight
```

如果安装停留在 preflight，另开终端查找实际 Job/Pod 名称并读取日志：

```bash
kubectl get job,pod -n agentteams-system
kubectl logs -n agentteams-system job/agentteams-llm-preflight
```

Release 名称不是 `agentteams` 时，Job 名称也会随之变化。

### 7.3 确认 Manager 就绪

默认 Manager 由 Controller 创建，不是 Helm 中的静态 Deployment。继续检查 CR 和对应 Pod：

```bash
kubectl get managers.agentteams.io -n agentteams-system
kubectl describe manager default -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=manager
```

如果初始化较慢，查看 Controller 日志：

```bash
kubectl logs -n agentteams-system deployment/agentteams-controller --tail=200
```

Release 名称不是 `agentteams` 时，Controller Deployment 名称通常也会变化。先通过 `kubectl get deployment -n agentteams-system` 确认实际名称。

Manager CR 的 `PHASE` 应为 `Running`。如果 Manager Pod 已经 Ready，还应登录 Element Web 向 Manager 发送一条短消息，确认它能实际调用模型并返回内容；仅有 Pod Ready 不能证明模型推理链路可用。

### 7.4 创建 Worker 并完成消息闭环

登录 Element Web 后，请 Manager 创建一个验收 Worker：

```text
请创建一个名为 e2e-worker 的 Worker，使用默认模型和 openclaw runtime；创建完成后简洁汇报状态。
```

确认 CR 和 Pod 就绪：

```bash
kubectl get workers.agentteams.io -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=standalone
```

然后继续让 Manager 向 Worker 分配一个最小任务：

```text
请在 e2e-worker 的 Worker Room 中 @mention 它，要求它只回复：K8S_E2E_OK；收到结果后向我汇报。
```

Worker 群聊默认启用 `requireMention`。消息必须在 Matrix 事件中包含完整 Worker ID 对应的 `m.mentions`；只在正文中手工输入 `@e2e-worker`、没有完整 Matrix 域名，或客户端没有生成 mention 元数据，都会被 Worker 忽略。通过 Manager 分派任务，或在 Element 中从成员列表选择 Worker 完成 @mention，可以避免发送成普通文本。

需要从日志确认消息是否已经进入模型时执行：

```bash
kubectl logs -n agentteams-system \
  -l agentteams.io/role=standalone \
  --tail=200 \
  | grep -E 'resolveAgentRoute|embedded run start|model='
```

看到 `embedded run start` 和期望的模型名，表示 Matrix → Worker → 模型链路已经启动；最终还应以 Worker 在房间中返回结果、Manager 收到并汇报为完整验收标准。

## 8. 访问 Element Web

### 8.1 临时端口转发

当 `gateway.publicURL` 为 `http://localhost:18080` 时执行：

```bash
kubectl port-forward -n agentteams-system svc/higress-gateway 18080:80
```

然后打开 `http://localhost:18080`。默认用户名来自 `credentials.adminUser`，默认是 `admin`。

如果安装时没有设置管理员密码，可以读取自动生成的值：

```bash
kubectl get secret agentteams-runtime-env \
  -n agentteams-system \
  -o go-template='{{index .data "AGENTTEAMS_ADMIN_PASSWORD" | base64decode}}{{"\n"}}'
```

Secret 名称取决于 Release 名称。无法找到时，先执行：

```bash
kubectl get secret -n agentteams-system
```

### 8.2 使用 Ingress 提供团队访问

正式环境应使用 HTTPS。以下示例假设集群使用 NGINX Ingress，并已经存在 `agentteams-tls` TLS Secret：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: agentteams
  namespace: agentteams-system
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - agentteams.example.com
      secretName: agentteams-tls
  rules:
    - host: agentteams.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: higress-gateway
                port:
                  number: 80
```

同时把 values 中的地址设置为相同 Origin：

```yaml
gateway:
  publicURL: https://agentteams.example.com
```

更新 Release 后验证 Web 与 Matrix 路由：

```bash
curl -fsSI https://agentteams.example.com/
curl -fsS https://agentteams.example.com/_matrix/client/versions
```

只需要对外暴露 Higress Gateway。Controller API、Tuwunel、MinIO 和 Higress Console 默认应保持集群内访问；如需暴露，应另外配置身份认证、TLS 和网络策略。

## 9. 创建第一个 Worker

推荐登录 Element Web 后直接告诉 Manager：

```text
请创建一个名为 alice 的开发 Worker，使用默认模型和 runtime。
```

Manager 会通过 Controller 创建 `Worker` CR、Matrix 身份、权限和 Pod。检查状态：

```bash
kubectl get workers.agentteams.io -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=standalone
```

熟悉 CRD 后，也可以直接应用 YAML：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
  namespace: agentteams-system
spec:
  model: gpt-5.4
  runtime: openclaw
  identity: 开发工程师，负责编码、测试和代码审查
```

```bash
kubectl apply -f worker-alice.yaml
```

更多字段参见[声明式资源管理](../resource-management.md)和 [Worker 指南](../worker-guide.md)。

## 10. 常用配置选项

### 镜像与版本

| 配置项 | 作用 |
|---|---|
| `global.imageTag` | 未单独指定 tag 的 AgentTeams 组件使用的统一版本；默认取 Chart `appVersion` |
| `controller.image.*` | Controller 镜像仓库、tag 和拉取策略 |
| `manager.image.*` | Manager 镜像仓库和 tag |
| `worker.defaultImage.<runtime>.*` | 各 Worker runtime 的默认镜像 |
| `imagePullSecrets` | 私有镜像仓库拉取凭证 |

`global.imageRegistry` 会传递给使用该全局值的子 Chart，但当前 AgentTeams Controller、Manager 和 Worker 使用各自的完整 `repository`。切换地域或私有镜像仓库时，应检查并覆盖所有相关 `*.image.repository`，不要假设只改一个全局值就会重写全部镜像地址。

`global.imageTag` 为空时会解析为 `v<Chart.appVersion>`。该标签必须同时存在于 Controller、Manager 和默认 Worker 的镜像仓库中；LLM preflight 也复用 Controller 镜像，因此缺失的 Controller tag 会在正式组件创建前表现为 preflight `ImagePullBackOff`。源码验收可以暂时使用 `latest`，生产环境应固定到已发布且经过验证的版本。

### 持久化

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `matrix.tuwunel.persistence.enabled` | `true` | 是否持久化 Matrix 数据 |
| `matrix.tuwunel.persistence.size` | `10Gi` | Tuwunel PVC 容量 |
| `matrix.tuwunel.persistence.storageClassName` | 空 | 空值使用默认 StorageClass |
| `storage.minio.persistence.enabled` | `true` | 是否持久化 MinIO 数据 |
| `storage.minio.persistence.size` | `10Gi` | MinIO PVC 容量 |
| `storage.minio.persistence.storageClassName` | 空 | 空值使用默认 StorageClass |

生产环境不要关闭持久化。升级前应确认 StorageClass 的扩容能力和备份策略；删除 Namespace 通常也会删除其中的 PVC。

### 资源

可以分别设置：

- `controller.resources`
- `manager.resources`
- `worker.resources`
- `matrix.tuwunel.resources`
- `storage.minio.resources`
- `elementWeb.resources`
- `credentialProvider.resources`
- `preflight.llm.resources`

`worker.resources` 是没有在 `Worker.spec.resources` 中显式指定资源时的默认值。为构建、大型仓库或浏览器任务创建 Worker 时，应按任务提高 CPU 和内存限制。

### Controller 与监控

- `controller.replicaCount`：Controller 副本数；提高副本数时依赖 leader election 保证协调一致。
- `controller.metrics.enabled`：是否暴露指标 Service。
- `controller.metrics.serviceMonitor.enabled`：集群已安装 Prometheus Operator 时创建 ServiceMonitor。
- `cms.enabled`：启用阿里云 CMS/ARMS 可观测集成；还需填写 endpoint、license key、project 和 workspace。
- `elementWeb.enabled`：是否部署默认 Web 客户端。关闭后需要准备其他 Matrix 客户端入口。

## 11. 外部 AI Gateway 与 OSS

该模式面向已经具备阿里云 AI Gateway、OSS 和企业凭证签发服务的环境。最小结构如下：

```yaml
gateway:
  provider: ai-gateway
  mode: existing
  publicURL: https://agentteams.example.com
  higress:
    enabled: false
  aiGateway:
    region: cn-hangzhou
    gatewayId: "<gateway-id>"
    modelApiId: "<model-api-id>"
    envId: "<environment-id>"

storage:
  provider: oss
  mode: existing
  bucket: agentteams-storage
  oss:
    region: cn-hangzhou
    endpoint: ""

credentialProvider:
  enabled: true
  image:
    repository: registry.example.com/agentteams/credential-provider
    tag: latest
  envFrom:
    - secretRef:
        name: credential-provider-config
```

`credentialProvider` 镜像没有通用默认值。它必须实现 AgentTeams 所需的临时凭证 API，并按照企业环境配置 RAM 角色、权限边界和身份来源。如果没有这样的服务，请继续使用默认 Higress + MinIO 模式。

## 12. 升级

先查看差异并继续使用同一份 values 文件：

```bash
helm repo update
helm get values agentteams -n agentteams-system
helm upgrade agentteams higress.io/agentteams \
  --namespace agentteams-system \
  --values agentteams-values.yaml \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

不要只依赖临时的 `--set` 历史。升级前备份 Matrix 和对象存储数据，并确认新版本的 Chart values、CRD 和镜像兼容性。

修改 `manager.runtime`、`manager.image` 或关键 Manager 配置可能重建 Manager Pod。应在没有进行中任务时操作。

## 13. 卸载

卸载会停止 AgentTeams，并可能清理 Manager、Worker、Matrix 用户和存储中的 Agent 数据。需要保留数据时，请先备份并确认 PVC、对象存储和 CR 的保留方案。

正常卸载：

```bash
helm uninstall agentteams -n agentteams-system
```

Chart 默认通过 pre-delete hook 先删除 Manager、Worker、Team 和 Human CR，等待 Controller 执行 finalizer 清理，再移除 Controller。不要随意使用 `--no-hooks`。

确认不再需要 Namespace 中的 Secret、PVC 和其他资源后，再删除 Namespace：

```bash
kubectl delete namespace agentteams-system
```

Helm 不会自动删除 CRD。只有确认集群中没有其他 AgentTeams Release 或需要保留的自定义资源时，才执行：

```bash
kubectl delete crd \
  managers.agentteams.io \
  workers.agentteams.io \
  teams.agentteams.io \
  humans.agentteams.io
```

## 14. 常见问题定位

### 安装停在 LLM preflight

检查 API Key、provider、模型名和 Base URL，并确认集群 Pod 可以访问模型服务。失败的 hook Job 可能很快被 Helm 清理，可先使用 `helm install --debug` 查看输出，或临时设置 `preflight.llm.strict=false` 收集更多运行时日志。

### Pod 一直是 `Pending`

```bash
kubectl describe pod <pod-name> -n agentteams-system
kubectl get events -n agentteams-system --sort-by=.lastTimestamp
kubectl get pvc -n agentteams-system
```

重点检查节点资源、默认 StorageClass、PVC 绑定、污点和调度约束。

### Pod 是 `ImagePullBackOff`

确认节点能访问镜像仓库、镜像 tag 存在、CPU 架构匹配，并为私有仓库配置 `imagePullSecrets`。切换 CoPaw Manager 时同时检查 runtime 和 Manager 镜像是否匹配。

如果从源码安装时日志显示类似 `agentteams-controller:v<Chart.appVersion>` 不存在，先确认渲染结果，再临时覆盖为确实存在的 tag：

```bash
helm template agentteams ./helm/agentteams \
  -n agentteams-system \
  -f agentteams-values.yaml \
  | grep 'image:' | sort -u

helm upgrade --install agentteams ./helm/agentteams \
  -n agentteams-system \
  --create-namespace \
  -f agentteams-values.yaml \
  --set global.imageTag=latest \
  --wait \
  --timeout 15m
```

验证通过后把 `latest` 换成固定版本。

### Helm 输出 Higress values 类型警告

当前依赖组合可能输出类似 `destination for higress-core.controller.image is a table` 的警告。如果 Helm 最终状态为 `deployed`，并且 Higress Controller 与 Gateway Pod 均 Ready，该警告本身不阻塞部署；仍应记录警告，并在升级 Higress 子 Chart 时重新检查 values 兼容性。

### Element Web 能打开但无法登录或连接 Matrix

确认 `gateway.publicURL` 与浏览器 Origin 一致，并验证 `/_matrix/client/versions`。如果使用 Ingress，还要检查 WebSocket、长连接、请求体大小和超时配置。

### Manager CR 存在但 Pod 没有就绪

```bash
kubectl describe manager default -n agentteams-system
kubectl get events -n agentteams-system --sort-by=.lastTimestamp
kubectl logs -n agentteams-system deployment/agentteams-controller --tail=200
```

如果 Manager Pod 已创建，再检查对应 Pod 的 describe 和 logs。常见原因包括模型探测虽然非严格通过但运行时仍不可访问、镜像与 runtime 不匹配、资源不足或初始化依赖尚未就绪。

### Worker 已经 Running 但不回复群聊消息

先确认消息真正 @mention 了 Worker，而不是只包含看起来像 mention 的普通文本。完整 Matrix ID 形如 `@e2e-worker:<matrix-domain>`，事件还必须包含对应的 `m.mentions.user_ids`。推荐在 Element 中从成员列表选择 Worker，或让 Manager 使用内置 Matrix 消息工具分派任务。

随后检查 Worker 日志：

```bash
kubectl logs -n agentteams-system \
  -l agentteams.io/role=standalone \
  --tail=200 \
  | grep -E 'matrix-auto-reply|resolveAgentRoute|embedded run start|error'
```

- 出现 `matrix-auto-reply skipping room message`：通常是消息没有有效 mention，或发送者不在允许列表。
- 只有 `resolveAgentRoute`、没有 `embedded run start`：检查完整 Matrix ID、`m.mentions` 和 `groupAllowFrom`。
- 已出现 `embedded run start`：消息已进入模型链路，继续检查模型错误、工具调用和 Matrix 发送日志。

更多问题参见 [FAQ](../troubleshooting/faq.md)。
