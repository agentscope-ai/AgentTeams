# AgentTeams 使用案例

[English](../../usage/use-cases.md) | 中文

本文通过可直接复用的案例，说明如何把一个目标交给 Manager，由多个 Worker 或 Team 协作完成，并在过程中保留人工监督和验收。开始前应先完成[快速入门](../quickstart.md)，确认可以与 Manager 对话并创建 Worker。

案例中的模型、Skills、MCP Server 和外部系统访问都不是自动获得的。只有在实例中配置并授权相应能力后，Worker 才能使用代码仓库、搜索、监控、工单等外部服务。

## 1. 选择合适的协作方式

| 任务特征 | 推荐方式 | 适用说明 |
|---|---|---|
| 单一目标、一个专业领域、短时间完成 | 一个独立 Worker | 创建成本最低，Human 和 Manager 都能直接观察和介入 |
| 多个相互独立的子任务，需要并行执行 | 多个独立 Worker | Manager 分别委派并汇总，适合临时协作 |
| 成员和分工长期稳定，需要反复处理同类项目 | Team | Team Leader 在团队内部拆分、分配和汇总，Manager 只与 Leader 协作 |
| 任务包含发布、删除、付费、外发或生产变更 | Worker 或 Team + 人工审批 | 在任务中明确审批点，未经确认不得执行不可逆操作 |

简单任务不要为了“多 Agent”而创建多个 Worker。拆分只有在子任务能够独立推进、需要不同工具权限，或并行带来的收益明显时才有价值。

独立 Worker 和 Team 可以在同一个实例中共存。临时专家可以作为独立 Worker，由 Manager 直接调度；稳定团队则由 Team Leader 负责内部协调。资源字段和创建方式参见[声明式资源管理](resource-management.md)。

## 2. 通用执行方法

一个可验收的 AgentTeams 任务通常包含以下步骤：

1. **定义结果**：说明最终要交付什么，而不只是“研究一下”或“处理一下”。
2. **划分职责**：给每个 Worker 一个清晰、互不重叠的责任范围。
3. **限定权限**：列出允许使用的仓库、目录、MCP Server 和外部系统；敏感操作设置人工审批点。
4. **约定协作产物**：要求计划、过程记录和最终产物写入共享任务目录，避免关键信息只存在于聊天上下文中。
5. **设置验收标准**：明确测试、引用、格式、风险说明和完成条件。
6. **过程干预**：Human 在 Matrix 中查看进展、补充要求或停止错误方向。
7. **最终汇总**：Manager 检查各子任务状态，向 Human 汇报结果、证据、已知限制和待决事项。

协作过程中还应遵循以下约定：

- **Manager 只负责编排**：Manager 应拆分、登记和委派任务，跟踪状态并汇总结果，不应使用文件或命令工具代替已分配的 Worker 完成实现。
- **核对真实委派状态**：Manager 回复“已分配”后，还应核对任务记录中的 `assigned_to`、`status`、确认时间和结果路径。消息已经发送、Worker 已同步文件或房间中出现了任务文本，都不等于 Worker 已确认并开始执行；超过约定时间仍无确认或产物时，应将任务标记为阻塞并记录原因，再改派给可用 Worker。
- **每个责任使用独立任务**：需要多个 Worker 共同确认同一产物时，为主笔、复核者分别创建任务和结果路径。不要让多个 Worker 共用一个任务状态或 `result.md`，否则其中一人提交后可能掩盖其他人没有执行的事实。
- **共享目录保持唯一**：每个任务约定一个明确的共享目录，并在委派和交接消息中传递完整路径。Worker 完成后先同步产物，后续 Worker 再从同一路径拉取，避免因同名副本而审查错误内容。
- **使用真实的 Matrix mention**：需要自动触发下一角色时，必须在正确房间中实际 @mention 接收者。只在消息正文中写下 `@name` 不一定包含 Matrix mention 元数据，不能作为自动流转已经生效的依据。

