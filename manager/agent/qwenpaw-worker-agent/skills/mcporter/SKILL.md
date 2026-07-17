---
name: mcporter
description: Discover and call MCP Server tools via the mcporter CLI. Use when your coordinator notifies you about new MCP tools, or when you need to call external APIs.
---

# mcporter — MCP Tool CLI

You call MCP Server tools via `mcporter`. Your config lives at `config/mcporter.json` in your workspace (mcporter's default lookup path — no `--config` flag needed).

That file is **controller-managed**: your worker daemon applies `desired.mcpServers` from `runtime.yaml` into `config/mcporter.json` on the desired-state loop. Do not hand-edit it — if a server is missing, ask your coordinator to authorize it.

> Note: `config/mcporter.json` (for the `mcporter` CLI) is separate from any QwenPaw-native package `mcp.json`. This skill is about the `mcporter` CLI.

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

1. **Wait for reconcile** — your daemon applies the new server to `config/mcporter.json` from `runtime.yaml`; you don't pull it manually.
2. **Discover** — `mcporter list`, then `mcporter list <server-name> --schema` to read each tool's parameters.
3. **Confirm to your coordinator** — reply that you see the new server and are ready to use its tools.

## Important Notes

- **Not installed?** If `mcporter` is not found, install it: `npm install -g mcporter`.
- **Transport**: MCP Servers use HTTP transport (configured in `config/mcporter.json`).
- **Auth**: The Authorization header (Bearer token) is auto-configured — you don't manage credentials.
- **Permissions**: Your MCP access is controlled by your coordinator. If you get 403, ask your coordinator to re-authorize your access.
- **Config not present yet?** Wait for the next desired-state reconcile; your coordinator projects it via `runtime.yaml`.
