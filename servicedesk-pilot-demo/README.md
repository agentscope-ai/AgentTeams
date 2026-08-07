# ServiceDesk Pilot · 端到端可运行 Demo

> GOAI 世界人工智能开源大赛 · Agent Infra 赛道初赛方案
> **「ServiceDesk Pilot：中小企业 IT 服务台多 Agent 智能处置系统」** 的可运行样例。

本 Demo 把方案定义的**端到端九阶段闭环**完整跑通，且**完全本地、零外部依赖**：
微信群消息用 `mock/wechat_messages.json` 模拟，各节点交互（审批人、业务系统、人工回填）都用
Mock Adapter 实现。无需 Docker、无需真实企业微信 / 飞书 / 业务系统即可一键演示全流程。

---

## 1. 它能演示什么

完整覆盖方案的端到端流程：

1. **微信群消息接入** —— IM Adapter 从企业微信群拉取原始报障消息
2. **事件解析与去重** —— Ticket Intake Agent 标准化、脱敏、合并同类项
3. **知识检索** —— Knowledge Agent 检索 Runbook / 历史案例 / 老系统人工经验（带 RAG 命中率）
4. **风险分诊** —— Triage Analyst 判定 L0–L3 风险等级与执行路径（规则引擎驱动）
5. **方案生成** —— Resolution Agent 生成分级处置方案 + 回滚点
6. **审批决策** —— **配置驱动**：人工审批 / 自动跳过 / 自动批准（含免审批规则与 L3 强制人工）
7. **执行与标记完成** —— 白名单动作自动执行 / 无接口老系统人工执行回填
8. **恢复验证** —— Verify Agent 探针校验 + 用户在群内确认
9. **复盘与知识沉淀** —— Postmortem + KnowledgeReflector **真实写回**知识库

**状态全程可见、可演示**：终端 ANSI 彩色渲染 + 导出 `trace.json` + 生成亮色主题 HTML 看板
（九阶段泳道图 / 审批决策卡 / 知识库 diff / 全链路调用链）。

---

## 2. 与初赛方案的映射表

| 方案阶段 | Demo 阶段 | 负责 Agent | 关键实现 |
|---|---|---|---|
| 8.1 核心协作流程 | S1–S9 编排 | **TeamLeader**（总控） | `agents/orchestrator.py` |
| 8.2 上下文传递 | `Incident` 唯一载体 | — | `core/models.py` |
| Ticket Intake | S2 | TicketIntakeAgent | `agents/service_agents.py` · 去重 / 脱敏 / 分类 |
| Knowledge / RAG | S3 | KnowledgeAgent | `skills/knowledge_skills.py` · BM25-lite 自实现 |
| Triage Analyst | S4 | TriageAnalystAgent | `agents/service_agents.py` · 规则引擎 `config/policy.json` |
| Resolution | S5 / S7 | ResolutionAgent | `skills/resolution_skills.py` · 动作模板 + 白名单 |
| Verify + 复盘 | S8 / S9 | VerifyAgent | `skills/verify_skills.py` · 探针 + Postmortem + 知识回流 |
| 统一 MCP 接入层 | 全链路工具调用 | MockMcpGateway | `adapters/mcp_gateway.py` |
| 审批分层（L0–L3） | S6 | ApprovalAdapter | `adapters/approval_adapter.py` + `config/policy.json` |
| 证据规范 / 可审计 | Trace 总线 | Tracer | `core/trace.py` · 全链路事件留痕 |

技能以模块组织：**分类 / 知识 / 处置 / 验证** 四大类，对应方案第十三章技能体系，
并通过 `knowledge_reflection` 配置项实现「运行即沉淀」的闭环。

---

## 3. 运行方式

```bash
cd servicedesk-pilot-demo
python run_demo.py                 # 跑全部 5 个场景（默认）
python run_demo.py --list          # 列出全部场景后退出
python run_demo.py --scenario S2_mfa_lost        # 只跑指定场景
python run_demo.py --approval-mode auto_skip     # 覆盖审批模式为自动跳过
python run_demo.py --approval-mode auto_approve  # 自动批准（留痕）
python run_demo.py --interactive   # 审批走终端真人输入 y/n
python run_demo.py --keep-kb       # 运行后不还原知识库（保留写入结果）
python run_demo.py --quiet         # 仅输出结果摘要
```

> 环境要求：Python 3.10+，**仅用标准库，无需 `pip install` 任何第三方包**。

---

## 4. 五个内置场景（mock 群消息）

| 场景 | 报障人 | 分类 | 风险 | 执行路径 | 演示要点 |
|---|---|---|---|---|---|
| `S1_vpn_locked` | 张伟（销售） | account_locked | L1 | auto_execute | 命中 Runbook → 白名单自动解锁，免审批闭环 |
| `S2_mfa_lost` | 李娜（市场） | mfa_lost | L2 | approval_then_execute | 需解绑 MFA → **走审批**（配置可免审批） |
| `S3_legacy_erp` | 王芳（财务） | legacy_account | L2 | legacy_manual | 用友 U8 无接口 → 人工执行清单 + 留证回填 |
| `S4_suspected_compromise` | 陈磊（研发） | suspected_compromise | L3 | human_only | **疑似失陷强制人工**，Agent 只交付方案不执行 |
| `S5_knowledge_gap` | 刘洋（供应链） | access_denied | L2 | escalate | SMB 共享盘拒绝访问，知识库无命中 → 升级 + Badcase |

