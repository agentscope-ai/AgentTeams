# 任务完成通知（submit_task）与生命周期 attention 事件

> **实现状态**：已在 `plugins/teamharness/mcp/server.py` 中实现（分支
> `fix/task-completion-notification`）。
> **范围**：TeamHarness MCP `taskflow` 工具所承载的任务生命周期（由
> Worker/Leader 运行时执行）。Manager 运行时钩子变体
> （`copaw_worker.hooks.tools.taskflow`）不在本文档范围内：Manager 负责协调
> 任务，但不承担任务执行职责，其 `submit_task` 路径并非生产完成路径。
>
> **v2（PR 评审 2026-09-05，设计 issue #1229）** 将最初的"完成行"扩展为完整
> 的生命周期 attention 模型：按状态的首行 token、`@initiator`（人类成员）
> 路由、P0 级"先同步、后通知"顺序（失败时返回可重试错误）、用于进行中人工
> 决策的新动作 `request_attention`，以及 `complete_project` 上的代码级
> `PROJECT_COMPLETED` 事件。

## 问题陈述

taskflow MCP 层存在不对称性：

- `delegate_task` **以原子方式**将分派写入任务状态，并通过 `m.mentions`
  将分派发布至任务房间（`_send_delegate_notification`，稳定 txn
  `delegate-<task-id>`，记录 `eventId`，重试时复用）。
- `submit_task` 记录终态并自动将结果产物发布为 `m.file` 事件
  （`_publish_task_artifacts`），但完成*消息*仅是一个 `notificationNeeded`
  **提示**——Worker 的 LLM 必须自行记得发送
  `@leader TASK_COMPLETED: <task-id> - Result: shared/tasks/<task-id>/result.md`。

真实部署（多轮会话）表明：Worker 在上下文压缩之后往往会遗漏该行。由此产生
以下后果：

- Leader 收不到唤醒信号：`check_task` 采用轮询方式，且 `m.file` 产物事件
  不携带 mention，仅凭产物事件不足以触发 Leader。
- 下游任务将停留在 `waiting` 状态，直至 Leader 发起轮询或由人工提醒房间。

## 设计

镜像既有的 delegate 模式：同一文件、同一发送路径：

| 关注点 | `delegate_task`（既有） | `submit_task`（本次变更） |
|:--|:--|:--|
| 发送辅助函数 | `_send_delegate_notification` | `_send_task_completion_notification` |
| Matrix 路径 | HTTP PUT `/rooms/{room}/send/m.room.message/{txn}`（与 message 工具相同） | 相同 |
| 凭证 | `AGENTTEAMS_MATRIX_URL` + `AGENTTEAMS_WORKER_MATRIX_TOKEN` | 相同（Worker 自身的 token——发送方*即* Worker） |
| 稳定 txn | `delegate-<task-id>` | `submit-<task-id>-<status>`（按状态作用域：同状态重试自动去重；状态变化则产生新事件） |
| Mention | 被分派人的 mxid | **leader 与人类成员**的 mxid（自运行配置解析——即下文 `@initiator` 路由） |
| 记录事件 | 任务状态 `eventId` | 任务状态 `completionEventId` |
| 重试 | 复用已记录 `eventId` | 复用已记录 `completionEventId` |
| 失败 | 任务永不标记 `assigned`（硬失败） | **best-effort**：提交照常完成，响应报告 `sent: false` 与错误信息 |

消息契约（首行可由 leader 侧提示词解析；与 task-execution 技能保持一致）。
每个被接受的结果状态都拥有各自的首行 token，使 leader 提示词能够直接按行
分支：

```
@leader TASK_COMPLETED: <task-id> - Result: shared/tasks/<task-id>/result.md
- Worker: @worker:matrix.local
<summary preview, ≤500 chars>
```

```
@leader TASK_REVISION_NEEDED: <task-id> - <summary>
- Worker: @worker:matrix.local
- Status: REVISION_NEEDED
```

```
@leader TASK_BLOCKED: <task-id> - <short blocker summary>
- Worker: @worker:matrix.local
- Status: BLOCKED
```

