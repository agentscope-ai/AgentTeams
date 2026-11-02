# Coding Runs API（只读编码任务转录可见性）

委托编码任务——Manager / 团队 Leader / 授权 Worker 经 `coding-cli` skill（#1342）在 worker 内一次性运行的编码 CLI（Qwen Code / OpenCode / …）——把完整 stdout（推理、工具调用、文件改动、退出原因）`tee` 到 per-task 日志：

```
/root/agentteams-fs/shared/tasks/{task-id}/workspace/coding-cli-logs/run-{YYYYMMDD}-{HHMMSS}.log
```

并经 `mc mirror` 推到对象存储。**转录已存在、已在 MinIO**，但此前无读取路径。Controller 因此**只读**地服务编码任务转录，仿 worker chats 代理（`worker-chats-api.md`）——同一套薄、字节透明、`404-not-403` 纪律，应用于编码任务而非 agent 对话。controller 不挂载 worker 文件系统，故经 `oss.StorageClient`（mc）读取，与 project task 端点一致——这也是**embedded 与 K8s 模式都可用**的原因（不代理 per-worker console 端口）。

**设计上只读。** 无 create / delete / tail-stream 路由：run 已结束（one-shot），CLI 退出后日志不可变。

## 路由

| 路由 | 存储读取 | 返回 |
|------|----------|------|
| `GET /api/v1/coding-runs` | 对调用者可见的 `…/coding-cli-logs/` 前缀 `ListObjects` | `list[{task_id, run, team}]`——调用者可读的 runs（200 条上限） |
| `GET /api/v1/coding-runs/{taskID}/runs/{run}` | `GetObject …/coding-cli-logs/{run}` | 原始 `.log`，字节透明（`text/plain`） |

- **数据布局（dual-prefix）**，经 `oss.StorageClient` 读：
  - standalone task：`shared/tasks/{taskID}/workspace/coding-cli-logs/run-*.log`
  - team task：`teams/{team}/shared/tasks/{taskID}/workspace/coding-cli-logs/run-*.log`
- **路径段在存储读取前校验**：`taskID` 须过 `isSafeTaskID`（字母数字，无首字符 `.`/`_`/`-`，无 `/`），`run` 须匹配 `run-\d{8}-\d{6}\.log`——两者都不能携带 path traversal 或指向 run 转录之外的文件。失败返回 `400`。
- **只读**（`text/plain`，无 body 上限——转录可能很大；project task artifact 路由同样姿态）。

## 授权

两层，AgentTeams 标准拆分（authorizer 放行*动作*，handler 执行*作用域*）。

### Authorizer 层（`internal/auth/authorizer.go`）

`GET /api/v1/coding-runs` 注册在 `ActionGet, "coding-runs"` 下。

| 角色 | `get` / `list` coding-runs |
|------|---------------------------|
| L1 admin | 放行（全量） |
| manager SA | 放行（全量，同 project 端点） |
| team leader | **放行**（handler 收窄到本团队） |
| L2 human | **放行**（handler 收窄到 `accessibleTeams`） |
| worker | **拒绝**（worker 跑 CLI，不经 API 读转录） |

`coding-runs` case 在 `authorizeTeamLeader` / `authorizeHuman` 里显式（读放行，其余拒绝）。**没有它**，L2 调用者落到 authorizer 的 `default: deny`，根本到不了 handler——handler 的 team-scope 逻辑成死代码，真实 L2 请求会 `403`。

### Handler 层（team scope + W4，`CodingRunsHandler`）

- **L1 / manager** 读所有前缀：所有 team **加** global standalone `shared/tasks/` 前缀。
- **L2 team leader / human** 只读其所属各 team 的前缀（`visibleTaskPrefixes` 收窄 dual-prefix 集）。**standalone task 无 team 归属，仅 L1 可见**——L2 调用者既不能 list 也不能读 `shared/tasks/…` run。
- **W4 反探测**：调用者不可见的 run 返回**与缺失 run 相同的 `404`**——跨 team 的 task/run 存在性不可探测（永不 `403`）。
- 可选 `?team=` 查询把 detail 读收窄到一个 team 前缀（消歧）；它是过滤器，**不是授权**——上面的 team-scope 边界仍生效。

