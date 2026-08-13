# TeamHarness Project/Task Runtime 设计方案

本文定义 TeamHarness Project/Task Runtime 的目标设计。设计基于现有
`projectflow`、`taskflow`、`roomflow`、TEAMS prompt 和 team skills，并向原
MAGIC/CoPaw 的 `ProjectMeta + TaskMeta + Store Protocol` 模型收敛。

核心目标不是共享 session memory，而是在 Leader 被 Worker、外部
channel 或后续事件唤醒时，通过持久化 project/task state 恢复上下文，继续验收结果、
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
  "team_id": "biz-team",
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
| `team_id` | 归属团队名（`teams/{team}/shared/projects/` 前缀）；独立 agent 为空。Controller 用它把 project 映射回团队做 RBAC。 |
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
  "submission_id": null,
  "result_digest": null,
  "continuation": null,
  "cancel_reason": null,
  "replacement_task_id": null,
  "cancelled_at": null,
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
| `status` | `assigned` / `in_progress` / `submitted`，或 Leader 写入的终态。 |
| `depends_on` | DAG 依赖的 task id 列表。 |
| `spec_path` | Worker 输入 spec。 |
| `result_path` | Worker 输出 result。 |
| `submitted_at` | 首次持久化提交的 UTC 时间，使用以 `Z` 结尾、精确到秒的 ISO 8601 字符串；重试不得改写。 |
| `submission_id` | 首次 `submit_task` 生成的不透明、不可变提交身份；调用方不得解析或假定 UUID 格式。 |
| `result_digest` | 结构化 TaskResult 的 canonical SHA-256 摘要，用于判断重试是否仍是同一提交。 |
| `continuation` | 待处理或已处理的 continuation marker；字段语义见下文。 |
| `cancel_reason` | Leader 首次取消 task 时持久化的单行原因；相同取消重试必须保持一致。 |
| `replacement_task_id` | 取消后用于替代原 task 的可选 task id；不同 replacement 表示冲突。 |
| `cancelled_at` | 首次取消成功的 UTC 时间；幂等重试不得改写。 |

首次提交将结构化 result、`status=submitted`、`submitted_at`、`submission_id`、
`result_digest` 和 pending `continuation` 一起写入 canonical TaskMeta。相同 result 的
重试必须复用上述身份和时间，不能创建新的逻辑提交；摘要不同的重试作为冲突被拒绝。
已提交结果不可原地替换。需要修改结果时，Leader 先留下明确的终态决定，再创建新的
task。`submission_id` 只是比较相等性的 fence；CoPaw 和 runtime-neutral MCP 可以使用
不同的生成格式，任何消费者都不得从它推导时间、runtime 或 task 信息。

#### Canonical result digest

两套 runtime 使用完全相同的摘要算法。先构造只有以下三个字段的 JSON object：

```json
{"deliverables":["shared/tasks/demo-project-001-01/result.md"],"status":"SUCCESS","summary":"Completed the assigned work."}
```

- `status` 去掉首尾空白。
- `summary` 把连续的 Unicode whitespace 折叠为一个 ASCII 空格，再去掉首尾空白。
- `deliverables` 必须先通过 task 目录边界和安全相对路径校验；摘要保持调用方给出的顺序，
  并保留持久化后的路径字符串，不排序、不去重。
- `notes`、`result.md` 的渲染文本和其他 runtime-specific 字段不参与摘要。
- JSON 使用 UTF-8、保留非 ASCII 字符、按 key 排序，并使用 `,` 和 `:` 作为无额外
  空白的分隔符。

最后计算下式，并把 `result_digest` 写成 64 个小写十六进制字符：

```text
sha256(UTF8("teamharness.task-result.v1") || NUL || canonical_json)
```

domain prefix 和 NUL 分隔符属于协议，不能省略。这样 CoPaw 与 runtime-neutral MCP
即使生成的 `submission_id` 格式不同，也能对同一个结构化结果得到相同 identity。

#### Continuation marker

首次提交写入：

```json
{
  "status": "pending",
  "delivery_id": "<sha256 hex>"
}
```

`delivery_id` 是未来 Controller 用于去重一次“结果已提交”唤醒尝试的稳定 key，公式为：

```text
sha256(UTF8(project_id || NUL || task_id || NUL || submission_id || NUL || "result-submitted:v1"))
```

它不是已经发送成功的 Matrix event id。PR1 不扫描 pending marker，也不发送 Matrix
消息。Leader 验收或取消 task 后，TeamHarness 保留原 `delivery_id`，并把 marker 更新为：

```json
{
  "status": "resolved",
  "delivery_id": "<original sha256 hex>",
  "resolution": "completed",
  "resolved_at": "2026-06-06T00:01:00Z"
}
```

