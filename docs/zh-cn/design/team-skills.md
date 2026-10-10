# Team 技能 —— 上传、目录与指派时物化

状态：已实现（embedded/docker 模式；k8s 模式扫描为已知局限）
API：`POST /api/v1/skills` · `GET /api/v1/skills?team=<name>` · `PUT /api/v1/workers {skills}`（既有）
配套：`skill-catalog-api.md`（本设计所基于的只读 L1 目录）

## 问题

团队技能当前要么 **builtin**（随 worker-agent 模板内置，所有 worker 相同），要么 **deployment-wide**（由 dashboard 置于 `agents/global/skills/`，所有团队可见）。没有每团队技能层：market 团队无法在不动部署级存储的前提下向其自身 worker 发布技能，且 worker 的团队上下文对技能目录不可见。

本设计新增**团队技能层**——存储、目录读取、上传与指派时物化——以内容扫描为硬闸门。

## 契约（评审的单一事实源）

```
read side (GET /api/v1/skills, existing endpoint):
  no team param     L1 (admin) → builtin + shared (deployment)        [unchanged]
  ?team=T           L1 any team / L2 own team / leader own team
                    → builtin + teams/T/skills/
                    L2/leader cross-team or unknown team → 404
                    (indistinguishable from "no such team", W8 anti-probing)

write side:
  POST /api/v1/skills            multipart/form-data
    scope=team        → teams/<t>/skills/<name>/      (admin any / L2 own team)
    scope=deployment  → agents/global/skills/<name>/  (admin only)
    common: structure validation + scan ① (best-effort) + exact-copy write

materialize (assign — PUT /workers {skills}, existing surface, NO new endpoint):
    → controller copies teams/<t>/skills/<s>/ → agents/<w>/skills/<s>/
      (scan ② MANDATORY: block or unavailable → not copied, warning surfaced)
    → worker sync loop materializes within its sync interval (≤5 min)
```

允许发布的角色：**admin**（任意范围）与 **L2 人类**（仅本团队、仅 `scope=team`）。Manager 与团队 leader **永不**发布——leader 读取本团队目录（指派面），但写在授权器被拒、在处理器复核（两层均由测试钉死）。Worker 永不。

## 存储布局

```
teams/<t>/skills/<name>/...        per-team skills (new)
agents/global/skills/<name>/...    deployment-wide (existing, dashboard)
agents/<w>/skills/<name>/...       materialized per worker (existing)
```

技能的**名称即 zip 的单一顶层目录**，且必须等于 `SKILL.md` frontmatter 中的 `name`——存储键、目录身份与指派引用三者一致。上传是精确复制（`Mirror{Overwrite, Remove}`——重新上传会删除新版本删去的文件），与 `push-worker-skills.sh` 已应用的语义相同。

团队存储由 `EnsureTeamStorage`（既有）播种——技能不需要新的播种前缀。

## 上传（扫描①，尽力而为）

`POST /api/v1/skills`，`multipart/form-data`：`file`（zip）、`scope`（`team`|`deployment`）、`team`（`scope=team` 时必填）。

顺序：角色门（403）→ 结构校验（400）→ 团队范围检查（403/404）→ 扫描① → 写入。

结构校验（400，自描述）：

- zip 内含**恰好一个顶层目录**（技能根）；
- `SKILL.md` 直接位于根内，YAML frontmatter 合法且 `name` 等于目录名；
- 目录名是合法技能名（kebab-case，≤64 字符）；
- zip-slip 防御：绝对路径、反斜杠、空 / `.` / `..` 路径分量与符号链接条目均被拒绝；
- 64 MB 上限同时适用于 zip **与**解压后载荷（zip 炸弹）。

扫描①语义（尽力而为——强制闸门是扫描②）：

| 扫描①结果 | 动作 |
|---|---|
| `block`（CRITICAL/HIGH） | 422 + findings（仅元数据） |
| `pass` / `warn` | 继续；`warn` findings 在响应中呈现 |
| unavailable（无后端 / 容器往返失败） | 继续，响应 `scan.status="skipped"`，记警告日志 |

响应：`200 {name, scope, team?, files, scan:{status, findings?}}`。错误：`400` 校验 · `403` 授权 · `404` 未知/他人团队 · `422` 扫描拦截。

## 内容扫描：容器往返

扫描器是 qwenpaw 技能扫描器（`qwenpaw.security.skill_scanner`）——它位于运行时镜像内，而控制器没有 Python。因此一次扫描是经 **Docker Engine API** 的往返（挂载的 unix socket 上的裸 HTTP——与 `internal/backend` 同一传输；控制器镜像是 alpine，无 docker CLI）：

