# Proxy an Existing MCP Server

> **Cloud deployment (SAE) note:** The `setup-mcp-proxy.sh` script is only available in local deployment mode. In cloud mode (`HICLAW_RUNTIME=aliyun`), direct admin to the Alibaba Cloud AI Gateway console.

## Setup Script

```bash
bash /opt/hiclaw/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh \
  <server-name> <url> <transport> [--header "Key: Value"] ...
```

| Argument | Required | Description |
|---|---|---|
| `server-name` | yes | Without `mcp-` prefix (e.g., `sentry`, `notion`) |
| `url` | yes | Backend MCP server URL (e.g., `https://mcp.sentry.dev/mcp`) |
| `transport` | yes | `http` (StreamableHTTP) or `sse` (Server-Sent Events). `stdio` is not supported |
| `--header` | no | Repeatable. Auth header for the backend (e.g., `"Authorization: Bearer xxx"`) |

### Examples

```bash
# No auth
bash .../setup-mcp-proxy.sh sentry https://mcp.sentry.dev/mcp http

# Bearer token auth
bash .../setup-mcp-proxy.sh notion https://mcp.notion.com/mcp http \
    --header "Authorization: Bearer ntn_xxx"

# API Key auth
bash .../setup-mcp-proxy.sh asana https://mcp.asana.com/sse sse \
    --header "X-API-Key: my-key"

# Multiple headers
bash .../setup-mcp-proxy.sh custom https://mcp.example.com/mcp http \
    --header "Authorization: Bearer xxx" \
    --header "X-Tenant-Id: my-tenant"
```

### What the script does

1. Registers DNS service source (domain extracted from URL)
2. Generates mcp-proxy YAML config with auth (if `--header` provided)
3. Creates/updates MCP Server via `PUT /v1/mcpServer` (upsert, type `OPEN_API`)
4. Updates Manager's `config/mcporter.json`
5. Authorizes all existing Workers and updates their `config/mcporter.json` (pushed to MinIO)

Fully idempotent — safe to re-run to update URL or headers.

### Auth header handling

The script auto-detects the header type and generates the appropriate Higress `securitySchemes`:

| Header | Generated scheme |
|---|---|
| `Authorization: Bearer <token>` | `type: http, scheme: bearer` |
| `Authorization: Basic <b64>` | `type: http, scheme: basic` |
| `X-Custom-Key: <value>` | `type: apiKey, in: header, name: X-Custom-Key` |

### When to use

- User wants to connect an existing MCP server (not a REST API)
- User provides an MCP server URL with SSE or StreamableHTTP transport
- User mentions services like Sentry, Notion, Asana, GitHub MCP, etc.

### When NOT to use

- User wants to wrap a REST API as MCP tools → use `setup-mcp-server.sh` instead
- User provides a `stdio` command → not supported through the gateway

## After running

1. Wait ~10s for auth plugin to activate
2. Verify with mcporter skill before notifying Workers:
   - `mcporter list` — confirm server appears
   - `mcporter list <server-name> --schema` — review tools (provided by backend MCP server)
   - Call at least one tool to verify end-to-end
   - If any test fails, debug before proceeding
3. Confirm to user
4. Notify only relevant Workers (not all):
   ```
   @{worker}:{domain} New MCP proxy server `{name}` configured.
   Please file-sync to pull updated config, then use mcporter to discover the new tools.
   ```
