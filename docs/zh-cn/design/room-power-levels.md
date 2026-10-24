# 人类成员的房间权限级别

状态：已实现
涉及面：团队 / worker / 项目房间中的 `m.room.power_levels`；`create-project.sh --grant-admin`

## 问题

人类由 human reconciler 邀请进 worker 与团队房间、由 Manager 邀请进项目房间——但从未有任何机制给他们授予 Matrix 权限级别（power level）。两个后果：

1. **在权限级别未把人类计算在内之前创建的房间**（以及人类后来才加入的房间）携带的 `m.room.power_levels` 状态只列了 manager / leader / admin 在 100、worker 在 0。人类落在隐式的 0 级。
2. **从未设置过该状态的房间**（遗留）回退到 homeserver 的严格默认值。

无论哪种情况，人类操作者对每个房间操作都得到 `403`——给房间改名、邀请同事、甚至日常维护。房间对拥有它的那些 bot 正常工作，对真正为它服务的那个人却不可用。

## 设计

**在 human 房间 reconcile 中声明式授予。** human reconciler 每个周期本就会遍历完整的期望房间集合（新房间：邀请 + 加入；已观测房间：跳过）。现在它额外确保该人类在*每个*期望房间——无论新建还是已观测——中的权限级别。已观测房间的遍历即修复路径：遗留房间在部署后的第一次 reconcile 就被修正，无需任何手工回填。

- 级别映射（`humanRoomPowerLevel`）：`permissionLevel 1` → 100（共同所有者：完整房间控制，与 admin 对等的范围）；级别 2/3 → 50（Matrix 的默认成员权限）。
- **50 级权限——明确接受。** Matrix homeserver 默认把 `kick`、`ban`、`redact` 门控在 50，因此处于 50 级的 L2/L3 人类可以改房间名、邀请成员、踢出/封禁严格低于 50 的成员（0 级的 worker——但绝不包括 100 的 manager/leader/admin）、并撤回（redact）房间内任何消息。他们不能修改权限级别（100）或创建房间。我们接受这一权限，而不是把 `kick`/`ban`/`redact` 阈值提到 100：
  - 处于 50 级的人类是作用范围限定于这些房间的操作者；房间的 manager 以 100 位于其上，故 kick/ban 无法被用来针对团队的控制平面。
  - worker 是服务账号；人类踢出/封禁一个卡住的 worker 是可逆的日常维护（reconciler 会在下一周期重新邀请其成员身份），是有用的运维杠杆，不是安全升级。
  - 50 级的 redact 是人类已是成员的房间内的消息清理；替代方案（在每个房间创建路径上提高阈值）会改变所有 worker/团队/项目房间的安全姿态——这是一次超出本 PR 范围的系统性策略变更。
- 授予是**合并**，绝不替换：`Provisioner.EnsureRoomPowerLevel` 读当前 `m.room.power_levels`（`matrix.Client.GetRoomState`，新增——actor 令牌带 admin 回退，404 → 空状态），在 `users` 中新增/上调该人类的条目，保留其他每个用户与每个非用户设置（`users_default`、`state_default`、`ban`、…），且只在级别确实变化时写回。稳态 = 每房间每周期一次 GET，零写。
- 错误按 reconcile 既有的错误策略为非致命：失败的授予记日志并在下一周期重试；房间仍被记录进 `status.rooms`。

**项目房间。** Controller 从不创建项目房间；由 Manager 经 `create-project.sh` 创建，该脚本已经写入 `power_level_content_override`（manager + admin 在 100，worker 在 0），但没有途径提升人类操作者。新增可选标志：

```
create-project.sh --id p1 --title T --workers w1,w2 --grant-admin carol,bob
```

`--grant-admin` 接受 localpart 或完整 Matrix ID，把每个用户以 100 级加入创建时的 override。此变更之前创建的房间由 Manager 一次性修复（每房间一次 `PUT m.room.power_levels`）——一次性运维任务，不属于本 PR。

