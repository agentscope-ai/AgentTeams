# 项目 / 工作流查看 API

> 由项目工作流查看 PR 新增（agentteams/AgentTeams#1169）。

Controller 提供两个只读端点，把 TeamHarness 项目状态
（`shared/projects/{id}/meta.json`）暴露为 LangGraph 对齐的工作流视图。
它们是面向人类视图（dashboard、QwenPaw console 插件）的数据源，
也被 `agt get projects` 消费。

## 适用范围与前置条件

这些端点在**任何运行 TeamHarness（projectflow/taskflow）的 AgentTeams 部署**中都能工作——存储布局通过 Controller 配置的对象存储客户端读取，因此 `AGENTTEAMS_STORAGE_PREFIX` 与 `AGENTTEAMS_FS_BUCKET`（包括非默认值）都被自动处理，无需按部署定制代码或配置。

前置条件：

* 编排项目的 Worker 上安装了 TeamHarness MCP（`plugins/teamharness`）。只有 `projectflow`（`create_project` / `create_quick_project`）创建的项目才会产生这些端点读取的 `shared/projects/{id}/meta.json`。没有用 projectflow 而手工管理任务的团队没有项目数据——这是预期行为，不是 bug。
* 项目写入通过 `_sync_project`（随本 API 一同引入）实时推送到共享存储，因此 Controller 读到的是近实时状态，而非启动快照。

部署模式（embedded Docker、incluster K8s）全部支持；在无 K8s 的开发模式下 Controller 与其他端点一样跳过认证，因此 RBAC 仅在配置了认证器时生效。

## 端点

### `GET /api/v1/projects`

列出所有团队（以及全局 `shared/projects/` 前缀）的项目。

查询参数：

| 参数 | 含义 |
|:--|:--|
| `team` | 只返回团队匹配的项目。团队 leader 已被限定到自己的团队（们）；独立项目（空团队）仅在未设置过滤时匹配。 |

响应 `200 OK`：

```json
{
  "projects": [
    {
      "project_id": "demo-project-001",
      "title": "Demo project",
      "status": "active",
      "plan_type": "dag",
      "team_id": "biz-team",
      "mode": "project"
    }
  ],
  "total": 1
}
```

* `status` 是 TeamHarness 写入的原始项目状态：
  `active` | `paused` | `completed`。
* 项目按 `project_id` 排序。跨前缀重复的 id 会去重（meta.json 可能同时镜像
  在 effective 团队名前缀和 CR 名前缀下）。
* meta.json 缺失或损坏的项目被跳过（目录可能存在而文件正在上游写入中）。

### `GET /api/v1/projects/{id}/workflow`

返回一个项目的 LangGraph 对齐工作流。

可选查询参数：

| 参数 | 类型 | 含义 |
|:--|:--|:--|
| `includeTasks` | `bool` | 为 `true` 时同时读取每个任务的 TaskMeta（`shared/tasks/{id}/meta.json`），在响应中附加 `tasks_detail` 数组（spec/result/交付物字段及不透明的 `submission_id` fence）。默认 `false` 保持响应轻量。 |
| `format` | `string` | 响应格式。缺省返回上方 JSON 快照；`format=mermaid` 返回同一快照渲染的 Mermaid 流程图（`text/plain`，不含 `tasks_detail`——渲染只需 nodes/edges/next）。其他值返回 `400`。 |

Mermaid 输出（`?format=mermaid`）对齐 LangGraph 的 `draw_mermaid` 助手：每个节点标签为 `name: status`，next/ready 节点高亮 `ready`，其余节点按状态着色（`pending` / `delegated` / `inProgress` / `completed` / `revision` / `blocked`）。所有 classDef 都会输出，图可独立渲染。任务标题与 ID 为用户可控输入，渲染前做 mermaid 安全归一：换行→`<br>`、双引号→`#quot;`、反斜杠丢弃、其他控制字符→空格；含 `[A-Za-z0-9_-]` 之外字符的 task ID 映射为防冲突节点 ID（标签保留原文）。畸形标题因此不可能改变渲染出的图结构。

响应 `200 OK`：