## 状态映射

| 情形 | 响应 |
|------|------|
| `taskID` / `run` 校验失败 | `400` |
| run 不存在，或不在调用者作用域（W4） | `404` `{"message":"coding run not found"}` |
| 调用者无可读 run | `200 []`（空 list，永不 `null`） |
| authorizer 拒绝（worker） | `403` |
| list 中途某前缀存储读错误 | 跳过该前缀（其余 list 仍返回） |

## 示例

```bash
# L1 admin 列出所有编码 run（team + standalone）
curl -s http://127.0.0.1:8090/api/v1/coding-runs -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 [{"task_id":"t1","run":"run-20261012-120000.log","team":"alpha-team"}, …]

# team leader（alpha）只看 alpha-team runs；standalone + 其他 team 缺席
curl -s http://127.0.0.1:8090/api/v1/coding-runs -H "Authorization: Bearer $ALPHA_LEAD_TOKEN"
# → 200 [{"task_id":"t1","run":"run-20261012-120000.log","team":"alpha-team"}]

# 打开一个 run 的转录（字节透明）
curl -s http://127.0.0.1:8090/api/v1/coding-runs/t1/runs/run-20261012-120000.log \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 "[run-coding-cli] cli=qwen workspace=…\n<完整 CLI 转录>"

# L2 调用者跨 team / standalone 读 → 404，与缺失不可区分
curl -s http://127.0.0.1:8090/api/v1/coding-runs/t9/runs/run-20261012-120100.log \
  -H "Authorization: Bearer $ALPHA_LEAD_TOKEN"
# → 404 {"message":"coding run not found"}

# traversal 尝试 → 400（存储读取前拒绝）
curl -s "http://127.0.0.1:8090/api/v1/coding-runs/../etc/runs/run-20261012-120000.log" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 400 {"message":"invalid task id"}
```

## 说明

- **数据源 = #1342**（`coding-cli` skill 的 `run-coding-cli.sh`）。#1342 合并前日志文件尚不存在，端点返回空 list / `404`——graceful，不报错。本面只是读取面，不需要 #1342 代码。
- **仅一次性 run**（#1342 当前形态）。serve 常驻实时 session 属 QwenCode runtime 线（#1357，deferred），另一个更晚的面。
- **转录敏感度**：日志字节透明，是 CLI 打印了什么——若委托 agent 把密钥写进 prompt 或工具回显了，这里就会出现。访问受上面 team scope 约束，但「密钥不进 prompt」的责任在委托 agent（coding skill 声明的同一契约）；对任意 CLI stdout 的 server-side redaction 不在 v1 范围。
- **dashboard 消费面**是独立后续（仿 worker-chats dashboard 面板消费 `worker-chats-api.md` 的模式）。

## 测试

- `internal/server/coding_runs_test.go`——基于 `mcLikeOSS`（mc ls 目录视图 fake）+ fake K8s client：
  - list：`TestListCodingRuns_AdminSeesTeamAndStandalone`、`TestListCodingRuns_L2SeesOnlyOwnTeam`、`TestListCodingRuns_SkipsNonRunFiles`
  - detail：`TestGetCodingRun_AdminReadsByteTransparent`、`TestGetCodingRun_L2OwnTeam`、`TestGetCodingRun_L2OtherTeam`（W4 404）、`TestGetCodingRun_L2CannotSeeStandalone`（仅 L1）、`TestGetCodingRun_InvalidTaskID`（traversal 400）、`TestGetCodingRun_InvalidRun`（400）、`TestGetCodingRun_NotFound`（404）
- `internal/auth/authorizer_test.go`——`TestAuthorizer_CodingRunsScoped` 固化 authorizer 边界：L2 human + team leader `get`/`list` 放行（让 handler 作用域生效），worker 拒绝。
