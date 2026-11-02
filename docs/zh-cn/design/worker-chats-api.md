# Worker Chats API（只读会话可见性）

worker 的 qwenpaw app 按 `(user, channel)` 保存每一段对话（一个 *chat*）——Matrix 房间线程、QQ 私聊、console 会话——并以只读形式暴露在 worker 的 console 端口（`8088`，worker 上下文内无鉴权）。该端口未发布，因此范围化调用方（L2 人类、team leader）无从经 controller 看到 worker 的对话历史。

Controller 因此代理 worker chat API 的三个**只读**子路径。代理是薄的、字节透明的（与 worker checkpoint 代理和通道代理同一模式）：它解析 worker，强制执行 worker 范围读边界**以及参与边界**（L2 人类看到的是 worker 在其当前所属 Matrix 房间中的对话——绝不是 worker 在其他房间、DM 或非 matrix 通道中的对话），并在共享 docker 网络上转发到 worker 的 qwenpaw app。dashboard 将结果渲染为 worker 的会话列表 / agent 上下文 / 运行状态。

**按设计只读。** 没有 create / archive / delete / stream 路由：对话生命周期留在 qwenpaw app 及其自有通道上。

## 路由

| 路由 | 上游（worker qwenpaw app） | 返回 |
|---|---|---|
| `GET /api/v1/workers/{name}/chats` | `GET /api/chats` | `list[ChatSpec]`——id、name、user_id、channel、created/updated、pinned、archived、…（L2 人类：仅其在当前所属房间中的会话） |
| `GET /api/v1/workers/{name}/chats/{chat_id}` | `GET /api/chats/{chat_id}` | `ChatHistory{messages: [Message], status}`——已保存的 **agent 上下文**转换成的消息（L2 人类：仅其房间中的会话） |
| `GET /api/v1/workers/{name}/chats/{chat_id}/status` | `GET /api/chats/{chat_id}/status` | `ChatStatusResponse{status: "idle" \| "running"}`（QwenPaw ≥ 2.2.1；L2 人类：仅其房间中的会话） |

- `{chat_id}` 是 qwenpaw chat id——恒为小写 UUIDv4（每个 chat 都以 `str(uuid4())` 创建）。其余任何值在拨号之前以 `400` 拒绝（不可能路径注入）。
- 列表端点转发文档化的只读过滤器 `?user_id=`、`?channel=`、`?archived=`、`?include_app_owned=`（白名单；未知参数 → `400`，绝不 `422`）。对 L2 人类，客户端提供的过滤器**在服务端丢弃**——取数被强制为 `channel=matrix`，响应过滤到调用方的房间（客户端提供的值是过滤器，绝不是授权）——见*参与边界*。详情与状态路由不接受查询参数。
- 2xx 响应逐字流式转发（上下文可以很大；无 body 上限，与 checkpoint 代理相同）。上游 4xx 响应逐字透传（见*QwenPaw 版本契约*）。上游 5xx 包装为带截断 body 的 `502`。

### Agent 上下文与聊天历史

详情路由返回 worker **已保存的 agent 上下文**转换成的消息——不是通道中实际往来内容的保证完整记录。上下文可能经过压缩，可能包含从未发送到通道的工具输出。对 Matrix 对话，房间里实际往来的消息在 Matrix 中保持权威（受 Matrix 成员/历史可见性规则约束）；本代理是上下文检查面，不是房间历史的第二来源。dashboard 应将该视图标注为"Agent 上下文"，而不是"聊天历史"。

## 授权

### Worker 范围（第 1 层）

这些路由复用标准的 worker `GET` 授权（L1 admin：任意 worker；L2 人类 / team leader：经 `TeamMatches` 范围检查的本团队 worker；独立人类：`404`）。跨团队访问返回 `404`，与"无此 worker"统一——worker 存在性不可被探测（404 而非 403，与其他 worker 范围读相同）。仅 embedded 模式：kube 模式统一返回 `503`。

