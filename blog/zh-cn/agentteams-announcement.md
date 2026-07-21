# HiClaw：OpenClaw 超进化，更安全更易用，5 分钟打造出一人公司

> 发布日期：2026 年 2 月 27 日

---

## 你是否也曾这样？

作为 OpenClaw 的深度用户，我深刻体会到它的强大——一个 Agent 就能帮你写代码、查邮件、操作 GitHub。但当你开始做更复杂的项目时，问题就来了：

**安全问题让人睡不着**：每个 Agent 都要配置自己的 API Key，GitHub PAT、LLM Key 散落各处。2026 年 1 月的 CVE-2026-25253 漏洞让我意识到，这种 "self-hackable" 架构在便利的同时也带来了风险。

**一个 Agent 承担太多角色**：让它做前端，又做后端，还要写文档。`skills/` 目录越来越乱，`MEMORY.md` 里混杂各种记忆，每次加载都要塞一大堆无关上下文。

**想指挥多个 Agent 协作，但没有好工具**：手动配置、手动分配任务、手动同步进度……你想专注于业务决策，而不是当 AI 的"保姆"。

**移动端体验一言难尽**：想在手机上指挥 Agent 干活，却发现飞书、钉钉的机器人接入流程要几天甚至几周。

如果你有同感，那 **HiClaw** 就是为而生的。

---

## HiClaw 是什么？

**HiClaw = OpenClaw 超进化**

核心创新是引入 **Manager Agent** 角色——你的 "AI 管家"。它不直接干活，而是帮你管理一批 Worker Agent。

```
┌─────────────────────────────────────────────────────┐
│                   你的本地环境                       │
│  ┌───────────────────────────────────────────────┐ │
│  │           Manager Agent (AI 管家)             │ │
│  │                    ↓ 管理                     │ │
│  │    Worker Alice    Worker Bob    Worker ...   │ │
│  │    (前端开发)       (后端开发)                  │ │
│  └───────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
         ↑
    你（真人管理员）
    只需做决策，不用当保姆
```

---

## 技术架构：OpenClaw 的"器官移植"

OpenClaw 的设计就像一个完整的生物体：有**大脑**（LLM）、**中枢神经系统**（pi-mono）、**眼睛和嘴**（各种 Channel）。但原生设计中，大脑和感知器官都是"外接"的——你需要自己去配置 LLM Provider、去对接各种消息渠道。

HiClaw 做了一次"器官移植"手术，把这些外接组件变成**内置器官**：

```
┌────────────────────────────────────────────────────────────────────┐
│                         HiClaw All-in-One                          │
│  ┌──────────────────────────────────────────────────────────────┐ │
│  │                     OpenClaw (pi-mono)                       │ │
│  │                      中枢神经系统                             │ │
│  └──────────────────────────────────────────────────────────────┘ │
│           ↑                              ↑                        │
│  ┌────────────────┐              ┌────────────────┐               │
│  │  Higress AI    │              │   Tuwunel      │               │
│  │  Gateway       │              │   Matrix       │               │
│  │  (大脑接入)     │              │   Server       │               │
│  │                │              │   (感知器官)    │               │
│  │  灵活切换       │              │                │               │
│  │  LLM供应商      │              │  Element Web   │               │
│  │  和模型         │              │  Element/Xbox    │               │
│  └────────────────┘              │  (自带客户端)   │               │
│                                  └────────────────┘               │
└────────────────────────────────────────────────────────────────────┘
```

### LLM 接入：Higress AI Gateway

**大脑不再外接，而是通过 AI Gateway 灵活管理**：

- **一个入口，多种模型**：在 Higress 控制台即可切换阿里云通义、OpenAI、Claude 等不同供应商
- **凭证集中管理**：API Key 只需要配置一次，所有 Agent 共享
- **按需授权**：每个 Worker 只获得调用权限，永远接触不到真实的 API Key

### 通信接入：内置 Matrix Server

**感知器官也变成内置的**：