```
@leader TASK_INTERRUPTED: <task-id> - <summary>
- Worker: @worker:matrix.local
- Status: INTERRUPTED
```

`SUCCESS` / `SUCCESS_WITH_NOTES` 保留 `TASK_COMPLETED` token 与 `Result:`
行（不带 `- Status:` 行——token 本身已说明状态）；其余 token 均携带
`- Status:` 行。`submit_task` 会依据接受集校验所提交的状态
（`_validate_task_result_status`，#1183），对未知值给出明确错误，而不是渲染
通用的说明行。

接受集注记（2026-09-15，变基至 #1183）：#1183 已将接受集收窄为
`{SUCCESS, SUCCESS_WITH_NOTES, REVISION_NEEDED, BLOCKED, INTERRUPTED}`
——**移除了 `FAILED` 与 `PARTIAL`**（原因："两者均无接受映射，会在下游
失败"），并新增 `INTERRUPTED`。本文档中的 5-token 设计早于该变更；token
映射保留了防御性的 `TASK_PARTIAL` / `TASK_FAILED` 条目，以便未来恢复这些
状态时，无需另作跟进改动即可输出正确的首行。

### Leader 解析

`_team_leader_matrix_id()` 读取 controller 投射到 Worker 的运行配置：
`team.members[]` 中 `role ∈ {team_leader, teamleader, leader}` →
`matrixUserId`（与 `_roomflow_room_meta` 相同的角色归一化规则）。结果为空
时，通知将被跳过并记录 `skipped` 原因（独立运行场景）。

### 人类成员解析（@initiator）

`_team_human_matrix_ids()` 读取同一份成员名单：所有归一化角色既非 leader
亦非 worker 的成员，外加 `team.admin` 条目。这些成员即团队的人类用户
（包含任务发起人），在完成与 attention 事件中与 leader 一并被 mention。
人类成员不做成员资格校验（仅 leader 校验）：不在房间内的人类用户自然无法
看到房间事件——这正是 Matrix 的正确语义。列表为空时仅 mention leader
（独立运行场景行为不变）。

### 成员资格校验

`_validate_assignee_membership(room_id, leader)` 原样复用：在配置了 Matrix
环境的情况下，leader 必须是任务房间的已加入成员；否则发送将被跳过（记录
原因），而不是为一个无法接收事件的用户产生错误事件。

### 幂等（按状态作用域，v2）

- 稳定 txn `submit-<task-id>-<status>`：对于*同一状态*下重复投递的相同
  PUT，Matrix 将自动去重。
- 首次发送成功后，`completionEventId` **与** `completionEventStatus` 将
  一并持久化到任务状态中。此后以**相同**状态重提交时，将返回已记录事件
  （`reused: true`），且不发起任何 HTTP 调用。以**变化后**的状态重提交时，
  已记录的这一对字段将失效，并发送新事件（使用不同 txn）——先报告
  `BLOCKED`、后报告 `SUCCESS` 的 worker 将再次唤醒 leader，而不会被复用
  分支静默吸收。
- 升级前记录的任务（不含 `completionEventStatus`）保持原有行为：任何重
  提交都复用已记录事件。

### P0 排序：先同步、后通知（v2）

完成事件是*注意力信号*，而非回执。提交顺序为：本地状态 → 发布产物 →
**同步共享存储 → 随后才发送通知**。

- **同步失败** → `ok: false`、`retryable: true`、**完全不发送通知**（响应
  中不含该字段）。由于本地任务状态已是 `submitted`，重试是幂等的：事件将
  在首次同步成功时恰好发送一次。由此消除"leader 已被告知完成、却无法读取
  产物"的窗口——leader 不会被一个其产物不可读的事件唤醒。
- **通知层失败**（无房间、无 leader、无 Matrix 环境、成员资格缺失、HTTP
  错误）保持 best-effort：返回 `{"sent": false, "skipped"?: true,
  "error": "..."}`，提交仍然完成：`ok: true`、`status: "submitted"`、
  产物已发布、状态已同步。