```json
{
  "project_id": "demo-project-001",
  "title": "Demo project",
  "status": "active",
  "plan_type": "dag",
  "team_id": "biz-team",
  "mode": "project",
  "source": "dingtalk",
  "nodes": [
    {"id": "t1", "name": "Task 1", "status": "completed", "assignee": "@w1:matrix.local"},
    {"id": "t2", "name": "Task 2", "status": "delegated", "assignee": "@w2:matrix.local"}
  ],
  "edges": [
    {"source": "t1", "target": "t2", "conditional": false}
  ],
  "next": ["t2"],
  "interrupts": [
    {"id": "t3", "value": "blocked"},
    {"id": "loop", "value": "waiting for human decision"}
  ],
  "values": {
    "project_id": "demo-project-001",
    "title": "Demo project",
    "status": "active",
    "plan_type": "dag",
    "team_id": "biz-team",
    "mode": "project",
    "task_count": {"completed": 1, "delegated": 1}
  },
  "loop": null,
  "requester": "dingtalk:user:session",
  "source_room_id": "!room:matrix.local",
  "tasks_detail": [
    {
      "task_id": "t1",
      "project_id": "demo-project-001",
      "status": "completed",
      "spec_path": "shared/tasks/t1/spec.md",
      "assigned_to": "@w1:matrix.local",
      "summary": "Alpha report done",
      "result_status": "SUCCESS",
      "submission_id": "submission-123",
      "deliverables": [{"type": "file", "path": "shared/tasks/t1/output.pdf"}],
      "result_path": "shared/tasks/t1/result.md"
    }
  ]
}
```

`tasks_detail` 仅在 `?includeTasks=true` 时出现。它透传项目级 `nodes[]` 摘要不包含的 TaskMeta 字段：`spec_path`（任务规格文件）、`summary` / `result_status` / `result_path`（提交结果）、`deliverables`（产物清单）、`cancel_reason`（取消原因）以及用于约束 accept/cancel 决定的不透明 `submission_id`。TaskMeta 只从项目所属作用域读取：团队项目读取 `teams/{team}/shared/tasks/{id}/meta.json`，standalone 项目读取 `shared/tasks/{id}/meta.json`，不跨作用域回退。`task_id` 或 `project_id` 不匹配的 TaskMeta 会被拒绝。没有 TaskMeta 文件的任务（如尚未委派）会被跳过；单个任务读取错误也会跳过，避免一个坏任务拖垮整个响应。

节点状态归一化为前端友好枚举：

| API 值 | 原始 TeamHarness 状态 |
|:--|:--|
| `pending` | `planned` |
| `delegated` | `assigned` |
| `in-progress` | `in_progress`、`submitted` |
| `completed` | `completed` |
| `revision` | `revision` |
| `blocked` | `blocked`、`cancelled` |

语义（镜像上游 `_ready_nodes` / `_ready_loop_nodes`）：

* `next` —— 就绪节点：原始状态为 `planned`/`assigned` 且依赖全部
  `completed` 的任务。项目非 active 或 loop 处于 `waiting_user` /
  `blocked` / `completed` 时为空。
* `interrupts` —— 等待人工决策点：blocked 任务，或 `waiting_user` /
  `blocked` 状态的 loop。
* `values.task_count` —— 按归一化状态统计的节点数。

错误响应：

| 状态码 | 含义 |
|:--|:--|
| `400` | 缺少项目 id。 |
| `403` | 已认证但该角色完全不能读取项目（如 Worker）。 |
| `404` | 项目不存在（所有扫描前缀下都无 meta.json）——**或**调用者是限定读者（团队 leader / L2 人类）且不拥有该项目（隐藏存在性以防 id 枚举）。 |
| `500` | K8s 或对象存储故障。 |

### `GET /api/v1/projects/{id}/tasks/{taskId}`

单任务节点级检视：聚合该任务的图节点（状态/负责人/依赖）、TaskMeta（spec/摘要/结果/交付物）、append-only 状态迁移历史，以及指向 tracing 后端的 trace 提示。

```text
GET /api/v1/projects/{id}/tasks/{taskId}?team=alpha-team
```

响应 `200 OK`：

