# Worker Channels API

每 worker 通道配置（QQ / Matrix / DingTalk / Feishu / WeChat / ...）的代理端点，把每个 worker 的 qwenpaw app 通道 API 的固定面转发到 Controller。

**用途。** L1 admin 与 L2 人类经图形前端（workbench 插件 / dashboard）把通道连到其团队内的 agent，而不是 SSH + 对 worker 的 `agent.json` 做容器手术。这些端点即该前端的 Controller 侧一半；qwenpaw 侧 API（`PUT /api/config/channels/{channel}`、`GET /api/config/channels/schemas`、...）是上游，保持不变。

## 路由

全部路由仅 `embedded` 模式可用。`kube` 模式下每个路由返回 `503`（统一地、在任何 worker 查找之前，使 worker 存在性不可被探测）。

| 方法与路径 | 上游（worker qwenpaw app） | 注记 |
|---|---|---|
| `GET /api/v1/workers/{name}/channels` | `GET /api/config/channels` | 该 worker agent 的全部通道配置 |
| `GET /api/v1/workers/{name}/channels/types` | `GET /api/config/channels/types` | 通道名列表 |
| `GET /api/v1/workers/{name}/channels/schemas` | `GET /api/config/channels/schemas` | 每通道表单 schema——UI 渲染驱动器（字段名/类型/标签/选项），前端无需每通道代码 |
| `GET /api/v1/workers/{name}/channels/{channel}` | `GET /api/config/channels/{channel}` | 单一通道配置 |
| `PUT /api/v1/workers/{name}/channels/{channel}` | `PUT /api/config/channels/{channel}` | Body = **完整**通道配置对象；响应 + `X-AgentTeams-MinIO-Persisted` 头 |
| `GET /api/v1/workers/{name}/channels/{channel}/health` | `GET /api/config/channels/{channel}/health` | 通道健康 / 连接状态 |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode` | `GET /api/config/channels/{channel}/qrcode` | 二维码认证通道（wechat / dingtalk 扫码登录） |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode/status` | `GET /api/config/channels/{channel}/qrcode/status?token=` | 轮询扫码状态；严格查询白名单（仅 `token`） |
| `POST /api/v1/workers/{name}/channels/{channel}/restart` | `POST /api/config/channels/{channel}/restart` | 不停 agent 地停/启通道 |
| `POST /api/v1/workers/{name}/channels/{channel}/conflict-check` | `POST /api/config/channels/{channel}/conflict-check` | 探测持有同一通道凭据的其他 agent（QQ 双 AppID 踢出防护）；非变更，在通道写之前执行。**增量 2.2.x 专属路由**——2.0.x worker 以自身 `404` 应答，逐字透传（版本门） |

`{channel}` 必须匹配 `^[a-z0-9][a-z0-9_-]*$`；其余任何值在上游拨号之前即 `400`（注入防护）。单段通道位置同时承载保留的固定资源 `types` 与 `schemas`。

## 授权

| 角色 | 读路由 | 写门控路由（`PUT` / `restart` / `conflict-check`） |
|---|---|---|
| `admin` / `manager`（L1） | 任意 worker | 任意 worker |
| `human`（L2，Matrix 令牌） | 自身 accessibleTeams 的 worker | 自身 accessibleTeams 的 worker，经 worker 范围更新策略（授权器 `ActionUpdate` → 同团队）；**替换凭据值或显式清空（空串）额外要求 `channel_secrets` capability**（403 点名违规字段）；未变更的凭据值是普通字段，缺省的被保留（见 PUT 语义） |
| `human`（L3，`permissionLevel: 3`，Matrix 令牌） | **仅已分配 worker**（`accessibleWorkers`，独立或团队成员；见 [l3-worker-scoped-read.md](l3-worker-scoped-read.md)）——**通道配置读取经服务端脱敏：凭据字段省略，常规字段保留** | **拒绝**——`403`（中间件 `requireSameTeam`：L3 不携带任何团队）与处理器范围检查的 `404`（变更走严格团队谓词）；按契约只读（Q2） |
| `team-leader` | 本团队 worker | **拒绝——`403`**（团队 leader 对通道只读；否则中间件的同团队 `ActionUpdate` 会放行，故处理器才是真实边界） |
| 范围化调用方，他人团队 | `404` | `404`（W8：永不 `403`，使跨团队存在性不可被探测） |
| 范围化调用方，独立 worker（无团队） | `404` | `404`（例外：`accessibleWorkers` 含该 worker 的 L3 调用方——只读） |