## 授权：actor 选择与同级死锁

homeserver 强制执行的房间认证规则，不是"直接用 admin 令牌写"就能想当然绕过的（spec v8 规则；`provisioner_team_test.go` 中的 fake 强制执行同样的规则，因此测试证明 controller 能在真实 homeserver 下存活）：

- **对其他用户条目的严格大于（规则 9.6）。** 发送者只有在其级别**严格大于**目标的当前级别时，才能修改或移除另一用户的 `users` 条目。因此处于 100 的发送者无法降级一位坐在 100 的前 L1 人类。发送者自己的条目豁免——自我降级总是被授权（向下）。房间版本 12+ 中创建者持有无限级别、从不被阻挡；生产房间是 v1–11，死锁在那里是真实的。
- **踢出（规则 4.5.4）。** 踢出者需要至少 `kick` 级别（50），且目标级别严格低于踢出者——同级踢出被拒绝。用户总是可以离开自己的房间（4.5.1）。
- **以成员身份为限的状态访问。** 以非成员身份读或写房间状态会被拒绝——包括在 **TeamAdmin 拥有的房间**中的 homeserver admin：`ProvisionTeamRooms` 刻意以 TeamAdmin 身份创建并 reconcile，把 admin 排除在外。

本 PR 实现的后果：

1. **Actor 选择。** `GetRoomState` / `SetRoomState` 接受显式令牌（"" = homeserver admin，与之前相同）。human reconciler 为每个期望房间标注其来源；带 `spec.admin` 的团队的 team 房间以该 TeamAdmin 身份授予（令牌经共享的 `resolveTeamAdminActor` 从 admin Human 解析），其他每个房间保持默认 admin actor。
2. **同级降级 → 自写回退。** `EnsureRoomPowerLevel` 以 actor 身份写；遇 `M_FORBIDDEN` 时以该人类自己的令牌（`selfToken`）重试，其自身条目豁免于 9.6。reconciler **惰性**获取该令牌（仅在 403 之后），因此稳态周期仍不发起 Matrix Login。
3. **撤销链**（从期望集合中移除），每一阶段覆盖前一阶段做不到的：
   1. 以 homeserver admin 身份 kick（其所在的房间，目标低于 100）；
   2. **self-leave**：以该人类自己的令牌（总是被授权，任何房间、任何级别——同级 100 的唯一带内路径）；
   3. Tuwunel admin-bot 强制离开（令牌不可用 / 密码已失效）。命令送达确认即视为已解决，与 team-reconcile 惯例一致。
