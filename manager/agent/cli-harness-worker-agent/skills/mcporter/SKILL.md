---
name: mcporter
description: Call MCP Server tools via the mcporter CLI. Use when a task requires a tool exposed through your coordinator's MCP server configuration.
---

# MCP Tools via mcporter (CLI-harness Worker)

If `~/config/mcporter.json` exists, your coordinator has authorized MCP Servers for you. Call their tools with the `mcporter` CLI.

## Usage

```bash
# List configured servers and their tools
mcporter --config ~/config/mcporter.json list

# Call a tool
mcporter --config ~/config/mcporter.json call <server>.<tool> '{"arg": "value"}'
```

## Notes

- Your access is scoped by your coordinator — if a tool call is rejected, report it in your reply instead of retrying
- Tool output can be large; save it to a file in the task directory when you need to reference it later
- If `~/config/mcporter.json` does not exist, you have no MCP authorization — tell your coordinator which tools the task needs