建议第一次运行新用例时使用较小范围，确认角色、工具权限和验收方式有效后，再扩大任务规模或将成员组织成 Team。

## 3. 案例一：软件功能交付

### 目标

从需求说明出发，完成设计、实现、测试和代码审查。适合需要多个技术角色并行协作，同时又希望 Human 随时查看代码和调整需求的场景。

### 推荐角色

| 角色 | 主要职责 |
|---|---|
| 后端 Worker | API、数据模型、服务端实现和单元测试 |
| 前端 Worker | 页面、交互、接口接入和前端测试 |
| 测试 Worker | 验收用例、集成测试、回归检查和缺陷报告 |
| 审查 Worker | 检查设计一致性、安全风险、兼容性和遗漏项 |

一次性功能可以使用多个独立 Worker。长期维护同一产品时，可以把这些成员组织为研发 Team，让 Team Leader 负责拆解和汇总。

### 发给 Manager 的示例

```text
请完成“为示例应用增加账号登录”功能。

交付内容：
1. 后端登录 API、一个需要登录令牌的最小受保护接口、输入校验和单元测试；
2. 前端登录页面、错误提示和接口接入；
3. 覆盖成功登录、密码错误、缺少字段，以及无令牌访问受保护接口的集成测试；
4. 一份变更说明，包含运行方式、测试结果、已知限制和安全风险。

请让一个 Worker 起草接口契约，再用独立任务让前端和测试 Worker 确认契约。契约确认后，后端和前端 Worker 并行实现，测试 Worker 提前准备验收用例，最后安排独立审查。
所有修改只允许发生在共享任务目录中的仓库副本，不要发布、合并或修改生产环境。
遇到接口契约冲突时先向我确认。完成标准是相关测试通过，并给出文件清单和测试证据。
```

### 建议流程

1. Manager 先让一个 Worker 起草接口契约并写入共享目录，再为前端和测试 Worker 创建各自的确认任务；契约至少应定义登录成功响应和一个需要令牌的受保护接口。
2. 后端和前端并行实现；测试 Worker 根据契约提前编写验收用例。
3. Human 可以在 Matrix 房间中查看实现进度，并补充边界条件。
4. 测试 Worker 在代码就绪后执行集成验证，记录失败用例和复现步骤。
5. 审查 Worker 只读检查变更，不直接覆盖其他 Worker 的实现。
6. Manager 汇总测试结果、审查意见和遗留问题。

### 验收清单

- [ ] 实现范围与需求一致，没有未授权的额外功能。
- [ ] 接口契约、实现和测试保持一致。
- [ ] 关键成功与失败分支有自动化测试。
- [ ] 测试命令、结果和失败原因可复现。
- [ ] 没有提交真实密钥、令牌或用户数据。
- [ ] 发布、合并和生产变更仍等待 Human 明确批准。

### 所需能力

如果任务需要访问远程仓库，应先配置 GitHub、GitLab 或对应代码平台的 MCP Server/Skill，并只授予必要仓库和操作权限。没有远程写权限时，Worker 仍可在共享目录中完成代码和补丁，由 Human 审查后自行提交。

## 4. 案例二：研究与分析报告

### 目标

围绕一个问题并行收集资料、分析数据、核对事实，最终生成带来源和不确定性说明的报告。

### 推荐角色

| 角色 | 主要职责 |
|---|---|
| 资料研究 Worker | 搜集一手资料，记录标题、链接、日期和核心结论 |
| 数据分析 Worker | 清洗和分析已有数据，说明口径、假设和计算方法 |
| 事实核查 Worker | 检查来源质量、时间范围、冲突信息和无证据结论 |
| 报告编辑 Worker | 统一结构、术语和表达，不新增未经验证的事实 |

### 发给 Manager 的示例

