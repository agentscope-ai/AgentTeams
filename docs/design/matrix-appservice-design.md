# Matrix AppService 集成方案

## 1. 背景

当前 HiClaw controller 管理 Matrix 资源时，主要通过用户名密码登录获取用户 access token，再以对应用户身份创建房间、邀请成员、加入房间、踢出成员和发送消息。

这种方式存在几个问题：

- controller 需要保存或依赖用户密码。
- Human 修改密码后，controller 可能无法继续自动维护 team room 和 DM room。
- Worker / Manager 这类 Agent 用户并不需要真人密码登录能力，却仍然需要长期密码凭据。
- team admin 拉群场景要求 controller 以 team admin 的 Matrix 身份创建和维护房间，单纯使用全局 admin token 无法保留现有语义。

Matrix AppService 可以把这些操作收敛为一个 controller 持有的受控代理能力。controller 通过 AppService token 代本地用户注册和登录，得到普通 Matrix access token 后再执行标准 Client API 操作。

## 2. 目标

- Matrix 用户模型保持现状，不引入新的 localpart 前缀。
- 整个 Matrix homeserver 是 HiClaw 专用实例，所有本地用户都由 HiClaw 纳管。
- controller 作为 Matrix AppService，持有唯一的 as_token。
- controller 不再依赖 Manager / Worker / Human 的密码执行 Matrix 管控动作。
- Manager / Worker 不再保存 Matrix 密码，只使用 controller 签发的 access token 运行。
- Human 仍保留用户名密码登录 Element 的能力。
- controller 可以通过 AppService 代 Human 登录，从而以 team admin 身份创建 team room 和 leader DM。
- 现有 room alias、Matrix ID、房间创建者语义尽量保持不变。

### 非目标

- 不改变现有 Matrix user ID 形式。
- 不迁移已有房间历史。
- 不在第一阶段支持 Matrix E2EE 的完整设备密钥恢复。
- 不把 as_token 下发给 Manager、Worker 或 Human。
- 不把 Matrix homeserver 设计成多租户公共服务。该方案假设 homeserver 专用于 HiClaw。

## 3. 架构

### 3.1 身份流

#### Matrix 用户模型

用户 ID 保持现状：

| 类型 | Matrix ID |
|------|-----------|
| Manager | `@manager:<domain>` |
| Worker | `@<workerName>:<domain>` |
| Human | `@<humanUsername>:<domain>` |
| Admin | `@<adminUser>:<domain>` |
| AppService sender | `@hiclaw-controller:<domain>` |

示例：

```
@manager:matrix-local.hiclaw.io:18080
@alice:matrix-local.hiclaw.io:18080
@worker-1:matrix-local.hiclaw.io:18080
@hiclaw-controller:matrix-local.hiclaw.io:18080
```

Room alias 保持现状：

```
#hiclaw-manager-<managerName>:<domain>
#hiclaw-worker-<workerName>:<domain>
#hiclaw-team-<teamName>:<domain>
#hiclaw-leader-dm-<leaderName>:<domain>
```

#### AppService 注册模型

因为 Matrix homeserver 是 HiClaw 专用实例，AppService 可以覆盖全部本地用户 namespace。

```yaml
id: hiclaw-controller
url: null
as_token: <random-as-token>
hs_token: <random-hs-token>
sender_localpart: hiclaw-controller
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '@.*:<domain>'
  aliases:
    - exclusive: false
      regex: '#hiclaw-.*:<domain>'
  rooms: []
```

字段说明：

- **id**: AppService 标识，固定为 `hiclaw-controller`。
- **url**: 第一阶段设为 `null`，controller 主动调用 homeserver，不要求 homeserver 回调 controller。
- **as_token**: controller 调用 Matrix API 的 AppService token。未设置时自动生成。
- **hs_token**: 预留给未来 AppService HTTP receiver 校验 homeserver 回调。未设置时自动生成。
- **sender_localpart**: AppService sender 用户 localpart。
- **users**: 使用 `exclusive: true` 覆盖所有本地用户。
- **aliases**: 使用 `exclusive: false` 覆盖 HiClaw 管理的 room alias（非独占以避免阻止常规 CreateRoom）。

注意：

- sender_localpart 不能和已有 Human / Worker 名称冲突。
- as_token 等同于整个 HiClaw Matrix 实例的代理能力，只能保存在 controller Secret 中。
- 该模型不适用于承载外部非 HiClaw 用户的公共 Matrix homeserver。

