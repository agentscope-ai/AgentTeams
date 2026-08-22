# Worker Skills 仓库

这个目录是所有可分配给 Worker 的 skills 的中央仓库。Manager 负责管理这些 skills 的定义，并通过 `push-worker-skills.sh` 将其分发给特定 Worker。

## 目录结构

```
worker-skills/
├── README.md                  # 本文件
└── <skill-name>/
    └── SKILL.md               # Skill 的说明（必须包含 frontmatter，见下）
    └── scripts/               # （可选）Skill 附带的脚本
```

## SKILL.md 格式要求

每个 `SKILL.md` **必须**以 YAML frontmatter 开头，包含 `assign_when` 字段：

```yaml
---
name: <skill-name>
description: <一句话说明这个 skill 做什么>
assign_when: <描述：什么样的 Worker 应该拥有此 skill，Manager 据此自动决定是否分配>
---
```

字段说明：
- `description`：简要说明 skill 的功能，供 Manager 和 human 快速了解
- `assign_when`：用自然语言描述 Worker 的**角色特征**或**职责范围**，Manager 在创建 Worker 时据此判断是否分配；不要写技术实现细节，只描述"什么样的人需要这个工具"

## 如何新增自定义 Skill

1. 在 `~/worker-skills/<skill-name>/` 下创建 skill 目录（Manager workspace 与 MinIO 通过 `push-worker-skills.sh` 同步，直接写这里即可持久化）
2. 编写 `SKILL.md`，**开头必须包含 frontmatter**（`name` + `assign_when`），后续正文说明使用方式
3. 如需脚本，放在 `<skill-name>/scripts/` 下
4. 使用 `push-worker-skills.sh --worker <name> --add-skill <skill-name>` 分配给 Worker

## 如何分配/更新 Skills

```bash
# 给指定 Worker 分配新 skill
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh \
  --worker <name> --add-skill <skill-name>

# 推送某个 skill 的更新到所有持有该 skill 的 Worker
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh \
  --skill <skill-name>

# 查看当前 Worker skill 分配情况
cat ~/workers-registry.json
```

## 注意

- 内置 skills（`file-sync`、`task-progress`、`project-participation`、`mcporter`、`find-skills`）由 Worker 镜像自动分配，无需通过此目录管理
- 此目录中的 skills 由 Manager 统一维护，Worker 不能修改自己的 skills
- Worker 的 on-demand skill 分配记录在 `~/workers-registry.json`

## 内置 Skills

| Skill | 说明 |
|-------|------|
| `file-sync` | Worker 与 MinIO 的文件同步 |
| `task-progress` | 任务执行进度跟踪 |
| `project-participation` | 多 Worker 项目协作 |
| `mcporter` | MCP Server 工具调用 |
| `find-skills` | 从 Agent Skills 生态系统发现和安装技能 |

### find-skills 配置

环境变量：
- `SKILLS_API_URL`：自定义技能库 API 端点（可选，默认 https://skills.sh）
