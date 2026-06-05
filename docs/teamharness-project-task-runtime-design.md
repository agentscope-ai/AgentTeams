# TeamHarness Project/Task Runtime 设计方案

本文定义 TeamHarness Project/Task Runtime 的目标设计。设计基于现有
`projectflow`、`taskflow`、`roomflow`、TEAMS prompt 和 team skills，并向原
MAGIC/CoPaw 的 `ProjectMeta + TaskMeta + Store Protocol` 模型收敛。

核心目标不是共享 session memory，而是在 Leader 被 Worker、hook 或外部
channel 唤醒时，通过持久化 project/task state 恢复上下文，继续验收结果、
推进 DAG/Loop，并把 requester report 发回 `ProjectMeta.reply_route` 记录的
原始请求通道。

## 设计原则

- `ProjectMeta.reply_route` 是项目级字段，记录最终回报路线；Task 不复制外部
  channel 路由。
- `TaskMeta.room_id` 是委派链路里的 assignment room，记录 Worker 执行任务的
  内部协作房间。
- TEAMS 只做三种模式的总索引；具体步骤由 `project-management`、
  `task-delegation`、`task-execution` 展开。
- Quick Task 需要 fast tool，避免 Leader 手工拼
  `create_project -> plan_dag -> ready_nodes -> delegate_task`。
- Hook 是兜底发现机制，只报告恢复信号，不执行业务编排。
- 存储协议兼容 CoPaw 的 TaskStore 文件协议，而不是兼容当前 TeamHarness 的
  `project.json` / `task.json` 轻量实现。
- 不直接依赖 CoPaw 包；TeamHarness 在 `plugins/teamharness/mcp/` 下维护
  runtime-neutral 的模型、store 和 MCP tool 语义。

## 1. Task/Project 模型定义与存储实现

### ProjectMeta

`ProjectMeta` 是跨 session 恢复项目上下文的根对象，canonical 存储为
`shared/projects/{project_id}/meta.json`。

```json
{
  "project_id": "demo-project-001",
  "title": "Demo project",
  "status": "active",
  "mode": "project",
  "plan_type": "dag",
  "source": "dingtalk",
  "requester": "dingtalk:user:session",
  "reply_route": {
    "channel": "dingtalk",
    "target_user": "user-id",
    "target_session": "session-id"
  },
  "parent_task_id": null,
  "created_at": "2026-06-06T00:00:00Z",
  "updated_at": "2026-06-06T00:00:00Z",
  "requester_report": {
    "pending": false,
    "reason": null,
    "task_id": null,
    "report_path": "shared/projects/demo-project-001/result.md",
    "sent_at": null
  }
}
```

| Field | 说明 |
| --- | --- |
| `project_id` | 项目唯一 ID，也是 project 目录名。 |
| `title` | 项目标题。 |
| `status` | `active` / `paused` / `completed` / `blocked`。 |
| `mode` | `quick` 或 `project`，Direct Reply 不创建 Project。 |
| `plan_type` | `dag` / `loop`，Quick Task 固定为 `dag`。 |
| `source` | 请求来源，例如 `matrix`、`dingtalk`、`api`。 |
| `requester` | 人类可读的 requester 标识，保留 CoPaw 原字段语义。 |
| `reply_route` | 最终 requester report 路由；不得包含 secret。 |
| `parent_task_id` | 子项目来自上游 task 时记录关联。 |
| `requester_report` | 是否存在待发送 requester report，以及对应 result/report 路径。 |

### TaskMeta

`TaskMeta` 是 Worker 执行任务的状态对象，canonical 存储为
`shared/tasks/{task_id}/meta.json`。

```json
{
  "task_id": "demo-project-001-01",
  "project_id": "demo-project-001",
  "task_title": "Write readiness note",
  "assigned_to": "@worker-a:matrix.local",
  "room_id": "!task-room:matrix.local",
  "status": "assigned",
  "depends_on": [],
  "assigned_at": "2026-06-06T00:00:00Z",
  "acknowledged_at": null,
  "submitted_at": null,
  "spec_path": "shared/tasks/demo-project-001-01/spec.md",
  "result_path": "shared/tasks/demo-project-001-01/result.md"
}
```