变更调用记入审计日志（`worker`、`upstream`、`actor`、`minio_persisted`）。

## PUT 语义

1. **校验边界 = 上游。** Body 被转发（经过下文第 5 条的凭据回填之后）；qwenpaw 以该通道的 pydantic 模型校验它。上游 `400`/`422`（校验明细）、`404`（未知通道）与 `409` 响应逐字透传。**空 body 由 Controller 以 `400` 拒绝**——上游会把它当空配置，把已保存的通道抹掉。不可解析的 JSON 原样透传（不做 diff、不做回填）——由上游校验拒绝。
2. **写入走 qwenpaw 权威路径。** 上游把配置持久化进 worker 的 `agent.json` 并热重载该通道——不重启 worker。响应体即持久化后的通道配置，逐字返回。
3. **回读校验（异步）。** `200` 之后，Controller 立即以 `X-AgentTeams-MinIO-Persisted: pending` 应答，并安排一次**在保守上界（120s）的单一后台复核，远超 worker push_loop 同步间隔**（当前 qwenpaw worker 的 `check_interval=5s`）。复核读 MinIO 基线（`agents/{name}/.qwenpaw/workspaces/default/agent.json`），把结果记入持久审计日志（action `channel_readback`，`converged=true|false`，归属该 PUT 的 actor）——收敛结果是审计信号，不是请求内信号。旧的有界请求内轮询（3×2s）在生产中对 push_loop 同步间隔系统性误报阴性，使每一次健康 PUT 都看似未持久化。**被取代的写入：** 每个 PUT 为其 (worker, channel) 认领下一个 readback 代次；若较新的 PUT 在较旧的复核运行之前落地，较旧的复核被跳过——一次较新的成功写永远不会被报告为一次较旧的（已被取代的）写的持久化失败。

   | 头值 | 含义 |
   |---|---|
   | `pending` | 后台复核已排程；收敛结果落入审计日志（`channel_readback`） |
   | `skipped` | 未配置存储客户端 |

   Controller 永不写基线——`push_loop` 仍是单一写者（手工编辑导致的持久化缺口，即 MinIO 副本滞后于活动容器，正是 `converged=false` 所暴露的）。`200` 体无论如何都是权威的：配置**已经**在 worker 上生效。
4. **凭据门（对已保存配置做 diff）。** 写入之前，Controller 从 worker 取回已保存的通道配置（同一上游的一次只读拨号；基线读不到的 `PUT` 以 `502` 失败，而非继续），并逐凭据字段（任意嵌套深度）把请求与之 diff。凭据字段是叶名在 `channelCredentialKeys` 拒绝名单中的字段：`access_token`、`bot_token`、`token`、`app_secret`、`app_token`、`client_secret`、`secret`、`encrypt_key`、`verification_token`、`password`、`sip_password`、`api_key`、`dashscope_api_key`、`livekit_api_key`、`livekit_api_secret`、`twilio_auth_token`。逐字段语义：

   | body 中的字段 | 含义 | 门（L2） | 审计 |
   |---|---|---|---|
   | 缺省 | **保留**——已保存值被回填进转发 body，因为上游整体替换通道（`config_class(**body)`），缺省的 secret 会被模型默认值抹掉 | — | — |
   | 存在、非空、等于已保存值 | 未变更往返 → 普通编辑 | — | — |
   | 存在、非空、不同于已保存值 | 凭据替换 | 要求 `channel_secrets`（403 点名字段） | `channel_credential_write` |
   | 存在且为 `""` | 显式清空（空串是清空，不是占位符） | 要求 `channel_secrets` | `channel_credential_write` |

   L1（admin/manager）写豁免该门但记入审计（action `channel_credential_write`，`who`/`role`/目标 `worker/channel`、capability、字段列表）。基线取回返回 worker 自身 `404`（未知通道）的 `PUT` 仍然继续——写入随后逐字透传上游自身的 `404`——但无回填，且每个存在的凭据字段都按写门控。
5. **基线取回成本。** 该 diff 每个 `PUT` 多一次只读上游调用（5s 拨号上界，与写相同）。它在空 body 与团队 leader 拒绝之后运行（那些不做拨号即应答）。

## 状态映射

| 上游 | Controller |
|---|---|
| `200` | `200`，body 逐字（`PUT` 附加回读头） |
| `400` / `404` / `409` / `422` | 同状态，body 逐字（`404` 兼作版本门：无通道路由器的 qwenpaw 构建返回其自身 `404` 明细） |
| 其他任何 / 拨号失败 | `502` + 截断的上游 body |