4. **Kick 幂等性修复。** `KickFromRoomWithToken` 过去把 403 `cannot kick ...` 吞成成功，使同级踢出看起来像移除：房间从 `status.rooms` 中删掉，而用户仍留在房间里。只有 404 / "not in room" 应答是幂等的；其他每个 403 都作为可解码的 `M_FORBIDDEN`（`matrix.APIError` / `matrix.IsForbidden`）返回，使调用方可以回退。
5. **Login 令牌缓存（稳态登录）。** `TuwunelClient` 按用户缓存 `/login` 访问令牌 30 分钟（密码登录与 AppService 冒充登录一视同仁），因此每周期的 TeamAdmin actor 解析——以及该人类自己的令牌解析——在稳态下不发起 Matrix Login。带内令牌失效器清除缓存条目：密码重置（孤儿恢复，`SetPasswordAsAdmin`）与账户停用；带外失效（服务端吊销、全端登出）靠 TTL 到期自愈。兼作账户存活检查的登录（`EnsureUser` / `EnsureAppServiceUser` 中驱动孤儿恢复的既有账户回退）永远直达 homeserver，因此缓存中的死令牌永远不会短路恢复流程。
6. **人类期望房间集合识别团队归属。** `buildDesiredHumanRooms` 除 `spec.accessibleTeams` 外，还包含该人类作为 `spec.admin` 或出现在 `spec.humanMembers` 中的所属团队的 team 房间。此点承重：`syncTeamRoomHumanStatuses`（team reconciler）把 team 房间写入 admin 的/成员的 `status.rooms`，却不触碰他们的 `spec.accessibleTeams`；若人类侧期望集合只由 `accessibleTeams` 构建，访问撤销路径就会把 team admin 踢出他们自己的 team 房间，此后该 team 每次 reconcile 都会在 join 上失败（`M_FORBIDDEN: cannot join a room that is not public`——按 creator-join 设计，admin 被刻意排除在 team 房间邀请列表之外）：永久死锁。回归测试：`TestHumanReconciler_TeamAdminRoomNotRevoked` / `TestHumanReconciler_HumanMemberRoomNotRevoked`。
7. **当团队归属声明挂起时，撤销路径绝不 kick 一个其来源无法解析的房间。** 第 6 条封掉了稳态情形（房间在 team 状态中可见）。但仍残留一个状态滞后窗口：team 开通之后，`syncTeamRoomHumanStatuses` 在团队的 `status.teamRoomID` 尚不可见于 human reconciler 缓存（跨对象的 informer 滞后）之前，就把新的 team 房间写入了 admin 的 `status.rooms`。在那个窗口内，房间在 `status.rooms` 里，但没有任何可见的 Team/Worker 声明它——来源 UNKNOWN——撤销路径照样 kick，把 team admin 逐出了自己的 team 房间；此后每次 team reconcile 都在 join 上失败（`M_FORBIDDEN: cannot join a room that is not public`），直到 test-19 的 180s 超时（CI SHARD_C 4/5）。`teamRoomRevocationLag` 检测该窗口（人类是某个房间尚不可见的团队的 `spec.admin` / `spec.humanMembers`），把未知来源房间的 kick 推迟一个周期——届时 team 状态已可见、来源可解析：仍归属 → desired（保留），确实被撤销 → kick。已知来源的撤销（人类不再归属的可见 team/worker 房间）保持即时。回归测试：`TestHumanReconciler_RevocationDeferredWhileTeamRoomUnresolved` / `TestHumanReconciler_RevocationProceedsForKnownOriginTeamRoom`。

已知局限（已记录，全部非致命/重试，或记录为卡住）：

1. **Matrix 密码不可用的 100 级人类无法被降级。** actor 写被拒绝（9.6，同级），又没有 self 令牌，因此降级每周期重试而无效果。Matrix 不提供带外的同级降级。房间*移除*不受影响（admin-bot 强制离开仍可用）。
2. **从房间已存在的 team 移除 `spec.admin`** 会使该房间仍由原 TeamAdmin 拥有（homeserver admin 不是成员）：actor 选择回退到 admin 身份，授予 403，每周期重试而无效果。撤销不受影响（self-leave / force-leave 仍可用）。
3. **撤销链从 homeserver-admin 的 kick 开始。** 被移除的房间按定义已不在期望集合中，且其来源未记录在 `status`，因此 actor 范围的 kick（`KickFromRoomAs`）还无法被选用；该机制已就位，待来源追踪落地到 status。

## 未变更的部分

- worker / team / DM 房间创建保持既有权限级别（manager / admin / leader 在 100，worker 在 0）。
- worker 服务账号仍无法管理房间（0 级不变）。
- 无 CRD 变更；映射从既有 `spec.permissionLevel` 派生。

## 测试

