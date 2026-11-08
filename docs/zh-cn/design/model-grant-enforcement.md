# 模型授权强制

状态：已实现（PR-B1，承接 #1513）
API：`GET /api/v1/model-access?worker=`、`PUT /api/v1/workers/{name}`、
`PUT /api/v1/workers/{name}/coding-cli/{cli}/settings`

## 强制范围

`#1513` 定义了按人的模型授权与读取契约，本改动让授权在两个写入口生效：

| 写入口 | 之前 | 之后 |
|:--|:--|:--|
| `PUT /workers/{name}`（`model`、`modelProvider`、`subagentModel`） | L2 人类硬禁止 | 按授权放行：`(provider, model)` 对必须在调用者的有效集合内；`subagentModel: ""` 需要 spawn inherit 授权 |
| `PUT /workers/{name}/coding-cli/{cli}/settings`（`model`） | 不限制 | 按该 CLI 的集合放行；定义 provider 的键（`security`、`provider(s)`、`baseUrl`、`env`）仍仅 L1 可写 |
| Team leader | 只读 | 不变（只读） |
| L1（admin/manager） | 不限制 | 不限制；同时写入隐式授权记录 |

只检查“值变化”：把当前值原样写回永远放行，哪怕一条授权都没有。改任一
半都按结果对判定，所以“同名模型换 provider”混不过去；只发
`modelProvider` 返回 `model_pair_required`。

## 有效集合

`有效 = 显式行 ∪ 当前值`

- **显式行** — `Human.spec.modelAccess` 作用域，或引用的
  `ModelAccessTable`（`GET /model-access` 按调用者解析）。
- **隐式注解** — Worker 上的 `agentteams.io/model-grants`。L1 在创建及之后
  每次模型写入时追加所选对（去重，≤ 32 条）。它只保留历史，不授予后续选择权限。L2 不能改写它。
- **当前值** — Worker 此刻实际运行的模型，保证“值没变”不会被授权变更锁死。

隐式注解上限 32 条；显式行（校验上限 200）永不被截断。注解超限返回 `409`
`model_grant_overflow` 并带条数，绝不静默截断视图。

## 撤权契约

撤销作用域或删除授权表中的行，立即取消后续选择权限。Worker 注解仅保留 L1
历史记录，不再恢复已经撤销的权限。当前模型可以继续运行、原样保存；切换走后
再选回历史模型，必须有当前显式授权。

## 原生配置保存与回滚

生产构造器为原生配置 PUT 和快照回滚接入相同授权。在每个 Worker 的修改锁内，
对准确的当前文件与合并/恢复后的完整原生文档检查，拒绝后不写文件、快照或期望状态。

OpenClaw 主模型与备用模型检查 main 授权，子模型检查 spawn 授权；Hermes 明确的
主/子模型对使用对应授权。provider 定义、凭据、选择策略及辅助/不透明路由由 L1
管理。非模型设置和未改变的值仍可编辑，Team Leader 保持只读。
无法明确对应 provider 的裸模型、auto 选择和 Hermes custom 路由别名返回
`403 model_pair_unresolvable`，不把运行时别名当作已验证的网关 provider 身份。

回归覆盖不同 Human、历史撤权、拒绝后文件与快照不变，以及 Hermes 真实临时文件
命令；不代表部署环境 Matrix 登录或真实模型热生效验收。

## 非目标（后续跟踪）

- 无法明确解析的运行时别名仍需补验证过的 provider 绑定。
- 授权物化到运行时文件——依赖重启/热应用路径。
- 目录校验 / `available` / 陈旧检测（A2）与 runner 默认模型（B2）。