1. **解析扫描容器。** 默认 Manager CR，仅当其 `spec.runtime` 为 `qwenpaw`（生产 manager 跑 qwenpaw）。无 Manager CR 或非 qwenpaw 运行时 → unavailable。
2. **上传载荷**为一个 tar（archive `PUT`）到 `/tmp/.skillscan/<uuid>/`。
3. **运行探针**（detached exec，inspect 轮询）：探针写一行 JSON 到 `/tmp/.skillscan/<uuid>.verdict`。
4. **读裁决**（archive `GET`），随后 `rm -rf` 临时区（尽力而为）。

**探针即闸门**——它绕过运行时的 off/warn/block 配置：

- 优先 `SkillScanner().scan_skill(...)`（无运行时配置门控）；
- 在缺少该类的旧运行时上，回退 `scan_skill_directory(block=True)`——抛出的 `SkillScanError` 是 **block** finding（block 模式仅对 CRITICAL/HIGH 抛出），`None` 结果（扫描器被运行时配置禁用或技能被运行时配置白名单化）是 **block** finding，而非 pass；
- 扫描器导入失败、技能目录缺失或硬崩溃产生 `unavailable`——基础设施故障，**永不是 pass**（控制器看到裁决文件缺失即报错）。

裁决映射：CRITICAL/HIGH → `block` · MEDIUM/LOW/INFO → `warn` · 无 → `pass`。Findings 仅携带元数据（规则 id、严重度、文件、行、标题）——永不携带文件内容。

裁决按内容哈希缓存（30 分钟 TTL，100 条，FIFO）——同一缓存被上传（①）与物化（②）共享，因此指派一个刚上传的技能不产生额外 exec。

**k8s 模式（已知局限，v1）：** SPDY-exec 变体未实现；扫描失败关闭。上传标记 `scan.status="skipped"`（尽力而为语义仍适用）；物化**拒绝复制**（扫描② 强制）。Embedded/docker 集群是当前生产部署形态。

## 指派时物化（扫描②，强制）

`PUT /workers {skills}` 是指派面（已上线；无新端点）。成员调和把 worker 的有效团队名传入 `PushOnDemandSkills`：

- **团队层在名称冲突时胜出**：`SKILL.md` 存在于 `teams/<t>/skills/<s>/` 下的技能由控制器物化（下载 → 扫描② → 精确复制镜像至 `agents/<w>/skills/<s>/`）；其余技能走不变的 builtin 恢复路径（Manager push 脚本 / Worker 复制校验）；
- 扫描②结果：`block` → **不复制**，返回警告（呈现于 worker 的调和警告，非阻塞）；unavailable（无后端 / 往返失败）→ **不复制**，同一警告（闸门不默认放行）；`warn` → 复制，findings 记日志；
- 一个坏技能累加进警告，不破坏该 worker 的其他指派；
- manager 的指派路径传 `""`（非团队范围）——不查询团队层。

审计：物化输出一条结构化日志行（worker/team/skill/scan/files）。待 capability 地基（审计记录层）落地后，此处切换为正式审计记录——接缝在代码中已标注。

## 安全评审注记

- **404 抗探测**：跨团队与未知团队的读写不可区分（状态 + 响应体），L2 与 leader 皆然。
- **层隔离**：团队技能永不进入 `agents/global/skills/`；deployment 范围仅 admin；builtin 模板目录永不被本 API 写入。
- **扫描绕过路径**：(a) 运行时配置 off/warn——由探针直接调用扫描器 / 强制 block 模式绕过；(b) 白名单——被白名单化的技能返回 `None` → block finding；(c) 扫描器缺失或损坏 → unavailable → ① 跳过（记日志）、② 拒绝；(d) 非 qwenpaw manager → 无扫描容器 → unavailable，同 (c)。
- **密钥卫生**：findings 与一切日志仅携带元数据。
- **zip 安全**：在任何文件触及控制器临时目录或存储之前，先做穿越/符号链接/大小检查。

## 测试覆盖（见各 commit）

- authorizer：发布角色矩阵（admin/L2 通过；manager/leader/worker 被拒——已钉死）；
- upload：范围矩阵、结构 8 负面（缺 SKILL.md、名不匹配、无 frontmatter、双根、zip-slip、符号链接、裸根、坏名）、zip 炸弹、扫描闸门（block 422 / warn 呈现 / unavailable → skipped + 写入 / nil scanner → skipped）、精确复制替换；
- skillscan：探针输出解析、容器选择矩阵、fake-Docker 端到端、缓存/TTL/FIFO、k8s 失败关闭、载荷 tar 布局、探针契约钉死；
- materialization：正路径、精确复制替换、block 不复制、unavailable 失败关闭、团队优先于 builtin、非团队技能留在 builtin 路径、混合、独立（`teamName ""`）。