资源寻址与运行时寻址：`{name}` 路径段寻址 Worker CR，授权以它为依据（团队范围、404 而非 403）。上游拨号改用容器身份——`WorkerSpec.EffectiveWorkerName(worker.Name)`（设置了 `spec.workerName` 时用它，否则用 CR 名）——使导入/重命名的 worker 到达正确的容器。

### 参与边界（第 2 层——L2 人类，房间级）

对 L2 人类而言，通过 worker 范围是必要但不充分的：他们可以与 worker 对话，但只能**查看 worker 在其当前所属 Matrix 房间中的对话**——不是 worker 在其他房间、与其他用户的 DM 中、或非 matrix 通道中的对话。该边界是房间级的（刻意设计，不是简化）：在 AgentTeams 工作流中，人类与一个 team 的协作发生在该 team 的 Matrix 房间里，而本面的诊断价值恰好就是那些房间中 agent 驱动的对话（manager/leader 委派、worker 汇报）——人类自己的 `@` 消息是少数情形，发送者级的"只看我自己的会话"会藏起诊断者需要的证据，同时还极易被钻空子（任何队友的 MXID 都行）。房间级是字面意义的"谁在房间里"检查：它匹配 Matrix 自身的可见性模型，正确处理 DM（双人房间对其参与者保持私密），且无需存储成员关系表。该边界在服务端对全部三个路由强制执行：

- **锚点。** 调用方**自己的** Matrix 访问令牌——鉴权器刚经 whoami 校验过的那个——从请求中重新读取，传给 homeserver 的 `GET /_matrix/client/v3/joined_rooms`（`matrix.Client.ListJoinedRooms` 带用户令牌；不存在"该用户具体在哪些房间"的 admin 代查视图）。此路径无权限升级、无对 Human CR 的依赖（团队归属用的仍是 Human CR，身份层照常使用）。
- **会话 → 房间映射。** matrix 通道按房间给每个会话建立键：`session_id = "matrix:{room_id}"`——在该通道的两种 group-session 模式（`share_session_in_group`，AgentTeams 决策 #7001，2026-09-05）下完全相同。模式之间只有逐会话的 `user_id` 不同：session 按发送者隔离时是发送者的 MXID（AgentTeams 默认——`share_session_in_group: false`），共享/legacy 模式下是房间 ID 本身。因此一个会话在调用方的房间集合中当且仅当 `matrixRoomID(chat.session_id)` 是调用方所在的房间——按房间放行每个 `(room, sender)` session（隔离模式）或那唯一的房间 session（共享模式）。非 matrix 通道（qq/console/cron）、应用自有的会话与 subagent session 没有 `"!"` 前缀的房间命名空间：`matrixRoomID` 产生 `""`，它们对 L2 人类永不可见（失败关闭，无按通道白名单）。
- **列表。** 客户端提供的过滤器被丢弃；controller 取 `GET /api/chats?channel=matrix`，只返回其 session 解析到调用方某个房间的条目，逐条目字节透明地重新编码（空结果是 `[]`，绝不 `null`）。
- **详情 / 状态。** 参与预检（worker 的 matrix 会话列表过滤到调用方房间后必须包含该 chat id）在拨号之前运行；缺失的会话返回**以上游自身未找到形状的统一 404**（`{"detail":"Chat not found: {id}"}`）——与真实缺失的会话不可区分，因此既不能探测其他房间的会话存在性，也不能探测其内容，且上游详情端点绝不为被拒请求拨号。预检上游失败返回 `502`，而不是假 404：无法证实的参与关系不应静默藏起一个健康的 worker。
- **失败关闭。** 若请求不带 bearer 令牌、Matrix 源不可用（未接入 Matrix 客户端）、或 `joined_rooms` 调用失败，整个面对该调用方隐藏（统一 `404`，不拨上游）——无法证实的锚点不应泄漏会话视图。
- **L3（worker 范围）人类**在 v1 没有 chats 访问——与 #1277 一致，#1277 刻意把其他读面（checkpoints、skills、…）保持团队范围，并把扩展列为后续项。他们的 `WorkerReadable` 分支不适用于本路由。