| Field | 说明 |
| --- | --- |
| `task_id` | 任务唯一 ID，也是 task 目录名。 |
| `project_id` | 所属 Project。 |
| `task_title` | 任务标题。 |
| `assigned_to` | Worker 标识。 |
| `room_id` | assignment room；只表示内部执行房间。 |
| `status` | `assigned` / `in_progress` / `submitted`。 |
| `depends_on` | DAG 依赖的 task id 列表。 |
| `spec_path` | Worker 输入 spec。 |
| `result_path` | Worker 输出 result。 |

### 状态定义

DAG/Loop plan node status：

| Status | 说明 |
| --- | --- |
| `pending` | 在 plan 中，但还没有委派。 |
| `delegated` | 已写入 TaskMeta/spec，并通知 Worker。 |
| `completed` | Leader 已接受 result，并更新 project plan。 |
| `blocked` | Leader 接受 blocker，等待人工决策、重排或终止。 |
| `revision` | Leader 认为 result 需要修订。 |

TaskMeta status：

| Status | 说明 |
| --- | --- |
| `assigned` | Leader 已委派，Worker 尚未 ack。 |
| `in_progress` | Worker 已 ack，正在执行。 |
| `submitted` | Worker 已提交 result，等待 Leader check/accept。 |

TaskResult status：

| Status | 说明 |
| --- | --- |
| `SUCCESS` | 可直接验收的成功结果。 |
| `SUCCESS_WITH_NOTES` | 成功但带注意事项。 |
| `REVISION_NEEDED` | Worker 主动说明需要修订。 |
| `BLOCKED` | Worker 被阻塞。 |
| `INTERRUPTED` | Worker 执行被中断。 |

### 存储布局

Canonical layout：

```text
shared/projects/{project_id}/
  meta.json
  plan.md
  result.md

shared/tasks/{task_id}/
  meta.json
  spec.md
  result.md
  workspace/
  deliverables/
```

CoPaw 协议兼容策略：

- 新实现以 `meta.json` 为唯一 canonical metadata 文件。
- 不把当前 TeamHarness 的 `project.json` / `task.json` 作为兼容协议。
- `ProjectMeta` 保持 CoPaw 原字段语义，并以可选字段扩展 `mode`、
  `plan_type`、`reply_route`、`updated_at`、`requester_report`。
- `TaskMeta` 保持 CoPaw 原字段语义，尤其保留 `room_id` 作为 assignment room。
- `plan.md`、`spec.md`、`result.md` 的路径和语义对齐 CoPaw
  `FileSystemTaskStore`。
- tool 输入可以接受 camelCase alias，例如 `projectId`、`replyRoute`；落盘字段使用
  snake_case。
- DingTalk client secret、access token、webhook signing secret 等不得进入
  `shared/projects`、`shared/tasks`、room log 或 project report。

### Store Protocol

TeamHarness MCP 内部维护 store protocol，先提供 filesystem 实现：

```text
read_project_meta(project_id)
write_project_meta(project_meta)
read_project_plan(project_id)
write_project_plan(project_id, plan_markdown)
read_task_meta(task_id)
write_task_meta(task_meta)
read_task_spec(task_id)
write_task_spec(task_id, spec_markdown)
read_task_result(task_id)
write_task_result(task_id, result_markdown)
list_project_ids()
```

store protocol 的职责只是读写结构化状态和文档文件，不做 DAG 决策、不发消息、
不调用外部 channel。

## 2. Task/Project Action 设计与 MCP 工具

### projectflow

`projectflow` 管 Project 生命周期、project plan、结果接受和 requester report
状态。

