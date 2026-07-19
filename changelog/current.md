# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `openclaw-base/` here before the next release.

---

**Bug Fixes**

- **CoPaw Team coordination routing**: Route Team Leader worker assignments sent through the `message` tool from Leader DM to Team Room, matching the Matrix channel send path. ([92c8145](https://github.com/agentscope-ai/AgentTeams/commit/92c8145))
- **openai-compat provider with IP:port base URL**: Strip the port from `openaiCustomUrl` in the controller initializer and `setup-higress.sh`, so Higress derives the host domain without the port. Previously a base URL like `http://10.43.46.12:3000/v1` produced host domain `10.43.46.12:3000`, breaking DNS resolution; the port is now supplied only via `openaiCustomServicePort`. (#1057)
