# Skill 格式与贡献指南

HiClaw Skill 是面向 Agent 的 Markdown 包。一个 Skill 应该足够自包含，让 Agent 能判断何时使用、理解可用命令，并在不阅读无关文档的情况下完成安全操作。

## Skill 位置

- `manager/agent/skills/<skill-name>/` - Manager 内置 Skill，由 OpenClaw 和 QwenPaw Manager 共享。
- `manager/agent/worker-skills/<skill-name>/` - 可按需分发给指定 Worker 的 Skill。
- `manager/agent/<runtime>-worker-agent/skills/<skill-name>/` - 各 Worker 运行时的内置 Skill。
- `manager/agent/team-leader-agent/skills/<skill-name>/` - Team Leader Skill。

运行中安装的自定义 Manager Skill 位于 `agents/manager/skills/<skill-name>/`，Manager 会自动发现。

## 目录结构

每个 Skill 使用一个独立目录：

```text
<skill-name>/
├── SKILL.md
├── scripts/       # 可选：可执行辅助脚本
└── references/    # 可选：较长文档或 API 参考
```

`SKILL.md` 应聚焦在加载条件和操作流程。较长的 API 参考、模板或示例放在 `references/`；可复用命令放在 `scripts/`。

## `SKILL.md` Frontmatter

每个 `SKILL.md` 都必须以 YAML frontmatter 开头：

```yaml
---
name: github-operations
description: Work with GitHub issues and pull requests from a Worker.
assign_when: Give this skill to Workers that need to inspect repositories, triage issues, or submit pull requests.
---
```

必填字段：

- `name` 必须与目录名一致，并使用小写 kebab-case。
- `description` 用一句短句说明 Skill 提供什么能力。

`manager/agent/worker-skills/` 下的 Worker Skill 还必须包含 `assign_when`。该字段描述什么角色或职责的 Worker 应获得该 Skill，不要写实现细节。

## 写作规则

- 面向 Agent 的内容使用第二人称，例如写“Use this script...”，不要写“The Manager can use this script...”。
- 当路径上下文重要时，命令示例使用完整路径，例如 `/opt/hiclaw/agent/skills/.../scripts/<name>.sh`。
- 在破坏性操作或外部系统操作前说明前置条件和副作用。
- 可重复逻辑优先放入脚本，不要在 Markdown 中堆叠长命令。
- 示例保持精简；命令参数变化时同步更新示例。

## 新增 Skill

1. Manager Skill 放在 `manager/agent/skills/<skill-name>/`；可分发 Worker Skill 放在 `manager/agent/worker-skills/<skill-name>/`。
2. 编写 `SKILL.md`，包含 frontmatter、使用规则、示例，以及必要的 reference 链接。
3. 如果流程需要重复执行的 shell 逻辑，将脚本放到 `scripts/`。
4. Worker Skill 需要写清 `assign_when`，方便 Manager 选择合适的 Worker。
5. 提交 PR 前，使用代表性参数直接测试脚本。

## Review Checklist

- `name` 与目录名一致。
- Frontmatter 是合法 YAML。
- 命令使用稳定路径，并正确引用用户输入值。
- Agent-facing 内容使用第二人称。
- 如修改 `manager/`、`worker/`、`copaw/`、`hermes/`、`openclaw-base/` 或 `hiclaw-controller/` 等会进入镜像的内容，需要更新 `changelog/current.md`。
