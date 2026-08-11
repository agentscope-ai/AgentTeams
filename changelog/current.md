# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **Duplicate Team creation guard**: Treat relationship and topology questions as read-only, verify a requested Team name against a valid current Team list, and fail closed when that lookup cannot be completed. ([#987](https://github.com/agentscope-ai/AgentTeams/issues/987))

---

**Bug 修复**

- **重复 Team 创建防护**：将关系与拓扑问题视为只读查询，创建前使用有效的当前 Team 列表核验目标名称，并在列表查询失败时停止创建。([#987](https://github.com/agentscope-ai/AgentTeams/issues/987))

---
