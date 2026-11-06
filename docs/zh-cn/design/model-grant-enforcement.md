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

`有效 = 显式行 ∪ 隐式注解 ∪ 当前值`

- **显式行** — `Human.spec.modelAccess` 作用域，或引用的
  `ModelAccessTable`（`GET /model-access` 按调用者解析）。
- **隐式注解** — Worker 上的 `agentteams.io/model-grants`。L1 在创建及之后
  每次模型写入时追加所选对（去重，≤ 32 条）。L2 永远写不了该注解。
- **当前值** — Worker 此刻实际运行的模型，保证“值没变”不会被授权变更锁死。

隐式注解上限 32 条；显式行（校验上限 200）永不被截断。注解超限返回 `409`
`model_grant_overflow` 并带条数，绝不静默截断视图。

## 撤权契约

撤销某个作用域会立即移除其显式行，但**不会**改写隐式注解：这些行记录的是
L1 曾经授予过的模型，且当前值继续运行。因此撤权后：

- 当前值仍可写回（值未变）；
- 存在于 L1 注解里的值仍可选择；
- 其它值一律拒绝（`model_not_allowed`），未知 provider 返回
  `provider_not_allowed`。

要移除某个历史值，需要 L1 做一次不再选择它的编辑——注解是只追加的历史，
不是 Human 当前作用域的缓存。

## 非目标（后续跟踪）

- 原生 runtime-config 策略钩子（`native_config_*` 403、
  `model_pair_unresolvable`）——依赖运行时配置线。
- 授权物化到运行时文件——依赖重启/热应用路径。
- 目录校验 / `available` / 陈旧检测（A2）与 runner 默认模型（B2）。
