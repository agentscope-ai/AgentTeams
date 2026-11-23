---
name: mcporter
description: Discover and call MCP Server tools via the mcporter CLI. Use when your coordinator notifies you about new MCP tools, or when you need to call external APIs. Includes workflow for generating skill documentation for new MCP servers.
---

# mcporter — MCP Tool CLI

First check your current runtime tool list. If your runtime exposes a configured
MCP server natively (including QwenPaw), call that native tool directly. Your
Controller synchronizes assigned MCP servers into your runtime; you do not need
to create a separate CLI configuration or copy gateway credentials.

## Bundled TeamHarness tools

If your native tool list does not expose TeamHarness but your image contains
`/opt/agentteams/plugins/teamharness/mcp/server.py`, use that bundled MCP server
through the supported stdio client. This does not need an external MCP config:

```bash
mcporter list --stdio python3 --stdio-arg /opt/agentteams/plugins/teamharness/mcp/server.py --json
mcporter call --stdio python3 --stdio-arg /opt/agentteams/plugins/teamharness/mcp/server.py health --output json
mcporter call --stdio python3 --stdio-arg /opt/agentteams/plugins/teamharness/mcp/server.py taskflow --args '{"action":"check_task","payload":{"taskId":"your-assigned-task"}}'
```

Use the discovered taskflow schema for start/ACK/submit/accept/complete actions.
Your runtime supplies its scope and credentials; do not import implementation
modules or create credentials to bypass MCP.

For other configured servers, use `mcporter` when your workspace already has `./config/mcporter.json`.
An empty mcporter list does not mean a native MCP server is missing. If a newly
assigned native tool is not visible yet, report that runtime synchronization is
pending and retry on the next turn; do not extract credentials or invent config.

## Commands

```bash
# List all configured MCP servers and their tool counts
mcporter list

# View a specific server's tools with full parameter schemas
mcporter list <server-name> --schema

# Call a tool — key=value syntax for simple args
mcporter call <server-name>.<tool-name> key=value key2=value2

# Call a tool — JSON syntax for complex args (arrays, objects, numbers)
mcporter call <server-name>.<tool-name> --args '{"key":"value","count":5}'
```

Output is JSON — parse with `jq` when needed.

## When Your Coordinator Notifies You About New MCP Tools

When your coordinator @mentions you saying a new MCP server has been configured, follow this workflow:

### Step 1: Pull the updated config

Use the `filesync` tool to pull the latest config files:

```json
{
  "action": "pull",
  "path": "shared/"
}
```

### Step 2: Discover the new server and its tools

```bash
# Verify the new server appears
mcporter list

# Get full tool schemas — read carefully to understand each tool
mcporter list <server-name> --schema
```

### Step 3: Generate a skill for the new MCP server

After understanding the tools, create a skill directory and SKILL.md so you have a permanent reference. Model it after your `github-operations` skill:

```bash
mkdir -p ~/skills/<server-short-name>-operations
```

Then write `~/skills/<server-short-name>-operations/SKILL.md` with:

1. Front-matter: `name`, `description`, `assign_when`
2. Overview section explaining what the MCP server does
3. For each tool: a section with description, example `mcporter call` command, and parameter notes
4. Important notes (rate limits, auth, known issues if any)

Use the tool schemas from Step 2 as the source of truth for parameter names, types, and required/optional status.

Example structure:

```markdown
---
name: weather-operations
description: Query weather data via the weather MCP server. Use when you need current weather, forecasts, or climate data.
assign_when: Worker needs weather information for a task
---

# Weather Operations

## Overview

Use this skill to query weather data via the centralized MCP Server.

## get_weather

Get current weather for a city:

\```bash
mcporter call mcp-weather.get_weather city=Tokyo units=metric
\```

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| city | string | yes | City name |
| units | string | no | Temperature units (default: metric) |
```

### Step 4: Confirm to your coordinator

After generating the skill, reply to your coordinator confirming:
- You pulled the updated config
- You discovered N tools on the new server
- You generated a skill at `~/skills/<name>-operations/SKILL.md`
- You're ready to use the new tools

## Important Notes

- **Not installed?** If `mcporter` command is not found, install it: `npm install -g mcporter`
- **Transport**: MCP Servers are configured in `config/mcporter.json`; HTTP, SSE, and local stdio command entries are supported when present in that file.
- **Auth**: Authorization header with Bearer token is auto-configured — you don't need to manage credentials
- **Permissions**: Your MCP access is controlled by your coordinator. If you get 403 from the MCP Server, ask your coordinator to re-authorize your access
- **Config not found**: If `./config/mcporter.json` doesn't exist yet, use your `file-sharing` skill first; your coordinator publishes the config after setting up MCP servers