```json
{
  "task_id": "t1",
  "project_id": "demo-project-001",
  "status": "in-progress",
  "spec_path": "shared/tasks/t1/spec.md",
  "assigned_to": "@w1:matrix.local",
  "summary": "Alpha report done",
  "result_status": "SUCCESS",
  "result_path": "shared/tasks/t1/result.md",
  "deliverables": [{"type": "file", "path": "shared/tasks/t1/output.pdf"}],
  "history": [
    {"ts": "2026-09-05T01:00:00Z", "from": "", "to": "planned", "actor": "manager", "action": "create"},
    {"ts": "2026-09-05T02:00:00Z", "from": "planned", "to": "in_progress", "actor": "w1", "action": "ack_task"},
    {"ts": "2026-09-05T03:00:00Z", "from": "in_progress", "to": "submitted", "actor": "w1", "action": "submit_task"}
  ],
  "dependencies": [],
  "trace": {"project_id": "demo-project-001", "task_id": "t1"}
}
```

字段说明：

- `status`：TaskMeta 存在时为**原始**状态（与 `?includeTasks=true` 的 `tasks_detail` 同语义）；TaskMeta 缺失时回退到图节点归一化状态（`pending | delegated | in-progress | completed | revision | blocked`）。
- `history`：由 TeamHarness taskflow（及 controller 的 cancel 路径）append-only 维护的已接受状态迁移审计，上限 50 条；工作流状态机落地（设计：agentscope-ai/AgentTeams#1223）前为空。畸形条目跳过，不报错。
- `trace` 是 tracing 后端的过滤提示：其 `project_id` / `task_id` 用于匹配 span 属性 `agentteams.project.id` / `agentteams.task.id`（worker entry span 已携带这两个属性）。本端点不构造后端 URL，tracing 后端是部署特定的。
- TaskMeta 只从项目所属 scope 读取（team 前缀优先，global 前缀仅 standalone 项目兜底）——与 `tasks_detail` 相同的禁止跨 scope 回退规则。

错误：`400`（task id 缺失/非法）、`404`（项目不存在——对限定读者隐藏存在性——或任务不在该项目图中）、`500`（存储读取失败）。

### `GET /api/v1/projects/{id}/tasks/{taskId}/artifact`

下载一个任务的一个产物，为 dashboard 和 console 插件补全「交付物 → 下载 → 审 → 接受」闭环。

可选查询参数：

| 参数 | 含义 |
|:--|:--|
| `path` | 要下载的产物路径。必须是任务**已声明**的产物之一——`result_path`、`spec_path` 或 `deliverables` 的某一项（均从 TaskMeta 读取）。省略时默认提供 `result_path`（已发布结果）。 |

不带 `?path=` 时下载任务的 `result_path`（已发布结果）。带 `?path=` 时，请求路径必须是任务已声明的产物之一——`result_path`、`spec_path`（任务规格书）或 `deliverables` 的某一项。随后路径通过严格白名单校验：必须位于 `shared/tasks/{taskId}/` 或 `shared/projects/{projectId}/` 之下，且不得包含 `..` 或以 `/` 开头。由于**白名单 + 已声明产物**双重校验，被攻破的 Worker 无法构造读取任意 MinIO 对象的路径，客户端也无法下载恰好位于任务目录但未声明的文件。

文件以 `Content-Disposition: attachment`（文件名为 basename，非 ASCII 名用 RFC 5987 `filename*=utf-8''...` 编码——中文文件名可正确下载）返回，`Content-Type` 由扩展名推断。

错误响应：

| 状态码 | 含义 |
|:--|:--|
| `400` | 缺少项目 id 或任务 id。 |
| `403` | 已认证但该角色完全不能读取项目（如 Worker）。 |
| `404` | 项目不存在 / 调用者不拥有它（隐藏存在性）/ 任务不在项目图中 / 任务没有已发布产物 / 请求路径不是已声明产物 / 产物文件缺失 / 产物路径被拒绝。 |
| `500` | K8s 或对象存储故障。 |

## 人类干预与生命周期端点（写 API）

上面的只读端点之外，还有让人类干预 agent 编排工作流的写端点。所有写入都经过
**代码级授权**：中间件拒绝跨团队写入（authorizer `requireSameTeam`），handler
解析出归属团队后还会显式调用 `checkProjectAccess`（因为中间件无法把 project
路径映射到团队）。每次写入都打上审计字段（`updated_by` / `updated_at`，给了
原因时还有 `pause_reason`），并应用 mtime 乐观锁——如果读取与写入之间 worker
推送了更新的 `meta.json`，写入以 `409` 失败而不是覆盖它。