```text
请研究“过去 12 个月企业采用多 Agent 系统时最常见的落地障碍”，形成一份面向技术负责人的报告。

要求：
- 优先使用官方文档、研究论文和公开的一手案例；
- 每个关键结论都附来源和发布日期；
- 区分来源事实、分析推断和建议；
- 对相互冲突的资料同时保留并解释差异；
- 不得编造无法访问的付费报告内容。

请安排资料研究、事实核查和报告编辑三个 Worker。先提交研究计划和来源范围供我确认，再开始完整写作。
最终交付 Markdown 报告、来源清单、关键结论摘要和仍不确定的问题。
```

### 建议流程

1. 研究 Worker 先给出关键词、时间范围和来源优先级。
2. Human 确认范围，避免在错误问题上进行大规模检索。
3. 多个 Worker 可以按来源类型或子主题并行收集，但使用统一的来源记录格式。
4. 事实核查 Worker 对每个关键结论建立“结论—证据”对应关系。
5. 编辑 Worker 只能基于已确认材料成文，对证据不足的内容明确标记。
6. Manager 汇总报告和未解决的冲突，不把推断包装成事实。

### 验收清单

- [ ] 报告明确说明研究问题、时间范围和方法。
- [ ] 关键事实能够追溯到可访问的来源。
- [ ] 来源日期与结论的时效性匹配。
- [ ] 推断、建议和来源事实被清楚区分。
- [ ] 数据计算可以根据原始数据和步骤复现。
- [ ] 不确定性、缺失数据和冲突信息没有被隐藏。

### 所需能力

Worker 只有在配置搜索、浏览器、数据库或企业知识库工具后才能直接获取相应资料。未配置外部检索时，应由 Human 把材料放入共享目录，并要求 Worker 只基于这些材料分析。

## 5. 案例三：内容生产与多语言本地化

### 目标

基于同一组事实和品牌要求，完成内容策划、初稿、审校和多语言版本，避免不同 Worker 分别创作造成事实或术语不一致。

### 推荐角色

| 角色 | 主要职责 |
|---|---|
| 内容策划 Worker | 受众、结构、信息层级和渠道要求 |
| 写作 Worker | 根据已确认大纲和事实材料完成初稿 |
| 审校 Worker | 核对事实、术语、链接、风格和合规要求 |
| 本地化 Worker | 在保留含义的前提下适配目标语言和地区表达 |

### 发给 Manager 的示例

```text
请根据共享目录中的 release-notes.md 和 product-facts.md，制作一篇产品发布文章及英文版本。

受众：已有基础技术背景的开发者。
中文正文 1200～1600 字，英文不是逐字直译，但事实、版本号、命令和链接必须一致。
只允许使用提供的事实文件；缺失信息列为待确认项，不要自行补全。

请安排内容策划、中文写作、事实审校和英文本地化 Worker。
先让我确认大纲，再写正文；最终交付中英文 Markdown、事实核对表和待确认问题。
```

### 建议流程

1. 策划 Worker 从源材料建立事实表和术语表。
2. Human 先确认受众、大纲、语气和禁止表达。
3. 写作 Worker 只引用事实表中的信息。
4. 审校 Worker 逐项核对版本号、命令、链接和能力边界。
5. 本地化 Worker 使用同一事实表和术语表生成目标语言版本。
6. Manager 比较双语标题、章节、代码块、链接和关键数字的一致性。

### 验收清单

- [ ] 中英文覆盖相同的关键事实和限制。
- [ ] 版本号、命令、链接和产品名称完全一致。
- [ ] 没有根据宣传需要扩大未经证明的能力。
- [ ] 目标语言表达自然，且没有改变原意。
- [ ] 待确认内容保留明确标记，没有被自动补写。

## 6. 案例四：故障分析与修复建议

### 目标

让多个 Worker 分别分析日志、配置和最近变更，形成可验证的根因假设与修复方案。该用例默认以只读诊断为主，不应自动执行生产变更。

### 推荐角色

