# wechat-agentteams-e2e · 微信群 → Agent Team → 回传 端到端演示

把上游 [AgentTeams](https://github.com/agentscope-ai/AgentTeams) 原生跑在 Docker 里，
用宿主机 Python 脚本模拟「微信群报障消息」推进入体系，再由容器内的 6 个职能 Agent
协作处理后把结果回传群里，并通过 **零 mock 双视图** 实时观测全过程。

> **数据真实性约束**：两个 HTML 视图渲染的每一条消息都来自宿主机桥接服务从容器内
> Matrix 服务器真实 `/sync` 出来的事件，没有任何 mock、没有硬编码对话。

---

## 目录

1. [架构概览](#1-架构概览)
2. [环境要求](#2-环境要求)
3. [步骤一：部署 AgentTeams Docker 栈](#3-步骤一部署-agentteams-docker-栈)
4. [步骤二：启动桥接服务](#4-步骤二启动桥接服务)
5. [步骤三：投喂组队指令](#5-步骤三投喂组队指令)
6. [步骤四：推送模拟微信群消息](#6-步骤四推送模拟微信群消息)
7. [步骤五：打开双视图观测](#7-步骤五打开双视图观测)
8. [关键文件说明](#8-关键文件说明)
9. [常见问题](#9-常见问题)

---

## 1. 架构概览

```
┌─────────────── 宿主机 ───────────────┐      ┌─────── Docker 容器 ──────┐
│                                        │      │  agentteams-controller     │
│  simulator/wechat_sim.py               │Matrix│   Tuwunel(Homeserver)      │
│    └─ 推送 [微信群消息] 包络           │C-S   │   Higress(Gateway)         │
│                                        │API   │   MinIO(FileStore)         │
│  bridge/server.py  ── /sync 事件 ──────┼─────►│   Element Web(:18088)      │
│    ├─ viewer/wechat.html  (视图二)     │      │                            │
│    ├─ viewer/agentflow.html(视图一)    │◄─────┤  agentteams-manager        │
│    └─ viewer/index.html   (总览/导航)  │      │   + worker-ticket-intake   │
│                                        │      │   + worker-triage-analyst  │
│  prompts/manager-team-prompt.md        │      │   + worker-resolution-exec │
│    └─ 组队指令 → admin→@manager DM     │      │   + worker-verify-closer   │
└────────────────────────────────────────┘      └────────────────────────────┘
```

| 组件 | 端口 | 说明 |
|------|------|------|
| Matrix 网关 (Higress) | `127.0.0.1:18080` | 宿主机↔容器唯一真实数据通道 |
| Matrix 直连 (Tuwunel) | `127.0.0.1:6167` | 备用直连（网关不可用时） |
| Element Web | `127.0.0.1:18088` | 容器原生 Matrix 聊天界面 |
| Higress 控制台 | `127.0.0.1:18001` | 网关管理 |
| Manager 控制台 | `127.0.0.1:18888` | Manager 运行状态 |
| 桥接服务 | `127.0.0.1:8770` | 自建双视图 + JSON API |

---

## 2. 环境要求

| 依赖 | 版本 | 说明 |
|------|------|------|
| Docker Desktop | 4.x+ | 需启用 WSL2 后端 |
| Python | 3.10+ | 仅标准库，零第三方依赖 |
| PowerShell | 7+ (推荐) | Windows 安装脚本需要 |
| 磁盘空间 | ~12 GB | 镜像占用 |

### ⚠️ Windows 11 24H2 用户：必须配置 WSL2 NAT 模式

Windows 11 24H2 默认启用 WSL2 mirrored networking，与 Docker Desktop 端口映射**已知不兼容**，
会导致 `18080` 等端口无法访问。请创建 `C:\Users\<用户名>\.wslconfig`：

```ini
[wsl2]
networkingMode=nat
localhostForwarding=true
memory=6GB
swap=2GB
processors=4

[experimental]
autoMemoryReclaim=gradual
```

保存后执行：`wsl --shutdown`，再重启 Docker Desktop。详见 [微软已知问题](https://juejin.cn/post/7593240931287547956)。

---

## 3. 步骤一：部署 AgentTeams Docker 栈

### 3.1 一键安装（推荐）

以 **PowerShell 7+** 在仓库根目录执行：

```powershell
cd D:\Develop\AgentTeams

# 设置环境变量（替换 <YOUR_API_KEY>）
$env:AGENTTEAMS_NON_INTERACTIVE = "1"
$env:AGENTTEAMS_LLM_PROVIDER     = "openai-compat"
$env:AGENTTEAMS_OPENAI_BASE_URL  = "https://api.stepfun.com/step_plan/v1"
$env:AGENTTEAMS_DEFAULT_MODEL    = "step-3.7-flash"
$env:AGENTTEAMS_LLM_API_KEY      = "<YOUR_API_KEY>"
$env:AGENTTEAMS_ADMIN_USER       = "admin"
$env:AGENTTEAMS_ADMIN_PASSWORD   = "AgentTeams2026"
$env:AGENTTEAMS_MOUNT_SOCKET     = "1"

# 安装（拉镜像 + 创建容器）
& ".\install\agentteams-install.ps1" manager
```

脚本会：
1. 拉取镜像（~5 个，总计约 12 GB）
2. 创建 `agentteams-controller` 容器（含 Tuwunel、Higress、MinIO、Element Web）
3. 自动创建 `agentteams-manager` 容器
4. 生成 `C:\Users\<用户名>\agentteams-manager.env` 配置文件

### 3.2 ⚠️ 修复 AppService 崩溃（已知 Issue）

`embedded:latest` 镜像默认启用 AppService 模式但安装脚本未生成对应令牌，会导致控制器 panic。
如果 `docker ps` 只有 controller、没有 manager，说明需要修复：

```powershell
# ① 关停 + 删旧控制器
docker rm -f agentteams-controller

# ② 用已打补丁的固定镜像重建（或手动加 -e APPSERVICE_ENABLED=false）
docker run -d --name agentteams-controller `
  --network agentteams-net `
  --network-alias matrix-local.agentteams.io `
  --network-alias aigw-local.agentteams.io `
  --network-alias fs-local.agentteams.io `
  -e AGENTTEAMS_LANGUAGE=zh `
  -e AGENTTEAMS_LLM_PROVIDER=openai-compat `
  -e AGENTTEAMS_DEFAULT_MODEL=step-3.7-flash `
  -e AGENTTEAMS_LLM_API_KEY="<YOUR_API_KEY>" `
  -e AGENTTEAMS_OPENAI_BASE_URL="https://api.stepfun.com/step_plan/v1" `
  -e AGENTTEAMS_ADMIN_USER=admin `
  -e AGENTTEAMS_ADMIN_PASSWORD=AgentTeams2026 `
  -e AGENTTEAMS_MANAGER_RUNTIME=copaw `
  -e AGENTTEAMS_DEFAULT_WORKER_RUNTIME=copaw `
  -e AGENTTEAMS_MANAGER_ENABLED=true `
  -e AGENTTEAMS_MATRIX_DOMAIN=matrix-local.agentteams.io:18080 `
  -e AGENTTEAMS_MATRIX_URL=http://127.0.0.1:6167 `
  -e AGENTTEAMS_MATRIX_E2EE=0 `
  -e AGENTTEAMS_MATRIX_APPSERVICE_ENABLED=false `
  -e TZ=Asia/Shanghai `
  -v //var/run/docker.sock:/var/run/docker.sock `
  --security-opt label=disable `
  -v agentteams-data:/data `
  -v C:\Users\$env:USERNAME\agentteams-manager:/root/agentteams-fs/agents/manager `
  -v C:\Users\$env:USERNAME:/host-share `
  -p 18080:8080 -p 18001:8001 -p 18088:8088 -p 6167:6167 `
  --restart unless-stopped `
  higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-embedded:latest

# ③ 等 120 秒后验证
Start-Sleep -Seconds 120
docker ps --format "table {{.Names}}\t{{.Status}}"
```

### 3.3 验证部署成功

```powershell
# 应看到 controller + manager 均为 Up
docker ps --format "table {{.Names}}\t{{.Status}}"

# Manager 模型确认为 step-3.7-flash（不应是 qwen3.5-plus）
docker exec agentteams-manager sh -c "env | grep DEFAULT_MODEL"

# 网关可达
curl.exe -s --max-time 10 -o NUL -w "%{http_code}\n" "http://127.0.0.1:18080/_matrix/client/versions"
```

---

## 4. 步骤二：启动桥接服务

```powershell
$PY = "python3"   # 或指定路径，如 C:\Users\...\.workbuddy\binaries\python\versions\3.13.12\python.exe
cd "D:\Develop\AgentTeams\wechat-agentteams-e2e\bridge"
& $PY server.py --port 8770
```

启动成功标志：
```
[bridge] admin login OK → @admin:matrix-local.agentteams.io:18080
[bridge] gateway room: !xxxxx → 微信群-IT服务台支持群
[bridge] sync started
```

此时可访问（数据全来自容器真实事件，零 mock）：
- 总览/导航：`http://127.0.0.1:8770/`
- 视图一（Agent 对话流）：`http://127.0.0.1:8770/agentflow.html`
- 视图二（模拟微信群）：`http://127.0.0.1:8770/wechat.html`

> 桥接服务会自动创建「微信群-IT服务台支持群」房间并邀请 @manager，后续模拟器推送的群消息在此房间由 Manager 接收。

---

## 5. 步骤三：投喂组队指令

组队指令走 **admin → @manager 私信（DM）**，让 Manager 自主拉起子 Agent 团队：

```powershell
$PY = "python3"
cd "D:\Develop\AgentTeams\wechat-agentteams-e2e\bridge"
& $PY feed_manager.py --wait 150
```

Manager 收到后会自动使用 `worker-management` 能力创建 4 个子 Agent：
- **ticket-intake** — 工单受理（解析包络、建工单、抽取实体）
- **triage-analyst** — 分诊分析（判断类别、严重度、影响面）
- **resolution-exec** — 处置执行（给出修复步骤/操作建议）
- **verify-closer** — 验证闭环（复核是否解决、是否需升级）

可在 Element Web (`http://127.0.0.1:18088`) 以 admin/AgentTeams2026 登录，直接看到 Manager 的组队过程。

> **自动推进规则**：本场景内所有子阶段默认自动推进（工单受理→分诊定级→处置执行→验证闭环），
> 仅不可逆操作（如永久禁用账户）会先征求管理员确认。详见 `prompts/manager-team-prompt.md`。

---

## 6. 步骤四：推送模拟微信群消息

```powershell
$PY = "python3"
cd "D:\Develop\AgentTeams\wechat-agentteams-e2e\simulator"

# 全场景（messages.json 中 6 条种子数据，间隔 60 秒）
& $PY wechat_sim.py --interval 60

# 指定条数
& $PY wechat_sim.py --interval 60 --count 3

# 单条 ad-hoc 消息
& $PY wechat_sim.py --text "我的 VPN 登不上了" --sender "王强(销售部)"
```

每条消息以 `[微信群消息]` 包络格式进入网关房间 → Manager 识别 → 拆解分派 → 各 Worker 协作 →
最终 `[群回复]` 回传群内。全过程实时渲染在 `http://127.0.0.1:8770/wechat.html`。

---

## 7. 步骤五：打开双视图观测

| 界面 | 地址 | 说明 |
|------|------|------|
| 总览/导航 | `http://127.0.0.1:8770/` | 事件流 + 状态 |
| **视图一** Agent 对话流 | `http://127.0.0.1:8770/agentflow.html` | 容器内 Manager 拆解、分派、Worker 执行的**内部对话流** |
| **视图二** 模拟微信群 | `http://127.0.0.1:8770/wechat.html` | 「群成员发消息 → Agent 处理中 → 服务台回复」的**外部闭环视图** |
| Element Web（原生） | `http://127.0.0.1:18088/` | 容器原生 Matrix 聊天客户端，用于对照验证零 mock |

> 登录 Element Web：用户名 `admin`，密码 `AgentTeams2026`，Homeserver URL `http://127.0.0.1:18080`

---

## 8. 关键文件说明

| 文件 | 作用 |
|------|------|
| `bridge/matrix_client.py` | 最小 Matrix Client-Server API 客户端（纯标准库），宿主机↔容器的唯一数据通道 |
| `bridge/server.py` | 桥接服务：登录 / 自动加房 / `/sync` / 事件日志 / 双视图 + JSON API |
| `bridge/feed_manager.py` | 将组队 prompt 通过 admin→@manager DM 投喂 |
| `simulator/wechat_sim.py` | 宿主机微信群消息模拟器（可配置间隔、场景、ad-hoc、watch-only） |
| `simulator/messages.json` | 种子场景（6 条账户与访问异常工单） |
| `prompts/manager-team-prompt.md` | 投喂给 Manager 的组队与协作指令（含自动推进规则） |
| `viewer/index.html` | 导航总览页（事件流 + 状态） |
| `viewer/agentflow.html` | 视图一：Agent 对话流（零 mock） |
| `viewer/wechat.html` | 视图二：模拟微信群（零 mock） |
| `_recreate_controller.sh` | 重建控制器容器脚本（便携） |

---

## 9. 常见问题

### Q1: `docker ps` 只有 controller、没有 manager
运行 `docker logs --tail 20 agentteams-controller`，如果看到 `AppService mode is enabled` panic，
按 [3.2 节](#32-修复-appservice-崩溃已知-issue) 修复。

### Q2: Worker 报 `Error: qwen3.5-plus`
Manager 的数据库里残留了旧模型名。最干净的做法：**清空数据卷从头安装**：
```powershell
docker rm -f $(docker ps -aq)
docker volume rm agentteams-data
docker network rm agentteams-net
# 再重跑 3.1 的安装命令
```
确保 `$env:AGENTTEAMS_DEFAULT_MODEL = "step-3.7-flash"`（或你自己的模型名）。

### Q3: 两个视图打开是空白的
确认桥接服务在跑（`http://127.0.0.1:8770/api/status` 应返回 JSON），
且 simulator 已推送过消息。如果还没有，先跑第 6 步。

### Q4: 端口 18080 始终 000 / 拒绝连接
① 确认 `.wslconfig` 设置了 `networkingMode=nat`（见第 2 节）。
② 确认 Docker Desktop 完全退出再重新打开（右键托盘 → Quit → 重新打开）。
③ `docker restart agentteams-controller` 等 60 秒后重试。

### Q5: 流程卡在某个阶段不动
本配置已启用**自动推进**（`prompts/manager-team-prompt.md` 第 34-38 行）。
如果仍然暂停，在群里或 Element Web 里发一条 `继续推进` 即可触发下一阶段。