### `POST /api/v1/projects`

创建项目（结构化，对齐 TeamHarness `create_project`）。admin/manager 可以
不带团队创建独立项目；team-leader 或 L2 人类必须传一个自己可访问的
`team_id`。

请求体：

```json
{
  "title": "新项目",
  "source": "matrix",
  "requester": "@luo:server",
  "team_id": "biz-team",
  "project_id": "可选自定义 id",
  "source_room_id": "!room:server"
}
```

省略 `project_id` 时自动生成；必须为纯 token（`[A-Za-z0-9._-]`）。响应
`201 Created`：

```json
{
  "project_id": "proj-2026-08-12T00:00:00Z",
  "title": "新项目",
  "status": "active",
  "team_id": "biz-team",
  "plan_type": "dag"
}
```

错误：`400` 缺 title/非法 id/受限调用方缺 team；`409` 项目已存在；
`403`/`404` 跨团队（拒绝 / 隐藏存在性）。

### `POST /api/v1/projects/{id}/pause`

把项目状态置为 `paused`。暂停会停止新任务派发（`ready_nodes` 返回空）但
**不会中断进行中任务**；它们的完成报告仍会到达（文档化行为——进行中任务不被
取消）。可选请求体 `{"reason": "..."}` 记录到 `pause_reason`。响应 `200`
返回更新后的工作流（`buildWorkflow`）。错误：`409` 已暂停/已完成；`404`
不存在或无权访问。

### `POST /api/v1/projects/{id}/resume`

把暂停的项目恢复为 `active`。响应 `200` 返回更新后的工作流。错误：`409`
未暂停；`404` 不存在或无权访问。

### `POST /api/v1/projects/{id}/replan`

替换项目的 DAG 计划。请求体携带新任务（可选 `tasks` 数组）：

```json
{
  "tasks": [
    {"taskId": "t1", "title": "步骤 1", "assignedTo": "@dev:server", "dependsOn": []},
    {"taskId": "t2", "title": "步骤 2", "dependsOn": ["t1"]}
  ]
}
```

字段按 TeamHarness `_normalize_task` 归一化（`taskId`/`task_id`、
`assignedTo`/`assigned_to`、`dependsOn`/`depends_on`，status 默认
`planned`，`pending` 映射为 `planned`）；已存在的 task id 在原始条目省略
字段时保留之前的 title/assignee/status。校验对齐 `_validate_task_graph`：
重复 id、未知依赖、依赖环都以 `400` 拒绝。前置条件（`409`）：`plan_type`
必须是 `dag`（loop 的重规划走 `record_loop_iteration`）、状态必须是
`active`、不能有 `in_progress`/`submitted` 任务。响应 `200` 返回更新后的
工作流。
重规划保留已有的 cancellation decision；同一个 task id 不能从已取消状态原地重开，
也不能先从计划删除再以同名任务添加。替代工作必须使用新的 task id。

### `POST /api/v1/projects/{id}/tasks/{taskId}/cancel`

取消单个任务：

```json
{
  "reason": "不再需要",
  "replacementTaskId": "replacement-01",
  "submissionId": "submission-123"
}
```

`reason` 必填，`replacementTaskId` 可选。`submissionId` 是条件必填字段：
TaskMeta 已有 `submission_id` 时，调用方必须传入完全相同的不透明值。缺失、
凭空构造或过期的 identity 会在 ProjectMeta/TaskMeta 发生任何写入前以 `409`
拒绝。

成功后，项目节点和 TaskMeta 都变成 `cancelled`；TaskMeta 持久化稳定的
`cancel_reason` / `replacement_task_id` / `cancelled_at`，并把已有 pending
continuation 解决为 `cancelled`，原 `delivery_id` 不变。相同取消请求可幂等
重试；reason、replacement 或 submission identity 不同则与既有决定冲突。
已经 `completed`、`revision` 或 `blocked` 的任务不能取消。响应 `200` 返回
更新后的工作流。错误：`400` 缺 reason 或 replacement task id 非法；`404` 任务不在项目里/TaskMeta
缺失；`409` 终态任务、submission fence 失败或取消决定冲突。

