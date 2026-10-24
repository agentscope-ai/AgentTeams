# Qwen Code Bridge

把 **Qwen Code CLI** 接进 **QwenPaw**：派单、状态、唤醒、原生 Web 控制台、直聊、审批等级映射——一个本地插件，**与 AgentTeams 无关**（裸 QwenPaw、AgentTeams 里的 QwenPaw 都能用）。

- plugin id：`qwen-code-bridge`　后端包：`qwen_code_bridge`　HTTP 前缀：`/api/qwen-code-bridge`
- 版本矩阵：QwenPaw **2.2.x**（旧插件框架）与 **#7565 新框架**双绿；Qwen Code CLI **0.26.0**（web shell 按同版本重建，见 `web-shell/`）
- 测试：`78 passed + 1 skipped`（旧框架）/ `78 passed`（#7565）；发布门 `scripts/verify-bridge.sh`（双框架 + 4 项 live 检查 + evidence JSON）

## 目录

```
qwen-code-bridge/
├─ plugin.json              # 清单（entry.backend=plugin.py, entry.frontend=frontend/index.js）
├─ plugin.py                # 注册：http router / middleware / hooks / slash / prompt section
├─ qwen_code_bridge/
│  ├─ registry.py           # CLI 规格（qwen-code / opencode / qoder）
│  ├─ service.py            # probe / settings / install（本地 CLI）
│  ├─ router.py             # 管理面 + 派单面 + chat-meta + /web 反代/静态
│  ├─ dispatch.py           # spawn / watchdog / 认领旗 / 唤醒 / 审批映射
│  ├─ chats.py              # 侧栏分组与每单会话（原生 chats API）
│  ├─ slash.py              # /qcode（短路 agent，逐会话续聊）
│  ├─ runtime_hook.py       # PRE_DISPATCH 钩子：直聊/运行卡行为
│  ├─ prompt.py             # 内置提示词段（全英文）
│  ├─ millforge.py          # 千锻 M5 通知分层 + T5 coding-runs 投影
│  ├─ state.py / endpoints.py / web.py
├─ frontend/index.js        # 控制台路由 /qwen-code + core.chat 包装 + 主题同步
├─ web-shell/               # 重建 base 版 Qwen Code web shell 的补丁与构建脚本
├─ scripts/verify-bridge.sh # 发布门
└─ tests/                   # 76+1 / 77（见上）
```

## 安装

### A. 本地/任意 QwenPaw（推荐，热加载）

```bash
# 1) 放到 plugins 目录
cp -a qwen-code-bridge "${QWENPAW_WORKING_DIR:-/app/working}/plugins/qwen-code-bridge"
# 2) 官方安装接口（热加载，无需重启；loaded=true 即生效）
curl -X POST http://127.0.0.1:8088/api/plugins/install \
  -H 'Content-Type: application/json' -H "X-Agent-Id: $AGENT" \
  -d '{"source":"<绝对路径>/plugins/qwen-code-bridge","force":true}'
# 3) 验收
curl -s http://127.0.0.1:8088/api/plugins -H "X-Agent-Id: $AGENT" | grep qwen-code-bridge
```

> 注意：**拷贝目录本身不算安装**——必须走 `POST /api/plugins/install`（这是踩过的坑：拷目录不热加载，列表里看不见）。

### B. AgentTeams 部署（内置插件）

插件本身零耦合；AgentTeams 只是把它列为 builtin：

1. 插件目录位于 `AgentTeams/plugins/qwen-code-bridge/`（当前在分支 `agentteams/coding-cli-plugin`，ahead 36 未推）
2. 镜像：`qwenpaw/Dockerfile` 增加
   `COPY plugins/qwen-code-bridge/ /opt/agentteams/plugins/qwen-code-bridge/`，
   并把该路径加入 `install-builtin-qwenpaw-plugins.py` 的参数列表
3. worker 启动：`qwenpaw_worker/worker.py::_prepare_default_plugins` 已调用
   `_prepare_builtin_plugin("qwen-code-bridge", builtin_root / "qwen-code-bridge")`
4. 重建 worker 镜像 → 新 worker 启动即带插件（`/api/plugins` 显示 loaded）


### C. ZIP 安装（分发用）

```bash
curl -X POST 'http://127.0.0.1:8088/api/plugins/upload?force=true' \
  -H "X-Agent-Id: $AGENT" -F 'file=@qwen-code-bridge-0.1.0.zip;type=application/zip'
```

两个产物（`dist/`）：