- 同一顺序适用于 `complete_project`（2026-09-14 评审）：项目终态 →
  项目目录同步 → 仅在同步成功后发送 `PROJECT_COMPLETED`（见下文小节）。
- 既有的 `notificationNeeded` 提示**予以保留**——它还驱动发起方的回复路由
  报告，该部分为代码级完成行有意不覆盖的内容（房间与受众均不同）。

## 生命周期 attention 事件（v2，issue #1229）

### `request_attention`（新增 taskflow 动作）

目前，进行中的人工决策依赖房间内的自由发言：需要审批、决策或升级的
worker 在群组中发言，并等待人工注意到。新动作用于将此类决策转变为具备
一等地位、幂等且可审计的事件：

- **角色**：worker / leader / remote-member。终态任务将被拒绝
  （`_require_task_mutable`）。
- **载荷**：`kind` ∈ `approval | decision | escalation | other`，
  `question`（必填，≤500 字符），以及可选 `resolved: true`（用于在不带
  结果的情况下关闭）。
- **契约行**：`@leader ATTENTION_<KIND>: <task-id> - <question>`
  + `- Worker:` 行；mention leader 与人类成员。
- **状态**：向任务 meta 追加一条 `attention` 记录
  （`kind / question / attempt / requestedAt / resolved / eventId?`）。
  在存在未解决记录时重复请求同一 `kind`，将复用已记录事件（不会重复
  打扰）；**pending** 记录（已创建但首次同步失败，尚无 `eventId`）将在
  重试时被复用：重试将重新同步，并为该记录恰好发送一次事件，绝不产生
  第二条记录；新的 kind 或新的 attempt 编号将产生新事件（txn
  `attention-<task-id>-<kind>-<attempt>`）。
- **同步优先**（与 submit 一致）：同步失败时将暂不发送通知，并返回可重试
  错误。*首次*同步失败会留下 **pending 记录**（尚无 `eventId`）；重试将
  复用该记录——重新同步，随后发送并持久化恰好一个事件（确定性 txn）——
  且不得创建第二条记录或第二个事件（2026-09-15 第三轮评审）。
- **关闭同样采用同步优先**（2026-09-15 评审）：显式 `resolved: true` 会先
  在本地将记录标记为已解决，推送任务目录，随后才报告成功。关闭同步失败时
  返回可重试错误；幂等重试会重新同步已解决状态（不产生新 ping，也不产生
  新记录）。对**不存在同类记录**的 `resolved: true` 调用将被拒绝（报错、
  不记录、不发送 ping）：此时不存在待关闭的开环，而预先创建一条已解决
  记录仍会发出新的 attention ping，与"关闭时不发送新 ping"的契约相矛盾。
- **解决**：`accept_task_result` 会将任务上**所有**未解决的 attention 记录
  标记为 `resolved: true`（即 leader 的裁决关闭了整个环）；显式
  `resolved: true` 调用亦可提前关闭单条记录（同步优先，见上文）。

路由显著性注记：v1 将全部 attention 投递至任务房间（以房间 @mention
方式）。面向人类的专用 DM 步骤（更高的显著性）记录为该 PR 的后续工作，
不包含在本次变更中。

### 审批（`kind=approval`）：载荷与路由（提议）

`request_attention` 已将审批作为一等 kind 承载。要将一次 *permission* 审批
完整打通（典型场景：被委托的编码会话请求执行命令——参见 coding-agent 委托
讨论 #1340），还需在上述既有语义之上补充两项约定。

**1）选项载荷——答复必须回显请求自身的 id。**
一次 permission 请求只能以其自身的某个 option id 予以答复（严格回显）。
因此，事件在 `question` 行之外同时携带候选项：

- `options`：`[{ id, label }]` —— 答复必须回显的准确 id（例如
  `proceed_once`、`deny`、`allow_once_and_switch_mode`）；
- 可选 `suggested: <id>` —— 仅供建议，**绝不自动应用**（模式切换类选项
  仅限人工选择）；