- **Tuwunel Matrix Server**：开箱即用的消息服务器，无需任何配置
- **自带 Element Web 客户端**：浏览器打开就能对话
- **移动端友好**：支持 Element、FluffyChat 等全平台客户端
- **零对接成本**：不需要申请飞书/钉钉机器人，不需要等待审批

> 💡 换个比喻：OpenClaw 原生就像一台组装电脑，你需要自己买显卡（LLM）、显示器（Channel）然后装驱动。HiClaw 则是一台开箱即用的笔记本，所有外设都集成好了，开机就能干活。

---

## Multi-Agent 系统：你的 AI 管家贾维斯

在组件封装的基础上，HiClaw 还实现了一套**开箱即用的 Multi-Agent 系统**——Manager Agent 管理 Worker Agent，就像钢铁侠的管家 **贾维斯** 一样。

### 按需启用，两种模式

这套系统是**按需启用**的，你可以灵活选择：

**模式一：直接对话 Manager**
- 简单任务直接告诉 Manager，它自己处理
- 适合快速问答、简单操作

**模式二：Manager 分派 Worker**
- 复杂任务由 Manager 拆解，分配给专业 Worker
- 每个 Worker 有独立的 Skills 和 Memory
- 技能和记忆**完全隔离**，不会互相污染

### 协作架构：Supervisor + Swarm 的融合

从 Manager-Worker 的角度看，这是一个 **Supervisor 架构**：Manager 作为中心节点协调所有 Worker。但因为基于 Matrix 群聊房间协作，它同时也具备了 **Swarm（蜂群）架构** 的特点。

**共享上下文，无需重复沟通**：每个 Agent 都能看到群聊房间里的完整上下文。Alice 说"我在做登录页面"，Bob 自动知道前端在做什么，API 设计时可以配合。

**防惊群设计**：Agent 只有被 @ 的时候才会触发 LLM 调用，不会因为无关消息被唤醒，成本可控。

**中间产物不污染上下文**：文件交换、代码片段等大量协作通过底层的 **MinIO 共享文件系统** 完成，不会发到群聊里导致上下文膨胀。

### 安全设计：Manager 能管理，但不能泄密

原生 OpenClaw 架构下，每个 Agent 都需要持有真实的 API Key，一旦被攻击或意外输出，凭证就可能泄露。

HiClaw 的解决方案是 **Worker 永远不持有真实凭证**：

```
┌──────────────┐      ┌──────────────────┐      ┌─────────────┐
│   Worker     │─────►│  Higress AI      │─────►│  LLM API    │
│   (只持有    │      │  Gateway         │      │  GitHub API │
│   Consumer   │      │  (凭证集中管理)   │      │  ...        │
│   Token)     │      │                  │      │             │
└──────────────┘      └──────────────────┘      └─────────────┘
```

- Worker 只持有一个 Consumer Token（类似于"工牌"）
- 真实的 API Key、GitHub PAT 等凭证存储在 AI Gateway
- **即使 Worker 被攻击，攻击者也拿不到真实凭证**

Manager 的安全设计同样严格：它知道 Worker 要做什么任务，但不知道 API Key、GitHub PAT。Manager 的职责是"管理和协调"，不直接执行文件读写、代码编写。

| 维度 | OpenClaw 原生 | HiClaw |
|------|--------------|--------|
| 凭证持有 | 每个 Agent 自己持有 | Worker 只持有 Consumer Token |
| 泄漏途径 | Agent 可直接输出凭证 | Manager 无法访问真实凭证 |
| 攻击面 | 每个 Agent 都是入口 | 只有 Manager 需要防护 |

### Human in the Loop：全程透明，随时干预

和 OpenClaw 原生的 Sub Agent 系统相比，HiClaw 的 Multi-Agent 系统不仅更易用，而且**更透明**：

```
┌─────────────────────────────────────────────────────────────┐
│                  Matrix 项目群聊房间                        │
│                                                             │
│  你: 实现一个登录页面                                        │
│                                                             │
│  Manager: 收到，我来分派...                                  │
│           → @alice 前端页面                                  │
│           → @bob 后端 API                                    │
│                                                             │
│  Alice: 正在实现登录组件...                                  │
│  Bob: API 接口已定义好...                                    │
│                                                             │
│  你: @bob 等下，密码规则改成至少8位                          │  ← 随时干预
│                                                             │
│  Bob: 好的，已修改...                                        │
│  Alice: 收到，前端校验也更新了                               │
│                                                             │
│  Manager: 任务完成，请 Review                                │
└─────────────────────────────────────────────────────────────┘
```

