# 技能目录 API

状态：已实现
API：`GET /api/v1/skills`

## 问题

workbench（及任意 API 客户端）需要一个可浏览的目录，列出团队可以指派给其 worker 的技能。此端点出现之前，经 controller API 无从列出它们——Dashboard 技能中心读的是自己的私有存储，用户侧客户端够不到；Worker CRD 只记录已指派的内容，不记录可用的内容。

目录回答以下问题：

1. **哪些内置技能存在、面向哪些 runtime？** 内置技能在开通（provisioning）时由部署方（deployer）从按角色/runtime 区分的 agent 模板目录置入 `agents/<worker>/skills/`。例如由 `copaw` 模板携带的技能只对 copaw worker 可用——目录必须说明这一点，否则客户端会提供实际静默空操作的指派。
2. **哪些技能已暂存、等待全部署分发？** dashboard 的技能上传流程把技能暂存到 `agents/global/skills/` 之下；它们可以从 dashboard 或经 `PUT /workers`（`skills` 字段）分发到任意 worker。
3. **哪些技能打包在插件包内？** TeamHarness 这类插件自带技能；在当前 QwenPaw runtime 上，它们在携带该插件的 worker 中处于生效状态，对其他目录层（templates、global、spec）却不可见——目录必须报告它们，否则就会少报 worker 实际能做的。

## 设计

L1 只读端点 `GET /api/v1/skills` 由 `SkillsHandler` 提供，有三个来源（团队层——见后续章节——经 `?team=` 增加第四个）：

1. **内置技能。** 处理器扫描部署方在开通 worker 时使用的 agent 模板目录。模板→runtime 的映射**派生自 `service.BuiltinAgentDir`**——部署方调用的同一个函数——对 worker 与 team-leader 角色遍历每个受支持的 runtime（`service.AllWorkerRuntimes`）。因此目录不可能与 worker 实际收到的内容漂移：部署方的模板选择一旦变化，目录的按 runtime 可用性自动随之变化。每个 `skills/<skill>/SKILL.md` 的 frontmatter 提供 `name` 与 `description`；目录名是回退名。同一技能被多个模板携带时只报告一次，`agents` 列出提供它的模板，`runtimes` 为其 runtime 的并集。
2. **插件技能。** 处理器扫描 `AGENTTEAMS_PLUGIN_DIR`（默认 `/opt/agentteams/plugins`，打进 controller 镜像；与插件构建打包进 worker 镜像的同一来源）下的捆绑插件包。发现是**manifest 驱动**的：某技能当且仅当其插件的 `plugin.yaml`（`kind: AgentTeamPlugin`）在 `skills:` 块中声明它时才出现——未列出的 `SKILL.md` 目录不会泄漏进目录，且不抓取任何 worker 状态。每个已声明技能解析到 `<pluginDir>/<plugin>/<path>/SKILL.md`，取 `name` / `description` / `version`（manifest 的 `id` 是回退名；插件包的 `metadata.version` 是回退版本）。条目携带 `source: "plugin"` 与 `plugin: <name>`；**不**携带 `runtimes` / `agents` / `updated_at`——可用性跟随插件的部署（worker 有该技能当且仅当它有该插件），而不是按 runtime 的模板或按 worker 的指派。它们按构造只读：团队技能上传只写团队层，从不触碰插件条目，且插件技能的生命周期归插件包所有。插件目录缺失、manifest 非法或 `SKILL.md` 缺失，都只把该插件（或该单个技能）降级为缺席——绝不报错。命名冲突时内置名胜出，与共享半边相同。
3. **共享技能。** 处理器列出 `agents/global/skills/` 的第一层（只读，每次请求时进行）。目录条目是技能；裸文件与点条目被跳过。命名冲突时内置名胜出。列取失败（前缀不存在、存储不可用）降级为空共享集——目录仍以 `200` 提供内置技能。

### 共享前缀的留存语义

`agents/global/skills/` 是**暂存区，不是分发通道**：没有任何 worker 入口自动消费该前缀。共享技能只有经按 worker 分发（dashboard 的 Worker 对话框，或 L1/L2 `PUT /workers` 的 `skills`）才到达 worker，该分发同时把指派记录进 `spec.skills`。因此，删除 `agents/global/skills/{name}/`：

- 把该技能从本目录和 dashboard 的全局区移除；
- **不**触碰已分发的按 worker 副本（`agents/<worker>/skills/{name}/`）或既有的 `spec.skills` 指派——没有级联。后续被移除指派的 worker，在其下一次同步/刷新时停止收到该技能。

### 无内容访问

只读内置与插件技能的 frontmatter 元数据（内置为 `name` / `description` / `version` / `requires`；插件技能为 `name` / `description` / `version`）；共享技能只按名称 + `updated_at`（列取时间戳）列出。技能正文与注册表凭据从不暴露；该端点不做任何注册表调用。响应 schema 刻意限定为 `name` / `description` / `source` / `version` / `requirements` / `updated_at` / `agents` / `plugin` / `runtimes`（由 `TestSkillsCatalogFieldDiscipline` 钉死）。

## 契约

`GET /api/v1/skills` → `200`

```json
{
  "skills": [
    {"name": "file-sync", "description": "Sync files with centralized storage.", "source": "builtin", "version": "1.0.0", "requirements": {"require_bins": ["mc"]}, "agents": ["copaw-worker-agent", "worker-agent"], "runtimes": ["copaw", "deepseek-harness", "openclaw", "openhuman", "qwenpaw"]},
    {"name": "teamharness-communication", "description": "Message delivery protocol.", "source": "plugin", "version": "1.2.0", "plugin": "teamharness"},
    {"name": "shared-kb", "source": "shared", "updated_at": "2026-09-11T08:00:00Z", "runtimes": ["copaw", "deepseek-harness", "hermes", "openclaw", "openhuman", "qwenpaw"]}
  ],
  "total": 3
}
```

