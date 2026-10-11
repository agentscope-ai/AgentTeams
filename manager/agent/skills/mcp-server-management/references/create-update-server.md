# Create / Update MCP Server

> **Cloud deployment (SAE) note:** The `setup-mcp-server.sh` script and Higress Console API commands are only available in local deployment mode. In cloud mode (`AGENTTEAMS_RUNTIME=aliyun`), direct admin to the Alibaba Cloud AI Gateway console.

## Setup Script

```bash
bash /opt/agentteams/agent/skills/mcp-server-management/scripts/setup-mcp-server.sh \
  <server-name> <credential-value> [--yaml-file <path>] [--api-domain <domain>]
```

| Argument | Required | Description |
|---|---|---|
| `server-name` | yes | Without `mcp-` prefix (e.g., `github`, `weather`) |
| `credential-value` | yes | The credential (GitHub PAT, API key, etc.) |
| `--yaml-file` | no | User-provided YAML config. Required when no built-in template exists |
| `--api-domain` | no | Use `[http://\|https://]host[:port]`. Required when your YAML URLs use variables instead of literal domains |

### Examples

```bash
# Built-in template: GitHub — domain auto-extracted
bash .../setup-mcp-server.sh github "ghp_xxxxxxxxxxxx"

# User-provided YAML: custom service
bash .../setup-mcp-server.sh weather "my-key" \
    --yaml-file /tmp/mcp-weather.yaml --api-domain "api.weather.com"
```

### Docker-internal HTTP services

Use an explicit HTTP scheme and your service's listening port:

```bash
bash .../setup-mcp-server.sh security-tool "your-key" \
    --yaml-file /tmp/mcp-security-tool.yaml \
    --api-domain "http://security-tool.example.local:8080"
```

Attach your tool container to `agentteams-net` with a dotted network alias such as
`security-tool.example.local`; Higress DNS service-source validation rejects
single-label names. Use the same reachable address in your YAML request URLs.
Do not include a path, query, or credentials in `--api-domain`.

When you omit the scheme, you retain the existing HTTPS default: `host` uses port
443 and `host:8080` uses HTTPS on port 8080. Use `http://host` for HTTP on port 80,
or specify the port explicitly. When you omit `--api-domain`, you continue to use
the scheme and port extracted from the first YAML request URL.

### What the script does

1. Registers DNS service source (auto-extracted from YAML or `--api-domain`)
2. Substitutes `accessToken: ""` with real credential
3. Creates/updates MCP Server via `PUT /v1/mcpServer` (upsert)
4. Updates Manager's `config/mcporter.json`
5. Authorizes all existing Workers and updates their `config/mcporter.json` (pushed to MinIO)

Fully idempotent — safe to re-run for credential rotation or updates.

### YAML resolution order

1. `--yaml-file` flag (highest priority)
2. Built-in template at `references/mcp-<server-name>.yaml`
3. Error with list of available templates

### When to use

- User provides a credential via chat
- User asks to enable a new integration or rotate a credential
- User provides a custom YAML config
- `AGENTTEAMS_GITHUB_TOKEN` was empty during install and user provides it later

## After running

1. Wait ~10s for auth plugin to activate
2. Verify with mcporter skill before notifying Workers:
   - `mcporter list` — confirm server appears
   - `mcporter list <server-name> --schema` — review tools
   - Call at least one tool to verify end-to-end
   - If any test fails, debug before proceeding
3. Confirm to user
4. Notify only relevant Workers (not all):
   ```
   @{worker}:{domain} New MCP server `{name}` configured with tools: {tool list from YAML}.
   Please file-sync to pull updated config, then use mcporter to discover the new tools.
   ```

## Built-in templates

| Template | Server Name | Description |
|---|---|---|
| `mcp-github.yaml` | `mcp-github` | GitHub: repos, issues, PRs, code search |

All other services require user-provided YAML via `--yaml-file`.