| Action | 说明 |
| --- | --- |
| `create_project` | 创建 `ProjectMeta`，记录 `reply_route`，不创建 task。 |
| `create_quick_project` | Quick Task fast tool，一次性创建 quick project、单节点 plan、TaskMeta/spec。 |
| `resolve_project` | 从 `projectId`、`taskId`、`parentTaskId`、`roomId`、`externalId` 恢复 project context。 |
| `plan_dag` | 写入或刷新 DAG plan，返回 ready nodes。 |
| `plan_loop` | 写入或刷新 Loop plan，返回 ready loop nodes。 |
| `ready_nodes` | 只计算 DAG 可委派节点。 |
| `ready_loop_nodes` | 只计算 Loop 可委派节点。 |
| `record_loop_iteration` | 记录 Loop 迭代决策。 |
| `accept_task_result` | Leader 显式把 checked result 接受到 DAG/Loop plan。 |
| `pause_project` | 暂停项目。 |
| `resume_project` | 恢复项目。 |
| `complete_project` | 完成项目。 |
| `check_active_tasks` | Hook recovery 使用，只返回恢复信号，不修改状态。 |
| `mark_requester_report_sent` | requester report 发送成功后清理 pending 状态。 |

`create_quick_project` 是 Quick Task 的快速工具。它应完成：

```text
create ProjectMeta(mode=quick, plan_type=dag)
write single plan node(status=delegated)
write TaskMeta(status=assigned, room_id=assignment_room)
write spec.md
return project_id, task_id, assignment_room, reply_route
```

它不负责发送 Worker assignment message，也不负责发送 requester report。消息发送仍由
`communication` skill 通过对应 channel 工具完成。

`accept_task_result` 是 project 状态推进的唯一入口。`check_task` 返回
`effective: true` 后，Leader 仍必须显式调用 `accept_task_result`，这样跨 session
恢复时不会把“result 已提交”和“project 已接受”混在一起。

### taskflow

`taskflow` 只管单个 Task 的委派、ack、提交和结果检查。

| Role | Action | 说明 |
| --- | --- | --- |
| Leader | `delegate_task` | 将 ready node 转成 TaskMeta/spec，并设置 plan node 为 `delegated`。 |
| Leader | `check_task` | 读取 TaskMeta/result，校验 result contract，返回 `effective`，不改 DAG/Loop。 |
| Worker | `ack_task` | Worker 接受任务，创建 workspace，将 TaskMeta 置为 `in_progress`。 |
| Worker | `submit_task` | Worker 写 result，将 TaskMeta 置为 `submitted`。 |

`delegate_task` 必须校验：

- task 来自 `ready_nodes` 或 `ready_loop_nodes`。
- 所有 dependencies 已是 `completed`。
- `room_id` 是已解析好的 assignment room。
- 不允许直接从用户请求创建 bare task。

### roomflow

`roomflow` 负责 assignment room 解析或创建：

- Matrix DM 来源的任务可以使用 Team Room 作为 assignment room。
- DingTalk、Feishu、WeChat、API 等外部 requester channel 来源的任务，使用
  `create_task_room` 创建内部 task room。
- assignment room 只进入 `TaskMeta.room_id`；最终 requester report 仍使用
  `ProjectMeta.reply_route`。

### Event Resume Contract

Worker completion/blocker message 必须包含 task id：

```text
TASK_COMPLETED: {task_id} - Result: shared/tasks/{task_id}/result.md
TASK_BLOCKED: {task_id} - Result: shared/tasks/{task_id}/result.md
```

Leader 收到事件后的固定恢复顺序：

```text
taskflow check_task(task_id)
projectflow resolve_project(taskId=task_id)
projectflow accept_task_result(projectId, taskId, decision)
communication report through ProjectMeta.reply_route when requester_report.pending
projectflow mark_requester_report_sent(projectId)
```

Leader 不从当前 session 猜 project、reply route 或下一步 DAG/Loop。

## 3. 流程组织模式与 TEAMS + Skill 实现

### 渐进式披露

TEAMS 定义三种模式，作为总索引：

```text
Direct Reply
Quick Task
Project Task
```

TEAMS 只说明何时选择哪种模式、每一步调用哪个 skill，不展开所有 tool payload
细节。细节由 skills 分层承载。

### Direct Reply

适用场景：

- 普通问答。
- 澄清。
- 状态确认。
- 不需要 Worker、不需要 shared state、不需要后续验收的轻量响应。

流程：

```text
classify as Direct Reply
reply in current channel/session
stop
```

约束：