- `source` 为 `"builtin"`、`"plugin"` 或 `"shared"`（L1 视图；`?team=` 视图增加 `"team"`——见 team-skills.md）。
- `version` 是 SKILL.md frontmatter 的 `version`（顶层，回退到 `metadata.version`）；插件技能进一步回退到插件包的 `metadata.version`。无人声明时省略。
- `plugin`（仅插件）是插件包名（manifest 的 `metadata.name`，否则为插件目录名）。
- `requirements`（仅内置）镜像 frontmatter 的 `requires` 声明（`require_bins` / `require_envs` / `require_mcps`），按 QwenPaw 2.2.x 语义解析（`metadata.{openclaw,qwenpaw,clawdbot}.requires` 遮蔽 `metadata.requires`，后者遮蔽顶层 `requires`；裸列表是 `bins` 的简写）。执行是 runtime 相关的：qwenpaw 2.2.x 注册表以它为技能激活的门控；其他 runtime 尚无对等门控——暴露该字段是为让 workbench 能在指派前预警。
- `updated_at`（仅共享，RFC3339 UTC）是列取时间戳（当后端暴露它时）；不可解析时省略。内置条目从不携带。
- `agents` 存在于内置技能（排序、去重的模板目录名）。
- `runtimes` 已排序。对内置技能，它是携带该技能的模板所属的 runtime 集合；对共享技能，它是完整 runtime 列表（任何 worker 都可以经按 worker 分发获得共享技能）；对插件技能则**省略**——插件技能对 worker 可用当且仅当该 worker 拥有该插件（可用性跟随插件的部署，而不是按 runtime 的模板）。
- 输出按 `name` 排序；缺失的模板目录（未部署某些 runtime 的部署）被静默跳过。
- 错误：后端读失败降级为剩余半边（`200`）。`400 team scope required`——非 admin 调用方：目录是部署级（individual）技能层，仅 L1 可用（见授权）；团队范围读（`?team=`）随团队技能工作跟进。

### 已知局限（已跟踪，v1 之外）

`deepseek-harness` worker 当前不消费 MinIO 置入或 `spec.skills` 指派的技能：dsh runtime 从镜像内的插件 manifest 准备技能，故 runtime.yaml 的 skill 段在那里是空操作。目录仍把 dsh 列入 `runtimes`，因为 controller 确实为 dsh worker 置入文件；对 dsh worker 的指派在该 runtime 消费它们之前（后续项）没有效果。客户端应对 dsh 指派相应加标注。

## 授权

`skills` 资源种类上的 `ActionList`（任何其他动作被拒绝，而非默认放行）。目录暴露部署级（"individual"）技能层，由 admin 管理，因此处理器强制**仅 admin（L1）**的角色门：非 admin 调用方（L2 人类、team leader、worker、manager）收到 `400 team scope required`——团队范围目录（`?team=`）随团队技能工作交付（跟踪于 #1221）。响应仅元数据（无 PII、无凭据）；按 worker 的指派仍是独立的（写）关注点，经 `PUT /workers`。

## 范围之外（v1）

- 注册表侧列取（查询注册表里还没有任何 worker 引用的技能）——需要注册表读凭据；独立需求。
- 技能内容下载/上传（Dashboard 技能中心的范畴）。
- 按 worker 的可用性（`GET /workers/{name}/skills/available`）——目录是全局元数据；按 worker 的事实（MinIO 对象存在性）是后续项。
- 插件技能保持只读且不可指派：不经 `spec.skills` 分发（其生命周期归插件包），因此上述按 worker 的可用性视图——交付时——必须从 worker 的插件集派生它们，而不是从指派。

## 后续：团队技能层

团队范围读（`?team=`）、上传面（`POST /api/v1/skills`，`scope=team|deployment`）与指派时物化（带强制内容扫描，扫描 ②）规定在配套设计 [team-skills.md](team-skills.md) 中。本文保持为 L1 只读目录半边的参考；团队层复用其响应形状（新增 `source: "team"` 条目）与其授权地基。

## 测试

- `internal/server/skills_handler_test.go`——黄金目录（跨模板的内置去重，带 `agents` + `runtimes` 并集；共享目录条目；冲突时内置胜出；裸文件与点条目跳过）、对每个 (role, runtime) 组合相对 `service.BuiltinAgentDir` 的模板→runtime 映射一致性、响应字段纪律（无意外字段）、frontmatter 扩展（`version` + `requires` 的 metadata 命名空间/顶层/裸列表形态、命名空间遮蔽优先级、未声明技能的 omitempty）、共享 `updated_at` 传播（后端提供的时间戳；内置条目从不携带）、非 admin 无团队范围的拒绝（L2 人类/team leader/worker/manager/缺失调用方 → 带自说明消息的 `400`；正向 admin `200` 路径由黄金测试钉死）、OSS 列取失败时共享半边降级、空 `WorkerAgentDir` → 仅共享目录、插件来源（manifest 驱动发现；未列出的 `SKILL.md` 目录被排除；frontmatter name/version 带 manifest-id 与包版本回退；插件条目无 `runtimes`/`agents`/`updated_at`；冲突时内置胜出；插件目录缺失与 manifest 非法降级为缺席）。
- `internal/auth/authorizer_test.go`——`skills` 资源种类的授权矩阵。