- 可选 `expires_at` —— 到期后适用所配置的超时策略（等待 / 拒绝 / 转交
  下一位响应者）；到期请求按"拒绝并记录原因"处理，不得静默处理。

**2）路由策略——默认 console 优先，房间为可选项。**
当前所有 attention 均投递至任务房间。考虑到审批通常携带代码上下文，其
默认路由应当可配置：

- `console-first`（推荐默认）：投递至操作者的 console 界面；未经显式选择
  时，房间不受影响；
- `room`：现有路径（房间 @mention，leader 与人类成员）——按需启用；
- 每个请求仅选择一条路由——不得重复投递。

**答复与审计。** 答复须回显请求 `options` 中的某个 id（该集合即为校验集）；
首位响应者生效；每次答复均将 何人 / 何时 / 哪个请求 / 依据 记入审计轨迹。
`accept_task_result` 的自动解决与显式 `resolved: true` 仍按现有方式关闭环。

*（本节为提议中的约定；不包含行为变更。在上述约定落地之前，上文的房间
路由注记仍然有效。）*

### `complete_project` 上的 `PROJECT_COMPLETED`（v2，依 2026-09-14 评审采用同步优先）

`complete_project` 此前仅写入状态；一个已完工的项目需等到下一次事故才会被
发现。现在它会以与 submit 相同的 P0 顺序发送房间事件：本地终态 → 同步
共享存储 → 随后才发送通知：

- **契约行**：`@leader PROJECT_COMPLETED: <project-id> - Project completed:
  <title>`；mention leader 与人类成员。
- 房间解析：优先取计划中的第一个任务 `room_id`；当 `source_room_id` 为
  Matrix 房间时回退到该字段。
- **同步失败** → `ok: false`、`retryable: true`、**完全不发送事件**，且不
  记录 `projectCompletionEventId`——重试绝不复用一个其完成态从未到达共享
  存储的通知。由于本地状态已是 `completed`，重试是幂等的：事件将在首次
  同步成功时恰好发送一次。
- **通知层失败**（无房间、无 leader、无 Matrix 环境、成员资格缺失、HTTP
  错误）保持 best-effort：事件将被跳过并附带明确错误，
  `complete_project` 仍返回 `ok: true`（状态已持久化并同步）。
- **幂等**：`projectCompletionEventId` 仅在发送成功后才会持久化到项目状态
  （txn `project-<project-id>-success`）；重试的 `complete_project` 将复用
  已记录事件。

## 变更

| 文件 | 变更 |
|:--|:--|
| `plugins/teamharness/mcp/server.py` | v1：`_team_leader_matrix_id()`、`_send_task_completion_notification()`、`_task_completion_notification()`；`submit_task` 分支在响应中加入 `notification`。v2：`_TASK_COMPLETION_EVENT_TOKENS` 与按状态首行渲染、状态作用域 txn、`completionEventStatus`（按状态复用）；`_team_human_matrix_ids()` @initiator mentions；`submit_task` 状态校验与先同步后通知顺序（可重试失败时暂不发送通知）；新增动作 `request_attention` 与 `_send_attention_notification()`（按 kind 幂等、终态守卫、同步优先）；`accept_task_result` 自动解决未决 attention；`_send_project_completion_notification()` 与 `complete_project` 的先同步后通知顺序（同步失败时暂不发送事件并返回可重试错误；`projectCompletionEventId` 仅在发送成功后才持久化） |
| `plugins/teamharness/skills/team/task-execution/SKILL.md` | 契约小节重写：按状态由代码生成事件行（worker 不再手工发送完成行）、状态列表扩展至完整接受集、`request_attention` 记录为进行中决策路径 |
| `plugins/tests/teamharness/mcp/tools/test-taskflow.rb` | 运行配置加入团队名单；fake Matrix server 增加 `submit-` 故障注入分支；`mc` shim 增加 `TEAMHARNESS_TEST_FAIL_SYNC_TASK` / `TEAMHARNESS_TEST_FAIL_SYNC_PROJECT` 推送失败钩子；新增断言（见下文）；上下文中文件事件的选取改为基于 mxcUri 而非位置（最后一个事件不再保证为文件事件） |

