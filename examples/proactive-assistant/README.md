# 主动助手摘要 POC（QwenPaw）

本分支验证一条最小链路：读取 Team Admin 私聊与角色摘要 → 定时采集 Nacos 官方发布记录 → 通过本地 QwenPaw 生成摘要 → `agt assistant send` → Controller 投递至原助手房间并返回 Matrix event ID → Dashboard 通知并跳转到该会话。

## 运行方式

在已经运行的隔离测试环境中，以当前 Team Leader 身份运行：

```sh
python /opt/agentteams/scripts/run-digest.py --team <Team-CR-name>
```

`--once` 表示等待下一次到期、成功投递一次后退出；默认持续轮询。调度状态保存在 Controller 的对象存储中，默认开启，间隔 86400 秒。测试可以用 `agt assistant remember --team <name> --file <json>` 将 `everySeconds` 临时设为 30 或更大，演示后恢复 86400。

```sh
agt assistant context --team <Team-CR-name>
agt assistant remember --team <Team-CR-name> --file context.json
agt assistant send --team <Team-CR-name> --file message.json
```

`context.json` 使用 GET 返回的当前 `owner`：

```json
{"owner":"<current-owner>","enabled":true,"everySeconds":86400,"summary":"用户关注方向和上次推送摘要"}
```

`message.json`：

```json
{"owner":"<current-owner>","requestId":"daily-20261121","text":"摘要正文及来源","summary":"留给接任者的上下文"}
```

房间和收件人由 Controller 根据 Team 当前状态解析，调用者不能指定。相同 requestId 必须保持正文和摘要不变；成功返回 `status: sent`、`roomId`、`messageId` 和 `sentAt`。这证明消息已被 Matrix 接受，不代表用户已读。

投递失败后用原 requestId 和内容重试。已存储回执的重试不会再次发送，并会补齐摘要与调度检查点。Runner 会先查待投递回执，再决定是否重新生成。公共 GitHub API 限流时回退到官方 release Atom feed；两者不可用时明确说明来源不可用，不捏造新闻。

## 已验证的边界

- Team 私聊显示 Team 名称，替换 Leader 复用 `status.leaderDMRoomID`；新团队的别名按 Team 生成。
- 摘要、调度与回执按 Team 的 Leader 角色及目标用户保存，不保存在某个 Worker 的原生会话文件中。
- Controller 校验当前 Leader、Worker 实例和 owner；旧执行者不能写入。Agent 的对象存储策略禁止直接写角色目录。
- 更换 Leader 后旧成员必须被确认移出私聊房间；无法确认时交接重试，不能报告成功。
- Dashboard 对真实 Matrix 提醒事件生成收件人隔离的通知；实时批次显示短提示，历史同步只补通知列表。点击进入原助手房间并标记该通知已读。
- 2026-11-21 本地验证了 QwenPaw 的真实模型生成、到期投递、重复请求去重、Leader 交接、旧 Leader 的 Controller 与 Matrix 拒绝，以及 Langfuse / Dashboard trace 回查。

## 本轮未覆盖

- 这是需要显式启动的 POC runner，尚未接入正式的启动、守护、自动恢复与运维调度；120 秒间隔仅用于本地演示。
- 采集器固定读取 Nacos 官方发布记录，尚不是通用的个性化检索服务。链接校验不能替代事实审核。
- 仅验证 Team Leader → 同一 Team Admin。Manager 入口当前只映射内置管理员房间；多 L1 Admin 的稳定私聊映射、Manager 实例撤权尚需补齐。
- 用户私聊摘要按用户隔离；Team Admin 更换时的团队公共业务摘要与个人兴趣分层迁移尚未实现。
- 接任者通过 Controller 摘要及 Matrix 消息重建上下文，不迁移 Harness 原生 session 文件。
- 单 Controller 内串行化与 Matrix txn 去重不能替代多副本的持久化角色租约。角色变更与正在执行的外部投递仍需跨组件原子交接；跨发送身份的崩溃窗口去重也需加强。
- 角色目录的读取隔离按本轮约定暂缓；目前强调 Controller 写入校验与旧执行者撤权。
- 本地复用已存在的基础镜像，仅替换本分支 Controller/CLI 和 POC runner；没有宣称所有运行时都完成适配。

## 构建

两个 Dockerfile 是本地 POC 的覆盖构建入口。`Dockerfile.controller` 基于已有 Controller 镜像更新两个二进制；正式构建也可使用仓库现有 Makefile（含 agent/plugin 资源准备）。`Dockerfile` 的 `embedded` 和 `qwenpaw` targets 分别覆盖 Controller 基础环境和 QwenPaw runtime。所有基础镜像须显式传入，使用独立 `local-YYYYmmddHHMMSS` 标签和独立网络、卷、端口。

```sh
docker build -f examples/proactive-assistant/Dockerfile.controller \
  --build-arg CONTROLLER_BASE=<existing-controller-image> \
  -t <poc-controller-image> .
docker build -f examples/proactive-assistant/Dockerfile --target qwenpaw \
  --build-arg CONTROLLER_IMAGE=<poc-controller-image> \
  --build-arg EMBEDDED_IMAGE=<existing-embedded-image> \
  --build-arg QWENPAW_IMAGE=<existing-qwenpaw-image> \
  -t <poc-qwenpaw-image> .
```