### 3.2 Matrix API 行为

#### AppService 注册用户

```
POST /_matrix/client/v3/register
Authorization: Bearer <as_token>
Content-Type: application/json

{
  "type": "m.login.application_service",
  "username": "alice"
}
```

期望返回：

```json
{
  "user_id": "@alice:<domain>",
  "access_token": "...",
  "device_id": "..."
}
```

如果用户已经存在，controller 应视 homeserver 返回码决定是否转入 AppService login。现有 EnsureUser 的幂等语义应保留。

#### AppService 代用户登录

```
POST /_matrix/client/v3/login
Authorization: Bearer <as_token>
Content-Type: application/json

{
  "type": "m.login.application_service",
  "identifier": {
    "type": "m.id.user",
    "user": "alice"
  }
}
```

返回的是普通 Matrix user access token。controller 后续用该 token 以 `@alice:<domain>` 身份调用 Matrix Client API。

#### 用户密码登录

Human 仍然可以使用普通 password login：

```
POST /_matrix/client/v3/login
Content-Type: application/json

{
  "type": "m.login.password",
  "identifier": {
    "type": "m.id.user",
    "user": "alice"
  },
  "password": "<human-password>"
}
```

AppService 纳管并不禁止密码登录。是否能密码登录取决于该用户是否设置了密码。

### 3.3 账号生命周期

#### Manager

Manager 不再需要 Matrix password。controller 保存并刷新 Manager 的 Matrix token。

#### Worker

Worker 不设置 Matrix password。Worker runtime 只使用 access token 执行 /sync、发送消息、接收消息、加入房间等操作。

#### Human

Human 与 Agent 的差异：

- Human 需要设置密码，供真人登录 Element。
- controller 后续不依赖 Human 密码，而是通过 AppService login 获取 Human actor token。
- Human 改密码不会破坏 controller reconcile。

### 3.4 Team 和 DM 房间设计

当前 team admin 创建房间的语义保留。变化只在于 team admin token 的来源。

**旧流程：**

```
Human initial password -> LoginAsHuman -> TeamAdminActorToken -> CreateRoom
```

**新流程：**

```
AppService as_token -> LoginAppServiceUser(human username) -> TeamAdminActorToken -> CreateRoom
```

#### Team room

有 team admin 时：

- creator 是 `@<teamAdmin>:<domain>`。
- team admin power level 为 100。
- leader power level 为 100。
- coordinator / member / worker 按现有规则加入。
- membership reconcile 使用 team admin token。

无 team admin 时：

- 保留现有 global admin fallback 逻辑。

#### Leader DM

有 team admin 时：

- creator 是 `@<teamAdmin>:<domain>`。
- DM 成员为 team admin + leader。
- membership reconcile 使用 team admin token。

无 team admin 时：

- 保留现有 global admin fallback 或 leader token fallback。

#### Human room reconcile

Human 加入可访问房间时：

```
desired room 新增
  -> invite human
  -> LoginAppServiceUser(human username)
  -> JoinRoomAs(roomID, humanToken)
  -> 更新 Human.status.rooms
```

这会消除当前因 Human 密码过期或被用户修改导致自动 join 失败的问题。

## 4. Go 接口设计

### 4.1 Matrix config

```go
type Config struct {
    ServerURL string
    Domain    string

    // Legacy fallback.
    RegistrationToken string
    AdminUser         string
    AdminPassword     string

    // AppService mode.
    AppServiceEnabled         bool
    AppServiceID              string
    AppServiceToken           string   // as_token
    AppServiceHSToken         string   // hs_token (预留)
    AppServiceSenderLocalpart string

    E2EEEnabled bool
}
```

### 4.2 Matrix client

显式增加 AppService 方法，避免把 password login 和 AppService login 混在同一个语义里。

