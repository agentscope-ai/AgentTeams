# HiClaw 1.0.6：企业级 MCP Server 管理 — 凭证零暴露，工具全接入

> 发布日期：2026年3月14日

---

## 凭证安全的困境

如果你在生产环境中运行 AI Agent，可能面临过这样的两难选择：

**"我想让 Agent 用 GitHub，但不想把我的 PAT 给它"** — 一个泄露的 token 就可能导致仓库被攻击。

**"我需要 Worker 调用内部 API，但那些密钥太敏感了"** — 计费系统、数据库、支付网关的 API key... 给 Agent 用风险太大。

**"不同 Worker 需要不同权限，但管理起来简直是噩梦"** — 前端 Worker 应该有生产数据库的访问权限吗？大概不应该。但怎么强制执行？

在 1.0.6 版本中，我们带来了全面的解决方案：**基于 Higress AI Gateway + mcporter 的企业级 MCP Server 管理**。

---

## 什么是 MCP？为什么它很重要？

**MCP (Model Context Protocol)** 是一个开放标准，用于将 API 暴露为 AI Agent 可以发现和调用的工具。可以把它理解为"给 AI Agent 用的 OpenAPI"—— 你不需要手动给 Agent 讲解每个 API 端点，只需要定义一次 MCP 工具，任何兼容 MCP 的 Agent 都可以立即使用。

MCP 的美妙之处在于它将**工具定义**和**凭证管理**分离。工具的 schema 说明了"这个 API 做什么，需要什么参数"，但不会说"这是 API key"。这种分离是企业级安全部署的基础。

---

## 介绍 mcporter：通用 MCP CLI