| 角色 | 主要职责 |
|---|---|
| 日志分析 Worker | 整理时间线、错误模式和受影响范围 |
| 配置分析 Worker | 对照正常配置检查差异和依赖关系 |
| 变更分析 Worker | 检查近期发布、提交或基础设施变更 |
| 验证 Worker | 设计复现、回滚和修复后的验证步骤 |

### 发给 Manager 的示例

```text
请分析共享目录 incident-2026-08-06/ 中的日志、配置快照和变更记录，判断服务间歇性 502 的可能原因。

约束：
- 只允许读取现有材料和查询只读监控接口；
- 不得重启服务、修改配置、扩容、回滚或执行生产命令；
- 每个根因假设必须列出支持证据、反证、置信度和验证方法；
- 如果材料不足，明确列出需要补充的数据。

请安排日志、配置和变更分析 Worker 并行调查，再由验证 Worker 整理最小风险的验证方案。
最终交付事件时间线、根因候选、推荐处理顺序、回滚条件和待审批操作。
```

### 建议流程

1. Manager 先确认时间范围、系统边界和允许访问的数据源。
2. 各 Worker 独立形成假设，避免过早共享单一结论造成锚定偏差。
3. 验证 Worker 比较假设，优先设计只读、可回退的验证步骤。
4. Human 决定是否授权任何重启、回滚或生产修改。
5. 执行获批操作后，Worker 只按批准范围验证，不扩大变更。
6. Manager 汇总最终结论、证据和后续预防措施。

### 验收清单

- [ ] 时间线和影响范围有日志或监控证据支持。
- [ ] 根因结论与相关性观察被明确区分。
- [ ] 每个建议都说明风险、回滚方式和验证方法。
- [ ] 未经 Human 批准，没有执行生产写操作。
- [ ] 报告不包含未脱敏的令牌、密码或用户数据。

## 7. 案例五：长期项目协作

### 目标

让一组稳定角色持续处理多阶段项目，并把任务状态和产物保存在共享空间中，使协作不依赖单次模型会话。

### 什么时候使用 Team

以下条件同时出现时，优先考虑 Team：

- 同一组角色会反复合作，而不是只执行一次任务。
- 项目包含多个阶段和相互依赖的子任务。
- 希望 Team Leader 负责团队内部拆解，Manager 只关注项目级结果。
- 需要通过共享任务目录、进度记录和验收状态恢复上下文。

### 发给 Manager 的示例

```text
请为“开发者门户改版”建立一个长期研发 Team，包括 Team Leader、前端、后端和测试成员。

项目分为需求澄清、技术设计、实现、集成测试和发布准备五个阶段。
每个阶段开始前提交计划，结束时提交产物、测试证据、风险和下一阶段依赖。
任何生产发布、数据迁移或外部通知都必须等待我的明确批准。

请先给出 Team 结构、阶段计划、共享目录约定和验收节点，不要立即开始实现。
```

### 建议流程

Team Worker 中的相对路径 `shared/` 指向该 Team 的专属共享空间，而不是独立 Worker 使用的全局共享目录。Manager 应通过 Team Leader 接收阶段结果；如果需要直接核对对象存储，应使用 Team 对应的存储前缀，不要把全局共享目录中暂时看不到文件误判为成员未提交。

1. Manager 创建或选择 Team，并把项目目标委派给 Team Leader。
2. Team Leader 建立阶段和任务依赖，只有前置任务完成后才推进后续工作。
3. Worker 把计划、进度和结果写入共享任务目录，并在需要决策时 @mention Leader。
4. Leader 汇总阶段结果，在完成、阻塞或需要审批时通知 Manager。
5. Manager 向 Human 汇报项目级状态，不直接越过 Leader 调度 Team Worker。
6. Human 在阶段验收点批准、调整或停止项目。

### 验收清单