全视图调用方（L1 admin、manager SA、team leader SA）完全跳过第 2 层——他们看到完整列表（带客户端过滤器），并直接拨号详情/状态。

**数据敏感性。** agent 上下文是 worker 已保存的对话上下文，可能包含工具输出或存储配置的片段。访问受 worker 范围**且**房间参与关系双重限定——不引入任何新的凭据或 capability（成员查询复用调用方自己已校验的令牌），团队外的调用方甚至无法确认该 worker 存在。

## 状态映射

| 上游 / 情形 | Controller 响应 |
|---|---|
| worker 名 / chat id 校验失败 | `400` |
| worker 未找到 | `404` `{"message":"worker not found"}` |
| 范围化调用方，不在该 worker 的团队 | `404`（与未知 worker 同 body） |
| L3（worker 范围）人类 | `404`（v1 无 chats 访问） |
| L2 人类，无令牌 / 无 Matrix 源 / `joined_rooms` 失败 | `404`（统一，失败关闭，不拨上游） |
| L2 人类，会话不在其房间内（详情/状态） | `404` `{"detail":"Chat not found: {id}"}`（上游自身形状） |
| L2 人类，预检上游调用失败 | `502`（参与关系无法证实不是 404） |
| kube 模式 | `503` |
| 上游不可达（5 秒超时） | `502` `{"message":"worker unreachable"}` |
| 上游 2xx | `200`，body + `Content-Type` 逐字 |
| 上游 4xx | 逐字透传（状态 + body） |
| 上游 5xx | `502`，body 截断至 128 字节 |

## 示例

```bash
# L2 human (alice) lists a worker's sessions in the rooms she is in —
# agent-driven (manager/leader) sessions included; the worker's chats in
# other rooms, DMs, and non-matrix channels never appear.
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 [{"id":"0d9f…","name":"matrix:…","session_id":"matrix:!…","user_id":"@manager:…","channel":"matrix",…}, …]

# open the agent context of a session in one of her rooms
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/0d9f3d6e-…-0e1f \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 {"messages":[{"type":"message","role":"user","content":[…],…}], "status":"idle"}

# a chat in a room she is not in → uniform 404, indistinguishable from absent
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/{other-room-chat-id} \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 404 {"detail":"Chat not found: {other-user-chat-id}"}

# is the worker currently replying in that session? (QwenPaw ≥ 2.2.1)
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/0d9f3d6e-…-0e1f/status \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 {"status":"running"}   (always 200 on 2.2.1+; 404 on older builds)

# L1 admin sees the full list (client filters apply as written)
curl -s "http://127.0.0.1:8090/api/v1/workers/daily-carol/chats?user_id=@bob:…" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 [ …all of the worker's chats matching the filter… ]
```

## 注记

- **单 agent worker。** 无 `X-Agent-Id` 头时，worker 的 qwenpaw app 从自身配置解析活动 agent；在单 profile worker 容器中即 worker 自己的 agent，因此全局（非 agent 范围）的上游路径无需头管线即指向正确的 agent（与通道代理相同）。
- **寻址。** 上游按 `http://{containerPrefix}{name}:{AGENTTEAMS_CONSOLE_PORT}` 拨号（默认 `8088`，系统优先的 env 解析——与创建容器时同一链）。
- **独立 worker**（不属于任何 team）对范围化调用方以 `404` 隐藏——它们没有可范围的团队。

## 测试（`internal/server/worker_chats_test.go`）

