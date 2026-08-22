---
name: mcp-server-management
description: Use when admin asks to configure an MCP tool server (e.g., GitHub, weather API), rotate credentials, grant/revoke worker access to MCP tools, or add a custom API integration via YAML.
---

# MCP Server Management

MCP Servers expose REST APIs as tools on the Higress AI Gateway, or proxy existing MCP servers (SSE/StreamableHTTP). Use `setup-mcp-server.sh` to create/update from YAML templates, or `setup-mcp-proxy.sh` to proxy existing MCP servers. Built-in templates are at `references/mcp-*.yaml`.

## Gotchas

- **Cloud mode (`HICLAW_RUNTIME=aliyun`) does not support script-based MCP management** — direct admin to the Alibaba Cloud AI Gateway console instead
- **Auth plugin takes ~10s to activate** after server creation/update — always wait and verify with mcporter before notifying Workers
- **Always verify end-to-end before notifying Workers** — call at least one tool via mcporter to confirm connectivity. Do not push broken tools
- **Never echo credentials in chat messages** — credentials are stored only in Higress config, Workers access via gateway proxy
- **Consumer authorization is a REPLACE operation** — when calling the API manually, include ALL consumers (manager + all workers), not just the new one
- **All YAML configs must use `accessToken: ""` as the credential key** — the setup script substitutes the real value
- **Only notify relevant Workers** — do not broadcast to all Workers, only those whose role/task needs the new tool
- **MCP Server SSE endpoint always returns 200** — auth is checked on `POST /mcp/<name>/message`, not on SSE connect
- **mcp-proxy only supports `http` and `sse` transport** — `stdio` servers cannot be proxied through the gateway

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Create/update an MCP server or rotate credentials | `references/create-update-server.md` |
| Generate YAML for a custom API integration | `references/custom-yaml-guide.md` |
| List, inspect, delete servers, or manage consumer access manually | `references/api-commands.md` |
| Proxy an existing MCP server (SSE/HTTP) | `references/setup-mcp-proxy.md` |
| Need the GitHub MCP tool definitions | `references/mcp-github.yaml` |