在深入了解 HiClaw 的集成之前，让我们先介绍 [**mcporter**](https://github.com/steipete/mcporter) —— 由 [Peter Steinberger](https://github.com/steipete)（OpenClaw 作者）开发的强大 MCP 工具包。

mcporter 是一个 TypeScript 运行时、CLI 和代码生成工具包。核心能力：

- **零配置发现**：自动发现 Cursor、Claude Code、Codex、Windsurf、VS Code 中配置的 MCP 服务器
- **友好的 CLI**：用 `mcporter call server.tool key=value` 调用任意 MCP 工具
- **类型安全**：生成带有完整类型推断的 TypeScript 客户端
- **一键 CLI 生成**：将任意 MCP 服务器转换为独立的 CLI 工具

```bash
# 列出所有已配置的 MCP 服务器
mcporter list

# 查看某个服务器的工具和完整参数 schema
mcporter list github --schema

# 调用工具
mcporter call github.search_repositories query="hiclaw" limit=5
```

在 HiClaw 1.0.6 中，Manager 和 Worker 都使用 mcporter 与 MCP 服务器交互 —— 但通过 Higress AI Gateway 实现了关键的安全增强。

---

## MCP 与 SKILLS：互补而非替代

在了解 HiClaw 的 MCP 集成架构前，有必要先澄清 **MCP 和 SKILLS 的关系**。

### HiClaw 的开放技能生态

HiClaw 通过对接 [skills.sh](https://skills.sh) 支持**开放的 SKILLS 市场**，并且支持对接 **Nacos 的企业自建 SKILLS 市场**。SKILLS 是面向实际场景的、可迭代的能力包装：

- **场景导向**：将多个原子工具组合成完整的业务流程
- **持续演进**：基于实战经验不断优化和改进
- **知识沉淀**：包含最佳实践、错误处理、参数说明

### MCP 在生态中的定位

MCP 并不是要取代 SKILLS，而是作为 **SKILLS 生态中的补充**，用于快速将已有的 API 转换成标准的 Agent 可使用的工具。MCP 的核心价值在于：

- **明确的约束和规范**：对工具有着更严格的定义和类型约束
- **权限治理体系**：可以复用 MCP 的认证鉴权能力（MCP Server 粒度的权限管理，企业版支持工具粒度）
- **批量转换能力**：尤其在企业场景下，基于 Higress 的 MCP 网关能力可以无痛地将大量存量 API 批量转换成 MCP 工具，实现精细化管理

### mcporter 的桥接作用

通过 mcporter 这样的 CLI 工具，HiClaw 实现了 **MCP 工具的重新编排和组织**，形成可迭代的 SKILL：

```
MCP 工具（原子能力）
    ↓ mcporter 编排
SKILL（场景化能力包）
    ↓ 实战使用
SKILL 迭代优化
```

### SKILL + MCP = 1+1 > 2

两者的最佳实践是：

- **SKILL 负责场景演进**：贴合实际业务场景，不断迭代技能的组合逻辑和最佳实践
- **MCP 负责细粒度权限管控**：贴合业务能力做好 MCP Server 和工具的权限治理与凭证管理

这种互补关系实现了 **SKILL + MCP 1+1 大于 2 的效果**：企业既能享受开放 SKILLS 市场的丰富能力，又能通过 MCP 网关对企业内部 API 实现安全、精细的权限管控。

---

## 架构：一切如何运作

当你想为 Worker 添加一个新的 API 工具时，完整流程如下：

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              你（人类）                                       │
│                                                                              │
│  "添加一个股票指数 API：GET https://api.finance.com/v1/index?symbol={symbol}"│
│  "通过 X-API-Key header 认证，这是我的 key：sk_xxx"                          │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           MANAGER CLAW                                       │
│                                                                              │
│  1. 根据你的描述生成 MCP Server YAML 配置                                    │
│  2. 运行 setup-mcp-server.sh stock-index "sk_xxx" --yaml-file /tmp/stock.yaml │
│  3. 用 mcporter 验证：mcporter call stock-index.get_index symbol=000001.SH  │
│  4. 通知 Worker："新 MCP 服务器 'stock-index' 已就绪"                        │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        HIGRESS AI GATEWAY                                    │
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  MCP Server: stock-index-mcp-server                                  │   │
│  │  ├─ 真实凭证: sk_xxx（安全存储，永不暴露）                            │   │
│  │  ├─ 工具: get_index(symbol: string) → 股票指数数据                   │   │
│  │  └─ 授权消费者: manager, worker-alice, worker-bob                    │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  向 Worker 签发临时 consumer token                                           │
│  Token 只能调用已授权的 MCP 服务器                                            │
│  真实 API key 永远不会离开网关                                                │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           WORKER CLAW                                        │
│                                                                              │
│  1. 收到 Manager 的通知                                                       │
│  2. 从 MinIO 拉取最新的 mcporter 配置                                         │
│  3. 发现工具：mcporter list stock-index --schema                             │
│  4. 测试工具：mcporter call stock-index.get_index symbol=000001.SH           │
│  5. 基于理解生成 SKILL.md                                                     │
│  6. 后续任务中即可使用该工具！                                                 │
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Worker 的视角：                                                      │   │
│  │  ├─ 拥有：Consumer token（就像一张"工牌"）                            │   │
│  │  ├─ 可以：通过网关调用 stock-index.get_index                         │   │
│  │  └─ 不可以：看到真实的 API key sk_xxx                                 │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

配置完成后，Manager 可以在 **Higress 控制台**查看 MCP Server 下的工具列表，并进行进一步的权限管理：

![Higress MCP 工具管理界面](https://img.alicdn.com/imgextra/i3/O1CN01armHyk1Lc9XTGz7ZX_!!6000000001319-2-tps-1452-849.png)

通过控制台，你可以：
- 查看 MCP Server 包含的所有工具
- 为每个 Consumer（Worker）配置 MCP Server 级别的访问权限
- 监控工具调用情况和性能指标
- 动态调整权限策略，无需重启服务

> **权限管理说明**：
> - **Higress 开源版**：支持 **MCP Server 粒度**的权限管理（例如：Worker A 可以访问整个 `stock-index` MCP Server）
> - **Higress 企业版**：支持 **工具粒度**的权限管理（例如：Worker A 只能调用 `stock-index.get_index`，但不能调用 `stock-index.update_index`）
> 
> HiClaw 1.0.6 基于开源版实现了 MCP Server 级别的安全隔离，已能满足大部分企业场景的权限管控需求。

**核心安全原则：Worker 永远看不到真实凭证。**

即使 Worker 被完全攻破，攻击者也只能获得一个 consumer token：
- 只能调用你授权的特定 MCP 服务器
- 可以被 Manager 瞬间吊销
- 不包含任何可复用的凭证信息

---

## 端到端示例：添加自定义 API

让我们走一遍真实场景。你有一个内部计费 API，想让 Worker 查询客户数据。

### 步骤 1：向 Manager 描述 API

在你的 Matrix 房间，告诉 Manager：

```
你：我想添加计费 API 作为 MCP 工具。
    端点：GET https://billing.internal.company.com/api/v1/customers/{customer_id}
    认证：Authorization header 里的 Bearer token
    这是 token：Bearer eyJhbGciOiJSUzI1NiIs...
```

### 步骤 2：Manager 完成繁重工作

Manager 会：

1. **生成 YAML 配置**：

```yaml
server:
  name: billing-mcp-server
  config:
    accessToken: ""  # Manager 在这里替换你的真实 token
tools:
- name: get_customer
  description: "通过 ID 获取客户详情"
  args:
  - name: customer_id
    description: "客户 ID（如 CUST-12345）"
    type: string
    required: true
  requestTemplate:
    url: "https://billing.internal.company.com/api/v1/customers/{{.args.customer_id}}"
    method: GET
    headers:
    - key: Authorization
      value: "Bearer {{.config.accessToken}}"
```

2. **部署到 Higress MCP Gateway**：
```bash
setup-mcp-server.sh billing "eyJhbGciOiJSUzI1NiIs..." --yaml-file /tmp/mcp-billing.yaml
```

3. **用 mcporter 验证**：
```bash
mcporter list billing --schema
mcporter call billing.get_customer customer_id=CUST-12345
```

4. **通知相关 Worker**：
```
Manager：@alice @bob 新 MCP 服务器 `billing` 已配置完成，包含工具：get_customer。
        请使用 file-sync skill 拉取更新后的配置，然后使用 mcporter skill 发现并学习新工具。
```

### 步骤 3：Worker 自动配置

Worker Alice：

```bash
# 从 MinIO 拉取更新的配置
hiclaw-sync

# 发现新工具
mcporter list billing --schema

# 测试工具
mcporter call billing.get_customer customer_id=CUST-12345

# 为将来参考生成 SKILL
mkdir -p ~/skills/billing-operations
# ... 写入 SKILL.md，包含工具文档
```

### 步骤 4：Worker 在任务中使用工具

之后，当你分配任务时：

```
你：@alice 检查客户 CUST-12345 是否有逾期账单

Alice：让我查询计费系统...
       mcporter call billing.get_customer customer_id=CUST-12345
       
       客户：Acme Corp
       状态：活跃
       逾期：是的，$4,500 自 1 月 15 日起
       
       该客户确实有逾期账单。
```

**Alice 从未看到真实的 API token。她只使用了 consumer token 通过网关调用。**

---

## 从 Swagger/OpenAPI 到 MCP 工具

如果你有现成的 Swagger 或 OpenAPI 格式的 API 文档？更简单。

```
你：这是我们的产品目录 API Swagger 文档：
    https://docs.internal.company.com/swagger.json
    通过 X-API-Key header 认证。Key：prod_cat_xxx

Manager：我会将 Swagger 文档转换为 MCP 工具...
         发现 12 个端点，创建 12 个 MCP 工具...
         部署到 Higress 为 `product-catalog` MCP 服务器。
         
         测试第一个工具... ✓
         测试第二个工具... ✓
         所有工具已验证。
         
         @warehouse-team 新 MCP 服务器 `product-catalog` 已就绪，包含 12 个工具。
```

Manager 可以读取 Swagger/OpenAPI 文档并自动生成对应的 MCP 服务器配置。

---

## 从 curl 到 MCP 工具

更简单 —— 直接粘贴 curl 命令：

```
你：添加这个 API 调用作为工具：
    curl -X GET "https://api.shipping.com/v1/track?tracking_id=ABC123" \
         -H "X-API-Key: ship_xxx"

Manager：创建 MCP 服务器 `shipping`，包含工具 `track_package`...
         部署并测试完成。@logistics-team 现在可以使用 track_package 了。
```

---

## Worker 生成的 Skills：自我完善的文档

HiClaw 1.0.6 的一个独特功能是：Worker 不仅仅是使用 MCP 工具 —— 它们会**为其编写文档**。

当 Worker 首次遇到一个新的 MCP 服务器时，它会：

1. **发现所有工具**：通过 `mcporter list --schema`
2. **测试代表性工具**：理解其行为
3. **生成 SKILL.md**，包含：
   - 用自然语言描述的工具说明
   - 示例 `mcporter call` 命令
   - 参数说明和常见模式
   - 测试中发现的注意事项

这个 SKILL 成为 Worker 对该 MCP 服务器的永久参考。随着时间推移，Worker 使用工具越多，可以**基于实战经验改进 SKILL**：

- 添加错误处理技巧
- 记录速率限制
- 记录哪些参数实际上是必需的 vs 可选的
- 分享最佳实践

就像有一个 AI 在编写自己的文档 —— 并且不断改进它。

---

## 安全模型：深度防御

让我们明确 Worker 能做什么、不能做什么：

### Worker 可以做的
- ✅ 调用任何已授权的 MCP 服务器
- ✅ 通过 Higress AI Gateway 使用工具
- ✅ 生成和改进 SKILL 文档
- ✅ 在授权范围内自主工作

### Worker 不能做的
- ❌ 看到真实的 API key、token 或凭证
- ❌ 调用未授权的 MCP 服务器
- ❌ 从网关提取凭证
- ❌ 与其他 Worker 共享凭证

### 如果 Worker 被攻破会发生什么
1. **攻击者获得一个 consumer token** —— 只在 HiClaw 内有用
2. **Manager 立即吊销该 token** —— 无需轮换凭证
3. **真实 API key 保持安全** —— 它们从未离开网关
4. **创建新 Worker** —— 几分钟内恢复工作

这就是**凭证零信任**原则：Agent 按需知悉原则运作，而它们从不需要知道真实凭证。

---

## 这对路线图意味着什么

本版本完成了路线图中**"通用 MCP 服务支持"**的规划：

- ✅ **预置 MCP 连接器**：GitHub，以及通过 YAML 配置的任意自定义服务
- ✅ **自定义 MCP 集成**：接入任意 HTTP API，通过 Swagger/curl/YAML 文档化
- ✅ **细粒度权限控制**：Manager 按 MCP 服务器授权 Worker

**任何兼容 MCP 的工具现在都可以安全地暴露给 Worker，凭证零泄露。**

---

## 其他改进

### Slash Commands：跨场景控制

1.0.6 版本还引入了随处可用的 slash commands —— 在 DM 或群聊中都可用：

#### /reset — 修复卡住或配置错误的 Claw

```
你：@alice /reset

Alice：上下文已清除。准备好接收新任务。
```

适用于：
- Claw 陷入循环
- 配置错误导致问题
- 想在任务上重新开始

#### /stop — 中断长时间运行的任务

```
你：@bob /stop

Bob：任务已中断。这是我正在做的事情：
     - 已分析代码库的 45%
     - 发现 3 个潜在问题
     - 还没开始修复
     
     为什么打断我？你想让我改变方向吗？
```

适用于：
- 任务耗时太长
- 想在任务中期提供指导
- Manager 想干预 Worker 的方法

**Manager 可以对 Worker 使用 `/stop`**，在任务完成前进行方向调整。

### 优化的文件同步

文件同步设计原则已统一：**"写入者推送并通知，接收者按需拉取"**，5 分钟定时拉取仅作为兜底。Manager 在 task/project 完成流程中增加按需 `mc mirror` 拉取，确保读取 Worker 最新结果。

---

## 快速开始

已经在使用 HiClaw？升级到最新版本：

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

HiClaw 新用户？一条命令开始：

```bash
# macOS / Linux
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)

# Windows (PowerShell 7+)
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

安装完成后，在 http://127.0.0.1:18088 打开 Element Web，告诉你的 Manager 添加一些 MCP 工具吧！

---

## 接下来是什么

我们正在持续改进 HiClaw。即将到来：

- **Team 管理中心**：所有 Agent 的实时可视化、任务时间线、资源监控
- **更多 Worker 运行时**：ZeroClaw（基于 Rust，3.4MB）、NanoClaw（极简 OpenClaw 替代品）
- **增强的 MCP 发现**：从流行的 MCP 注册表自动导入

加入我们的社区：[Discord](https://discord.com/invite/NVjNA4BAVw) | [钉钉](https://qr.dingtalk.com/action/joingroup?code=v1,k1,q3lHf2AY4o0W2aBsoyJE0kgYnGcBFqpBuwDTjJ36iu8=)

---

## 更新日志摘要

### 新增功能

- **MCP Server 管理技能完善** — 统一的 `setup-mcp-server.sh` 脚本支持运行时 MCP 服务器创建/更新。Worker 获得独立的 mcporter skill，支持工具发现和自动 SKILL 生成。
- **Slash Commands** — `/reset` 清除上下文，`/stop` 中断长时间任务。在 DM 和群聊中都可用。
- **优化的文件同步** — "写入者推送并通知，接收者按需拉取"设计原则。

### Bug 修复

- 修复了孤立 session write lock 清理、Remote->Local 同步逻辑、Matrix room preset、mcporter 配置路径兼容性等问题。

详见 [完整发布说明](https://github.com/alibaba/hiclaw/releases/tag/v1.0.6)。