- 不创建 Project。
- 不创建 Task。
- 不调用 `projectflow` / `taskflow`。
- 如果来自 DingTalk，就在 DingTalk 当前 session 直接回复。

### Quick Task

适用场景：

- 正好一个 Worker-owned task。
- 有明确 acceptance criteria。
- 需要 task spec、Worker result、Leader check 和 requester report。
- 不需要多节点 DAG、Loop、并行 Worker 或复杂重排。

流程：

```text
TEAMS selects Quick Task
project-management calls projectflow create_quick_project
communication notifies Worker in assignment_room
worker uses task-execution ack_task / submit_task
Leader resumes from TASK_COMPLETED task_id
task-delegation calls check_task
project-management calls resolve_project + accept_task_result
communication reports through ProjectMeta.reply_route
project-management calls mark_requester_report_sent
```

约束：

- 不再单独调用 `create_project`、`plan_dag`、`ready_nodes`、`delegate_task`。
- Quick Task 的 fast tool 只创建状态和 spec，不发消息。
- 如果 check/accept 后发现还需要多任务、修订波次或 Loop，升级为 Project Task。

### Project Task

适用场景：

- 多步骤协作。
- 多 Worker。
- DAG dependencies。
- Loop iteration。
- 需要持续验收、replan、blocker decision 或最终汇总。

流程：

```text
TEAMS selects Project Task
project-management create_project or resolve_project
project-management plan_dag or plan_loop
project-management ready_nodes or ready_loop_nodes
task-delegation resolves assignment_room, using roomflow when needed
task-delegation calls delegate_task
communication notifies Worker
worker uses task-execution ack_task / submit_task
Leader resumes from TASK_COMPLETED / TASK_BLOCKED task_id
task-delegation calls check_task
project-management calls resolve_project + accept_task_result
project-management decides next ready nodes, loop decision, blocker, or complete_project
communication reports through ProjectMeta.reply_route when needed
```

### Skill 分工

| Skill | 职责 | 不负责 |
| --- | --- | --- |
| `team-coordination` | 判断 Direct Reply / Quick Task / Project Task，决定 DAG vs Loop。 | 写 project/task state。 |
| `project-management` | ProjectMeta、Project Resolver、plan、ready nodes、acceptance、requester report pending。 | 写 Worker task spec 的细节和 Worker 执行。 |
| `task-delegation` | assignment room 解析、`delegate_task`、`check_task`、Worker assignment/completion message contract。 | project 生命周期推进。 |
| `task-execution` | Worker `ack_task`、执行 spec、`submit_task`、result contract。 | 修改 ProjectMeta 或 plan。 |
| `communication` | Matrix/Team Room/DM requester report 路由。 | 推断 project context。 |
| `dingtalk-channel` | DingTalk inbound 识别、保留 `reply_route`、最终回 DingTalk。 | 成为 TeamHarness 内置基础 channel 或保存 DingTalk secret。 |

## 4. Task/Project 兜底策略与 Hook 实现

Hook 替代原 heartbeat 触发方式，但保留 CoPaw Team Leader heartbeat 的边界语义：
只监控、只报告、不自动编排。

### Hook 位置

- 不在 TeamHarness `plugin.yaml` 增加 runtime-neutral top-level hooks。
- Hook 放在 runtime adapter 层，例如 QwenPaw adapter。
- Hook 可以由 runtime event、周期检查或外部 scheduler 触发，但它对
  TeamHarness 只调用 `projectflow check_active_tasks`。

### check_active_tasks 输出

`check_active_tasks` 只读 project/task state，并可结合 adapter 提供的保守
runtime signal。它返回：

