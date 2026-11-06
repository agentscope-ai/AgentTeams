# 运维说明：人均文件区（列表）

人均文件区读取/列表面（`GET /api/v1/humans/{name}/files`）的运维要点。
设计细节见 [`docs/design/human-file-area.md`](../../design/human-file-area.md)。

## 递归列表的分页契约

`?recursive=1` 分页的是**存储**，不是 API 响应：

- 每页一次后端列表调用（S3 `ListObjectsV2`，带 `Prefix` + `StartAfter` +
  `MaxKeys`），因此无论区域多大，单页成本恒定。
- `limit` 是页大小：默认 **1000**，上限 **10000**；非整数或 `< 1` → `400`。
- 隐藏对象会占页槽：删除墓碑、`.keep` 占位对象，以及保留顶层 `.sync/`、
  `.control/`、`.trash/` 下的任何对象都不会返回给客户端，但仍占用存储侧
  的页槽。因此一页可能返回少于 `limit` 条，甚至 0 条——客户端必须一直
  走到 `nextKey` 为 `null`，**不能**因为"短页"就推断"没有更多文件"。
- `nextKey` 是该页最后一个对象的**完整存储键**，例如
  `humans/alice/files/docs/b.txt`。控制器只校验返回的游标是否以调用者
  自己的区域前缀（`humans/<username>/files/`）开头，然后原样回传给存储；
  跨区域游标 → `400`。

## 墓碑是空对象

软删除**不会**把对象从存储里移除：它在原键上写一个**空（零字节）对象**，
内容类型标为 `application/vnd.agentteams.tombstone`。由此带来：

- 列表 API 不返回内容类型，因此 handler 无法仅凭响应头过滤墓碑。它只对
  **列表已报告大小为 0** 的条目做 stat，若 stat 显示墓碑内容类型则剔除该
  条目；非空文件永远不会因此被 stat。
- stat 失败时保留该条目（fail-open，与单层列表一致）。
- 墓碑体恒为空，因此大小恒为 0，必定走上面的 stat 路径。
- 绕过本 API 被直接删除的对象不会留下标记；若非空，会作为普通条目出现。

## `files/` 之外的保留层级

- `humans/<user>/.keep`（以及 `.trash/.keep`）由
  `Deployer.EnsureHumanStorage` 创建，并从列表中隐藏。
- `humans/<user>/.control/quota.json` 是跨副本的配额账本：一个 JSON 字节
  计数器，写入者在 PUT 前先向其预留。它位于 `files/` 与 `.trash/` 之外，
  不计入存活 `usedBytes`，且对两种列表都隐藏。`.control` 同时是保留的
  API 路径（经文件 API 写入 → `400`）。
- `.sync/` 为文件同步阶段保留，同样隐藏。

## 降级列表（不上报大小）

若后端只提供 `mc ls --json` 兜底（没有 S3 API），列表不会上报大小；
handler 随后会对每个文件条目做 stat，即递归列表上线前的行为。递归列表
仍然可用，但每页会对每个零大小条目 stat 一次——在该后端下，区域较大时
预期延迟更高。