## 测试（契约测试，`test-taskflow.rb`）

1. **发送与内容**：恰好一条 `submit-t-001` 消息事件；
   `m.mentions.user_ids` 包含 leader；正文携带契约行、`- Worker:` 行与
   摘要；鉴权使用 Worker token。
2. **持久化**：任务状态 `completionEventId` 与响应中的
   `notification.eventId` 相等。
3. **重试**：以相同载荷重提交，返回 `notification.reused: true`，事件 id
   相同，且不发送第二个事件。
4. **BLOCKED**：输出 `BLOCKED: <task-id> - <summary>` 行，不含
   `TASK_COMPLETED` 文本。
5. **失败**：对 `submit-` txn 强制返回 HTTP 500 → 提交仍为 `ok: true` /
   `submitted`，`notification.sent: false` 并携带 HTTP 错误，
   `completionEventId` 不被持久化。

v2 新增（issue #1229）：

6. **P0 顺序**：`mc` shim 对某个任务强制失败 → 提交返回 `ok: false` /
   `retryable: true`，且**不含 `notification` 字段**，本地状态仍为
   `submitted`；存储恢复后的幂等重试恰好发送一次事件。
7. **按状态 token 与 @initiator**：`REVISION_NEEDED` / `BLOCKED` /
   `INTERRUPTED` 各自渲染对应的首行 token 与 `- Status:` 行，且事件同时
   mention leader 与人类成员；`SUCCESS` 保留 `Result:` 行且不带
   `- Status:` 行。
8. **状态校验**：以未知状态提交将被拒绝
   （`unsupported result status: <value>`，#1183 helper），且非法值不被
   持久化。
9. **重提交一致性**（变基至 #1183）：durable-continuation digest 栅栏将
   已提交任务锁定为（status, summary, deliverables）。**精确重试将复用
   已记录事件**（`reused: true`，事件 id 相同）；**结果发生变化则产生冲突**
   （"submit_task conflicts with existing submission"）并等待 Leader 裁决
   ——已记录事件保持原样，幂等重试仍会复用它。
10. **request_attention**：进行中的 `approval` ping 发送
    `ATTENTION_APPROVAL` 行并 mention leader 与人类成员；未解决的同类重复
    请求是幂等的（不产生第二个事件）；首次同步失败后的同类重试复用
    pending 记录（一条记录、一个事件）；不同 kind 不复用；终态
    （cancelled）任务被拒绝；`accept_task_result` 解决未决记录。关闭契约
    （2026-09-15 评审）：`resolved: true` 关闭采用同步优先——关闭同步失败
    返回 `ok: false` / `retryable: true`，幂等重试重新同步已解决状态
    （不产生新 ping、也不产生新记录）；首次即以 `resolved: true` 调用且
    无同类记录时被拒绝（不产生幽灵记录、不发送 ping）。
11. **PROJECT_COMPLETED**：`complete_project` 发送 `PROJECT_COMPLETED` 行
    并 mention leader 与人类成员；重试的 `complete_project` 复用已记录
    事件。
12. **complete_project 的 P0 顺序**（2026-09-14 评审）：`mc` shim 强制
    项目目录推送失败 → `complete_project` 返回 `ok: false` /
    `retryable: true`，**不发送通知**、**不发送 `PROJECT_COMPLETED`
    事件**、且不记录 `projectCompletionEventId`（本地状态仍为
    `completed`）；存储恢复后的幂等重试恰好发送一次事件并持久化事件 id。

## 开放问题

1. **是否应从 leader 提示词中移除 `check_task` 轮询？** 自动通知使盲目轮询
   变得多余；在通知于生产环境中得到验证之前，应将其保留为对账路径（开销
   低）。
2. **Manager 运行时 taskflow**：`copaw_worker.hooks.tools.taskflow` 存在同样
   的不对称性（其 `delegate_task` 会通知，而 `submit_task` 不会）。本文档
   不处理该问题——Manager 并非任务执行者；若未来某部署使其成为执行者，
   再行审视。
