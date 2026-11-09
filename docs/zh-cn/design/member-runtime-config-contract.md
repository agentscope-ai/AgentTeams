# 成员运行时配置契约

本文档定义 AgentTeams 控制器为受管运行时成员写入对象存储的 YAML 配置快照。受管运行时 worker 与 TeamHarness 插件适配器读取此文件，而不是查询 `agt` CLI 以获取团队与成员事实。

该配置仅供运行时消费。它承载非密期望状态与团队事实。密钥保留在环境变量、挂载文件或服务账户令牌中。

## 存储路径

推荐对象路径：

```text
shared/runtime/members/{memberName}/runtime.yaml
```

## 范围

- 控制器在成员的非密期望状态或团队事实变更时写入此文件。
- QwenPaw worker 轮询此文件，并在运行时内应用变更后的 model、AgentSpec 包、MCP、通道与团队上下文配置。
- 对 `runtime=qwenpaw`，控制器不写面向运行时的 `AGENTS.md`、`SOUL.md`、技能、`openclaw.json` 或 `mcporter-servers.json`。
- AgentSpec 包版本变更会更新此文件，且应由 QwenPaw 在不重启 pod 的前提下应用。

`desired.agentPackage` 是 AgentTeams AgentSpec 包，不是 TeamHarness 插件包。TeamHarness 插件是运行时基础设施；AgentSpec 包是用户部署的 agent 模板与业务能力包。

## 运行时 provider ID 警告

`spec.modelProvider` 是 **AI 网关 provider 名**（Higress / APIG），用于控制器解析
AI Route、网关 base URL 与 consumer 授权；它**不是**可自由复用的运行时 provider id。

不要把 QwenPaw 内置 provider id 写进运行时投影。QwenPaw 用运行时 provider id 选择
本地 provider 条目；内置 provider 会从 QwenPaw 内置目录恢复，`freeze_url=True` 会阻止
写入网关 base URL，worker 启动时 base URL 回读失败。网关 provider 名本身是合法的，
不应因为这个问题改名。

AgentTeams 管理的运行时统一使用非内置的运行时 provider id `agentteams-gateway`。
`spec.modelProvider` 仍然可以选名为 `deepseek` 的网关 provider；运行时 provider id
必须与它分离。

### QwenPaw 内置 provider id（v2.2.2.post1）

来源：QwenPaw `v2.2.2.post1` 的 `qwenpaw/providers/provider_catalog.py`。

`freeze_url=True`——绝不能用作运行时 provider id：

```text
modelscope
aliyun-codingplan
aliyun-codingplan-intl
aliyun-tokenplan
aliyun-tokenplan-intl
zhipu-cn
zhipu-cn-codingplan
zhipu-intl
zhipu-intl-codingplan
openai
openai-response
kilo
minimax
minimax-cn
kimi-cn
kimi-intl
kimi-codingplan
deepseek
gemini
openrouter
siliconflow-cn
siliconflow-intl
volcengine-cn
volcengine-cn-codingplan
volcengine-cn-agentplan
mimo-tokenplan
mimo
```

`freeze_url=False`——不是硬冻结集合，但仍是带 provider 特定行为的内置 id，
不要用作 AgentTeams 运行时 provider id：

```text
dashscope
agentscope-platform
qwenpaw-local
opencode
azure-openai
anthropic
ollama
lmstudio
```

因此对 AgentTeams 管理的 worker，安全规则是：**网关 provider 名可以任意；运行时
provider id 恒为 `agentteams-gateway`（或显式的非内置运行时 id），绝不使用 QwenPaw
内置 provider id。**

## 字段契约

