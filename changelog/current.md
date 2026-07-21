# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, `agentteams-controller/`, and release-facing install/chart changes here before the next release.

---

**Bug Fixes**

- **CoPaw Team coordination routing**: Route Team Leader worker assignments sent through the `message` tool from Leader DM to Team Room, matching the Matrix channel send path. ([92c8145](https://github.com/agentscope-ai/AgentTeams/commit/92c8145))
- **openai-compat provider with IP:port base URL**: Strip the port from `openaiCustomUrl` in the controller initializer and `setup-higress.sh`, so Higress derives the host domain without the port. Previously a base URL like `http://10.43.46.12:3000/v1` produced host domain `10.43.46.12:3000`, breaking DNS resolution; the port is now supplied only via `openaiCustomServicePort`. (#1057)

**Branding and Compatibility**

- **Complete AgentTeams runtime rename**: Rename installer and Helm entrypoints, the controller Go module and CLI, and container filesystem paths to AgentTeams while preserving thin compatibility aliases and upgrade migration for existing HiClaw installations. ([3121f5f](https://github.com/agentscope-ai/AgentTeams/commit/3121f5f))