- `internal/matrix/client_test.go`——`TestGetRoomState`：以 admin 令牌返回状态**内容**（不是事件信封）；状态缺失 → `(nil, nil)`，不是错误。`TestGetRoomState_WithUserToken`（显式令牌以该令牌身份认证，而非 admin）、`TestGetRoomState_Forbidden` / `TestSetRoomState_Forbidden`（非成员/被拒的写以 `matrix.IsForbidden` 可解码的 `M_FORBIDDEN` 呈现）、`TestKickFromRoom_EqualPowerForbidden`（403 `cannot kick` 是错误，不是静默成功——旧的吞掉分支已移除）、`TestLeaveRoom_IdempotentNotFound`、`TestLogin_TokenCachedPerUser`（按用户缓存：第二次登录取自缓存，无 HTTP；不同用户仍直达 homeserver）、`TestLogin_TokenCacheExpires`（TTL 到期 → 重新登录）、`TestLogin_AppServiceTokenCached`、`TestInvalidateUserToken_FreshLogin`、`TestEnsureUser_OrphanRecovery_IgnoresStaleCachedToken`（过期的缓存令牌**不**短路孤儿恢复——存活检查登录绕过缓存，重置密码流程完成）。
- `internal/service/provisioner_power_test.go`（跑在**感知授权**的 fake 上，该 fake 强制执行上述 spec 规则——宽松型测试替身抓不到任何一个 P1）：遗留房间 → 以该用户级别写；既有用户被合并且不受触碰；扩展字段（`events`、`invite`、`notifications`）在写回后保留——只有目标用户条目被修改；级别完全一致 → 不写；读错误传播且无写；没有 `users` map 的状态被处理；第二次授予保留第一次（JSON 往返语义）；**同级降级**——`TestEnsureRoomPowerLevel_DemotionRevokesLevel`（actor 100 对 人类 100：actor 写被强制的 9.6 拒绝，以人类自己令牌的自写完成 100 → 50）、`TestEnsureRoomPowerLevel_EqualLevelDemotionWithoutSelfTokenFails`（无 self 令牌 → 呈现 `M_FORBIDDEN`，状态不变——无静默成功）、`TestEnsureRoomPowerLevel_SimpleGrantIsSingleActorWrite`（0 → 50 是普通 actor 写，一次尝试）、`TestEnsureRoomPowerLevel_TeamAdminOwnedRoom`（admin 读 TeamAdmin 拥有的房间 → `M_FORBIDDEN`；team-admin actor 读并写授予）、`TestEnsureRoomPowerLevel_TeamAdminRoomEqualLevelDemotion`（同一面 9.6 墙，team-admin actor 亦被挡，自写回退完成降级）。
- `internal/controller/human_controller_test.go`：`TestHumanReconciler_PowerLevelMapping`（级别 1 → 新房间与已观测房间都是 100；授予指向该人类的 Matrix ID）、`TestHumanReconciler_PowerLevelL2GetsDefault`（级别 2 → 50）、`TestHumanReconciler_PowerLevelErrorNonFatal`（授予失败不阻塞 reconcile；房间仍被记录）、`TestHumanReconciler_PowerGrantUsesTeamAdminActor`（team 房间以 TeamAdmin 的令牌授予，worker 房间以默认 admin；admin 令牌经 admin 人类的 login 解析）、`TestHumanReconciler_EqualLevelDemotionSelfWriteFallback`（403 → 以人类自己的令牌重试；login 惰性发起，恰好一次）、`TestHumanReconciler_PowerGrantNoLoginOnSuccess`（无 403 → 无 login）、`TestHumanReconciler_RevocationSelfLeaveFallback`（kick 被拒 → 以人类令牌 self-leave → 房间移除）、`TestHumanReconciler_RevocationForceLeaveLastResort`（密码失效、无 self 令牌 → admin-bot 强制离开 → 房间移除）、`TestHumanReconciler_RevocationDeferredWhileTeamRoomUnresolved`（team 把该人类标为 admin，房间在 `status.rooms`，team 的 `status.teamRoomID` 尚不可见 → kick 推迟，房间保留）、`TestHumanReconciler_RevocationProceedsForKnownOriginTeamRoom`（对另一个 team 的归属声明挂起，但被撤销房间的来源**可**见 → kick 立即执行）。