```yaml
apiVersion: agentteams.io/v1beta1 # master current: no runtime yaml protocol yet
kind: MemberRuntimeConfig # master current: no runtime yaml protocol yet

# Config snapshot metadata. QwenPaw uses this section to detect whether the
# config changed since the last poll.
metadata:
  generation: 12 # master current: controller/status has CR generation, but it is not written to worker
  updatedAt: "2026-06-03T12:00:00Z" # master current: no corresponding worker injection

# Team facts. This replaces runtime/plugin calls that would otherwise query
# team information through the agt CLI.
team:
  name: demo-team # master current: controller derives this from Team.spec.teamName or Team.metadata.name, but does not inject it to worker
  storageId: demo-team # master current: no independent field
  teamRoomId: "!team:matrix.local" # master current: stored in Team.status.teamRoomID, but not injected to worker
  leaderName: leader # controller derives this from the Team workerMembers entry whose role is team_leader
  leaderRuntimeName: leader # master current: controller can derive this from leader.workerName or leader.name, but does not inject it to worker
  leaderDmRoomId: "!dm:matrix.local" # master current: stored in Team.status.leaderDMRoomID, but not injected to worker
  admin:
    name: admin # master current: controller derives this from Team.spec.admin.name or default admin, but does not inject it to worker
    matrixUserId: "@admin:matrix.local" # master current: controller resolves this for rooms and policy, but does not inject it to worker

# Current member facts. This tells the runtime who it is and which role/runtime
# adapter should be applied.
member:
  name: worker-a # master current: controller management name, not injected as a field
  runtimeName: worker-a # master current: injected through AGENTTEAMS_WORKER_NAME
  role: worker # master current: controller knows the member role internally, but does not inject it to worker
  runtime: qwenpaw # master current: comes from spec.runtime and is passed to backend runtime/image selection, not injected as env
  matrixUserId: "@worker-a:matrix.local" # master current: stored in Worker or Team member status; worker receives token but not user id
  personalRoomId: "!worker-dm:matrix.local" # master current: stored in Worker.status.roomID or Team.status.members[].roomID, but not injected to worker

# Desired state projected from CRD. Controller writes what should be true;
# QwenPaw runtime applies it to local QwenPaw/TeamHarness configuration.
desired:
  model:
    providerId: agentteams-gateway # master current: implicit in generated openclaw.json
    model: qwen-plus # master current: comes from spec.model and is written to openclaw.json, not directly injected to worker
    gatewayUrl: http://aigw-local.agentteams.io:8080 # master current: injected through AGENTTEAMS_AI_GATEWAY_URL

  agentPackage:
    ref: nacos://market.agentteams.io:80/public/dev-worker?version=1.2.0 # master current: comes from spec.package; controller resolves and deploys it directly to OSS
    name: dev-worker # master current: not written to worker
    version: 1.2.0 # master current: not written to worker
    digest: "sha256:..." # master current: not written to worker

  mcpServers:
    - name: github # master current: comes from spec.mcpServers and is written to mcporter-servers.json
      url: https://aigw.example.com/mcp-servers/github/mcp # master current: comes from spec.mcpServers and is written to mcporter-servers.json
      transport: http # master current: comes from spec.mcpServers and is written to mcporter-servers.json

  channelPolicy:
    groupAllowExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    groupDenyExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    dmAllowExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    dmDenyExtra: [] # master current: controller merges policies and writes the result to openclaw.json

  state: Running # master current: controller consumes spec.state to create, stop, or sleep the container; it is not injected to worker

# Object storage location facts. This section has remote storage coordinates
# only. It does not contain access keys or local runtime paths.
storage:
  provider: oss # master current: worker infers provider from env and available CLI, no independent field
  bucket: agentteams-storage # master current: injected through AGENTTEAMS_FS_BUCKET
  endpoint: http://minio:9000 # master current: injected through AGENTTEAMS_FS_ENDPOINT
  teamPrefix: teams/demo-team # master current: no standard field
  sharedPrefix: teams/demo-team/shared # master current: no standard field
  globalSharedPrefix: shared # master current: no standard field
  memberPrefix: agents/worker-a # master current: controller implicitly uses agents/{runtimeName}; it is not injected as a field

# Secret locations. The YAML stores where to read secrets, never the secret
# values themselves.
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN # master current: real token is injected into this env
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY # master current: real gateway key is injected into this env
  storageAccessKeyEnv: AGENTTEAMS_FS_ACCESS_KEY # master current: real storage access key is injected into this env
  storageSecretKeyEnv: AGENTTEAMS_FS_SECRET_KEY # master current: real storage secret key is injected into this env
  serviceAccountTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token # master current: provided by Kubernetes service account mount, not env injection
```