```go
type Client interface {
    EnsureUser(ctx context.Context, req EnsureUserRequest) (*UserCredentials, error)
    Login(ctx context.Context, username, password string) (string, error)

    EnsureAppServiceUser(ctx context.Context, username string) (*UserCredentials, error)
    LoginAppServiceUser(ctx context.Context, username string) (string, error)
    SetPasswordAsAdmin(ctx context.Context, userID, password string) error

    CreateRoom(ctx context.Context, req CreateRoomRequest) (*RoomInfo, error)
    ResolveRoomAlias(ctx context.Context, alias string) (string, bool, error)
    DeleteRoomAlias(ctx context.Context, alias string) error

    JoinRoom(ctx context.Context, roomID, userToken string) error
    LeaveRoom(ctx context.Context, roomID, userToken string) error
    SendMessage(ctx context.Context, roomID, token, body string) error
    SendMessageAsAdmin(ctx context.Context, roomID, body string) error

    ListJoinedRooms(ctx context.Context, userToken string) ([]string, error)
    ListRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error)
    ListRoomMembersWithToken(ctx context.Context, roomID, userToken string) ([]RoomMember, error)

    InviteToRoom(ctx context.Context, roomID, userID string) error
    InviteToRoomWithToken(ctx context.Context, roomID, userID, inviterToken string) error
    KickFromRoom(ctx context.Context, roomID, userID, reason string) error
    KickFromRoomWithToken(ctx context.Context, roomID, userID, reason, kickerToken string) error

    UserID(localpart string) string
}
```

### 4.3 Credentials

UserCredentials 在 AppService 模式下允许 Password 为空：

```go
type UserCredentials struct {
    UserID      string
    AccessToken string
    Password    string
    Created     bool
}
```

- **Manager / Worker**：Password 为空，AccessToken 必须非空。
- **Human**：Password 是 controller 初始化或重置后生成的 Human 登录密码。controller 不使用该密码做自动化管控。

### 4.4 Provisioner 改造

#### ProvisionManager

```
load/generate credentials
  -> EnsureAppServiceUser("manager")
  -> LoginAppServiceUser("manager")
  -> creds.MatrixToken = token
  -> creds.MatrixPassword = ""
  -> create admin DM room
  -> write manager config
```

兼容策略：

- 如果 `AppServiceEnabled=false`，走现有 registration token + password login 逻辑。
- 如果旧 credentials 里存在 MatrixPassword，AppService 模式下不再使用，但不必立即删除，可在后续版本清理。

#### ProvisionWorker

```
load/generate credentials
  -> EnsureAppServiceUser(workerName)
  -> LoginAppServiceUser(workerName)
  -> creds.MatrixToken = token
  -> creds.MatrixPassword = ""
  -> create worker room
  -> write worker config
```

Worker 运行期只依赖 MatrixToken。

#### EnsureHumanUser

```
EnsureAppServiceUser(username)
  -> if first create or password reset requested:
       GeneratePassword()
       SetPasswordAsAdmin(userID, password)
  -> LoginAppServiceUser(username)
  -> return HumanCredentials{UserID, AccessToken, Password}
```

需要注意：

- Human 的 InitialPassword 继续服务于真人首次登录。
- controller 后续自动化流程不依赖 InitialPassword。
- Human 改密码后，InitialPassword 可能不再有效，但不会影响 controller。

#### LoginAsHuman

过渡期保留原签名：

```go
func (p *Provisioner) LoginAsHuman(ctx context.Context, username, password string) (string, error) {
    if p.appServiceEnabled {
        return p.matrix.LoginAppServiceUser(ctx, username)
    }
    return p.matrix.Login(ctx, username, password)
}
```

### 4.5 Controller 改造点

#### TeamReconciler.resolveTeamAdminActor

AppService 模式下不再要求 InitialPassword 非空：

```
load Human CR
  -> username := human.Spec.EffectiveUsername(human.Name)
  -> matrixUserID := Provisioner.MatrixUserID(username)
  -> validate team.Spec.Admin.MatrixUserID if present
  -> token := Provisioner.LoginAsHuman(ctx, username, "")
  -> return teamAdminActor{MatrixUserID, Token, Username}
```

#### HumanReconciler.ensureUserToken

AppService 模式下，desired rooms 新增时可以稳定自动 join：

```
if cached token exists:
    return cached token
token := Provisioner.LoginAsHuman(ctx, username, "")
cache token in scope
return token
```

#### Worker / Manager Reconciler

不再把 Matrix password 作为 Agent 运行配置的一部分。

## 5. Worker Matrix 密码下发改造

### 5.1 Controller deployer

DeployWorker 和 DeployManager 中写入 Matrix password 的逻辑需要加 AppService 分支。

旧逻辑：

```python
if req.MatrixPassword != "":
    put agents/<name>/credentials/matrix/password
```

目标逻辑：

```python
if matrix.appservice.enabled:
    do not write agents/<name>/credentials/matrix/password
else if req.MatrixPassword != "":
    put agents/<name>/credentials/matrix/password
```