- 全视图透传：`TestChatList_ForwardsVerbatim`、`TestChatList_AllWhitelistedParamsForwarded`、`TestChatDetail_ForwardsVerbatim`、`TestChatStatus_Forwards`——admin 查询被转发（已排序），body 逐字，大 body 流式。
- **参与边界（房间级）：**
  - `TestChat_L2RoomParticipation_ScopeCheckStillApplies`——范围内的 L2 人类解析为 200；团队范围检查仍然适用（`findTeamMember` 的第二个返回值是成员名，不是团队名）。
  - `TestChat_L2RoomParticipation_ListFiltersToCallerRooms`——列表恰好保留调用方房间内的会话（一个 agent 驱动的会话，`user_id` = manager 的 MXID，包含在内）；其他房间与非 matrix 通道被排除；客户端过滤器被丢弃（上游被强制为 `channel=matrix`）；`joined_rooms` 以调用方**自己的**令牌调用。
  - `TestChat_L2RoomParticipation_InRoomAgentDrivenDetail200`——调用方房间内、`user_id` 为 manager MXID 的会话通过预检并逐字流式返回（相对发送者级 v1 发生变化的行为）。
  - `TestChat_L2RoomParticipation_OtherRoomDetail404`——调用方不在其中的房间里的会话以上游自身形状 404；上游详情端点**从不拨号**（无内容、无存在性探测）。
  - `TestChat_L2RoomParticipation_NonMatrixChatDetail404`——console/qq 会话（无 `"!"` 前缀房间命名空间）404，从不拨号（失败关闭，无按通道白名单）。
  - `TestChat_L2RoomParticipation_SharedSessionModeVisible`——共享的 `share_session_in_group` 模式（chat 的 `user_id` = 房间 id）对每个当前房间成员可见。
  - `TestChat_L2RoomParticipation_StatusSameBoundary`——状态路由运行同样的预检（他房间 404 / 本房间 200）。
  - `TestChat_L2RoomParticipation_JoinedRoomsFailure404`——失败的 `joined_rooms` 调用：统一 404，**零**上游拨号（失败关闭）。
  - `TestChat_L2RoomParticipation_NoToken404` / `TestChat_L2RoomParticipation_NoMatrixSource404`——无 bearer 令牌 / 未接入 Matrix 客户端：统一 404，不拨号。
  - `TestChat_L2RoomParticipation_PrecheckUpstreamFailure502`——失败的预检列表调用 502（无法证实的参与关系 ≠ 404）。
  - `TestChat_L3HumanDenied`——L3（worker 范围）人类：404，上游从不拨号（v1：无 chats 访问，#1277 姿态）。
- 范围层：`TestChat_TeamLeaderCrossTeamDenied`、`TestChat_StandaloneHumanDenied`（两者不变——404，无探测）。
- 版本门：`TestChatStatus_Upstream404IsTheVersionGate`（旧构建的 404 逐字透传）。
- 健壮性：校验 400、kube 模式 503、未知 worker 404、有界的 502 body、不可达 worker 502、前缀/端口解析。

## QwenPaw 版本契约

本代理**版本无关**：它转发到固定、带前缀的路径（`/api/chats…`），不含任何版本逻辑。版本门靠透传：上游 4xx 逐字返回，使客户端能区分"该 worker 构建早于该路由"与真实失败，并相应隐藏该功能（skills 代理的模式）。

三个路由已直接对官方 PyPI release wheel（hash 校验）验证：

| QwenPaw release | `GET /api/chats` | `GET /api/chats/{id}` | `GET /api/chats/{id}/status` |
|---|---|---|---|
| 2.0.1（2026-07-24） | 存在（`user_id` / `channel` / `archived` 过滤器） | 存在 | **不存在** → 上游 `404` 透传 |
| 2.2.0（2026-09-03） | 存在 | 存在 | **不存在** → 上游 `404` 透传 |
| 2.2.1（2026-09-11） | 存在（`+include_app_owned`） | 存在（`+include_app_owned`） | 存在——恒为 `200 {status}`（查询 run tracker，而非 chat 持久化） |

因此列表 + 详情在任何 2.0.1 → 2.2.1 构建上不变地工作；状态路由是 2.2.1+ 特性，旧构建将其呈现为自身的 `404` `{"detail":"Not Found"}`——dashboard 应将其视为"该 worker 构建不可用状态"（隐藏指示器），而不是错误。转发到旧构建的 `include_app_owned` 被其 router 静默忽略（未知查询参数不被拒绝）。