**核心优势**：
- **全程可见**：所有 Agent 的协作过程都在 Matrix 群聊里
- **随时介入**：发现问题可以直接 @某个 Agent 修正
- **自然交互**：就像在微信群里和一群同事协作

### Manager 的核心能力

| 能力 | 说明 |
|------|------|
| **Worker 生命周期管理** | "帮我创建一个前端 Worker" → 自动完成配置、技能分配 |
| **自动分派任务** | 你说目标，Manager 拆解并分配给合适的 Worker |
| **Heartbeat 自动监工** | 定期检查 Worker 状态，发现卡住自动提醒你 |
| **项目群自动拉起** | 为项目创建 Matrix Room，邀请相关人员 |

### Worker 技能扩展：放心用的开放 Skills 生态

OpenClaw 有一个很棒的开放技能生态 [skills.sh](https://skills.sh)，社区里已经有 80,000+ 个技能可以一键安装——写 Higress WASM 插件、做 PR Review、生成 Changelog……

但是，**在原生 OpenClaw 里你可能不敢轻易用它**。毕竟一个公开技能库里的 SKILL.md 你没法完全审查，如果某个技能诱导 Agent 输出凭证、执行危险命令，后果不堪设想——因为 Agent 本身就持有你的 API Key 和各种凭证。

**HiClaw 里则完全不同**。每个 Worker 运行在完全隔离的容器里，而且设计上就不持有任何真实凭证：

```
Worker 能看到什么？
✅ 任务文件、代码仓库、它自己的工作目录
✅ Consumer Token（类似"门禁卡"，只能调用 AI API）
❌ 看不到你的 LLM API Key
❌ 看不到 GitHub PAT
❌ 看不到任何加密凭证
```

即使某个技能试图窃取凭证，它也什么都拿不到。这让 Worker **可以放心地从公开技能库里按需获取能力**。

HiClaw 给 Worker 内置了 `find-skills` 技能，当 Worker 遇到需要特定领域知识的任务时，它会主动搜索并安装合适的技能：

```
Manager 派发任务: "开发一个 Higress WASM Go 插件"
                  ↓
Worker 发现自己缺少工具
                  ↓
skills find higress wasm
  → alibaba/higress@higress-wasm-go-plugin  (3.2K installs)
                  ↓
skills add alibaba/higress@higress-wasm-go-plugin -g -y
                  ↓
技能安装完成，Worker 获得完整的插件开发脚手架和工作流
```

**如果你有顾虑，或者有内部技能需要积累，HiClaw 也支持切换到自建私有技能库**——安装 Manager 时选择 Manual 模式，在 Skills Registry 配置项填入你的私有地址即可。如果安装时没有配置，也可以随时告诉 Manager 在创建 Worker 时使用指定的技能库地址。

只要你的私有库实现了和 skills.sh 相同的 API，Worker 就会无缝切换到内部搜索。两种场景下，Worker 的使用方式完全一致。

### 移动端体验

HiClaw 内置 Matrix 服务器，支持多种客户端：

- **一键安装后直接用**：无需配置飞书/钉钉机器人
- **手机上随时指挥**：下载 Matrix 客户端（Element、FluffyChat 等）
- **消息实时推送**：不会折叠到"服务号"
- **所有对话可见**：你、Manager、Worker 在同一个 Room，全程透明

> 💡 **移动端**：支持 Element、FluffyChat 等主流 Matrix 客户端，iOS/Android/Web 全平台覆盖。

---

## 5 分钟快速开始

### 第一步：安装

**macOS / Linux：**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows（PowerShell 7+）：**

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

> ⚠️ Windows 用户需要先安装 **PowerShell 7+** 和 **Docker Desktop**。

安装脚本特点：
- **跨平台**：Mac / Linux 用 bash，Windows 用 PowerShell，体验一致
- **智能检测**：根据时区自动选择最近的镜像仓库
- **Docker 封装**：所有组件跑在容器里，屏蔽操作系统差异
- **最少配置**：只需要一个 LLM API Key，其他都是可选的

安装完成后，你会看到：

```
=== HiClaw Manager Started! ===

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ★ Open the following URL in your browser to start:                           ★
                                                                                
    http://127.0.0.1:18088/#/login
                                                                                
  Login with:                                                                   
    Username: admin
    Password: [自动生成的密码]
                                                                                
  After login, start chatting with the Manager!                                 
    Tell it: "Create a Worker named alice for frontend dev"                     
    The Manager will handle everything automatically.                           
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

> 💡 **无需配置 hosts**：`*-local.hiclaw.io` 会自动解析到 `127.0.0.1`，开箱即用！

### 第二步：登录开始对话

1. 打开浏览器访问安装时显示的 URL（如 `http://127.0.0.1:18088`）
2. 输入安装时显示的用户名和密码登录
3. 你会看到一个 "Manager" 的对话

### 第三步：创建你的第一个 Worker

```
你: 帮我创建一个 Worker，名字叫 alice，负责前端开发

Manager: 好的，正在创建...
         Worker alice 已创建，Room: !xxx:matrix-local.hiclaw.io
         你可以在 "Worker: Alice" Room 里直接给 alice 分配任务
```

### 第四步：分配任务

```
你: @alice 请帮我实现一个简单的登录页面，使用 React

Alice: 好的，我正在处理...
       [几分钟后] 完成了！代码已提交到 GitHub，PR 链接: https://github.com/xxx/pull/1
```

### 第五步：在手机上查看进度

1. 下载 Matrix 客户端（Element、FluffyChat 等，支持 iOS/Android/全平台）
2. 登录时选择"其他服务器"，填入你的 Matrix 服务器地址
3. 随时查看 Worker 的进度，随时干预

---

## 一人公司实战：一个人，一支队伍

假设你想做一个 SaaS 产品——从 idea 到上线到增长，传统上你需要产品、设计、开发、测试、运营……但现在你可以这样：

```
你: 帮我创建 4 个 Worker：
    - alex: 产品经理
    - sam: 全栈开发
    - taylor: 内容运营
    - jordan: 数据分析

Manager: 好的，4 个 Worker 已创建，各自有独立的技能和记忆。

你: 启动项目：AI 写作助手 MVP，下周要上线

Manager: 收到，我来规划项目...

         [创建项目群，邀请所有 Worker 加入]
         [起草项目计划]
         
         计划如下，请确认：
         
         Phase 1: 产品定义
         - task-001: 竞品调研 & PRD（alex）
         
         Phase 2: 开发
         - task-002: 技术选型 & 架构（sam，依赖 task-001）
         - task-003: 核心功能开发（sam，依赖 task-002）
         - task-004: 代码审查（alex，依赖 task-003）
         
         Phase 3: 上线准备
         - task-005: 发布素材准备（taylor，依赖 task-001）
         
         确认后我开始分配任务。

你: 确认

Manager: [更新项目状态为 active]
         
         @alex 你有新任务 [task-001]：竞品调研 & PRD
         目标：输出 PRD 文档，明确 MVP 功能边界
         规格：~/hiclaw-fs/shared/tasks/task-001/spec.md
         完成后在项目群 @我 汇报

[2 小时后]

alex: @manager task-001 完成
      PRD 已输出：核心功能包括 AI 续写、多模型切换、历史记录
      结果：~/hiclaw-fs/shared/tasks/task-001/result.md

Manager: [读取 result.md，状态：SUCCESS]
         [更新项目计划，task-001 标记完成]
         
         @sam 你有新任务 [task-002]：技术选型 & 架构
         PRD 参考：~/hiclaw-fs/shared/tasks/task-001/result.md
         
         @taylor 你有新任务 [task-005]：发布素材准备
         产品定位参考：~/hiclaw-fs/shared/tasks/task-001/result.md

[4 小时后]

sam: @manager task-002 完成
     技术栈：Next.js + Vercel + Supabase
     预计 8 小时完成开发

Manager: [更新计划，分配 task-003]
         
         @sam 继续开发核心功能 [task-003]

[8 小时后]

sam: @manager task-003 完成，已部署到 Vercel
     Demo 地址：https://xxx.vercel.app

Manager: [更新计划，分配 task-004]
         
         @alex 请验收 [task-004]：产品验收
         测试地址：https://xxx.vercel.app
         对照 PRD 验收功能完整性

[验收中发现问题]

alex: @manager task-004 需要修改
      问题：多模型切换没有引导，用户不知道怎么选
      建议：增加模型对比说明页
      结果：~/hiclaw-fs/shared/tasks/task-004/result.md
      状态：REVISION_NEEDED → 返回 task-003

Manager: [读取 result.md，状态：REVISION_NEEDED]
         [创建修订任务 task-006]
         
         @sam task-004 验收发现问题，需要修改
         
         修改任务 [task-006]：增加模型选择引导页
         反馈详情：~/hiclaw-fs/shared/tasks/task-004/result.md
         完成后 @我 汇报

[修改完成]

sam: @manager task-006 完成，引导页已加上

Manager: [task-006 完成，task-004 重新验收通过]
         [所有开发任务完成]
         
         @你 MVP 开发完成！
         - 代码已部署：https://xxx.vercel.app
         - 产品验收通过
         - 发布素材已就绪
         
         项目计划：~/hiclaw-fs/shared/projects/proj-xxx/plan.md

你: 准备上线。@taylor 明天 Product Hunt 发布

taylor: 收到，已排期。素材已准备完毕。

[上线当天 — Manager 自动检测进度]

Manager: @你 Product Hunt 发布提醒
         当前排名：第 3 名
         Upvotes: 423
         评论数: 87
         
         @jordan 请配置数据埋点

jordan: 收到，开始配置 GA4 + 自定义事件...

[数据就绪后]

jordan: @manager 埋点配置完成
        看板地址：https://analytics.google.com/xxx
        
        首日数据：
        - 注册用户：1,247
        - 次日留存：34%
        - AI 续写使用率：78%
        - 多模型切换使用率：23%

Manager: @你 项目「AI 写作助手 MVP」上线数据日报
         
         核心指标：
         - 首日注册：1,247
         - 次日留存：34%
         - 功能使用：续写 78%，切换 23%
         
         洞察：多模型切换使用率偏低
         建议：@alex 分析原因，优化引导流程

[就这样，Manager 贯穿始终：规划 → 分配 → 监控 → 协调 → 汇报]
```

**Manager 做了什么？**

| 环节 | Manager 的作用 |
|------|---------------|
| **项目规划** | 把目标拆解成任务，识别依赖关系 |
| **任务分配** | @mention 指派任务，提供上下文 |
| **进度监控** | 收到汇报后更新计划，触发下一步 |
| **处理问题** | 验收不通过 → 自动创建修订任务 |
| **状态同步** | 关键节点主动汇报给你 |
| **风险预警** | 发现数据异常，主动建议优化 |

**你只需要做决策，剩下的交给 Manager。**

---

## 开源地址

- **GitHub**: https://github.com/higress-group/hiclaw
- **文档**: https://github.com/higress-group/hiclaw/tree/main/docs
- **社区**: 加入我们的 Discord / 钉钉群 / 微信群

---

## 写在最后

HiClaw 是对 OpenClaw 的一次"超进化"——不是推翻，而是增强。

我们保留了 OpenClaw 的核心理念（自然语言对话、Skills 生态、MCP 工具），同时解决了安全和易用性上的痛点。

如果你是：
- **独立开发者**：一个人想干一个团队的活
- **OpenClaw 深度用户**：想要更安全、更易用的体验
- **一人公司创始人**：需要 AI 员工帮你分担工作

HiClaw 就是为你准备的。

**现在就开始：**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

*HiClaw 是开源项目，基于 Apache 2.0 协议。如果你觉得有用，欢迎 Star ⭐ 和贡献代码！*
