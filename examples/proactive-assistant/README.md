# 主动助手：统一 Teams 协议

Refs https://github.com/agentscope-ai/AgentTeams/issues/1683

本实现基于 AT、Dashboard 和六种 Harness 的 main。Controller 管理角色工作区、用户上下文、定时调度和发送回执；Agent 通过 AT 的 Teams 消息／@ 唤起。Harness 不维护另一套业务上下文、定时器或私有 HTTP 唤醒入口。所有相关 PR 保持 Draft，暂不合并、不启用自动合并。

## 上下文与交接

- Team Leader 的共享业务摘要保存在 `teams/<Team-CR>/roles/leader/business.json`，私人关注方向和私聊摘要保存在该角色下面按 Matrix 身份与 Human UID 隔离的用户目录。
- Manager 复用主线的 `Human.status.managerRooms`，分别管理每位当前 L1 Admin 的私聊；系统管理员使用原 Manager 私聊。降权、删除或身份改变后，旧路由不能继续使用。
- 更换 Leader／Manager 执行者后，同一用户的会话 ID 和角色上下文保持连续，旧 owner 写入、投递和入站事件校验失效。
- 更换 Team Admin 时保留团队业务摘要，为新用户选择独立私聊；不得将旧用户的兴趣、私聊摘要或历史搬给新 Admin。Team status 记录私聊的 Human 绑定；普通 Leader 更换不改变该绑定。
- context 与 business 使用存储层条件写入。每次编辑须提交读取到的 `owner` 和 `revision`；409 表示需要重读，不能覆盖保存。Agent 不能直接写角色资料前缀。细粒度对象读取隔离按讨论暂缓，API 仍按当前用户隔离。

## 公共流程

1. Controller 每分钟检查当前就绪角色与已注册用户，默认启用每日摘要。关闭或调整间隔后，旧排队事件会重新校验配置。
2. 公共 outbox 根据角色、用户、执行者与上次发送检查点去重，发送 `assistant_digest` Teams 控制消息和 `m.mentions`。
3. 六种 Harness 从 Controller 的鉴权回执取得规范输入，核对当前 owner、用户会话、启用状态和原生事件 ID。正文声明不授予权限；Controller 不可用时保留重试。
4. Agent 读取公共上下文、最近消息，必要时分页读取同一用户的历史；维护私人摘要与共享业务摘要，并按权限核实来源。
5. Agent 使用 `agt assistant send` 提交结果。Controller 以存储 CAS 预留发送回执并取得有界租约，使用固定 Matrix transaction ID 投递。
6. 真实 Matrix messageId 才表示送达。回执与上下文检查点可在重试时补齐；发送过程不会覆盖用户并发修改的设置／关注方向。已开始的发送可能在关闭操作后返回，其结果仍须如实记录。
7. 已有 Task 准备边界识别该控制事件，但不执行 Task begin/get/accept/complete，不创建假 Task。Dashboard 展示助手通知、未读和原会话跳转；首次历史同步不重复弹实时提醒。

## 公共命令

```sh
agt assistant context --team <Team-CR>
agt assistant conversation --team <Team-CR>
agt assistant conversation --team <Team-CR> --before <previous-end>
agt assistant business --team <Team-CR>
agt assistant remember --team <Team-CR> --file context.json
agt assistant remember-business --team <Team-CR> --file business.json
agt assistant receipt <request-id> --team <Team-CR>
agt assistant send --team <Team-CR> --file message.json
```

Manager 使用 `--manager <Manager-CR> --user <Human-CR>`。用户界面不接受任意接收者，由登录用户身份确定自己的会话。公共 HTTP 接口仅提供数据／操作，推理触发载体始终为 Teams。

更新 context／business 时必须提供精确读取到的 revision，首次不存在的版本为空字符串。用户设置请求可以只带 enabled／everySeconds，不会清空 summary。

```json
{"owner":"<current-owner>","revision":"<read-revision>","enabled":true,"everySeconds":86400,"summary":"用户私人关注与对话摘要"}
```

```json
{"owner":"<current-owner>","requestId":"<scheduled-request-id>","contextRevision":"<revision-read-before-inference>","text":"提醒正文及核实来源","summary":"更新后的私人摘要"}
```

同一 requestId 的正文、摘要和 contextRevision 必须保持不变，owner 使用当前执行者。已保存 sent 回执不会重发。若未知发送结果跨越了执行者更换，Controller 先查找具有相同原发送者、requestId、接收者和正文的真实消息；证据不足时返回冲突并保留回执，不能换发送身份盲目重发。没有证据不能声称已发送或已读。

## 构建与验收

六种 Harness 的公共脚本、Task 语义和 agt 均来自各仓库锁定的 Core 源码。agt 构建时检查 assistant 命令，防止新脚本配上旧 CLI。Core 的 Harness 子模块锁定经过验证的适配提交，独立仓库保留各自构建入口。

本目录 Dockerfile 只用于隔离验证镜像。使用明确的基础镜像、独立时间标签、容器名、端口、网络和卷；不能把手工覆盖算成首次干净安装通过。完整源码／载体测试、真实模型链路、Tracing 与 Dashboard 交互证据记录在总 Issue；单运行时真实执行不等于六运行时完整模型或故障恢复均验收通过。

正式合并前须重新确认最终主线、全部相关 CI、镜像依赖以及真实运行证据。无发布版本授权时不指定未来版本，也不自动合并。
