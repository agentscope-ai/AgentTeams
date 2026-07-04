---
name: mcporter
description: Discover and call MCP Server tools via the mcporter CLI. Use when your coordinator notifies you about new MCP tools, or when you need to call external APIs. Includes workflow for generating skill documentation for new MCP servers.
---

# mcporter — MCP Tool CLI

You call MCP Server tools via `mcporter`. Your config is at `/home/openhuman/.openhuman/config/mcporter.json` (mcporter's default path — no `--config` flag needed).

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

Run your file-sync skill to pull the latest files from MinIO.

### Step 2: Discover the new server and its tools

```bash
# Verify the new server appears
mcporter list

# Get full tool schemas — read carefully to understand each tool
mcporter list <server-name> --schema
```

### Step 3: Generate a skill for the new MCP server

After understanding the tools, create a skill directory and SKILL.md so you have a permanent reference:

```bash
mkdir -p /home/openhuman/.openhuman/skills/<server-short-name>-operations
```

Then write `/home/openhuman/.openhuman/skills/<server-short-name>-operations/SKILL.md` with:

1. Front-matter: `name`, `description`, `assign_when`
2. Overview section explaining what the MCP server does
3. For each tool: a section with description, example `mcporter call` command, and parameter notes
4. Important notes (rate limits, auth, known issues if any)

### Step 4: Confirm to your coordinator

After generating the skill, reply to your coordinator confirming:
- You pulled the updated config
- You discovered N tools on the new server
- You generated a skill at the new path
- You're ready to use the new tools

## Important Notes

- **Not installed?** If `mcporter` command is not found, install it: `npm install -g mcporter`
- **Transport**: MCP Servers use HTTP transport (configured in config/mcporter.json)
- **Auth**: Authorization header with Bearer token is auto-configured — you don't need to manage credentials
- **Permissions**: Your MCP access is controlled by your coordinator. If you get 403 from the MCP Server, ask your coordinator to re-authorize your access
- **Config not found**: If `/home/openhuman/.openhuman/config/mcporter.json` doesn't exist yet, use your file-sync skill first — your coordinator pushes the config to MinIO after setting up MCP servers