| Issue | 说明 | 推荐处理 |
| --- | --- | --- |
| `task_not_running` | Task 仍处于 delegated/assigned/in_progress，但 Worker runtime 明确 idle/stopped。 | 唤醒 Leader，让正常流程要求 Worker 继续或提交 blocker/result。 |
| `worker_runtime_unknown` | Adapter 无可靠 Worker runtime 状态。 | 只报告未知，不误判 Worker stopped。 |
| `task_result_pending_check` | `result.md` 已存在，等待 Leader `check_task`。 | 唤醒 Leader 走 Event Resume Flow。 |
| `invalid_task_result` | result 文件存在但不符合 contract。 | 唤醒 Leader 要求 Worker 修正 result protocol。 |
| `missing_task_meta` | plan 中有 delegated node，但 TaskMeta 缺失。 | 唤醒 Leader 修复状态，不自动重建 task。 |
| `ready_tasks_pending` | 有 ready nodes 未被正常 Leader loop 调度。 | 唤醒 Leader 正常调度。 |
| `project_completion_pending` | DAG 全部 completed，项目待汇总完成。 | 唤醒 Leader 汇总和 complete。 |
| `loop_iteration_decision_pending` | Loop 当前迭代需要 Leader 决策。 | 唤醒 Leader 决定下一轮、停止或 replan。 |
| `requester_report_pending` | ProjectMeta 有待发送 requester report。 | 唤醒 Leader 通过 `reply_route` 发送并 mark sent。 |

### Hook 禁止行为

Hook 不允许执行：

```text
delegate_task
plan_dag
plan_loop
record_loop_iteration
accept_task_result
complete_project
create_project
create_quick_project
创建新 task
发送 requester report
```

Hook 的输出应该是恢复信号，而不是业务动作。例如：

```json
{
  "type": "TEAMHARNESS_RECOVERY_SIGNAL",
  "issues": [
    {
      "issue_type": "task_result_pending_check",
      "project_id": "demo-project-001",
      "task_id": "demo-project-001-01",
      "recommendation": "leader_resume_event_flow"
    }
  ]
}
```

Leader 被 hook 唤醒后，仍按普通事件流执行：

```text
check_task
resolve_project
accept/revision/blocker/replan/report
```

## 现有实现差距

当前 TeamHarness 已经具备轻量 project/task state 和 `create_quick_project`，但还没有
完全对齐本设计：

| Area | 当前情况 | 目标 |
| --- | --- | --- |
| Canonical state | 主要写 `project.json` / `task.json`。 | 迁移到 CoPaw 协议的 `meta.json`，不保留 TeamHarness legacy 文件作为兼容目标。 |
| ProjectMeta | 已有 `reply_route` 字段雏形，但不在 CoPaw `meta.json` 协议上。 | 基于 CoPaw `ProjectMeta` 扩展 `reply_route` 和 `requester_report`。 |
| TaskMeta | 已有 `room_id`。 | 对齐 CoPaw `TaskMeta.room_id`，明确它是 assignment room。 |
| Plan node status | 当前使用 `planned/assigned/completed` 等轻量状态。 | 收敛到 CoPaw 的 `pending/delegated/completed/blocked/revision`。 |
| Quick Task | 已有 `create_quick_project` shortcut。 | 明确为 Quick Task fast tool 合约。 |
| Result acceptance | `check_task` 与 project 推进边界不够完整。 | 新增/明确 `accept_task_result`，由 Leader 显式推进 project。 |
| Resume | 依赖当前 session 容易丢上下文。 | `resolve_project(taskId)` 返回恢复上下文。 |
| Requester report | 可能依赖即时 session。 | `requester_report.pending` 进入 ProjectMeta。 |
| Recovery | 暂未形成 TeamHarness hook contract。 | Adapter hook 调 `check_active_tasks`，只报告恢复信号。 |

## 推荐落地顺序

1. 先补模型和 store protocol，直接对齐 CoPaw 的 `meta.json` canonical 协议。
2. 再补 `resolve_project`、`accept_task_result`、`mark_requester_report_sent`、
   `check_active_tasks`。
3. 固化 `create_quick_project` fast tool 合约，并保证 Quick Task 不重复调用
   `delegate_task`。
4. 调整 TEAMS 和 skills 分层：TEAMS 做总索引，skills 展开步骤。
5. 在 QwenPaw adapter 层实现 hook recovery signal，不放到 TeamHarness 顶层
   plugin hooks。
6. 最后补 DingTalk 手动 E2E：Direct Reply 不创建 state；Quick/Project Task 跨
   session 能从 task id 恢复 project context，并最终回 DingTalk。