## 示例

```bash
# Connect a QQ channel to daily-carol (L1 admin, cli token)
curl -s -X PUT http://127.0.0.1:8090/api/v1/workers/daily-carol/channels/qq \
  -H "Authorization: Bearer $AGENTTEAMS_TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled":true,"app_id":"1904153419","client_secret":"***","markdown_enabled":true}'
# → 200 {"enabled":true,...}  X-AgentTeams-MinIO-Persisted: pending

# L2 user's form: fetch schemas, render, save
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/channels/schemas \
  -H "Authorization: Bearer $MATRIX_TOKEN"
```

## 注记

- **未脱敏凭据（L1/L2）。** 配置按设计未脱敏往返：范围化调用方只能触达自身团队的 agent，且表单需要已保存值才能不变往返。L1 可见全部 worker，与其既有的 worker 管理面一致。读面契约已按 #1220 §13 Q5（2026-09-16）定案：L1/L2 保留往返——脱敏读是另一次变更（遮罩 helper + reveal capability），不是配置开关。L3 读者是例外（下条）。
- **L3 读取已脱敏。** L3（worker 范围）人类可读已分配 worker 的常规配置/状态，但不可见明文凭据（维护者决定，#1277 评审）：通道配置读路由（`GET /channels`、`GET /channels/{channel}`）对 worker 范围调用方**在服务端**剥除承载凭据的字段（qwenpaw 通道模型的 secret 字段，任意嵌套深度）——字段省略、常规字段保留、`types`/`schemas` 不动。剥除在服务端，因为原始响应即契约；非 JSON 的 200 体对 L3 读者失败关闭为 `{}`。L1/L2 响应从不被触碰。
- **单 agent worker。** 无 `X-Agent-Id` 头时，worker 的 qwenpaw app 从自身配置解析活动 agent；在单 profile worker 容器中，即 worker 自己的 agent。因此全局（非 agent 范围）的上游路径无需头管线即指向正确的 agent。
- **寻址。** 上游按 `http://{containerPrefix}{name}:{AGENTTEAMS_CONSOLE_PORT}` 拨号（默认 `8088`，系统优先的 env 解析——与创建容器时同一链），与 worker checkpoint/approval 代理相同。

## QwenPaw 版本契约

本代理**版本无关**：它转发到固定、带前缀的路径（`/api/config/channels/...`），不含任何版本逻辑，因此合并或运行它不需要固定任何特定 QwenPaw 版本。该路径是 worker 自身客户端（`qwenpaw_worker/api.py`）与集成覆盖所依据的契约，`TestChannelsUpstreamPathsMatchWorkerContract` 把每个转发路径与之钉死，使任何未来的 worker API 移动令测试失败，而不是在运行时静默 404。

**9 路由最小契约**已直接对官方 PyPI release wheel（经 hash 校验）验证，均在 `/api` 挂载下暴露相同路径：

| QwenPaw release | 9 个转发路由 | `conflict-check` |
|---|---|---|
| 2.0.1（2026-07-24） | 全部存在，路径相同 | 不存在（包内零引用） |
| 2.2.0（2026-09-03） | 全部存在，路径相同 | 存在（`config.py:379`） |
| 2.2.1（2026-09-11） | 全部存在，路径相同 | 存在（`config.py:379`；router 与 2.2.0 逐字节相同） |

因此该 API 在 2.0.1 → 2.2.1 范围内对任何 QwenPaw 不变地工作：

- **`conflict-check` 是增量 2.2.x 专属路由。** 2.2.x worker 暴露它；2.0.x worker 不暴露。代理现在转发它：在 2.2.x worker 上检查运行，在旧构建上上游自身的 `404` 明细被逐字返回，使客户端能区分"该 worker 构建无冲突检查"与真实失败，并相应隐藏入口（下文的透传版本门）。
- **透传版本门。** 对完全没有通道路由器的 QwenPaw 构建，上游自身的 `404` 明细被逐字返回，使调用方看到可区分、源自上游的失败，而不是静默的代理错误。
- **MinIO 回读时机。** `X-AgentTeams-MinIO-Persisted: pending` 意味着基线收敛检查在后台运行（2× push_loop 间隔），其结果是审计日志条目（`channel_readback`，`converged=true|false`），不是头值。`converged=false` 意为"检查时点未收敛"，不是"写失败"（200 体是权威的）。需要持久化信号的客户端查询审计对象，而非头。