Controller 先把一份最小 cancellation decision envelope 写入项目节点，再写
TaskMeta。如果第二次写入失败，完全相同的请求可以补齐 TaskMeta/continuation；
reason、replacement 或 submission identity 不同的重试会被拒绝。

### `POST /api/v1/projects/{id}/complete`

把项目标记为已完成（终态）。所有任务必须处于终态
（completed/revision/blocked/cancelled——不能有 in_progress/submitted/
planned），否则 `409`。响应 `200` 返回更新后的工作流。

### 通知

写入成功后，Controller 用 `SendMessageAsAdmin` 向项目的 `source_room_id`
（回退到 `reply_route.target_session`）发送管理员消息，让房间里的 agent
无需轮询就能知道干预发生。尽力而为：房间未知或未配置 Matrix 时不发通知。

## 认证与授权

接受两种 bearer 令牌路径（复合认证器）：

1. **Kubernetes service account 令牌**（TokenReview）：admin / manager /
   worker。团队 leader（`team_leader` 角色的 worker）只能看自己团队的项目。
2. **Matrix 访问令牌**（L2 人类）：令牌用
   `GET /_matrix/client/v3/account/whoami` 验证；归属的 Matrix localpart
   匹配 `permissionLevel: 2`（Team）的 `Human` CR。人类的 `accessibleTeams`
   作为多团队范围——他们控制的所有团队聚合到单个列表/读取视图。非 L2 人类
   （permissionLevel 1 或 3）被拒绝。

授权矩阵：

| 调用方 | List | 获取工作流 | 写入（create/pause/resume/replan/cancel/complete） |
|:--|:--|:--|:--|
| admin / manager | 所有团队 | 任意项目 | 任意项目 |
| team-leader（SA） | 仅自己团队 | 仅自己团队 | 仅自己团队 |
| L2 人类（Matrix） | 所有 `accessibleTeams` | 任意可控团队 | 任意可控团队 |
| worker | 拒绝 | 拒绝 | 拒绝 |

## `agt` CLI

`agt get projects [name]` 包装两个端点：

```bash
agt get projects                      # 列出全部
agt get projects --team biz-team      # 按团队过滤
agt get projects demo-project-001     # 工作流详情
agt get projects demo-project-001 -o json
agt get projects demo-project-001 --include-tasks -o json
agt get projects demo-project-001 --mermaid   # 渲染 DAG 为 mermaid（含状态着色）
```

`--mermaid` 与 API 的 `?format=mermaid` 使用同一渲染器：next/ready 节点高亮，其余节点按状态着色。

节点级检视暂无专门 CLI 子命令，直接用 API：

```bash
curl -H "Authorization: Bearer $AGENTTEAMS_AUTH_TOKEN" \
  "$AGENTTEAMS_API_BASE/api/v1/projects/demo-project-001/tasks/t1"
```

CLI 原样转发配置的 bearer 令牌（`AGENTTEAMS_AUTH_TOKEN` 或
`AGENTTEAMS_AUTH_TOKEN_FILE`），所以 L2 人类也可以用——把任一变量指向自己的
Matrix 访问令牌即可，无需单独的 CLI 认证模式。

`--include-tasks` 必须和 `-o json` 一起使用；默认详情视图不渲染原始 TaskMeta 字段。

### `agt project`（写命令）

`agt project` 包装写端点，人类无需 raw curl 即可干预：

```bash
agt project create --title "新项目" --team biz-team --source matrix
agt project pause demo-project-001 --reason "客户评审"
agt project resume demo-project-001
agt project replan demo-project-001 --tasks tasks.json   # JSON 数组文件
agt project cancel demo-project-001 demo-project-001-01 \
  --reason "不再需要" --submission-id submission-123 --team biz-team
agt project complete demo-project-001
```

同样的 bearer 令牌转发适用（L2 人类用 Matrix 令牌）。

## Worker 知识库（工作区文件）端点

Controller 代理每个 worker 的 QwenPaw app（QwenPaw ≥ 2.1）的四个端点，
让 L2 人类与前端可以查看——并在 Human CR 允许时更新——worker 的知识库：
长期记忆文件 `MEMORY.md`、日记目录树 `memory/` 与沉淀知识目录树 `digest/`。