- [ ] Team 成员职责和通信路径清晰。
- [ ] 阶段、任务依赖和完成条件有持久化记录。
- [ ] 会话重置或 Worker 重建后可以从共享产物恢复进度。
- [ ] 阻塞、审批和范围变更能够及时升级给 Human。
- [ ] Manager、Leader 和 Worker 没有重复分派同一任务。

Team、Team Leader、Human 权限和任务流转参见[声明式资源管理](resource-management.md)。

## 8. 案例六：添加并使用自定义 Skill

### 目标

为已有 Worker 添加一个不依赖外部服务的 `bilingual-doc-review` Skill，再让 Worker 使用它检查一组中英文文档。该案例同时验证三件事：Skill 文件已经分发、Worker runtime 已经发现 Skill、Worker 确实按照 Skill 中的规则完成了任务。

开始前，准备一个空闲的已有 Worker，并在以下示例中将它记为 `doc-reviewer`；如果实际名称不同，请统一替换。Worker 的创建方式参见 [Worker 指南](worker-guide.md)。

### 1. 准备 Skill 包

创建以下目录：

```text
bilingual-doc-review/
├── SKILL.md
└── references/
    └── checklist.md
```

`SKILL.md` 内容如下：

```markdown
---
name: bilingual-doc-review
description: Compare paired Chinese and English Markdown documents for structural and factual consistency.
---

# Bilingual documentation review

Use this Skill when you need to compare paired Chinese and English Markdown documents.

1. Read `references/checklist.md` before reviewing any files.
2. Report the ruleset ID from that file at the beginning of your result.
3. Compare heading order, code blocks, links, commands, configuration names, numbers, defaults, and limitations.
4. Distinguish factual mismatches from acceptable translation differences.
5. Do not edit source files unless the task explicitly requests changes.
6. Save a Markdown report containing a summary and a table with severity, location, Chinese value, English value, and recommended resolution.
```

`references/checklist.md` 内容如下：

```markdown
# Review checklist

Ruleset ID: agentteams-bilingual-doc/v1

- Heading order and section coverage match.
- Code blocks, commands, paths, links, and configuration names match.
- Versions, ports, timeouts, limits, defaults, and other numeric facts match.
- Warnings, prerequisites, unsupported cases, and fallback behavior exist in both languages.
- Stylistic differences are not reported as factual mismatches.
```

从包含 `bilingual-doc-review/` 的目录执行：

```bash
zip -r bilingual-doc-review.zip bilingual-doc-review/
```

### 2. 分发 Skill

