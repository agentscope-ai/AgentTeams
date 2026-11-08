# 人类工作区：文件列表

每个真人都有一个私有文件区——**工作区**——通过 controller API 的
`/api/v1/humans/{name}/files` 访问。只有本人、manager 和 admin 可以读取；
其他任何调用方（其他真人、team leader、worker）拿到的是与"该 Human 不
存在"完全一致的 `404`——API 不会向陌生人确认某个 Human 是否存在，也不会
暴露其文件布局。

## 根目录列表

`GET /api/v1/humans/{name}/files` 列出区域顶层：

```json
{"path":"files/","entries":[
  {"name":"b.txt","type":"file","size":6,"updatedAt":"2026-10-24T12:00:00Z"},
  {"name":"nested","type":"directory","updatedAt":"2026-10-24T12:00:00Z"}
]}
```

目录由嵌套 key 合成、不带尾斜杠；`.keep` 占位对象不显示；条目按名字排
序。文件大小直接来自存储列表——只有零字节条目会再 stat 一次，因为零字
节条目可能是删除墓碑（会被从列表中剔除）。

## 递归平铺列表

加 `?recursive=1` 可一次性拿到**整个区域所有层级**的文件，按 key 字典
序平铺：

```json
{"entries":[
  {"path":"files/a.txt","size":5,"etag":"d41d...","updatedAt":"2026-11-01T02:33:00Z"},
  {"path":"files/docs/nested/c.txt","size":1,"etag":"9e10...","updatedAt":"2026-11-01T02:33:00Z"}
],
"nextKey":"humans/alice/files/docs/nested/c.txt"}
```

- `path` 始终带 `files/` 前缀——这是 API 路径，不是原始存储 key。
- `limit` 限制的是**存储分页**大小，而不是条目数：默认 **1000**，上限
  **10000**；非整数或 `< 1` 一律 `400`。被隐藏的对象（删除墓碑、
  `.keep`、保留的 `.sync`/`.control`/`.trash` 层级）同样占用分页名额，
  所以单页可能返回少于 `limit` 条，甚至为零。
- `nextKey` 是本页最后一个对象的完整存储 key；列表走尽时为 `null`。要
  遍历整个区域，把它原样作为 `?cursor=<key>` 传回；cursor 必须位于该
  区域自己的前缀之内（来自其他区域的 cursor 会被 `400` 拒绝）。
- 已删除文件不会出现：软删留下零字节墓碑对象，只有零字节条目会被 stat
  检查墓碑标记。
- 响应字段刻意命名为 `nextKey`（而不是 `cursor` / `nextCursor`），用
  以与后续同步 API 的增量游标（`v1:<seq>`）区分。

完整走一遍：

```
GET /api/v1/humans/alice/files?recursive=1&limit=1000
→ 收下 entries；只要 nextKey != null：
GET /api/v1/humans/alice/files?recursive=1&limit=1000&cursor=<nextKey>
```