`resolution` 是 `completed`、`revision`、`blocked` 或 `cancelled`；`resolved_at` 是
首次解决 marker 的 UTC 时间。相同终态决定的重试复用已经 resolved 的 marker，不得
旋转 `delivery_id` 或重新打开 continuation。

只有 runtime 配置识别出的可信 Leader 能写入上述终态。payload 里的 `role` 只是无
可信 runtime identity 时的兼容输入，不能覆盖一个 Worker runtime 的身份；Worker 只能
`ack_task` 和 `submit_task`，不得调用 accept、cancel 或以其他方式 resolve continuation。

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
| `submitted` | Worker 已提交 result，等待 Leader check 和显式 accept/revise/block。 |
| `completed` / `revision` / `blocked` / `cancelled` | Leader 已留下终态决定。 |

TaskResult status：

| Status | 说明 |
| --- | --- |
| `SUCCESS` | 可直接验收的成功结果。 |
| `SUCCESS_WITH_NOTES` | 成功但带注意事项。 |
| `REVISION_NEEDED` | Worker 主动说明需要修订。 |
| `BLOCKED` | Worker 被阻塞。 |
| `INTERRUPTED` | Worker 执行被中断。 |

Leader 写入 TaskMeta 和 plan node 的状态映射固定如下：

| TaskResult / decision | TaskMeta 与 plan node 终态 | continuation resolution |
| --- | --- | --- |
| `SUCCESS` / `SUCCESS_WITH_NOTES`，Leader 接受 | `completed` | `completed` |
| `SUCCESS` / `SUCCESS_WITH_NOTES`，Leader 要求修订 | `revision` | `revision` |
| `REVISION_NEEDED` | `revision` | `revision` |
| `BLOCKED` / `INTERRUPTED` | `blocked` | `blocked` |
| Leader `cancel_task` | `cancelled` | `cancelled` |

`check_task` 只读取并校验 result，不写终态。只有可信 Leader 调用
`accept_task_result` 或 `cancel_task` 才能解决 pending continuation。正常验收请求必须
把 `check_task` 返回的当前 `task.submission_id` 原样放进请求字段 `submissionId`，并
携带布尔值 `accepted`。只要 TaskMeta 已有 `submission_id`，accept 和 cancel 都必须
携带这个 `submissionId`；缺失或过期 identity、不同决定的重试以及 Worker 发起的决定
都必须在写入前被拒绝。无 identity 的 legacy 迁移例外见下文。

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

#### 写入与并发边界

PR1 的正确性边界是“每个 task 在同一时刻只有一个 runtime writer”。单个
`meta.json` 或 `plan.md` 使用同目录临时文件、flush/fsync 和原子 replace，避免读者
看到截断 JSON；CoPaw 还用进程内 per-task lock 串行化同一进程中的并发调用。这些
机制不是跨进程锁，也不提供共享存储上的 compare-and-swap (CAS)。两个 Controller、
两个 Pod 或两个 runtime 同时修改同一 task 不在 PR1 的保证范围内；后续 Controller
必须先通过 leader election 或等价所有权机制满足 single-writer 前提。

ProjectMeta/plan 和 TaskMeta 是多个独立文件，共享存储同步也不是一个分布式事务。
runtime-neutral MCP 因此把 project projection 和 task state 都提交到远端，并把中间
失败返回为 `retryable: true`、`statePersisted: true`、`synced: false`。调用方必须用
完全相同的 submission 或终态决定重试：

- `submit_task` 重试用 `result_digest` 识别原提交，补齐 project 的 `submitted`
  projection，再补齐远端 project/task state；它不旋转 `submission_id`。
- `accept_task_result` 先持久化 project 决定；如果随后 TaskMeta 或远端同步失败，
  相同 `submissionId` 与相同决定的重试补写 TaskMeta resolved marker，不重复推进 plan
  或重新打开 requester report。
- `cancel_task` 同样通过已持久化的取消原因、replacement task 和终态修复缺失的
  project/task 远端 projection；不同取消 payload 被视为冲突。

这里的保证是“可检测、可重试、可修复”，不是 exactly-once，也不是跨文件原子提交。

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
| `accept_task_result` | Leader 显式把 checked result 接受到 DAG/Loop plan；正常提交必须用当前 `submissionId` 校验。 |
| `pause_project` | 暂停项目。 |
| `resume_project` | 恢复项目。 |
| `complete_project` | 完成项目。 |
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

`accept_task_result` 是 result 验收后推进 project 状态的唯一入口。`check_task` 返回
`effective: true` 后，Leader 仍必须显式调用 `accept_task_result`，这样跨 session
恢复时不会把“result 已提交”和“project 已接受”混在一起。正常 TaskMeta 已有
`submission_id` 时，调用方必须原样传入 `submissionId`；缺失或不匹配都会被拒绝，且
不得修改 TaskMeta 或 plan。同一 submission 与同一决定的重试是幂等的，不会重复推进
plan 或重新打开 requester report。如果 project 决定已经持久化、但 TaskMeta 同步失败，
使用完全相同的 payload 重试会修复 TaskMeta，不会产生第二次业务决定。

