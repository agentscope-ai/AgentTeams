# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

- fix(manager): reject `--mcp-servers` during Worker runtime switches because MCP authorization must be applied separately after the replacement container converges. ([#1053](https://github.com/agentscope-ai/AgentTeams/pull/1053))