注意：

- AppService 模式下不要把空 password 文件写入 OSS / MinIO。
- 是否删除存量 password 文件应作为显式迁移动作，不建议在普通 reconcile 中静默删除，避免回滚到 legacy 模式时失去凭据。

### 5.2 Worker runtime

AppService 模式下 runtime 不再拥有密码，也不应自行 password login。

目标行为：

```
read openclaw/copaw/hermes config
  -> if channels.matrix.accessToken exists:
       use token directly
  -> if token missing and appservice mode:
       fail fast with clear error
  -> if token missing and legacy mode:
       fallback to credentials/matrix/password login
```

runtime 需要满足：

- `credentials/matrix/password` 不存在时不报错，除非当前处于 legacy password 模式且 token 也不存在。
- token-first，不因为 password 文件存在而主动覆盖 controller 下发的新 token。
- token 失效时只报告 unhealthy，不直接尝试 password login。
- token 刷新由 controller 通过 AppService login 完成。

### 5.3 Token 刷新职责迁移

AppService 模式下，刷新 Matrix token 的职责从 worker runtime 迁回 controller：

```
worker detects Matrix 401 / sync failure
  -> health/status reports token invalid
  -> controller RefreshCredentials
  -> LoginAppServiceUser(workerName)
  -> update channels.matrix.accessToken in remote config
  -> restart worker pod/container or trigger runtime reload
```

## 6. 环境变量

| 环境变量 | 必填 | 默认值 | 说明 |
|---------|------|--------|------|
| `HICLAW_MATRIX_APPSERVICE_ENABLED` | 否 | `true` | 设为 `0`/`false` 可回退 legacy 模式 |
| `HICLAW_MATRIX_APPSERVICE_ID` | 否 | `hiclaw-controller` | AppService 注册 ID |
| `HICLAW_MATRIX_APPSERVICE_AS_TOKEN` | 否 | 自动生成 | AS token，用于代理用户注册/登录 |
| `HICLAW_MATRIX_APPSERVICE_HS_TOKEN` | 否 | 自动生成 | HS token（Phase 1 预留） |
| `HICLAW_MATRIX_APPSERVICE_SENDER_LOCALPART` | 否 | `hiclaw-controller` | AS sender 的 Matrix localpart |

当 AS 开启但未提供 token 时，controller 自动生成 32 字节随机 token 并在启动日志中展示。

## 7. 模式切换与回滚

### AS → Legacy 自动密码补设

当用户从 `ENABLED=true` 切换到 `ENABLED=false` 时，controller 启动时自动检测并为缺少密码的 Worker/Manager 补设密码：

```
start in legacy mode
  -> CredentialStore.List() 获取所有凭证
  -> 对每个 MatrixPassword == "" 的条目:
       GeneratePassword(16)
       SetPasswordAsAdmin(userID, password)
       Save updated credentials
  -> 日志记录 backfill 结果
```

特性：

- 幂等：已有密码的条目直接跳过。
- 容错：单个失败不影响其他。
- Human 不受影响：AS 模式下已设置初始密码。

### Legacy → AS 升级

升级到 AppService 模式后，存量 password 文件可能仍然存在。升级策略：

1. 注册 AppService。
2. controller 对现有 Manager / Worker 执行 `LoginAppServiceUser(localpart)`。
3. controller 写入新的 `channels.matrix.accessToken`。
4. runtime 优先使用新 token，忽略 password 文件。
5. 观察一个版本周期后，通过显式 migration/cleanup 删除旧 password 文件。

不建议第一阶段自动删除旧 password 文件，便于回滚到 legacy 模式。

## 8. Tuwunel AppService 注册

### Admin command 方式

Tuwunel 支持通过 admin room 注册 AppService。controller 以 admin 用户向 `#admins:<domain>` 房间发送：

````
!admin appservices register

```yaml
id: hiclaw-controller
url: null
as_token: "<random-as-token>"
hs_token: "<random-hs-token>"
sender_localpart: "hiclaw-controller"
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '@.*:matrix-local.hiclaw.io:18080'
  aliases:
    - exclusive: false
      regex: '#hiclaw-.*:matrix-local.hiclaw.io:18080'
  rooms: []
```
````

实现步骤：

```
wait Tuwunel ready
  -> ensure admin token
  -> resolve #admins:<domain>
  -> render AppService registration YAML
  -> send "!admin appservices register" + fenced YAML block
  -> poll AppService smoke test
  -> mark Matrix AppService ready
```

