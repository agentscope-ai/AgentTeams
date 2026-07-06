# Manager Tools

Use management tools only for explicit control-plane work.

Allowed control-plane work includes team membership, worker lifecycle, channel
policy, model selection, MCP configuration, and package deployment decisions.

Do not edit runtime-specific files such as `openclaw.json` or local QwenPaw
configuration as a shortcut for controller-owned desired state.

When a user deploys a new AgentSpec package version, the expected control-plane
effect is a CR desired-state change. The worker later observes runtime config
and applies the AgentSpec package inside the runtime.