| 端点 | 含义 |
|:--|:--|
| `GET /api/v1/workers/{name}/workspace-files/tree` | 分页列出某个知识目录：`?path=`（必填，`memory` / `digest` 或其子路径），可选 `?cursor=`（不透明串）与 `?limit=`（1..500）。返回 `{directory, entries[], has_more, next_cursor}`。 |
| `GET /api/v1/workers/{name}/workspace-files/file-metadata` | `?path=`（必填，允许的知识库文件）：`{etag, modified_at, path, preview_kind, size}`。 |
| `GET /api/v1/workers/{name}/workspace-files/file-content` | `?path=`（必填）加可选 `?offset=`（≥0）与 `?limit=`（1..1048576）：有界 UTF-8 分块 `{content, eof, next_offset, truncated, etag, ...}`——`truncated` 为真时用 `offset=next_offset` 续读。 |
| `PUT /api/v1/workers/{name}/workspace-files/file-content` | 保存一个知识库文件：`?path=`（必填），body `{"content": "<文本>"}`（≤1 MiB，非空），`If-Match` 请求头（并发规则见下）。返回新的 `{etag, path, size}`。 |
| `GET /api/v1/workers/{name}/workspace-files/file-download` | 以附件形式流式下载一个知识库文件：`?path=`（必填）。透传上游 `Content-Disposition` / `Content-Length` / `ETag` 头。 |

- **范围**：与 `GET /api/v1/workers/{name}` 相同的 worker 读授权——团队
  leader / L2 人类只能看自己可访问团队内的 worker；未知或越权 worker 一律
  隐藏为 `404`。
- **写范围**：`PUT file-content` 对 admin/manager 全团队开放；L2 人类仅可写
  自己团队内的 worker，且 `Human.spec.workspaceFileAccess` 显式为
  `"readwrite"`（缺省/未设置即 `read` 只读——Controller 升级不会静默授予
  既有用户写权限；L1 把字段设为 `readwrite` 即授予）。团队 leader 在本 API
  上保持只读。跨团队写隐藏 worker 为 `404`（存在性不可探测）；范围内但无
  写权限的调用得到明确的 `403`。
- **并发（ETag）**：写之前代理先探测 `file-metadata`。文件已存在时
  `If-Match` 头必填（worker 会向自己的记忆文件自动追加，无条件覆盖即丢
  更新）；新建文件时不得携带。上游 ETag 不匹配原样透传 `409`——重新加载
  后重试。
- **写限制**：body 上限 1 MiB（与读分块上限一致），`content` 必须为非空
  字符串，每次成功写均由 controller 审计记录（worker、路径、调用者、字节
  数）。
- **`workspaceFileAccess`（Human CRD 字段）**：`read` | `readwrite`
  （缺省为 `read`，写权限为显式 opt-in）——L1 可逐用户授予/收回的团队知识
  库文件写权限。建 Human 时（`agt apply`）与 `PUT /api/v1/humans/{name}`
  均可设置（后者随 humans-update PR 把该字段纳入可更新集）。
- **路径白名单（知识边界）**：只放行 `MEMORY.md`、`memory/**` 与
  `digest/**`（读写同界）。工作区内其他一切位置——`SOUL.md`、`PROFILE.md`、`TODO.md`、
  `checkpoints/`、`skills/`，以及所有 dot 目录（`.copaw/agent.json` 承载
  worker 凭据）——在请求到达 worker 之前即被 `400` 拒绝。根目录按完整首段
  精确匹配，`memories/` 与 `memoryX/` 不构成 `memory/` 的前缀。文件根只能是
  单个顶层文件：`MEMORY.md` 可寻址，但 `MEMORY.md/foo` 被拒（它是文件而非
  目录——嵌套文件必须在 `memory/` 或 `digest/` 下）。
- **root 固定**：QwenPaw 的 `root=workspace` 参数（agent 自身存储根，相对
  于 `root=project` 即主绑定项目目录）由服务端固定，不属于客户端查询面。
- **仅 embedded 模式**：端点经共享 docker 网络代理 worker 的 qwenpaw app，
  地址解析与 checkpoint 端点相同（生效容器前缀 + system-wins 控制台端口）。
  kube 模式返回 `503`。