| 包 | 大小 | 内容 | 适用 |
|---|---|---|---|
| `qwen-code-bridge-0.1.0.zip` | ~74 KB | 插件代码/文档/测试/构建脚本（无 web-shell 产物） | 已自行构建 web shell 的部署 |
| `qwen-code-bridge-0.1.0-full.zip` | ~5.6 MB | 同上 + `web-shell/dist`（409 文件，原生控制台开箱即用） | 绝大多数部署 |

> 上传安装已实测：`loaded=true`，插件目录内含 `web-shell/dist`；`/web/` 200、客户端路由刷新回退正常。

### 3) 原生 Web 控制台（可选但推荐）

QwenPaw 单端口内跑**原生 Qwen Code web**需要一份按 base 重建的 shell：

```bash
sh web-shell/build.sh <SC/qwen-code 源码目录> [qwenpaw working 目录]
```

产物写入 `<state>/web-shell/dist`（约 35 MB，不入库）；插件检测到它即静态伺服，否则回退为 daemon 反代。上游 Qwen Code 升级时重建一次。

## 使用

| 入口 | 行为 |
|---|---|
| 左栏 **QwenCode** 组（置顶） | 每次派单一行：`<task> · <cli> · <state>`（终态改名） |
| 点任一派单会话 | 页面**整屏**变成原生 Qwen Code 控制台（无插件自建标题栏；`route.wrap("core.chat")` + 全屏 iframe） |
| `QwenCode · 直接对话` | 原生控制台（整屏）；**不需要前缀**，直接打字即由 Qwen Code 回答（PRE_DISPATCH 短路，逐会话续聊） |
| 任意聊天里 `/qcode <prompt>` | 零模型成本的直连回答（`--session-id` 首轮 / `-r` 续聊） |
| 审批等级 | QwenPaw 等级 → CLI tier：`STRICT→plan / SMART→auto-edit / AUTO→auto / OFF→yolo`（请求/env 可覆盖） |
| **审批映射（worker 代审批）** | `default/auto-edit/auto` 走 `--input-format stream-json`；插件作为宿主回答 `can_use_tool`：工作区内写/测试 → 自动放行；出界写/凭据/危险命令 → 自动拒绝；未知 → 升级人工且 **fail-closed**；每次决策写 receipts |
| 门铃（可选） | `room_bell`：终态 3 行摘要投到房间（需部署侧有消息频道） |

派单 API：

```bash
curl -X POST http://127.0.0.1:8088/api/qwen-code-bridge/dispatch \
  -H 'Content-Type: application/json' -H "X-Agent-Id: $AGENT" \
  -d '{"task":"t1","prompt":"...","cwd":"/path","cli":"qwen","timeout":600,
       "origin":{"session_id":"<你的会话>","user_id":"<你>","channel":"console"},
       "wake":{"agent_id":"<agent>"}}'
```

- `origin` = **源会话三元组**（终态唤醒原样回放；与频道无关）
- 无 gates：并发/冲突闸已按 10/22 定案删除（模型侧排队）
- 观测：`GET /dispatch/{task}`（状态）、`/dispatch/{task}/log`（日志）、`/dispatch/{task}/progress`（phase/工具/测试/停滞）、`/dispatch?format=table`（一张状态表）、`<state>/dispatch/<task>.receipts.jsonl`（审批审计）、`<state>/web-proxy.log`（反代诊断，200 KB 上限）

### 审批映射（worker 代审批）

`default / auto-edit / auto` 三种 tier 下，插件以 `--input-format stream-json` 启动 CLI，
先发 `initialize` 控制请求（CLI 启用控制系统的前提），随后对每个 `can_use_tool` 请求按
**确定性策略**答复：

| 风险类 | 判据 | 决策 |
|---|---|---|
| R0 只读 | 读/搜索类工具 | 自动 allow |
| R1 工作区内写 | `write_file/edit/...` 路径 ⊆ 本次运行根 | 自动 allow |
| R2 工作区内执行 | 测试/构建/lint/只读 git 命令 | 自动 allow |
| R3 出界/危险 | 路径越根、凭据文件、`git push`、`rm -rf /`、`sudo`、外网命令 | 自动 deny |
| R4 未知/模糊 | 不在策略表、`ask_user_question`、MCP 工具 | escalate（默认 deny，`requires_human`，记 receipt） |

- 超时/异常一律 **deny**（fail-closed）；`yolo` 不做审批，`plan` 只读
- 终态 3 行摘要（源会话）：状态/轮次/工具/审批数 + 失败原因或"需人工 N 条"
- 测试：`tests/test_approval_policy.py`（19 例）+ `tests/test_progress.py`（7 例）

## 维护