### AppService smoke test

注册后验证 homeserver 已认可 as_token：

```
POST /_matrix/client/v3/login
Authorization: Bearer <as_token>

{
  "type": "m.login.application_service",
  "identifier": {
    "type": "m.id.user",
    "user": "hiclaw-controller"
  }
}
```

成功条件：HTTP 200 + 非空 access_token + user_id 为 `@hiclaw-controller:<domain>`。

重试策略：最多 5 次，每次间隔 2 秒（admin command 是异步处理的）。

### 文件挂载方式

文件挂载方式作为 Helm 或 Synapse 兼容方向保留，第一阶段不使用。

## 9. 安全设计

### Secret 边界

as_token 必须满足：

- 只保存在 controller Secret / 环境变量。
- 不写入 MinIO / OSS agent workspace。
- 不注入 Manager / Worker pod。
- 不输出到日志（自动生成时仅在启动日志中展示一次）。
- 不通过 controller API 返回。

### 审计日志

所有 AppService impersonation 都应记录结构化日志：

```
operation=createRoom
impersonatedUser=@alice:<domain>
resourceKind=Team
resourceName=demo
roomAlias=#hiclaw-team-demo:<domain>
reason=team-room-reconcile
```

建议覆盖：

- AppService user register。
- AppService user login。
- set Human password。
- create room as user。
- invite / join / kick / leave as user。
- send message as user。

### 权限边界

虽然 AppService 可以代理整个 homeserver 的本地用户，但业务权限仍必须由 HiClaw controller 自己判断：

- Team room membership 来自 Team CR。
- Human 可访问房间来自 Human CR。
- Worker membership 来自 Worker / Team spec。
- controller 不提供任意用户 impersonation 的外部 API。

## 10. E2EE 策略

第一阶段建议在 AppService 模式下禁用 Matrix E2EE。

原因：

- AppService login 会产生设备会话。
- E2EE 需要稳定 device identity、cross-signing 和 recovery key。
- Worker / Manager 不再有 Matrix password，不能依赖密码恢复设备密钥。

未来如果要支持 E2EE，需要单独设计固定 device_id、controller 侧 device lifecycle、recovery key 存储和轮换等。

## 11. 迁移策略

因为 Matrix ID 不变，迁移不需要搬迁用户或房间。

### 阶段一：双轨兼容（当前）

- 新增 AppService 配置。
- `AppServiceEnabled=true`（默认）时优先 AS login。
- `AppServiceEnabled=false` 时保留旧逻辑。
- 旧 MatrixPassword 字段仍保留。
- AS → Legacy 切换时自动补设密码。

### 阶段二：清理旧密码依赖

- 去除 Agent runtime 对 password 文件的依赖。
- 清理不再使用的 Worker / Manager Matrix password 存储。
- Human InitialPassword 只作为首次登录提示和兼容字段，不再参与 controller reconcile。

## 12. 风险与缓解

| 风险 | 影响 | 缓解 |
|------|------|------|
| as_token 泄漏 | 可代理所有本地 Matrix 用户 | Secret 隔离、日志脱敏、最小下发范围 |
| Tuwunel AppService 注册行为不稳定 | 初始化失败 | 增加 smoke test 和幂等检测 |
| token drift | controller token 与 homeserver 注册不一致 | 启动时检测，不自动覆盖，要求显式轮换 |
| E2EE 不兼容 | Agent 无法读加密房间 | 第一阶段禁用 E2EE |
| Human 改密码后的状态困惑 | InitialPassword 不再有效 | 文档说明该字段只代表初始密码 |
| AppService 覆盖全域用户 | 不适合公共 homeserver | 明确 Matrix homeserver 为 HiClaw 专用 |

## 13. 最终效果

完成后：

- Matrix ID 完全保持现状。
- 所有本地 Matrix 用户都由 HiClaw AppService 纳管。
- controller 不再依赖用户密码执行 Matrix 管控。
- Worker / Manager 无密码运行，只使用 access token。
- Human 仍可通过用户名密码登录 Element。
- team admin 拉群、leader DM 创建仍显示为真实 Human 身份。
- Human 改密码不会影响 controller reconcile。
- Matrix 账号、房间、成员关系可以由 controller 稳定收敛。
- 支持 AS ↔ Legacy 双向无缝切换。
