# 治理变更 PR 模板（policyEffects）

> TH-19（批 B 治理与审批）：任何改变 **runtime 允许/预期行为** 的变更——SOUL、allowlist、
> settings 等治理文件，跨语言契约，插件代码，角色 prompt——在 PR 中必须带上 **policyEffects
> 影响面一节**。纯文档 / 测试 / CI 变更无影响面，写「无影响」即可。

## 一、何时需要 policyEffects 节

变更路径命中以下任一类别即需要（判定逻辑与
`plugins/teamharness/mcp/lifecycle/policy_effects.py` 的 `classify_change` 一致）：

| 类别 | 路径形态（示例） | 受影响面 |
|---|---|---|
| governance | `worker/SOUL.md`、`leader/allowlist.json`、`SOUL.md`（无角色段） | 该 runtime（无角色段 → 保守报全部 team 角色）；allowlist 额外改变该 runtime 的**全部工具面** |
| contract | `plugins/*/contracts/signal-mapping.json`、`task-transitions.json`、`decision-classes.json` | 全部 team 角色 + `taskflow`/`projectflow` |
| plugin-code | `plugins/*/mcp/lifecycle/**`、`project/**`、`room/**`、`artifact/**`、`server.py` 等 | 全部 team 角色 + 对应模块组的工具面（`lifecycle` → `taskflow`；`project` → `projectflow`；`room` → `roomflow`；`artifact` → `artifact`+`filesync`；共享装配 → 全部工具） |
| prompt | `plugins/*/prompts/leader/**` 等 | 该角色 |

其余（`docs/`、`README`、`.github/`、`plugins/tests/**`）→ **无影响**。

## 二、PR 模板

```markdown
## policyEffects（影响面）

<!-- 由 policy_effects.compute_policy_effects 计算；无影响时写「无影响（no impact）」 -->

- runtimes: worker
- tool surface: health, message, roomflow, filesync, artifact, projectflow, taskflow
- in-flight tasks: 2
  - t-20261103-001 (in_progress, waiting on worker)
  - t-20261103-002 (submitted, waiting on leader)

<!-- 生成方式（任选其一）：
1. 直接读 PR 描述里的手工清单（小变更）；
2. 脚本：
   git diff --name-only origin/main...HEAD > /tmp/changed.txt
   python -c "
import json, sys
sys.path.insert(0, 'plugins/teamharness')
from mcp.lifecycle import policy_effects
paths = [l.strip() for l in open('/tmp/changed.txt') if l.strip()]
print(policy_effects.format_policy_effects(
    policy_effects.compute_policy_effects(paths, workspace='.')))
"
-->
```

## 三、在跑任务如何判定

`compute_policy_effects(changes, workspace=<工作区>)` 扫描 `<工作区>/shared/tasks/*/meta.json`：

- `assigned` / `in_progress` → 等待 **worker**；
- `submitted` → 等待 **leader**（决策）；
- `planned` / `prepared`（未开跑）与终态（`completed` / `revision` / `blocked` / `cancelled`）
  永不列出。

一个任务**仅当其等待的 runtime 角色在受影响集合内**才列出——治理变更不产生在跑任务假阳性。
省略 `workspace` 时只给变更集合本身的影响面（PR 模板场景）。

## 四、与 TH-21 dispatch 四元组的衔接

派单（`delegate_task`）会把当时工作区的 **policy 指纹**
（`policy_fingerprint`，治理文件的确定性哈希；无治理文件 = 0）写进任务 meta 的
`dispatch.policy_version`。治理变更合入后，受影响任务恢复执行（`ack_task` 的
assigned→in_progress）时按当前指纹重新校验：版本变化 → 拒绝并提示按现行政策重新派单。
policyEffects 节的「在跑任务」清单即这些会被重校验的任务。