- **版本门（404 透传）**：worker 运行 QwenPaw < 2.1 时没有工作区文件
  路由，所有请求均为上游 `404` 原样透传。区分"worker 版本过旧"与"文件不
  存在"的方法：探测 `file-metadata?path=MEMORY.md`——该文件在每个已初始
  化的 QwenPaw 工作区中都存在，因此这里的 `404` 表示 worker 为 2.1 以下
  （或工作区未初始化），其余 `404` 即普通文件缺失。
- **Runtime 范围**：端点面向运行 QwenPaw app 的 worker（`qwenpaw`
  runtime）。其他 runtime 的 worker 没有 QwenPaw 工作区 API：该 runtime 的
  应用若在服务 console 端口，代理原样透传其响应（通常 `404`）；无人监听
  时返回 `502`。MEMORY.md 探测因此只对 QwenPaw worker 有意义。
- 转发为固定子路径（tree / file-metadata / file-content GET+PUT /
  file-download）+ 严格查询白名单——不是通用反向代理。multipart 的
  `file-upload` 端点与工作区 API 的其余面均不可达。

错误响应：

| 码 | 含义 |
|:--|:--|
| `400` | worker 名非法 / 不支持的子路径或查询参数 / 路径不在知识白名单内 / `limit` 或 `offset` 越界 /（写）`If-Match` 缺失或误用、body 超限或为空。 |
| `403` | （仅写）范围内但无写权限的调用者——只读人类或团队 leader。 |
| `404` | worker 不存在或不在调用方团队内（存在性隐藏）；或（透传）文件不存在——见上方版本门探测。 |
| `409` / `416` | （透传）读取期间或写入等待期间文件被修改（ETag 不匹配——重载重试）/ offset 超出文件末尾。 |
| `502` | worker app 不可达，或上游错误（状态码回显在 body 中）。 |
| `503` | kube 模式（无稳定的 worker pod DNS 可代理）。 |

## Worker 工具审批端点

每个 QwenPaw worker 的 agent profile 带一个工具执行安全级别（`agent.json` 的
`approval_level`），决定哪些工具调用自动执行、哪些暂停等人工审批。Controller
代理 worker 的 `/workspace/running-config` API 的最小读写面，L2 人类可管理
自己团队内 worker 的该级别：

| 端点 | 含义 |
|:--|:--|
| `GET /api/v1/workers/{name}/approval` | 当前级别：`{"approval_level": "AUTO"}`。 |
| `PUT /api/v1/workers/{name}/approval` | 设置级别。Body：`{"approval_level": "STRICT"}`。 |

- **档位**：`STRICT`（所有工具需审批）/ `SMART`（低风险工具自动放行）/
  `AUTO`（仅受管工具——上游默认）/ `OFF`（关闭守卫）。其他值在触碰 worker
  之前即被 `400` 拒绝（上游模型不校验取值，代理是校验边界）。
- **OFF 为提权档**：`approval_level=OFF` 会完全关闭 Tool Guard，属安全策略
  操作而非普通配置。默认 L2 人类设 `OFF` 得 `403`——只能在受管档位
  （`STRICT`/`SMART`/`AUTO`）间切换；`OFF` 需 L2 权限设计（#1220）定义的
  提权工具审批能力、由 admin 显式授予，该能力模型落地前 admin/manager 保留
  全档位。
- **写范围**：`PUT` 对 admin/manager 全团队开放；L2 人类仅可设自己团队内
  worker——跨团队隐藏为 `404`（存在性不可探测）。团队 leader 保持只读
  （`PUT` 得 `403`，与知识库写 API 同界）。
- **安全写**：上游 `PUT /workspace/running-config` 持久化*完整*运行配置对象，
  代理执行 GET→仅改 `approval_level`→PUT 回全量；其余字段原样往返。上游
  `409`（并发配置变更）透传，客户端以新 `GET` 重试。
- **仅 embedded 模式**：worker 寻址与 checkpoint 代理相同（有效容器前缀 +
  系统优先端口）。kube 模式返回 `503`。
- **降级**：无 running-config 路由的旧版 QwenPaw worker 原样透传上游
  `404`（版本门）。
- 每次成功变更记审计日志（worker、新级别、调用者、角色）。