选择以下任一方式。完整约束和两种方式的状态差异参见 [Worker 指南：为 Worker 安装 Skill](worker-guide.md#为-worker-安装-skill)。

**方式 A：通过 Manager 分发**

把 `bilingual-doc-review.zip` 作为附件发送给 Manager，然后发送：

```text
请将附件中的 bilingual-doc-review Skill 安装给 Worker doc-reviewer。
请安全解压并校验，分发完整 Skill，然后确认 Worker 的 Skill 分配已经更新。
```

**方式 B：通过 Dashboard 分发**

进入**资源中心 → 市场**。如果 `bilingual-doc-review` 尚未入库，先点击**上传技能**并上传 `bilingual-doc-review.zip`；然后在该 Skill 所在行点击**分发到 Worker**，选择 `doc-reviewer`，再点击**分发到 1 个 Worker**。也可以从 **Workers → doc-reviewer → 详情 → 上传技能包**直接上传 ZIP。加载过程可能重启 Worker，因此应在 Worker 空闲时分发。

分发完成后，在 Worker 详情的“已分发技能”和 `Worker.spec.skills` 中确认 `bilingual-doc-review`。Manager 与 Dashboard 分发都会维护声明式分配记录。

### 3. 准备检查材料

在共享任务目录中准备 `sample.zh-CN.md`：

```markdown
# 示例服务安装

## 环境要求

服务监听端口为 `8080`。

## 启动服务

运行 `demo-server --port 8080`。启动失败时，服务将在 30 秒后重试。
```

再准备存在两处事实差异的 `sample.en.md`：

```markdown
# Sample service installation

## Requirements

The service listens on port `8081`.

## Start the service

Run `demo-server --port 8080`.
```

### 4. 让 Worker 使用 Skill

通过 Manager 把任务委派给 `doc-reviewer`：

```text
请让 Worker doc-reviewer 使用 bilingual-doc-review Skill，比较共享任务目录中的
sample.zh-CN.md 和 sample.en.md。

要求：
1. 先确认 runtime 能够发现该 Skill；
2. 按 Skill 的规则读取所需参考文件，并在报告开头写出规则集 ID；
3. 不修改原文件；
4. 将结果保存为 bilingual-review-report.md；
5. 回复报告路径、差异数量和简要结论。
```

应通过 Manager 委派这一步，并确认 Manager 在正确房间中真实 @mention 了 Worker。直接在 Worker 房间中输入普通文本不一定携带 Matrix mention 元数据，可能只进入房间历史而不会触发 runtime 执行。

不要在任务指令中直接提供规则集 ID 或预期差异，以免 Worker 只复述提示而没有读取 Skill。

### 5. 验收结果

预期报告应满足：

- 开头包含规则集 ID `agentteams-bilingual-doc/v1`，证明 Worker 读取了 Skill 的参考文件。
- 识别端口说明中的 `8080` 与 `8081` 不一致。
- 识别中文包含“30 秒后重试”，而英文缺少该行为说明。
- 不把标题的自然翻译差异误报为事实不一致。
- 没有修改两个输入文件，并生成了 `bilingual-review-report.md`。

如果 Worker 只能在 Dashboard 中看到 Skill，却无法报告规则集 ID，应先确认 Worker 已完成重新加载或等待周期同步，再重新发起任务。仍然失败时，按照 [Worker 指南](worker-guide.md#故障排查)检查 ZIP 结构、`SKILL.md` 元数据和 runtime 日志。

## 9. 可复用的任务模板

向 Manager 提交复杂目标时，可以从以下模板开始：

```text
目标：
<最终要解决的问题>

交付物：
1. <产物一>
2. <产物二>

角色与分工：
- <Worker/Team 角色>：<责任范围>

输入与允许访问范围：
- <共享目录、仓库、数据源、MCP Server>

约束：
- 不允许：<发布、删除、付费、生产修改等>
- 必须审批：<需要 Human 确认的操作>

协作要求：
- 计划、进度和结果写入 <共享目录>
- 遇到 <条件> 时暂停并向我确认

验收标准：
- <测试、引用、格式、性能或安全要求>

请先返回任务拆分、角色安排、依赖关系和待确认问题；我确认后再执行。
```

模板中的约束和验收标准比角色数量更重要。没有明确完成条件时，Manager 很难判断何时应该继续、返工或结束任务。

## 10. 使用边界

以下情况通常不适合直接交给多个 Agent 自主完成：

- 需求尚未明确，且不同理解会导致完全不同的结果。
- 操作不可逆，但没有可用的审批、备份或回滚机制。
- 数据包含不能提供给模型或 Worker 的敏感信息。
- 任务依赖尚未配置的外部系统、凭据或专业工具。
- 工作量很小，单个 Agent 或普通脚本已经足够。

AgentTeams 提供协作、隔离、可见性和人工介入机制，但不会替代业务授权、数据治理、专业审核和生产变更流程。

## 11. 下一步

- 使用[快速入门](../quickstart.md)完成最小的 Human → Manager → Worker 工作流。
- 阅读 [Manager 指南](manager-guide.md)和 [Worker 指南](worker-guide.md)了解运行与维护方式。
- 阅读[声明式资源管理](resource-management.md)创建可复用的 Worker、Team 和 Human 资源。
- 本地实例参见[本地部署指南](deployment/local.md)，共享实例参见 [Kubernetes 部署指南](deployment/kubernetes.md)。