> 注：`S1_vpn_locked` 场景里有两条消息（张伟 + 赵敏），演示了 **S2 去重合并**。

切换审批模式可改变 S2 的演示形态：
- `manual`（默认）：S2 生成审批单，由脚本化 mock 审批人「批准」
- `auto_skip`：S2 命中免审批规则 **ASR-001**（身份双因子核验 + 高成功率 Runbook + 系统直连）→ 免审批直接执行
- `auto_approve`：仍生成审批单留痕，但由策略引擎自动批准

---

## 5. 产物说明（`out/` 目录）

每次运行在 `out/` 下生成三件套：

| 文件 | 内容 |
|---|---|
| `trace.json` | 全链路 Trace（所有阶段 / Agent / 工具调用 / 群消息事件），可被看板回放 |
| `report.md` | 文字版复盘报告：关键指标 + 逐工单复盘 + 知识库变更 |
| `dashboard.html` | 亮色主题可视化看板：KPI 卡 / 九阶段泳道 / 审批决策卡 / 知识库 diff / 调用链 |

打开 `dashboard.html` 即可看到整场运行的「可视化战报」。

---

## 6. 审批三模式（配置驱动，无需改代码）

核心开关是 `config/policy.json` 的 `approval.mode`：

- **manual** —— L2 动作生成审批单，等待审批人决策（交互式或脚本化 mock 审批人）
- **auto_skip** —— L2 动作按 `auto_skip_rules`（ASR-001）免审批直接执行，演示「配置驱动的免审批通道」
- **auto_approve** —— L2 动作仍生成审批单并留痕，但由策略引擎自动批准（保留审计证据）

两条硬约束（无论模式如何）：
- `never_skip_categories: ["suspected_compromise"]` —— 疑似失陷永远不允许跳过人工闭环（对应 L3 强制人工）
- `execution_whitelist.human_only` —— 账号注销 / 令牌吊销 / 删号 / 数据导出，Agent 永远不执行

---

## 7. 知识库写回与还原机制

方案强调「运行即沉淀」，Demo 用**真实文件写回**来体现：

- `KnowledgeReflector` 在 S9 真实写入 `mock/knowledge_base.json`
  （案例入库 / Badcase 记录 / Runbook 草稿 / 老系统经验 / 落盘）
- 进入知识库前，原始消息已**脱敏**（密钥、个人信息只进脱敏文本，见 `NormalizedEvent.redacted_text`）
- **可复现保证**：每次运行从 `knowledge_base.base.json`（pristine 基线）重置，
  运行结束后**默认还原基线**，因此仓库干净、可重复演示；diff 已写入 `trace.json` / `report.md` / `dashboard.html`
- 想保留本次写入结果，加 `--keep-kb`

知识库变更维度（运行前 → 运行后）：`runbooks` / `cases` / `legacy_operator_notes` / `badcases`，
看板里有逐项 diff 与新增条目清单。

---

## 8. 目录结构

```
servicedesk-pilot-demo/
├── run_demo.py                 # CLI 入口：装配上下文、跑场景、导出三件套、管理知识库基线
├── config/
│   └── policy.json             # 风险分级 / 审批策略 / 执行白名单 / 知识沉淀 等核心开关
├── core/
│   ├── models.py               # 数据模型：Incident / 枚举 / 上下文 Schema
│   └── trace.py                # 全链路 Trace 总线（阶段 / Agent / 工具 / IM 事件）
├── adapters/
│   ├── im_adapter.py           # 微信企业群消息 Mock Adapter（唯一输入源）
│   ├── mcp_gateway.py          # 统一 MCP 接入层 Mock（业务系统工具调用）
│   └── approval_adapter.py     # 审批人 Mock（脚本化 / 交互式 / 免审批规则引擎）
├── agents/
│   ├── orchestrator.py         # TeamLeader：S1–S9 编排与升级分支
│   └── service_agents.py       # TicketIntake / Knowledge / Triage / Resolution / Verify 五个职能 Agent
├── skills/
│   ├── classify_skills.py      # 异常分类（含置信度与候选）
│   ├── knowledge_skills.py     # KnowledgeBase（BM25-lite 检索）+ 知识回流
│   ├── resolution_skills.py    # SafeResolver：动作模板 + 白名单 + 回滚
│   └── verify_skills.py        # RecoveryVerifier / ObservabilityProbe / Postmortem
└── mock/
    ├── wechat_messages.json    # 企业微信群消息流（唯一输入）
    ├── systems.json            # 业务系统 Mock（连通性 / 人工操作员）
    ├── approvers.json          # 审批人 Mock
    ├── knowledge_base.json     # 运行时知识库（被写回，运行后还原基线）
    └── knowledge_base.base.json# 知识库 pristine 基线
```

---

## 9. 从 Demo 到真实接入

各 Mock 适配器都按「真实接入复用同一 Schema」设计，替换成本最低：

- `im_adapter.py` → 企业微信 / 飞书 / 钉钉 webhook 事件（Schema 一致）
- `mcp_gateway.py` → 真实 MCP Server（VPN 网关 / IDaaS / 邮件 / 代码仓库 / 老系统网关）
- `approval_adapter.py` → 真实审批中心（保留 `skip_rule_id` 审计痕迹）
- `knowledge_skills.py` 的 BM25-lite → 真实向量库 RAG

所有 Agent 间传递的上下文都收敛在 `core/models.py` 的 `Incident` 上，与真实接入阶段共用同一套契约。