- **发布门**：`scripts/verify-bridge.sh`（每版本必跑；证据 JSON 落 `<state>/evidence/`）
- **web shell**：Qwen Code 版本升级 → 重跑 `web-shell/build.sh`（补丁见 `web-shell/README.md`）
- **状态目录**：`<workspace>/.qwenpaw/qwen-code-bridge/`（任务/会话映射/证据/重建产物）
- 已知边界：控制台内会话列表依赖“重建 base 版 shell”；未重建时只能用直聊/`/qcode`，见 `web.py` 的回退逻辑

## 排障：控制台白屏/崩溃（10/22 实修）

**现象**：打开 QwenPaw console 的任意聊天即崩溃，`ReferenceError: bridgeChatLookup is not defined`（blob 里的 `BridgeChatRoute`）。

**根因**：插件前端用 `H.route.wrap("qwen-code-bridge", "core.chat", ...)` **全局包裹**聊天路由；wrapper 里调用了未定义的 `bridgeChatLookup` → 每次聊天渲染都抛错 → React 根错误边界把整个 console 打掉。**删插件/重建容器都不够**：已打开的浏览器标签仍持有旧 blob 与注册的 wrapper，容器重建不会刷新浏览器。

**修复（0.1.0+）**：
- `bridgeChatLookup` 已实现（读 `qwenpaw-agent-storage.state.selectedAgent` → `X-Agent-Id` + auth token；404/网络错误返回 null）
- wrapper 全程 fail-open（try/catch + `.catch` → 渲染宿主聊天）
- **应急开关**：`localStorage["qwen-code-bridge-disable-wrap"]="1"` 后刷新，跳过 `core.chat` 包裹（不必卸载插件）
- 回归：`tests/test_frontend_wrapper.py` 用 node 执行 wrapper（断言不抛错、回退宿主组件、lookup 被调用）

**另一个常见现象**：原生控制台显示「守护进程尚未就绪，正在重试…」。两个已实证原因：
1. **qwen CLI 在容器重建后丢失**（上游镜像不带 CLI）→ `bash tools/qwen-setup/reinstall.sh`；
2. **`qwen serve` 拒绝嵌套 `--workspace`**（例如同时传 `/tmp` 与 `/tmp/qcb-persist`）→ 进程秒退。插件已过滤嵌套 workspace；若再遇到看 `state/web-serve.log`（spawn 的 stderr 已落盘）。

**持久化边界（QwenPaw 只持久化三个目录）**：`/app/working`（qwenpaw-data）、`/app/working.backups`、`/app/working.secret`；其余路径重建即丢。因此：
- **CLI 二进制**：`npm_config_prefix=/app/working/.qwen-cli npm install -g @qwen-code/qwen-code@0.26.0`（161 MB，落在 qwenpaw-data）；`/usr/local/bin/qwen` 是指向它的 symlink（启动钩子会重建），插件配置 `qwen_cli` 用绝对路径，不依赖 PATH。
- **CLI 数据**：`cli_home=/app/working/.cli-home`（HOME 覆盖；会话在 `$HOME/.qwen/projects/...`，settings/serve-token 首次自动播种；git 用 `GIT_CONFIG_GLOBAL` 保留）。
- 应急/升级：`tools/qwen-setup/reinstall.sh` 已改为持久化安装（同上路径 + symlink + settings/token）；派生镜像 `qwenpaw:node` 仍可预装 CLI（更快）。

**左栏 QwenCode 组**：`state/config.json` 写 `{"chat_sync": false}`（或环境变量 `QWEN_CODE_BRIDGE_CHAT_SYNC=0`）即**不再建组/运行卡**，`POST /sidebar` 变 no-op；`/q-code` 路由与 `/dispatch` 状态 API 不受影响。

**恢复步骤**：① 硬刷新/关闭标签重开（清 site data 若仍白屏）② 选择 agent `AgentTeamGuide` ③ 打开派单会话 → 原生 Qwen Code 控制台；若仍异常，用上面的 localStorage 开关。

## 排障

- **直接访问客户端路由并刷新出现 `{"detail":"Not Found"}`**：已修（10/22）；页面内已无“新标签打开”入口。浏览器导航到客户端路由（`/session/<id>`、`/threads`、`/settings`…）时，代理会在两种情况下都回退到 SPA：① 有重建产物 → 我们的 index；② 没有重建产物 → daemon 自己的 index（前缀改写）。若仍复现：硬刷新（Ctrl+Shift+R）并给出完整 URL。
- **控制台里会话列表为空**：说明跑的是**未重建**的 shell（`<state>/web-shell/dist` 不存在）——按 `web-shell/README.md` 构建一次，或从别的实例拷贝 dist。
- **`/web/settings` 返回 `Unknown coding CLI: web`**：已修（`/web/*` 路由前置，不再被 `/{cli_id}/settings` 吞）。