runtime-neutral standalone MCP 的 `accept_task_result` 在接受完成结果时还会设置
`ProjectMeta.requester_report.pending`。CoPaw 原生 `projectflow` 只提交 plan node 与
TaskMeta 的终态，不凭空创建 `requester_report`。这只是状态投影差异，不是报告责任
差异：Leader 在两套 runtime 中都必须按已有 `reply_route` 和 requester report 流程行动；
CoPaw 返回中没有 pending marker 时，也不能据此省略应发送的 requester report。

两套运行入口保持相同状态语义，但取消路由不同：CoPaw 原生工具使用
`projectflow(action=cancel_task)`，runtime-neutral standalone MCP 使用
`taskflow(action=cancel_task)`。两者都只允许可信 Leader；只要 task 已有
`submission_id`，两者都必须携带当前 `submissionId`。调用方不得因为工具名不同而
绕过 submission fence。

升级时仅允许基于已持久化证据收养 legacy `submitted` task。CoPaw 要求 legacy
TaskMeta 已有 `submitted_at`，且 Worker 显式重试的完整结构化 result 与磁盘
`result.md` 完全一致；随后按
`project_id || NUL || task_id || NUL || submitted_at || NUL || result_digest || NUL || "legacy-adoption:v1"`
确定性生成不透明 identity；此后 CoPaw accept/cancel 必须携带该 identity，CoPaw 决策
入口本身不迁移缺 ID 状态。standalone MCP 的 `submit_task` 不收养无 identity 的 legacy
提交；可信 Leader 可在未提供 `submissionId` 时验收一个可校验的持久化 legacy result，
先补齐 identity/digest/pending marker，再立即写入同一终态决定。standalone 还保留两条
既有兼容路径：没有 TaskMeta 的 plan-only acceptance 不制造 TaskMeta；已有 legacy TaskMeta
但没有 identity 的 cancel 可以继续完成取消。证据缺失、结果不一致或调用方提供未知
identity 时一律 fail closed。

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
taskflow check_task(task_id) -> current task.submission_id
projectflow resolve_project(taskId=task_id)
projectflow accept_task_result(projectId, taskId, submissionId, accepted)
communication report through ProjectMeta.reply_route when requester_report.pending
projectflow mark_requester_report_sent(projectId)
```

Leader 不从当前 session 猜 project、reply route 或下一步 DAG/Loop。正常提交必须携带
`submissionId`，`accept_task_result` 会把它作为当前提交的 fence；缺失或过期标识被拒绝。
同一提交和同一决定的重复调用复用原状态，用于安全修复上一次共享存储同步失败。

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

## 4. Durable continuation 的 PR1 边界

PR1 只提供 durable continuation 所需的状态语义和部分写入修复入口，不等于任务已经
能够自驱恢复。`submit_task` 持久化稳定的 submission identity 与 pending marker；
可信 Leader 使用当前 `submissionId` 和 `accepted` 调用 `accept_task_result`，拒绝过期
决定，并使相同决定在部分同步失败后可以安全重试；Worker 无权 resolve marker。

异常 loop 中断后的自驱恢复、Controller 周期调度、Matrix 唤醒、runtime hook、
active task 扫描和 pending requester report 重投均明确 deferred。PR2 的 Controller
负责 leader-elected 周期扫描和调度；Matrix channel 负责可靠唤醒。它们应直接消费
`continuation.status=pending` 与 `delivery_id`，不得在 Controller 或 channel 中重新
定义 Task 状态映射。直到 PR2 落地，pending marker 只是持久化事实，不会自动触发
Leader，也不能声称任务已恢复。

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
| Recovery | PR1 提供 submission identity、pending/resolved marker 和可重试修复语义。 | PR2 由 Controller 周期调度并通过 Matrix 唤醒；PR1 不声称自动恢复。 |

## 推荐落地顺序

1. 先补模型和 store protocol，直接对齐 CoPaw 的 `meta.json` canonical 协议。
2. 再补 `resolve_project`、`accept_task_result`、`mark_requester_report_sent`。
3. 固化 `create_quick_project` fast tool 合约，并保证 Quick Task 不重复调用
   `delegate_task`。
4. 调整 TEAMS 和 skills 分层：TEAMS 做总索引，skills 展开步骤。
5. 最后补 DingTalk 手动 E2E：Direct Reply 不创建 state；Quick/Project Task 跨
   session 能从 task id 恢复 project context，并最终回 DingTalk。
