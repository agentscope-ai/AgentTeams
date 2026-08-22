# MCP Server API Commands

Direct Higress Console API calls for listing, inspecting, deleting servers, and managing consumer access.

## List servers

```bash
curl -s http://127.0.0.1:8001/v1/mcpServers -b "${HIGRESS_COOKIE_FILE}" | jq
```

## Get server details

```bash
curl -s "http://127.0.0.1:8001/v1/mcpServer?name=<mcp-server-name>" -b "${HIGRESS_COOKIE_FILE}" | jq
```

## Delete server

```bash
curl -X DELETE "http://127.0.0.1:8001/v1/mcpServer?name=<mcp-server-name>" -b "${HIGRESS_COOKIE_FILE}"
```

## Consumer authorization

The setup script handles this automatically. For manual adjustments:

```bash
# Authorize consumers (REPLACE operation — include ALL consumers)
curl -X PUT http://127.0.0.1:8001/v1/mcpServer/consumers \
  -b "${HIGRESS_COOKIE_FILE}" \
  -H 'Content-Type: application/json' \
  -d '{"mcpServerName":"<name>","consumers":["manager","worker-alice"]}'

# Revoke a consumer from ALL MCP servers
curl -X DELETE "http://127.0.0.1:8001/v1/mcpServer/consumers?consumer=worker-alice" \
  -b "${HIGRESS_COOKIE_FILE}"
```

## Tool-level access control

Add `allowTools` to the YAML's `server` section:

```yaml
server:
  name: github-mcp-server
  config:
    accessToken: "..."
  allowTools:
    - search_repositories
    - get_file_contents
```

When set, only listed tools are exposed. Re-run setup script to apply.
