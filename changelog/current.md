# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **Team Room membership convergence**: Explicitly join Team Leaders and Workers with their own Matrix tokens after invitation, so pending invites cannot leave a Team Active while Hermes members remain unreachable. ([41f6e517](https://github.com/agentscope-ai/AgentTeams/commit/41f6e5175a6fb8bf1c7ee19632d3d2fac6f66f26), [#1142](https://github.com/agentscope-ai/AgentTeams/issues/1142))

---

**Bug 修复**

- **Team Room 成员收敛**：邀请 Team Leader 和 Worker 后，使用各自的 Matrix token 显式加入 Team Room，避免 Team 已处于 Active 状态时 Hermes 成员仍停留在 invite、无法接收消息。([41f6e517](https://github.com/agentscope-ai/AgentTeams/commit/41f6e5175a6fb8bf1c7ee19632d3d2fac6f66f26), [#1142](https://github.com/agentscope-ai/AgentTeams/issues/1142))
